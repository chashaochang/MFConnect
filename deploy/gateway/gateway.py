#!/usr/bin/env python3
"""Container-owned ingress and TLS. Provider credentials never reach the API."""
import contextlib
import fcntl
import getpass
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'unified'))
import manage

STATE = Path('/state')
CONFIG = Path('/config')
UPDATES = Path('/updates')
ASSETS = Path('/opt/mfconnect/plugin')
PROVIDERS = {
    'cloudflare': ('dns_cf', ('CF_Token',), ('CF_Zone_ID', 'CF_Account_ID')),
    'alidns': ('dns_ali', ('Ali_Key', 'Ali_Secret'), ()),
    'dnspod': ('dns_tencent', ('Tencent_SecretId', 'Tencent_SecretKey'), ()),
}
PLACEHOLDER = 'https://downloads.example.invalid/mfconnect'


def write(path, data, mode=0o600):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + '.preparing')
    temporary.write_bytes(data.encode() if isinstance(data, str) else data)
    temporary.chmod(mode)
    temporary.replace(path)


def settings(env=os.environ, state=STATE):
    entry = env.get('MF_ENTRY_ORIGIN', '').strip().lower().rstrip('/')
    c = manage.configuration(dict(entry_origin=entry, deployment_dir=str(state),
        downloads_root=str(state / 'public'), acme='/usr/local/bin/mfconnect-acme',
        trust_bundle=env.get('MF_CA_BUNDLE', '/etc/ssl/certs/ca-certificates.crt'),
        container_proxy=True, dns_sleep=60), require_dns=False)
    mode = env.get('MF_TLS_MODE', 'provided')
    if mode not in ('provided', 'acme'):
        raise ValueError('MF_TLS_MODE must be provided or acme')
    c['tls_mode'] = mode
    if mode == 'acme':
        provider = env.get('MF_DNS_PROVIDER', '')
        if provider not in PROVIDERS:
            raise ValueError('Select cloudflare, alidns or dnspod; other providers can supply their own certificate')
        c['provider'], c['acme_dns'] = provider, PROVIDERS[provider][0]
    return c


def credentials(c, path=CONFIG / 'dns.env'):
    if c['tls_mode'] != 'acme':
        return {}
    if path.stat().st_mode & 0o077:
        raise ValueError('config/dns.env must be private: chmod 600 config/dns.env')
    _, required, optional = PROVIDERS[c['provider']]
    values = {}
    for line in path.read_text().splitlines():
        if not line.strip() or line.lstrip().startswith('#'):
            continue
        name, separator, value = line.partition('=')
        if not separator or name not in required + optional or name in values or not re.fullmatch(r'[A-Za-z0-9_+/=.-]{1,256}', value):
            raise ValueError('Invalid DNS credential file; use only the selected provider fields, without quotes')
        values[name] = value
    if not all(values.get(name) for name in required):
        raise ValueError('Required DNS provider credentials are missing')
    return values


@contextlib.contextmanager
def dns_environment(values):
    previous = {name: os.environ.get(name) for name in values}
    os.environ.update(values)
    try:
        yield
    finally:
        for name, value in previous.items():
            if value is None: os.environ.pop(name, None)
            else: os.environ[name] = value


def publish(c, assets=ASSETS, updates=UPDATES):
    state = Path(c['deployment_dir'])
    for directory in (state, state / 'public', state / 'public/downloads', state / 'public/downloads/mfconnect'):
        directory.mkdir(parents=True, exist_ok=True); directory.chmod(0o755)
    updates.mkdir(parents=True, exist_ok=True); updates.chmod(0o700)
    os.chown(updates, 10001, 10001)
    metadata = json.loads((assets / 'release.json').read_text())
    filename = metadata['payload']
    if not re.fullmatch(r'mfconnect-[0-9.]+-linux-amd64\.tar\.gz', filename):
        raise ValueError('Invalid packaged plugin filename')
    data = (assets / filename).read_bytes()
    if hashlib.sha256(data).hexdigest() != metadata['sha256']:
        raise ValueError('Packaged plugin checksum mismatch')
    template = (assets / 'mfconnect.plg').read_text()
    if PLACEHOLDER not in template:
        raise ValueError('Plugin download template is invalid')
    manifest = template.replace(PLACEHOLDER, c['entry_origin'] + '/downloads/mfconnect')
    public = state / 'public/downloads/mfconnect'
    write(public / filename, data, 0o644)
    write(public / 'mfconnect.plg', manifest, 0o644)
    write(public / ('mfconnect-' + metadata['version'] + '.plg'), manifest, 0o644)
    write(updates / 'plugin-install.plg', manifest, 0o644)
    os.chown(updates / 'plugin-install.plg', 10001, 10001)


def prepare_tls(c, supplied=CONFIG, renew=False):
    certs = Path(c['deployment_dir']) / 'certs'
    if c['tls_mode'] == 'provided':
        pending = certs / 'pending'; pending.mkdir(parents=True, exist_ok=True)
        for name in ('fullchain.pem', 'privkey.pem'):
            if not (supplied / name).is_file():
                raise ValueError('Put fullchain.pem and privkey.pem covering the entry and its wildcard in config/')
            write(pending / name, (supplied / name).read_bytes())
        manage.verify_certificate(c, pending)
        if all((certs / name).exists() and (certs / name).read_bytes() == (pending / name).read_bytes()
               for name in ('fullchain.pem', 'privkey.pem')):
            return False
    else:
        with dns_environment(credentials(c)):
            if (certs / 'fullchain.pem').is_file() and not renew:
                manage.verify_certificate(c, certs)
                return False
            changed = manage.certificate(c, renew=renew)
        if not changed and (certs / 'fullchain.pem').exists():
            pending = certs / 'pending/fullchain.pem'
            if not pending.is_file() or pending.read_bytes() == (certs / 'fullchain.pem').read_bytes():
                return False
    manage.activate(c, prepare=True)
    return True


