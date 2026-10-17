#!/usr/bin/env python3
"""Interactive installation from a verified prebuilt bundle; no production defaults."""
import argparse
import fcntl
import getpass
import ipaddress
import json
import os
from pathlib import Path
import platform
import re
import shutil
import socket
import subprocess
import sys
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'unified'))
import manage

ROOT = Path('/srv/mfconnect')
PLACEHOLDER = 'https://downloads.example.invalid/mfconnect'


def run(args, *, cwd=None, env=None, capture=False, check=True):
    result = subprocess.run([str(a) for a in args], cwd=cwd, env=env, text=True,
                            stdout=subprocess.PIPE if capture else None,
                            stderr=subprocess.PIPE if capture else None)
    if check and result.returncode:
        # Captured output can contain host configuration. Keep it off terminal.
        raise RuntimeError(f'命令失败：{args[0]}（退出码 {result.returncode}）')
    return result


def write(path, text, mode=0o600, owner=None):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + '.installing')
    temporary.write_text(text)
    temporary.chmod(mode)
    if owner is not None:
        os.chown(temporary, *owner)
    os.replace(temporary, path)


def operating_system(path=Path('/etc/os-release')):
    fields = {}
    for line in path.read_text().splitlines():
        if '=' in line:
            key, value = line.split('=', 1)
            fields[key] = value.strip('"\'')
    return fields


def supported_system(system):
    supported = {'ubuntu': ('22.04', '24.04'), 'debian': ('12', '13'),
                 'opencloudos': ('9',), 'rocky': ('9',), 'almalinux': ('9',),
                 'centos': ('9',), 'rhel': ('9',)}
    distro, version = system.get('ID', ''), system.get('VERSION_ID', '')
    if distro != 'ubuntu':
        version = version.split('.')[0]
    if version not in supported.get(distro, ()):
        raise RuntimeError('自动安装支持 Ubuntu 22.04/24.04、Debian 12/13、RHEL 9 系列（含 OpenCloudOS 9）。')


def settings(raw, root=ROOT):
    entry = raw.get('entry_origin', '').strip().lower().rstrip('/')
    if not entry.startswith('https://'):
        entry = 'https://' + entry
    ca = next((p for p in ('/etc/ssl/certs/ca-certificates.crt', '/etc/pki/tls/certs/ca-bundle.crt') if Path(p).is_file()), '/etc/ssl/certs/ca-certificates.crt')
    result = manage.configuration({
        'entry_origin': entry, 'zone': raw.get('zone', '').strip().lower(),
        'ipv4': raw.get('ipv4', '').strip(), 'dns_provider': 'tencent',
        'deployment_dir': str(root / 'current'), 'downloads_root': str(root / 'public'),
        'trust_bundle': ca, 'acme': str(root / 'ops/acme'), 'dns_sleep': 60,
    })
    if not ipaddress.ip_address(result['ipv4']).is_global:
        raise ValueError('请输入服务器的公网 IPv4，不是内网地址。')
    return result


def prompt(with_secrets=True):
    print('填写部署参数（DNS 凭据只保存在本服务器）：')
    result = {
        'entry_origin': input('服务入口域名，例如 connect.example.com：').strip(),
        'zone': input('DNSPod 托管域名，例如 example.com：').strip(),
        'ipv4': input('云服务器公网 IPv4：').strip(),
    }
    if with_secrets:
        result['secret_id'] = getpass.getpass('腾讯云 SecretId（隐藏输入）：').strip()
        result['secret_key'] = getpass.getpass('腾讯云 SecretKey（隐藏输入）：').strip()
    return result


def secret_environment(raw):
    values = {}
    for name, field in (('Tencent_SecretId', 'secret_id'), ('Tencent_SecretKey', 'secret_key')):
        value = raw.get(field, '')
        if not isinstance(value, str) or not re.fullmatch(r'[A-Za-z0-9_+/=.-]{16,256}', value):
            raise ValueError('DNS 凭据格式无效。')
        values[name] = value
    return values


