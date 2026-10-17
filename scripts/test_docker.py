#!/usr/bin/env python3
"""Validate deployment files and native entrypoint behavior; not a Docker test."""
import hashlib
import http.client
import json
import os
from pathlib import Path
import re
import shutil
import socket
import ssl
import subprocess
import tempfile
import time

import yaml

ROOT = Path(__file__).resolve().parents[1]
ENTRYPOINT = ROOT / 'deploy/docker/entrypoint.sh'
HEALTHCHECK = ROOT / 'deploy/docker/healthcheck.sh'
SERVER = Path(os.environ.get('MF_TEST_SERVER', ROOT / 'dist/mfconnect-server'))
FRPS = ROOT / 'dist/tools/frps'


def run(args, env=None, success=True):
    result = subprocess.run(args, env=env, text=True, capture_output=True, timeout=30)
    if success and result.returncode != 0:
        raise AssertionError(result.stderr)
    if not success and result.returncode == 0:
        raise AssertionError('Expected command rejection')
    return result


def main():
    compose = yaml.safe_load((ROOT / 'deploy/legacy/compose.yaml').read_text())
    service = compose['services']['mfconnect']
    assert list(compose['services']) == ['mfconnect']
    assert service['init'] and service['read_only']
    assert service['user'] == '10001:10001' and service['cap_drop'] == ['ALL']
    assert 'privileged' not in service and 'network_mode' not in service and 'devices' not in service
    assert service['ports'] == ['${MF_HTTPS_BIND:-127.0.0.1:18443}:8443/tcp', '${MF_TUNNEL_PORT:-17000}:${MF_TUNNEL_PORT:-17000}/tcp']
    tls_mount = next(m for m in service['volumes'] if isinstance(m,dict) and m['target']=='/etc/mfconnect/tls')
    assert tls_mount['read_only'] and not tls_mount['bind']['create_host_path']
    assert compose['volumes']['data']['name'] == 'mfconnect_data'
    for script in (ENTRYPOINT, HEALTHCHECK):
        run(['sh', '-n', str(script)])
    for block in re.findall(r'```bash\n(.*?)\n```', (ROOT / 'docs/deployment.md').read_text(), re.S):
        result = subprocess.run(['bash', '-n'], input=block, text=True, capture_output=True)
        assert result.returncode == 0, result.stderr
    print('PASS: YAML structure, explicit isolation/ports/persistence, shell and documented command syntax', flush=True)

    # The native smoke test uses the same two ports as the container's entrypoint.
    # Fail rather than interrupting another process that owns either port.
    for port in (8443, 9091, 17000):
        with socket.socket() as sock:
            sock.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
            sock.bind(('0.0.0.0', port))
    with tempfile.TemporaryDirectory(prefix='mfconnect-docker-wrapper-') as folder:
        base = Path(folder)
        fixture = base / 'fixture'
        state = base / 'data'
        tls = base / 'certs'
        tls.mkdir(mode=0o700)
        run([str(SERVER), 'init', '--dir', str(fixture), '--api-host', 'api.example.com',
             '--base-domain', 'apps.example.com', '--tunnel-host', 'relay.example.com', '--frps', str(FRPS)])
        shutil.copyfile(fixture / 'server.crt', tls / 'fullchain.pem')
        shutil.copyfile(fixture / 'server.key', tls / 'privkey.pem')
        (tls / 'privkey.pem').chmod(0o600)
        env = {**os.environ, 'MF_API_HOST': 'api.example.com', 'MF_BASE_DOMAIN': 'apps.example.com',
               'MF_CONNECT_HOST': 'browser.example.com',
               'MF_TUNNEL_HOST': 'relay.example.com', 'MF_TUNNEL_PORT': '17000', 'MF_PROXY_PORT_START': '20000', 'MF_PROXY_PORT_END': '20999',
               'MF_STATE_DIR': str(state), 'MF_TLS_DIR': str(tls), 'MF_SERVER_BIN': str(SERVER),
               'MF_FRPS_BIN': str(FRPS), 'SSL_CERT_FILE': str(fixture / 'ca.crt'),
               'CURL_CA_BUNDLE': str(fixture / 'ca.crt')}
        cmd = ['sh', str(ENTRYPOINT)]
        for changes in ({'MF_API_HOST': 'https://api.example.com'}, {'MF_TUNNEL_PORT': '80'},
                        {'MF_PROXY_PORT_END': '19999'}, {'MF_API_HOST': 'other.example.com'},
                        {'MF_TUNNEL_HOST': 'other.example.com'}, {'MF_TUNNEL_PORT': '8443'},
                        {'MF_BASE_DOMAIN': 'other.example.com'}, {'MF_TLS_DIR': str(base / 'missing')},
                        {'SSL_CERT_FILE': '/etc/ssl/certs/ca-certificates.crt'}):
            run(cmd + ['init'], {**env, **changes}, success=False)
            assert not (state / 'server.json').exists()
        original_key = (tls / 'privkey.pem').read_bytes()
        run(['openssl', 'genpkey', '-algorithm', 'ED25519', '-out', str(tls / 'privkey.pem')])
        run(cmd + ['init'], env, success=False)
        assert not (state / 'server.json').exists()
        (tls / 'privkey.pem').write_bytes(original_key)

        initialized = run(cmd + ['init'], env)
        config = json.loads((state / 'server.json').read_text())
        assert config['listen'] == '0.0.0.0:8443'
        assert 'easytier' not in config and config['frp']['binary'] == str(FRPS)
        assert config['tunnel_port'] == 17000 and config['port_end'] == 20999
        admin = (state / 'admin-token').read_text().strip()
        assert admin not in initialized.stdout + initialized.stderr
        assert (state / 'server.json').stat().st_mode & 0o777 == 0o600
        before = {name: hashlib.sha256((state / name).read_bytes()).digest()
                  for name in ('server.json', 'state.db', 'admin-token')}
        run(cmd + ['init'], env, success=False)
        assert all(hashlib.sha256((state / name).read_bytes()).digest() == value for name, value in before.items())
        run(cmd + ['serve'], {**env, 'MF_TUNNEL_PORT': '17001'}, success=False)
        run(cmd + ['issue-user', '--user', 'pilot'], env)
        pilot = (state / 'pilot-token').read_text().strip()
        assert (state / 'pilot-token').stat().st_mode & 0o777 == 0o600
        print('PASS: bad domains/ports, untrusted/wrong/missing certificates, wrong key and configuration drift rejected; repeat init preserves state', flush=True)

        context = ssl.create_default_context(cafile=str(fixture / 'ca.crt'))
        def request(host, path, token=None):
            conn = http.client.HTTPConnection('127.0.0.1', 8443, timeout=2)
            conn.sock = context.wrap_socket(socket.create_connection(('127.0.0.1', 8443), timeout=2), server_hostname=host)
            try:
                headers = {'Host': host}
                if token:
                    headers['Authorization'] = 'Bearer ' + token
                conn.request('GET', path, headers=headers)
                response = conn.getresponse()
                return response.status, json.loads(response.read())
            finally:
                conn.close()
        for cycle in range(2):
            with (base / 'process.log').open('wb') as log:
                process = subprocess.Popen(cmd + ['serve'], env=env, stdout=log, stderr=log)
                try:
                    deadline = time.monotonic() + 15
                    while True:
                        assert process.poll() is None, 'Entrypoint exited before readiness'
                        try:
                            status, body = request('api.example.com', '/healthz')
                            break
                        except (OSError, http.client.HTTPException):
                            if time.monotonic() >= deadline:
                                raise
                            time.sleep(0.1)
                    assert status == 200 and body['version'] == subprocess.check_output([str(SERVER),'version'],text=True).strip()
                    # The wrapper health check additionally requires frps.
                    deadline = time.monotonic() + 10
                    while not (state / 'run/frps-status.json').is_file():
                        assert time.monotonic() < deadline
                        time.sleep(0.1)
                    run(['sh', str(HEALTHCHECK)], env)
                    assert request('api.example.com', '/v1/services')[0] == 401
                    assert request('api.example.com', '/v1/services', pilot) == (200, [])
                    assert request('0' * 32 + '.apps.example.com', '/')[0] == 404
                    assert (state / 'admin-token').read_text().strip() == admin
                    assert (state / 'pilot-token').read_text().strip() == pilot
                finally:
                    process.terminate()
                    try:
                        process.wait(timeout=20)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait()
                        raise
                    assert process.returncode == 0, process.returncode
        print('PASS: real server via entrypoint, strict TLS healthcheck, auth 200/401, wildcard 404, restart identity persistence, graceful SIGTERM', flush=True)
    print('LIMIT: native subprocess checks only; Docker build, namespaces, UID mapping, named volume and published ports remain unverified')


if __name__ == '__main__':
    main()
