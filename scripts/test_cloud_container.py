#!/usr/bin/env python3
"""Run isolated Docker API/volume checks on the authorized cloud host.

Requires an already built image and unpacked release. Uses a test CA, a unique
Compose project/volume, and a loopback-only published port. No real NAS or phone
is simulated, and no production DNS, firewall, or web server is modified.
"""
import argparse
import http.client
import json
import os
from pathlib import Path
import shutil
import socket
import ssl
import subprocess
import time
import uuid


def command(args, *, cwd=None, env=None, success=True):
    result = subprocess.run(args, cwd=cwd, env=env, text=True, capture_output=True, timeout=90)
    if success and result.returncode:
        raise RuntimeError(result.stderr or result.stdout)
    if not success and result.returncode == 0:
        raise AssertionError('Expected initialization to be rejected')
    return result.stdout


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--release-dir', type=Path, required=True)
    parser.add_argument('--work-dir', type=Path, required=True)
    parser.add_argument('--rollback-test-binary', type=Path)
    parser.add_argument('--signed-release', type=Path)
    args = parser.parse_args()
    work = args.work_dir.resolve()
    work.mkdir(mode=0o700, parents=True, exist_ok=False)
    project = 'mfconnect-smoke-' + uuid.uuid4().hex[:8]
    port = 19443
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', port))
    env = {**os.environ, 'MF_ENTRY_ORIGIN': '', 'MF_CONNECT_HOST': '', 'MF_API_HOST': 'api.connect.example.com',
           'MF_BASE_DOMAIN': 'apps.connect.example.com', 'MF_TUNNEL_HOST': 'relay.connect.example.com'}
    release = args.release_dir.resolve()
    (release / 'updates').mkdir(mode=0o700, exist_ok=True)
    fixture = work / 'fixture'
    command([str(release / 'dist/mfconnect-server'), 'init', '--dir', str(fixture),
             '--api-host', env['MF_API_HOST'], '--base-domain', env['MF_BASE_DOMAIN'],
             '--tunnel-host', env['MF_TUNNEL_HOST'], '--frps', str(release / 'dist/tools/frps')])
    certs = work / 'certs'
    certs.mkdir(mode=0o700)
    for source, target in (('server.crt', 'fullchain.pem'), ('server.key', 'privkey.pem'), ('ca.crt', 'ca.crt')):
        path = certs / target
        shutil.copyfile(fixture / source, path)
        path.chmod(0o600)
        os.chown(path, 10001, 10001)
    os.chown(certs, 10001, 10001)
    legacy = release / 'deploy/legacy/compose.yaml'
    if not legacy.is_file(): legacy = release / 'compose.yaml'
    config = json.loads(command(['docker', 'compose', '--project-directory', str(release), '-f', str(legacy), 'config', '--format', 'json'], cwd=release, env=env))
    config['name'] = project
    config['networks']['default']['name'] = project
    config['volumes']['data']['name'] = project
    service = config['services']['mfconnect']
    service.pop('build', None)
    service['restart'] = 'no'
    if args.rollback_test_binary:
        service['image']='mfconnect-server:${MF_IMAGE_TAG}'
        service['healthcheck']={'test':['CMD-SHELL','test "$(mfconnect-server version)" = "0.4.0" && /usr/local/bin/mfconnect-healthcheck'], 'interval':'2s','timeout':'5s','retries':1,'start_period':'2s'}
        (work / '.env').write_text('MF_IMAGE_TAG=0.4.0-docker.1\n')
        (work / '.env').chmod(0o600)
    service['ports'] = [{'target': 8443, 'published': str(port), 'host_ip': '127.0.0.1', 'protocol': 'tcp'}]
    service['environment'].update(SSL_CERT_FILE='/etc/mfconnect/tls/ca.crt', CURL_CA_BUNDLE='/etc/mfconnect/tls/ca.crt')
    for mount in service['volumes']:
        if mount['target'] == '/etc/mfconnect/tls':
            mount['source'] = str(certs)
        if mount['target'] == '/var/lib/mfconnect-updates':
            updates = work / 'updates'
            updates.mkdir(mode=0o700)
            os.chown(updates,10001,10001)
            mount['source'] = str(updates)
    compose_file = work / ('compose.yaml' if args.rollback_test_binary else 'compose.json')
    compose_file.write_text(json.dumps(config, indent=2) + '\n')
    compose_file.chmod(0o600)
    compose = ['docker', 'compose', '-f', str(compose_file)]
    context = ssl.create_default_context(cafile=str(certs / 'ca.crt'))
    api = env['MF_API_HOST']
    def request(host, path, token=None):
        conn = http.client.HTTPConnection('127.0.0.1', port, timeout=3)
        conn.sock = context.wrap_socket(socket.create_connection(('127.0.0.1', port), timeout=3), server_hostname=host)
        try:
            headers = {'Host': host}
            if token:
                headers['Authorization'] = 'Bearer ' + token
            conn.request('GET', path, headers=headers)
            response = conn.getresponse()
            return response.status, json.loads(response.read())
        finally:
            conn.close()
    def ready():
        deadline = time.monotonic() + 40
        while time.monotonic() < deadline:
            try:
                if request(api, '/healthz')[0] == 200:
                    return
            except (OSError, http.client.HTTPException):
                pass
            time.sleep(0.5)
        raise RuntimeError('Container did not become ready')
    started = False
    try:
        command(compose + ['config', '--quiet'])
        started = True
        command(compose + ['run', '--rm', 'mfconnect', 'init'])
        command(compose + ['run', '--rm', 'mfconnect', 'init'], success=False)
        command(compose + ['run', '--rm', 'mfconnect', 'issue-user', '--user', 'pilot'])
        command(compose + ['up', '-d', '--no-build'])
        ready()
        cid = command(compose + ['ps', '-q', 'mfconnect']).strip()
        pilot = command(['docker', 'exec', cid, 'cat', '/var/lib/mfconnect/pilot-token']).strip()
        command(['docker', 'exec', cid, '/usr/local/bin/mfconnect-healthcheck'])
        assert request(api, '/v1/services')[0] == 401
        assert request(api, '/v1/services', pilot) == (200, [])
        assert request('0' * 32 + '.apps.connect.example.com', '/')[0] == 404
        info = json.loads(command(['docker', 'inspect', cid]))[0]
        assert info['Config']['User'] == '10001:10001'
        assert info['HostConfig']['ReadonlyRootfs'] and info['HostConfig']['CapDrop'] == ['ALL']
        assert info['HostConfig']['PortBindings']['8443/tcp'][0]['HostIp'] == '127.0.0.1'
        assert command(['docker', 'exec', cid, 'stat', '-c', '%u:%g %a', '/var/lib/mfconnect']).strip() == '10001:10001 700'
        command(['docker', 'exec', cid, 'touch', '/must-not-be-writable'], success=False)
        command(compose + ['stop', 'mfconnect'])
        assert command(['docker', 'inspect', '--format', '{{.State.ExitCode}}', cid]).strip() == '0'
        command(compose + ['up', '-d', '--no-build'])
        ready()
        assert request(api, '/v1/services', pilot) == (200, [])
        print('PASS: real Docker non-root startup, read-only root, named-volume ownership, loopback published port, strict TLS 200/401/404, healthcheck, repeated-init rejection, restart identity persistence and graceful stop', flush=True)
        if args.rollback_test_binary:
            signed=json.loads(args.signed_release.read_text())
            import base64
            release_data=json.loads(base64.b64decode(signed['payload']))
            runtime=work/'worker-runtime';runtime.mkdir(mode=0o700)
            job_id='upd_'+uuid.uuid4().hex
            job={'id':job_id,'component':'server','requested_by':'fixture','request_id':uuid.uuid4().hex,
                 'release_id':release_data['id'],'version':release_data['version'],'previous_version':'0.4.0',
                 'state':'queued','created_at':int(time.time()),'updated_at':int(time.time()),'expires_at':int(time.time())+600,'release':signed}
            fixture_path=work/'worker-fixture.json'
            fixture_path.write_text(json.dumps({'config':{'component':'server','control_dir':str(work/'updates'),'runtime_dir':str(runtime),
                'compose_dir':str(work),'container':project+'-mfconnect-1','project':project,'download_host':'api.connect.example.com','download_ip':'127.0.0.1'},'job':job}))
            deadline=time.monotonic()+20
            while command(['docker','inspect','--format','{{.State.Health.Status}}',cid]).strip()!='healthy':
                assert time.monotonic()<deadline;time.sleep(1)
            subprocess.run([str(args.rollback_test_binary),'-test.run','^TestCloudWorkerRollbackIntegration$','-test.v'],
                           env={**os.environ,'MF_UPDATE_ROLLBACK_FIXTURE':str(fixture_path)},check=True,timeout=240)
            assert request(api,'/v1/services',pilot)==(200,[])
            assert request(api,'/healthz')[1]['version']=='0.4.0'
            report=json.loads((work/'updates/update-status.json').read_text())
            assert report['state']=='rolled_back'
            (work/'rollback-result.json').write_text(json.dumps({'passed':True,'state':report['state'],'restored_version':'0.4.0','user_token_preserved':True,'production_affected':False},indent=2)+'\n')
            print('PASS: real worker rejected new image health and restored old image, API and user authorization.',flush=True)
        evidence = {'image_id': info['Image'], 'project': project, 'port': f'127.0.0.1:{port}',
                    'checks': 'passed', 'scope': 'API and container runtime only; no NAS tunnel, public DNS, trusted public certificate, Nginx route or phone test'}
        (work / 'result.json').write_text(json.dumps(evidence, indent=2) + '\n')
        print(json.dumps(evidence), flush=True)
    finally:
        if started:
            command(compose + ['down', '--volumes', '--remove-orphans'])
            print('Removed only the uniquely named smoke-test containers, network and volume; image and private evidence retained.', flush=True)


if __name__ == '__main__':
    main()