def nginx_config(c):
    return ('pid /run/mfconnect-nginx.pid;\nerror_log /dev/stderr warn;\nuser nginx;\nevents {}\nhttp {\n'
            'client_body_temp_path /tmp/body;\nproxy_temp_path /tmp/proxy;\n'
            'fastcgi_temp_path /tmp/fastcgi;\nuwsgi_temp_path /tmp/uwsgi;\nscgi_temp_path /tmp/scgi;\n'
            + manage.nginx(c) + '\n}\n')


def serve():
    c = settings()
    STATE.mkdir(exist_ok=True)
    with (STATE / 'gateway.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        identity = STATE / 'entry.json'
        if identity.exists() and json.loads(identity.read_text())['entry_origin'] != c['entry_origin']:
            raise ValueError('Existing entry differs; migrate the deployment explicitly instead of overwriting its identity')
        write(identity, json.dumps({'entry_origin': c['entry_origin']}) + '\n')
        publish(c)
        prepare_tls(c)
        config = Path('/run/mfconnect-nginx.conf')
        write(config, nginx_config(c), 0o644)
        subprocess.run(['nginx', '-c', str(config), '-t'], check=True)
        process = subprocess.Popen(['nginx', '-c', str(config), '-g', 'daemon off;'])
        stopping = False
        def stop(*_):
            nonlocal stopping
            stopping = True
            process.terminate()
        for event in (signal.SIGTERM, signal.SIGINT): signal.signal(event, stop)
        print('HTTPS gateway ready: ' + c['entry_origin'], flush=True)
        print('Plugin: ' + c['entry_origin'] + '/downloads/mfconnect/mfconnect.plg', flush=True)
        interval = 5 if c['tls_mode'] == 'provided' else 43200
        deadline = time.monotonic() + interval
        try:
            while not stopping:
                if process.poll() is not None:
                    raise RuntimeError('Nginx exited unexpectedly')
                if time.monotonic() >= deadline:
                    certs = STATE / 'certs'
                    previous = os.readlink(certs / 'live')
                    try:
                        if prepare_tls(c, renew=True):
                            subprocess.run(['nginx', '-c', str(config), '-t'], check=True)
                            subprocess.run(['nginx', '-c', str(config), '-s', 'reload'], check=True)
                            print('Verified TLS files activated; backend will reload its API and tunnel.', flush=True)
                    except Exception as error:
                        if os.readlink(certs / 'live') != previous:
                            manage.point_live(certs, previous)
                            subprocess.run(['nginx', '-c', str(config), '-s', 'reload'], check=False)
                        print('TLS refresh failed; previous certificate retained (' + type(error).__name__ + ').', file=sys.stderr, flush=True)
                    deadline = time.monotonic() + interval
                time.sleep(1)
        finally:
            if process.poll() is None: process.terminate()
            process.wait(timeout=30)


def health():
    pid = int(Path('/run/mfconnect-nginx.pid').read_text().strip())
    os.kill(pid, 0)
    subprocess.run(['openssl', 'x509', '-in', str(STATE / 'certs/fullchain.pem'), '-noout', '-checkend', '0'],
                   check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def configure(output=Path('/output')):
    if (output / '.env').exists():
        raise ValueError('.env already exists; edit it directly to preserve existing deployment settings')
    entry = input('服务入口，例如 https://connect.example.com：').strip()
    if not entry.startswith('https://'): entry = 'https://' + entry
    choice = input('证书：1 已有证书（默认）；2 可选的 DNS 自动申请：').strip()
    env = {'MF_ENTRY_ORIGIN': entry, 'MF_TLS_MODE': 'acme' if choice == '2' else 'provided'}
    values = {}
    if choice == '2':
        env['MF_DNS_PROVIDER'] = input('域名的 DNS 服务商（cloudflare / alidns / dnspod）：').strip()
    c = settings(env)
    if choice == '2':
        _, required, optional = PROVIDERS[c['provider']]
        for name in required + optional:
            value = getpass.getpass(name + ('（选填）' if name in optional else '') + '：').strip()
            if value: values[name] = value
        if any(not re.fullmatch(r'[A-Za-z0-9_+/=.-]{1,256}', v) for v in values.values()) or any(not values.get(k) for k in required):
            raise ValueError('Missing or invalid DNS credential; no configuration written')
    output.mkdir(parents=True, exist_ok=True)
    (output / 'config').mkdir(mode=0o700, exist_ok=True)
    if values: write(output / 'config/dns.env', ''.join(k + '=' + v + '\n' for k,v in values.items()))
    write(output / '.env', ''.join(k + '=' + v + '\n' for k,v in env.items()))
    print('配置已生成。请确认入口及 *.入口的 DNS 指向本服务器。')
    if c['tls_mode'] == 'provided': print('请将证书完整链和私钥分别放入 config/fullchain.pem、config/privkey.pem。')
    print('启动：docker compose up -d')


if __name__ == '__main__':
    os.umask(0o077)
    try:
        command = sys.argv[1] if len(sys.argv) > 1 else 'serve'
        {'serve': serve, 'health': health, 'configure': configure}[command]()
    except (ValueError, RuntimeError, OSError, subprocess.SubprocessError, KeyError) as error:
        # Credential contents and cloud responses are not echoed here.
        print('MF Connect gateway: ' + (str(error) if isinstance(error, (ValueError, RuntimeError)) else type(error).__name__), file=sys.stderr)
        raise SystemExit(1)
