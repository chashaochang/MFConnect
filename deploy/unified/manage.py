#!/usr/bin/env python3
"""Cloud-only DNS/TLS/ingress management. Default is a plan, never a mutation."""
import argparse
import fcntl
import ipaddress
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import time
from urllib.parse import urlsplit

from tencent import call

LABEL = re.compile(r'[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\Z')


def dns_name(value):
    if len(value) > 253 or '.' not in value or not all(LABEL.fullmatch(p) for p in value.split('.')):
        raise ValueError('Invalid DNS hostname')
    return value


def configuration(c, require_dns=True):
    u = urlsplit(c['entry_origin'])
    if u.scheme != 'https' or u.netloc != u.hostname or u.path or u.query or u.fragment:
        raise ValueError('entry_origin must be a plain HTTPS DNS origin')
    host = dns_name(u.hostname)
    try:
        ipaddress.ip_address(host)
    except ValueError:
        pass
    else:
        raise ValueError('Entry must use a DNS name, not an IP literal')
    if len(host) > 216:
        raise ValueError('Entry is too long to accommodate a 36-character application label')
    if require_dns:
        zone = dns_name(c['zone'])
        if host != zone and not host.endswith('.' + zone):
            raise ValueError('Entry must belong to the explicitly selected DNS zone')
        ipaddress.IPv4Address(c['ipv4'])
    c = dict(c, host=host)
    names = [host, '*.' + host]
    ingress = [host, '*.' + host]
    for key, wildcard in [('legacy_api', False), ('legacy_apps', True), ('legacy_browser', True), ('tunnel_host', False)]:
        value = c.get(key)
        if value:
            dns_name(value)
            names.append(value)
            if key != 'tunnel_host':
                ingress.append(value)
            if wildcard:
                names.append('*.' + value)
                ingress.append('*.' + value)
    c['sans'] = list(dict.fromkeys(names))
    c['ingress'] = list(dict.fromkeys(ingress))
    # These paths are emitted in nginx syntax. Do not allow nginx directives.
    for key in ['deployment_dir', 'trust_bundle', 'acme'] + (['downloads_root'] if c.get('downloads_root') else []) + (['nginx_binary'] if c.get('nginx_binary') else []):
        value = c[key]
        if not re.fullmatch(r'/[A-Za-z0-9_./-]+', value) or '..' in Path(value).parts:
            raise ValueError('Use an absolute path without spaces or directives: ' + key)
    if require_dns and c.get('dns_provider', 'tencent') != 'tencent':
        raise ValueError('Automated DNS currently supports Tencent DNSPod only')
    if c.get('acme_dns', 'dns_tencent') not in ('dns_tencent', 'dns_cf', 'dns_ali'):
        raise ValueError('Unsupported ACME DNS provider')
    if 'dns_sleep' in c and (type(c['dns_sleep']) is not int or not 30 <= c['dns_sleep'] <= 600):
        raise ValueError('dns_sleep must be an integer between 30 and 600 seconds')
    return c


def dns_records(c):
    relative = '@' if c['host'] == c['zone'] else c['host'][:-(len(c['zone']) + 1)]
    return [relative, '*' if relative == '@' else '*.' + relative]


def configure_dns(c, rpc=call):
    # Preflight both names before writing anything. Existing aliases are retained.
    missing = []
    for name in dns_records(c):
        records = rpc('DescribeRecordList', {'Domain': c['zone'], 'Subdomain': name}).get('RecordList', [])
        active = [r for r in records if r.get('Type') in ('A', 'AAAA', 'CNAME', 'NS')]
        if active and not all(r['Type'] == 'A' and r['Value'] == c['ipv4'] and r.get('Status', 'ENABLE') == 'ENABLE' for r in active):
            raise ValueError('Existing DNS conflicts at ' + name + '; no records overwritten')
        if not active:
            missing.append(name)
    for name in missing:
        rpc('CreateRecord', {'Domain': c['zone'], 'SubDomain': name, 'RecordType': 'A',
                            'RecordLine': '默认', 'Value': c['ipv4'], 'TTL': 600})