def free_port(port):
    with socket.socket() as sock:
        try:
            sock.bind(('0.0.0.0', port))
            return True
        except OSError:
            return False


def find_nginx():
    return shutil.which('nginx') or next((p for p in ('/www/server/nginx/sbin/nginx', '/usr/sbin/nginx') if Path(p).is_file()), None)


def nginx_directory(binary, entry, owned=None):
    result = run([binary, '-T'], capture=True)
    text = result.stdout + result.stderr
    if owned:
        text = re.sub(r'# configuration file ' + re.escape(str(owned)) + r':\n.*?(?=# configuration file |\Z)', '', text, flags=re.S)
    # Reject an existing site under the new namespace. Never replace its routing.
    for names in re.findall(r'^\s*server_name\s+([^;]+);', text, re.M):
        for name in names.split():
            name = name.lower().strip('"\'')
            if name.startswith('~'):
                raise RuntimeError('现有 Nginx 使用正则域名，无法自动确认冲突，请使用高级部署说明。')
            wildcard = name.startswith(('*.', '.'))
            name = name.lstrip('*.')
            if name == entry or name.endswith('.' + entry) or (wildcard and entry.endswith('.' + name)):
                raise RuntimeError('已有 Nginx 站点使用该域名范围，停止安装以保留现有站点。')
    choices = re.findall(r'\binclude\s+(/[A-Za-z0-9_./-]+)/\*\.conf\s*;', text)
    preferred = ['/www/server/panel/vhost/nginx', '/etc/nginx/conf.d']
    for directory in preferred + choices:
        if directory in choices and Path(directory).is_dir():
            target = Path(directory) / 'mfconnect.conf'
            if target.exists() and target != owned:
                raise RuntimeError('已有 mfconnect.conf，安装器不会覆盖。')
            return target
    raise RuntimeError('无法确认 Nginx 的站点包含目录，请使用高级部署说明。')


def install_dependencies(system, nginx_present):
    supported_system(system)
    distro = system.get('ID', '')
    if distro in ('ubuntu', 'debian'):
        run(['apt-get', 'update'])
        packages = ['ca-certificates', 'curl', 'openssl', 'python3', 'tar']
        if not nginx_present:
            packages.append('nginx')
        run(['apt-get', 'install', '-y', *packages], env={**os.environ, 'DEBIAN_FRONTEND': 'noninteractive'})
    elif distro in ('opencloudos', 'rocky', 'almalinux', 'centos', 'rhel'):
        run(['dnf', 'install', '-y', 'ca-certificates', 'curl', 'openssl', 'python3', 'tar', *([] if nginx_present else ['nginx'])])
    else:
        raise RuntimeError('自动安装依赖支持 Ubuntu、Debian 和 RHEL 9 系列（含 OpenCloudOS 9）。')
    if shutil.which('docker'):
        if run(['docker', 'compose', 'version'], capture=True, check=False).returncode:
            raise RuntimeError('现有 Docker 缺少 Compose V2；请先补装，安装器不会替换已有 Docker。')
    elif distro in ('ubuntu', 'debian'):
        codename = system.get('VERSION_CODENAME', '')
        if not re.fullmatch(r'[a-z]+', codename):
            raise RuntimeError('无法识别发行版代号。')
        key = Path('/etc/apt/keyrings/mfconnect-docker.asc')
        key.parent.mkdir(parents=True, exist_ok=True)
        key.parent.chmod(0o755)
        run(['curl', '-fsSL', '--proto', '=https', f'https://download.docker.com/linux/{distro}/gpg', '-o', key])
        key.chmod(0o644)
        write('/etc/apt/sources.list.d/mfconnect-docker.sources',
              f'Types: deb\nURIs: https://download.docker.com/linux/{distro}\nSuites: {codename}\nComponents: stable\nArchitectures: amd64\nSigned-By: {key}\n', 0o644)
        run(['apt-get', 'update'])
        run(['apt-get', 'install', '-y', 'docker-ce', 'docker-ce-cli', 'containerd.io', 'docker-buildx-plugin', 'docker-compose-plugin'])
    else:
        if system.get('VERSION_ID', '').split('.')[0] != '9':
            raise RuntimeError('自动安装 Docker 的 RHEL 系统目前只支持 9 系列。')
        write('/etc/yum.repos.d/mfconnect-docker.repo',
              '[mfconnect-docker]\nname=Docker CE stable\nbaseurl=https://download.docker.com/linux/centos/9/x86_64/stable\nenabled=1\ngpgcheck=1\ngpgkey=https://download.docker.com/linux/centos/gpg\n', 0o644)
        run(['dnf', 'install', '-y', 'docker-ce', 'docker-ce-cli', 'containerd.io', 'docker-buildx-plugin', 'docker-compose-plugin'])
    run(['systemctl', 'enable', '--now', 'docker'])
    run(['docker', 'info'], capture=True)
    run(['docker', 'compose', 'version'], capture=True)


