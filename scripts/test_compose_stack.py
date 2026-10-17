#!/usr/bin/env python3
"""Real two-container Docker / Compose test using only fixture domains and a private CA.

No DNS account, public certificate, host Nginx, firewall mutation or real NAS.
Only this test's uniquely named containers/networks/volumes are removed.
"""
import argparse
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
import uuid

ROOT = Path(__file__).resolve().parents[1]


def run(args, **kwargs):
    result = subprocess.run([str(a) for a in args], capture_output=True, text=True, timeout=180, **kwargs)
    if result.returncode: raise RuntimeError(result.stderr[-4000:] or result.stdout[-4000:])
    return result.stdout


def wait(fn, seconds=120):
    deadline = time.monotonic() + seconds
    while True:
        try:
            if fn(): return
        except (OSError, http.client.HTTPException, ValueError, RuntimeError): pass
        if time.monotonic() >= deadline: raise AssertionError('Timed out waiting for Docker/TLS state')
        time.sleep(1)


def test(host, version, mode):
    project = 'mfconnect-stack-test-' + uuid.uuid4().hex[:8]
    with tempfile.TemporaryDirectory(prefix=project + '-') as directory:
        work = Path(directory); config = work / 'config'; config.mkdir()
        fixtures = []
        for number in (1,2):
            fixture = work / ('fixture-' + str(number)); fixtures.append(fixture)
            run([ROOT / 'dist/mfconnect-server','init','--entry','https://' + host,'--dir',fixture,'--frps',ROOT / 'dist/tools/frps'])
        (config / 'ca.crt').write_bytes(b'\n'.join((p / 'ca.crt').read_bytes() for p in fixtures))
        def provide(fixture):
            for source, target in [('server.crt','fullchain.pem'),('server.key','privkey.pem')]:
                temp = config / (target + '.next'); temp.write_bytes((fixture / source).read_bytes()); temp.replace(config / target)
        provide(fixtures[0])
        (work / 'compose.yaml').write_bytes((ROOT / 'compose.yaml').read_bytes())
        (work / '.env').write_text('MF_ENTRY_ORIGIN=https://' + host + '\nMF_HTTP_BIND=127.0.0.1:19080\nMF_HTTPS_BIND=127.0.0.1:19443\nMF_TUNNEL_PORT=17001\nMF_NETWORK_SUBNET=172.31.251.0/28\nMF_GATEWAY_IP=172.31.251.2\nMF_SERVER_IMAGE=mfconnect-server:' + version + '-docker.1\nMF_GATEWAY_IMAGE=mfconnect-gateway:' + version + '\nMF_DOCKER_PROJECT=' + project + '\n')
        (work / '.env').chmod(0o600)
        # No provider or provider credentials. Explicit test-only CA is mounted
        # into each container, while normal deployments use system trust.
        override = {'services': {
            'gateway': {'environment': {'MF_CA_BUNDLE':'/config/ca.crt'}},
            'mfconnect': {'environment': {'SSL_CERT_FILE':'/test/ca.crt','CURL_CA_BUNDLE':'/test/ca.crt'},
                          'volumes':[{'type':'bind','source':str(config),'target':'/test','read_only':True}],
                          'ports':[{'target':8443,'published':'19444','host_ip':'127.0.0.1','protocol':'tcp'}]}}}
        (work / 'test.json').write_text(json.dumps(override))
        compose = ['docker','compose','--project-name',project,'-f',str(work / 'compose.yaml'),'-f',str(work / 'test.json')]
        readme_started = False
        def command(arguments):
            nonlocal readme_started
            if mode == 'compose': return run(compose + arguments, cwd=work)
            names = {'gateway':project + '-gateway', 'mfconnect':project + '-server'}
            action = arguments[0]
            if action == 'config': return run(['sh','-n',work / 'docker-run.sh'])
            if action == 'up':
                if mode == 'readme' and not readme_started:
                    blocks = [block for block in re.findall(r'```bash\n(.*?)\n```',(ROOT / 'README.md').read_text(),re.S)
                              if block.startswith(('docker network create','docker run -d --name mfconnect-docker-'))]
                    assert len(blocks) == 3, 'README must contain network/volumes, gateway and server commands'
                    for block in blocks:
                        for source, target in [('mfconnect-docker',project),('172.31.253.0/28','172.31.251.0/28'),
                            ('172.31.253.2','172.31.251.2'),('-p 80:80','-p 127.0.0.1:19080:80'),
                            ('-p 443:443','-p 127.0.0.1:19443:443'),('-p 17000:17000','-p 17001:17001'),
                            ('ghcr.io/chashaochang/mfconnect-gateway:' + version,'mfconnect-gateway:' + version),
                            ('ghcr.io/chashaochang/mfconnect-server:' + version,'mfconnect-server:' + version)]:
                            block = block.replace(source,target)
                        run(['sh','-eu'],input=block,cwd=work,env=docker_env)
                        if block.startswith('docker run -d --name ' + names['gateway']):
                            wait(lambda: run(['docker','inspect','--format','{{.State.Health.Status}}',names['gateway']]).strip() == 'healthy')
                    readme_started = True
                    return ''
                return run(['sh',work / 'docker-run.sh','up'],cwd=work,env=docker_env)
            if action == 'ps': return run(['docker','inspect','--format','{{.Id}}',names[arguments[-1]]])
            if action == 'exec': return run(['docker','exec',names[arguments[2]],*arguments[3:]])
            if action == 'restart': return run(['docker','restart',*names.values()])
            if action == 'recreate': return run(['sh',work / 'docker-run.sh','recreate'],cwd=work,env=docker_env)
            if action == 'logs':
                output = ''
                for name in names.values():
                    result = subprocess.run(['docker','logs','--tail=50',name],capture_output=True,text=True)
                    output += result.stdout + result.stderr
                return output
            if action == 'down':
                run(['sh',work / 'docker-run.sh','down'],cwd=work,env=docker_env)
                # Only resources with this unique test project prefix.
                run(['docker','network','rm',project + '-network'])
                run(['docker','volume','rm',*[project + '-' + item for item in ('gateway','tls','updates','data')]])
                return ''
            raise ValueError(action)
        if mode in ('docker','readme'):
            (work / 'docker-run.sh').write_bytes((ROOT / 'deploy/docker/run.sh').read_bytes())
            shim = work / 'bin'; shim.mkdir()
            # Test-only injection of the private CA, just like the Compose
            # override. The shipped launcher has no trust bypass or test mount.
            additions = {
                'mfconnect-gateway:' + version: ['--env','MF_CA_BUNDLE=/config/ca.crt'],
                'mfconnect-server:' + version + '-docker.1': ['--volume',str(config) + ':/test:ro',
                    '--env','SSL_CERT_FILE=/test/ca.crt','--env','CURL_CA_BUNDLE=/test/ca.crt',
                    '--publish','127.0.0.1:19444:8443/tcp']}
            wrapper = '#!/usr/bin/python3\nimport subprocess,sys\na=sys.argv[1:]\n'
            wrapper += 'extras=' + repr(additions) + '\n'
            wrapper += "if a and a[0]=='run':\n for image,extra in extras.items():\n  if image in a:\n   i=a.index(image); a[i:i]=extra; break\n"
            wrapper += 'sys.exit(subprocess.call([' + repr(shutil.which('docker')) + ']+a))\n'
            (shim / 'docker').write_text(wrapper); (shim / 'docker').chmod(0o755)
            docker_env = {**os.environ,'PATH':str(shim) + os.pathsep + os.environ['PATH']}
        context = ssl.create_default_context(cafile=str(config / 'ca.crt'))
        def connection(port=19443, name=host, frp=False):
            raw = socket.create_connection(('127.0.0.1',port),timeout=3)
            if frp: raw.sendall(b'\x17')
            return context.wrap_socket(raw,server_hostname=name)
        def request(path, name=host, token=None):
            conn = http.client.HTTPConnection('127.0.0.1',19443,timeout=3); conn.sock = connection(name=name)
            headers = {'Host': name}
            if token: headers['Authorization'] = 'Bearer ' + token
            try:
                conn.request('GET',path,headers=headers)
                response = conn.getresponse()
                return response.status, response.read()
            finally: conn.close()
        def fingerprint(port, frp=False):
            with connection(port=port,frp=frp) as sock: return hashlib.sha256(sock.getpeercert(binary_form=True)).hexdigest()
        started = False
        try:
            command(['config','--quiet'])
            started = True
            command(['up','-d','--no-build'])
            wait(lambda: request('/api/healthz')[0] == 200)
            assert json.loads(request('/api/healthz')[1])['version'] == version
            assert json.loads(request('/.well-known/mfconnect')[1])['api_base_url'] == 'https://' + host + '/api'
            assert request('/api/v1/devices')[0] == 401
            token = command(['exec','-T','mfconnect','cat','/var/lib/mfconnect/admin-token']).strip()
            assert request('/api/v1/devices',token=token)[0] == 200
            status, manifest = request('/downloads/mfconnect/mfconnect.plg')
            assert status == 200 and ('version="' + version + '"').encode() in manifest
            assert b'example.invalid' not in manifest
            assert request('/downloads/mfconnect/mfconnect.plg',name='unknown.' + host)[0] == 404
            assert request('/config/dns.env')[0] == 404
            for service in ('gateway','mfconnect'):
                cid = command(['ps','-q',service]).strip()
                info = json.loads(run(['docker','inspect',cid]))[0]
                assert info['HostConfig']['ReadonlyRootfs']
                assert not any('docker.sock' in item['Destination'] for item in info['Mounts'])
                if service == 'mfconnect':
                    assert info['Config']['User'] == '10001:10001'
                    assert not any(key.startswith(('Tencent_','CF_','Ali_')) for key in info['Config']['Env'])
            initial = {port: fingerprint(port,port == 17001) for port in (19443,19444,17001)}
            assert len(set(initial.values())) == 1
            provide(fixtures[1])
            wait(lambda: all(fingerprint(port,port == 17001) != initial[port] for port in initial),45)
            wait(lambda: request('/api/v1/devices',token=token)[0] == 200)
            rotated = fingerprint(19443)
            (config / 'privkey.pem').write_text('invalid replacement key')
            time.sleep(7)
            assert fingerprint(19443) == rotated
            assert request('/api/v1/devices',token=token)[0] == 200
            provide(fixtures[1])
            command(['restart'])
            wait(lambda: request('/api/v1/devices',token=token)[0] == 200)
            assert command(['exec','-T','mfconnect','cat','/var/lib/mfconnect/admin-token']).strip() == token
            if mode == 'docker':
                before = command(['ps','-q','mfconnect']).strip()
                command(['up'])
                assert command(['ps','-q','mfconnect']).strip() == before
                command(['recreate'])
                wait(lambda: request('/api/v1/devices',token=token)[0] == 200)
                assert command(['ps','-q','mfconnect']).strip() != before
                assert command(['exec','-T','mfconnect','cat','/var/lib/mfconnect/admin-token']).strip() == token
            print('PASS: no cloud credentials, /api, plugin download, host isolation, TLS rotation in gateway/API/FRP, invalid-key retention and persistence: ' + mode + ' / ' + host,flush=True)
        except Exception:
            if started:
                logs = command(['logs','--tail=50'])
                print(logs,file=__import__('sys').stderr)
                if os.environ.get('GITHUB_STEP_SUMMARY'):
                    with open(os.environ['GITHUB_STEP_SUMMARY'],'a') as summary:
                        summary.write('Controlled Docker fixture failure (' + host + '):\n\n```text\n' + logs[-12000:] + '\n```\n')
            raise
        finally:
            if started: command(['down','--volumes','--remove-orphans'])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--version',default='0.8.1')
    parser.add_argument('--runtime',choices=('all','docker','compose','readme'),default='all')
    args = parser.parse_args()
    for mode in (('docker','compose','readme') if args.runtime == 'all' else (args.runtime,)):
        for host in ('example.test','connect.example.test'): test(host,args.version,mode)
    print('LIMIT: controlled test CA and fixture DNS names; no public ACME issuance or real NAS/client login.')


if __name__ == '__main__': main()