def nginx(c):
    # Nginx wildcard server_name also accepts deeper labels. The application
    # server performs exact host matching and rejects those deeper hosts.
    root = c['deployment_dir']
    upstream = ('set $mf_upstream mfconnect:8443;\n        proxy_pass https://$mf_upstream;'
                if c.get('container_proxy') else 'proxy_pass https://127.0.0.1:18443;')
    def tls_server(names, downloads=False):
        download_location = ''
        if downloads:
            download_location = f'''
    location ^~ /downloads/mfconnect/ {{
        root {c['downloads_root']};
        try_files $uri =404;
        autoindex off;
        default_type application/octet-stream;
        add_header Cache-Control "no-cache" always;
        add_header X-Content-Type-Options "nosniff" always;
        limit_except GET {{ deny all; }}
    }}
'''
        return f'''server {{
    listen 443 ssl;
    server_name {' '.join(names)};
    ssl_certificate {root}/certs/fullchain.pem;
    ssl_certificate_key {root}/certs/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_session_tickets off;
    # No access logs: installer paths contain one-use credentials.
    access_log off;
{download_location}    location / {{
        {upstream}
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $mf_connection_upgrade;
        proxy_set_header X-MF-Client-IP $remote_addr;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto https;
        proxy_ssl_server_name on;
        proxy_ssl_name $host;
        proxy_ssl_verify on;
        proxy_ssl_verify_depth 4;
        proxy_ssl_trusted_certificate {c['trust_bundle']};
        proxy_cache off;
        proxy_buffering off;
        proxy_request_buffering off;
        proxy_next_upstream off;
        proxy_redirect off;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
        client_max_body_size 0;
    }}
}}
'''
    rendered = f'''# Install inside nginx's http context; one copy of the map per instance.
map $http_upgrade $mf_connection_upgrade {{ default upgrade; '' close; }}
server {{
    listen 80;
    server_name {' '.join(c['ingress'])};
    return 308 https://$host$request_uri;
}}
'''
    if c.get('container_proxy'):
        rendered = 'resolver 127.0.0.11 valid=10s ipv6=off;\n' + rendered
    if c.get('downloads_root'):
        # Only infrastructure roots serve release artifacts. NAS and application
        # hosts retain their own /downloads paths without shadowing by nginx.
        hosts = list(dict.fromkeys(filter(None, [c['host'], c.get('legacy_api')])))
        rendered += tls_server(hosts, downloads=True)
        remaining = [host for host in c['ingress'] if host not in hosts]
        if remaining:
            rendered += tls_server(remaining)
    else:
        rendered += tls_server(c['ingress'])
    return rendered


def environment(c):
    return '\n'.join(f'{k}={v}' for k, v in {
        'MF_ENTRY_ORIGIN': c['entry_origin'], 'MF_API_HOST': c.get('legacy_api', ''),
        'MF_BASE_DOMAIN': c.get('legacy_apps', ''), 'MF_CONNECT_HOST': c.get('legacy_browser', ''),
        'MF_TUNNEL_HOST': c.get('tunnel_host', ''), 'MF_HTTPS_BIND': '127.0.0.1:18443',
    }.items()) + '\n'


def run(args, **kw):
    return subprocess.run(args, check=True, **kw)


def certificate(c, renew=False):
    root = Path(c['deployment_dir'])
    staging = root / 'certs' / 'pending'
    staging.mkdir(parents=True, exist_ok=True, mode=0o700)
    cmd = [c['acme'], '--renew' if renew else '--issue', '--server', 'letsencrypt']
    if renew:
        cmd += ['-d', c['host'], '--ecc']
    else:
        cmd += ['--dns', c.get('acme_dns', 'dns_tencent'), '--keylength', 'ec-256']
        for name in c['sans']:
            cmd += ['-d', name]
    if c.get('dns_sleep'):
        # Some hosts cannot reach acme.sh's public DoH preflight service. The
        # certificate authority still performs its own DNS-01 validation.
        cmd += ['--dnssleep', str(c['dns_sleep'])]
    result = subprocess.run(cmd, check=False)
    if renew and result.returncode == 2:  # acme.sh: renewal not due
        return False
    if result.returncode not in (0, 2):
        raise RuntimeError('ACME failed; existing certificate remains active')
    # Use the public installation interface, never ~/.acme.sh internal files.
    # This manager, not acme.sh's independent cron, controls activation/reload.
    run([c['acme'], '--install-cert', '-d', c['host'], '--ecc',
         '--key-file', str(staging / 'privkey.pem'), '--fullchain-file', str(staging / 'fullchain.pem'),
         '--reloadcmd', '/bin/true'])
    return True


def verify_certificate(c, directory):
    cert, key = directory / 'fullchain.pem', directory / 'privkey.pem'
    san = subprocess.check_output(['openssl', 'x509', '-in', str(cert), '-noout', '-ext', 'subjectAltName'], text=True)
    present = set(re.findall(r'DNS:([^,\s]+)', san))
    if not set(c['sans']).issubset(present):
        raise ValueError('Certificate must explicitly cover all entry, wildcard and compatibility names')
    for name in c['sans']:
        if not name.startswith('*.'):
            run(['openssl', 'verify', '-CAfile', c['trust_bundle'], '-untrusted', str(cert), '-purpose', 'sslserver',
                 '-verify_hostname', name, str(cert)], stdout=subprocess.DEVNULL)
    pub = subprocess.check_output(['openssl', 'x509', '-in', str(cert), '-pubkey', '-noout'])
    if pub != subprocess.check_output(['openssl', 'pkey', '-in', str(key), '-passin', 'pass:', '-pubout']):
        raise ValueError('Certificate/private key mismatch')