def install_nginx_site(binary, target, contents):
    previous = target.read_text() if target.exists() else None
    write(target, contents, 0o644)
    try:
        if shutil.which('restorecon'):
            run(['restorecon', target], capture=True)
        run([binary, '-t'], capture=True)
        result = run([binary, '-s', 'reload'], capture=True, check=False)
        if result.returncode:
            run([binary], capture=True)
    except Exception:
        if previous is None:
            target.unlink()
        else:
            write(target, previous, 0o644)
        raise


def configure_selinux(root, site):
    if not shutil.which('getenforce') or run(['getenforce'], capture=True).stdout.strip() == 'Disabled':
        return
    # Keep SELinux enabled. Nginx needs access to only the public assets and
    # certificates; the standard boolean permits its HTTPS upstream connection.
    run(['dnf', 'install', '-y', 'policycoreutils-python-utils'])
    for label, pattern in (
        ('httpd_sys_content_t', str(root) + '/public(/.*)?'),
        ('httpd_config_t', str(root) + '/releases(/.*)?'),
        ('httpd_config_t', str(root)),
        ('httpd_config_t', str(root) + '/current'),
    ):
        result = run(['semanage', 'fcontext', '-a', '-t', label, pattern], capture=True, check=False)
        if result.returncode:
            run(['semanage', 'fcontext', '-m', '-t', label, pattern], capture=True)
    run(['restorecon', '-R', root / 'public', root / 'releases'])
    run(['restorecon', root, root / 'current', site.parent])
    run(['setsebool', '-P', 'httpd_can_network_connect', 'on'])


def install_timer(root, config):
    write('/etc/systemd/system/mfconnect-tls.service',
          '[Unit]\nDescription=MF Connect certificate renewal\nAfter=network-online.target docker.service\nWants=network-online.target\n\n'
          f'[Service]\nType=oneshot\nUMask=0077\nEnvironmentFile={root}/secrets/dns.env\n'
          f'ExecStart=/usr/bin/python3 {root}/current/deploy/unified/manage.py renew --config {config} --apply\n', 0o644)
    write('/etc/systemd/system/mfconnect-tls.timer',
          '[Unit]\nDescription=Check MF Connect certificate daily\n\n[Timer]\nOnCalendar=daily\nRandomizedDelaySec=1h\nPersistent=true\n\n[Install]\nWantedBy=timers.target\n', 0o644)
    run(['systemctl', 'daemon-reload'])
    run(['systemctl', 'enable', '--now', 'mfconnect-tls.timer'])