def point_live(directory, target):
    temporary = directory / '.live-next'
    temporary.unlink(missing_ok=True)
    temporary.symlink_to(target)
    temporary.replace(directory / 'live')


def activate(c, prepare=False):
    directory = Path(c['deployment_dir']) / 'certs'
    pending = directory / 'pending'
    verify_certificate(c, pending)
    # Keep the bind-mounted directory inode stable. Only an internal symlink
    # changes, atomically selecting the certificate AND matching private key.
    for name in ['fullchain.pem', 'privkey.pem']:
        alias = directory / name
        if alias.exists() or alias.is_symlink():
            if not alias.is_symlink() or os.readlink(alias) != 'live/' + name:
                raise ValueError('Existing unmanaged TLS files preserved; migrate them explicitly first')
    directory.chmod(0o750)
    os.chown(directory, 0, 10001)
    release = Path(tempfile.mkdtemp(prefix='issued-', dir=directory))
    release.chmod(0o755)
    for name in ['fullchain.pem', 'privkey.pem']:
        shutil.copyfile(pending / name, release / name)
        (release / name).chmod(0o644 if name == 'fullchain.pem' else 0o640)
        os.chown(release / name, 0, 10001)
    old = os.readlink(directory / 'live') if (directory / 'live').is_symlink() else None
    point_live(directory, release.name)
    for name in ['fullchain.pem', 'privkey.pem']:
        if not (directory / name).is_symlink():
            (directory / name).symlink_to('live/' + name)
    if prepare:
        return
    try:
        reload_services(c)
    except Exception:
        if old:
            point_live(directory, old)
            reload_services(c)
        raise


def reload_services(c):
    # Existing Go/frps listeners load PEM at startup; renewal needs a restart.
    nginx_binary = c.get('nginx_binary', 'nginx')
    run([nginx_binary, '-t'])
    run(['docker', 'compose', 'restart', 'mfconnect'], cwd=c['deployment_dir'])
    for attempt in range(30):
        result = subprocess.run(['docker', 'compose', 'exec', '-T', 'mfconnect', '/usr/local/bin/mfconnect-healthcheck'],
                                cwd=c['deployment_dir'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if result.returncode == 0:
            break
        if attempt == 29:
            raise RuntimeError('Renewed service did not become healthy; reverting certificate')
        time.sleep(1)
    run([nginx_binary, '-s', 'reload'])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['plan', 'render', 'dns', 'certificate', 'activate', 'renew'])
    parser.add_argument('--config', required=True, type=Path)
    parser.add_argument('--apply', action='store_true')
    parser.add_argument('--prepare', action='store_true', help='first activation only: install TLS without reloading services')
    args = parser.parse_args()
    os.umask(0o077)
    c = configuration(json.loads(args.config.read_text()))
    if not args.apply or args.action == 'plan':
        print(json.dumps({'entry_origin': c['entry_origin'], 'api_base_url': c['entry_origin'] + '/api',
                          'dns_records': dns_records(c), 'certificate_names': c['sans'],
                          'action': args.action, 'applied': False}, indent=2))
        return
    root = Path(c['deployment_dir'])
    root.mkdir(parents=True, exist_ok=True)
    lock = (root / '.unified-manager.lock').open('a')
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        raise RuntimeError('Another DNS/TLS management operation is running') from None
    if args.action == 'render':
        root = Path(c['deployment_dir'])
        root.mkdir(parents=True, exist_ok=True)
        # Candidates are intentionally separate from existing live files.
        (root / 'nginx.unified.candidate.conf').write_text(nginx(c))
        (root / 'unified.env.candidate').write_text(environment(c))
    elif args.action == 'dns':
        configure_dns(c)
    elif args.action == 'certificate':
        certificate(c)
    elif args.action == 'activate':
        activate(c, args.prepare)
    elif args.action == 'renew':
        renewed = certificate(c, renew=True)
        pending = Path(c['deployment_dir']) / 'certs/pending/fullchain.pem'
        live = Path(c['deployment_dir']) / 'certs/fullchain.pem'
        # Retry a previously issued certificate if activation failed, even when
        # ACME says another renewal is not due yet.
        if renewed or (pending.exists() and (not live.exists() or pending.read_bytes() != live.read_bytes())):
            activate(c)


if __name__ == '__main__':
    main()