def install(package, raw, *, root=ROOT, resume=False):
    if os.geteuid() != 0 or platform.system() != 'Linux' or platform.machine() != 'x86_64':
        raise RuntimeError('请在 Linux amd64 服务器上以 root 运行。')
    if not Path('/run/systemd/system').is_dir():
        raise RuntimeError('当前自动安装需要 systemd。')
    system = operating_system()
    supported_system(system)
    c = settings(raw, root)
    secrets = secret_environment(raw)
    metadata = json.loads((package / 'bundle.json').read_text())
    state_file = root / 'installer-state.json'
    state = {}
    # Re-running over an established instance is never an implicit upgrade.
    if root.exists() and any(root.iterdir()):
        if not resume or not state_file.is_file():
            raise RuntimeError(f'{root} 已存在部署数据，不覆盖已有实例。若为本安装器未完成的安装，请加 --resume。')
        state = json.loads(state_file.read_text())
        if any(state.get(key) != c[key] for key in ('entry_origin', 'zone', 'ipv4')) or state.get('image_id') != metadata['image_id']:
            raise RuntimeError('恢复参数或发行包与原安装不一致，停止。')
        if (root / 'installation.json').is_file():
            print('该实例已安装完成：' + c['entry_origin'])
            print('插件安装链接：' + c['entry_origin'] + '/downloads/mfconnect/mfconnect.plg')
            return
    nginx = find_nginx()
    if not nginx and (not free_port(80) or not free_port(443)):
        raise RuntimeError('80/443 已被其他服务占用，且未找到可接入的 Nginx。')
    for port in (() if state else (17000, 18443)):
        if not free_port(port):
            raise RuntimeError(f'端口 {port} 已被占用，停止安装。')
    if not state and shutil.which('docker') and run(['docker', 'volume', 'inspect', 'mfconnect_data'], capture=True, check=False).returncode == 0:
        raise RuntimeError('已有 mfconnect_data 数据卷；安装器不会复用或清空。')
    owned_site = Path(state['nginx_site']) if state.get('nginx_site') else None
    if owned_site and owned_site.exists() and owned_site.read_text() != manage.nginx({**c, 'nginx_binary': nginx}):
        raise RuntimeError('已生成的 Nginx 站点被改动，停止恢复以保留修改。')
    site = nginx_directory(nginx, c['host'], owned_site) if nginx else None
    print('[1/7] 检查并安装 Docker、Compose 和 Nginx', flush=True)
    install_dependencies(system, nginx is not None)
    nginx = nginx or find_nginx()
    site = site or nginx_directory(nginx, c['host'])
    c['nginx_binary'] = nginx
    root.mkdir(parents=True, exist_ok=True)
    root.chmod(0o755)
    for name, mode in (('secrets', 0o700), ('ops', 0o700), ('updates', 0o700), ('public', 0o755)):
        (root / name).mkdir(mode=mode, exist_ok=True)
        (root / name).chmod(mode)
    write(root / 'secrets/setup-input.json', json.dumps(raw) + '\n')
    write(state_file, json.dumps({key: c[key] for key in ('entry_origin', 'zone', 'ipv4')} |
                                {'image_id': metadata['image_id'], 'nginx_site': str(site)}) + '\n')
    os.chown(root / 'updates', 10001, 10001)
    write(root / 'secrets/dns.env', ''.join(f'{key}={value}\n' for key, value in secrets.items()))
    config = root / 'secrets/unified.json'
    write(config, json.dumps(c, indent=2) + '\n')
    release = root / 'releases' / ('mfconnect-' + metadata['server_version'] + '-docker.1')
    shutil.copytree(package / 'runtime', release, dirs_exist_ok=True)
    if (root / 'current').is_symlink():
        if (root / 'current').resolve() != release:
            raise RuntimeError('current 指向了不同实例，停止。')
    else:
        (root / 'current').symlink_to(release)
    print('[2/7] 加载发行镜像，准备插件安装包', flush=True)
    run(['docker', 'load', '--input', package / 'server-image.tar.gz'])
    image_id = run(['docker', 'image', 'inspect', '--format', '{{.Id}}', metadata['image']], capture=True).stdout.strip()
    if image_id != metadata['image_id']:
        raise RuntimeError('加载的镜像 ID 与发行记录不一致。')
    public = root / 'public/downloads/mfconnect'
    public.mkdir(parents=True, exist_ok=True)
    for directory in (public.parent, public):
        directory.chmod(0o755)
    payload = package / 'plugin' / metadata['plugin_payload']
    shutil.copyfile(payload, public / payload.name)
    (public / payload.name).chmod(0o644)
    template = (package / 'plugin/mfconnect.plg').read_text()
    if PLACEHOLDER not in template:
        raise RuntimeError('插件清单缺少发布地址占位符。')
    manifest = template.replace(PLACEHOLDER, c['entry_origin'] + '/downloads/mfconnect')
    write(public / 'mfconnect.plg', manifest, 0o644)
    write(public / ('mfconnect-' + metadata['plugin_version'] + '.plg'), manifest, 0o644)
    write(root / 'updates/plugin-install.plg', manifest, 0o644, (10001, 10001))
    print('[3/7] 配置 DNS 并申请入口与通配符证书', flush=True)
    acme_source = package / 'third_party/acme.sh/acme.sh'
    run(['sh', acme_source, '--install', '--home', root / 'acme', '--config-home', root / 'acme-config',
         '--cert-home', root / 'acme-certs', '--nocron', '--noprofile'], cwd=acme_source.parent)
    write(root / 'ops/acme', '#!/bin/sh\nexec ' + str(root / 'acme/acme.sh') +
          f' --home {root}/acme --config-home {root}/acme-config --cert-home {root}/acme-certs "$@"\n', 0o700)
    env = {**os.environ, **secrets}
    actions = ['dns']
    if not (release / 'certs/fullchain.pem').is_file():
        actions += ['certificate', 'activate']
    else:
        manage.verify_certificate(c, release / 'certs')
    for action in actions:
        command = [sys.executable, release / 'deploy/unified/manage.py', action, '--config', config, '--apply']
        if action == 'activate':
            command.append('--prepare')
        run(command, env=env)
    print('[4/7] 初始化并启动 MF Connect', flush=True)
    env_path = release / '.env'
    values = dict(line.split('=', 1) for line in (release / '.env.example').read_text().splitlines() if '=' in line and not line.startswith('#'))
    if env_path.exists():
        saved = dict(line.split('=', 1) for line in env_path.read_text().splitlines() if '=' in line and not line.startswith('#'))
        values['MF_TRUSTED_PROXY_IPS'] = saved.get('MF_TRUSTED_PROXY_IPS', '[]')
    values.update(MF_ENTRY_ORIGIN=c['entry_origin'], MF_IMAGE_TAG=metadata['image'].split(':')[-1], MF_UPDATE_DIRECTORY=str(root / 'updates'))
    def save_env():
        write(env_path, ''.join(f'{key}={value}\n' for key, value in values.items()))
    save_env()
    compose = ['docker', 'compose', '--project-name', 'mfconnect']
    run(compose + ['create', '--no-build'], cwd=release)
    network = json.loads(run(['docker', 'network', 'inspect', 'mfconnect_default'], capture=True).stdout)[0]
    gateways = [str(ipaddress.ip_address(item['Gateway'])) for item in network['IPAM']['Config'] if item.get('Gateway')]
    values['MF_TRUSTED_PROXY_IPS'] = json.dumps(gateways, separators=(',', ':'))
    save_env()
    initialized = run(compose + ['run', '--rm', '--entrypoint', 'sh', 'mfconnect', '-c', 'test -s /var/lib/mfconnect/server.json'], cwd=release, capture=True, check=False).returncode == 0
    if not initialized:
        run(compose + ['run', '--rm', 'mfconnect', 'init'], cwd=release)
    elif not resume:
        raise RuntimeError('发现已有云端状态，停止初始化。')
    run(compose + ['up', '-d', '--no-build'], cwd=release)
    for attempt in range(60):
        if run(compose + ['exec', '-T', 'mfconnect', '/usr/local/bin/mfconnect-healthcheck'], cwd=release, capture=True, check=False).returncode == 0:
            break
        if attempt == 59:
            raise RuntimeError('服务尚未健康；请保留现场并查看 docker compose logs。')
        time.sleep(2)
    print('[5/7] 接入 Nginx 并检查 HTTPS', flush=True)
    configure_selinux(root, site)
    install_nginx_site(nginx, site, manage.nginx(c))
    run(['curl', '--fail', '--silent', '--show-error', '--resolve', c['host'] + ':443:127.0.0.1', c['entry_origin'] + '/api/healthz'], capture=True)
    print('[6/7] 配置证书续期和主机防火墙', flush=True)
    install_timer(root, config)
    if shutil.which('firewall-cmd') and run(['firewall-cmd', '--state'], capture=True, check=False).returncode == 0:
        for port in ('80/tcp', '443/tcp', '17000/tcp'):
            run(['firewall-cmd', '--permanent', '--add-port=' + port])
        run(['firewall-cmd', '--reload'])
    elif shutil.which('ufw'):
        status = run(['ufw', 'status'], capture=True, check=False).stdout
        if 'Status: active' in status:
            for port in ('80/tcp', '443/tcp', '17000/tcp'):
                run(['ufw', 'allow', port])
    print('[7/7] 安装完成', flush=True)
    write(root / 'installation.json', json.dumps({'entry_origin': c['entry_origin'], 'bundle': metadata['release_version'], 'completed_at': int(time.time())}) + '\n')
    print('\nRaidMate 服务入口：' + c['entry_origin'])
    print('Unraid 插件安装链接：' + c['entry_origin'] + '/downloads/mfconnect/mfconnect.plg')
    print('在 Unraid → Plugins → Install Plugin 中粘贴上面的链接。')
    print('查看运行状态：cd /srv/mfconnect/current && docker compose ps')
    print('云安全组须放行 TCP 80、443、17000。App 自动更新须另行配置签名发行频道，见部署说明。')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--package', type=Path, required=True)
    parser.add_argument('--config', type=Path, help='root-only JSON with entry_origin, zone, ipv4, secret_id, secret_key')
    parser.add_argument('--plan', action='store_true', help='print steps without changing the host')
    parser.add_argument('--resume', action='store_true', help='resume only this installer’s unfinished deployment')
    args = parser.parse_args()
    os.umask(0o077)
    try:
        if args.resume and not args.config:
            args.config = ROOT / 'secrets/setup-input.json'
        if args.config:
            if args.config.stat().st_mode & 0o077:
                parser.error('配置文件权限必须为 600。')
            raw = json.loads(args.config.read_text())
        else:
            raw = prompt(with_secrets=not args.plan)
        c = settings(raw)
        if args.plan:
            print(json.dumps({'entry_origin': c['entry_origin'], 'dns_records': manage.dns_records(c),
                              'certificate_names': c['sans'], 'deployment_dir': str(ROOT),
                              'steps': ['dependencies', 'image/plugin', 'DNS/TLS', 'Docker', 'Nginx', 'renewal/firewall'],
                              'applied': False}, ensure_ascii=False, indent=2))
            return
        # System-wide lock is independent of the deployment directory.
        with open('/run/mfconnect-install.lock', 'a') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            install(args.package.resolve(), raw, resume=args.resume)
    except (ValueError, RuntimeError, OSError, json.JSONDecodeError) as error:
        print('安装未完成：' + str(error), file=sys.stderr)
        print('已有实例不会被清空。排查后运行 bash install.sh --resume 继续本安装器的未完成安装。', file=sys.stderr)
        raise SystemExit(1)


if __name__ == '__main__':
    main()
