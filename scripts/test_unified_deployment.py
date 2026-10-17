#!/usr/bin/env python3
"""Controlled DNS/TLS plan tests and real native entrypoint + nginx smoke tests.

No public DNS, ACME, Docker daemon or production server is contacted.
"""
import http.client
import http.server
import base64
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import socket
import ssl
import secrets
import subprocess
import sys
import tempfile
import time
import threading
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / 'deploy/unified'))
import manage
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat


def cfg(host='example.test'):
    return manage.configuration(dict(entry_origin='https://' + host, zone='example.test', ipv4='203.0.113.10',
                                     deployment_dir='/srv/mfconnect', trust_bundle='/etc/ssl/certs/ca-certificates.crt', acme='/opt/acme.sh'))


class Plans(unittest.TestCase):
    def test_apex_and_nested_entry(self):
        for host, records in [('example.test', ['@', '*']), ('connect.example.test', ['connect', '*.connect']),
                              ('a.b.example.test', ['a.b', '*.a.b'])]:
            c = cfg(host)
            self.assertEqual(manage.dns_records(c), records)
            self.assertEqual(c['sans'], [host, '*.' + host])
            self.assertIn('proxy_ssl_name $host;', manage.nginx(c))
            self.assertIn('proxy_set_header Upgrade $http_upgrade;', manage.nginx(c))
            self.assertIn('MF_ENTRY_ORIGIN=https://' + host, manage.environment(c))

    def test_migration_certificate_union(self):
        c = cfg()
        c.update(legacy_api='api.old.test', legacy_apps='apps.old.test', legacy_browser='browser.old.test', tunnel_host='relay.old.test')
        c = manage.configuration(c)
        self.assertEqual(set(c['sans']), {'example.test', '*.example.test', 'api.old.test', 'apps.old.test',
                                        '*.apps.old.test', 'browser.old.test', '*.browser.old.test', 'relay.old.test'})

    def test_downloads_only_on_entry_and_legacy_api(self):
        c = cfg(); c.update(legacy_api='api.old.test', downloads_root='/srv/public')
        rendered = manage.nginx(manage.configuration(c))
        entry, applications = rendered.split('server_name example.test api.old.test;', 1)[1].split('server_name *.example.test;', 1)
        self.assertIn('location ^~ /downloads/mfconnect/', entry)
        self.assertNotIn('location ^~ /downloads/mfconnect/', applications)
        self.assertIn('proxy_pass https://127.0.0.1:18443;', applications)
        c['downloads_root'] = '/srv/public; include /evil'
        with self.assertRaises(ValueError): manage.configuration(c)

    def test_dns_conflict_preflight_and_idempotency(self):
        calls = []
        def conflict(action, body):
            calls.append(action)
            return {'RecordList': [] if body.get('Subdomain') == '@' else [{'Type': 'CNAME', 'Value': 'foreign.test'}]}
        with self.assertRaises(ValueError):
            manage.configure_dns(cfg(), conflict)
        self.assertNotIn('CreateRecord', calls)
        calls.clear()
        def same(action, body):
            calls.append(action)
            return {'RecordList': [{'Type': 'A', 'Value': '203.0.113.10', 'Status': 'ENABLE'}]}
        manage.configure_dns(cfg(), same)
        self.assertEqual(calls, ['DescribeRecordList', 'DescribeRecordList'])
        calls.clear()
        def empty(action, body):
            calls.append((action, body))
            return {}
        manage.configure_dns(cfg('connect.example.test'), empty)
        self.assertEqual([b['SubDomain'] for a,b in calls if a == 'CreateRecord'], ['connect', '*.connect'])

    def test_invalid_configuration(self):
        for entry in ['https://example.test/api', 'http://example.test', 'https://u@example.test', 'https://other.test', 'https://example.test:443']:
            c = cfg(); c['entry_origin'] = entry
            with self.assertRaises(ValueError): manage.configuration(c)
        c = cfg(); c['deployment_dir'] = '/tmp/a; include /evil'
        with self.assertRaises(ValueError): manage.configuration(c)
        for delay in [True, 0, 601, '60']:
            c = cfg(); c['dns_sleep'] = delay
            with self.assertRaises(ValueError): manage.configuration(c)

    def test_atomic_certificate_selection_and_rollback(self):
        with tempfile.TemporaryDirectory() as folder:
            c = cfg(); c['deployment_dir'] = folder
            directory = Path(folder)/'certs'; pending=directory/'pending';pending.mkdir(parents=True)
            for name in ['fullchain.pem','privkey.pem']: (pending/name).write_text('old-'+name)
            # Trust validation itself is exercised against real CA/SAN fixtures
            # by --native. Here induce a service failure after atomic selection.
            with patch.object(manage,'verify_certificate'), patch.object(manage.os,'chown'):
                manage.activate(c,prepare=True)
                old=os.readlink(directory/'live')
                for name in ['fullchain.pem','privkey.pem']: (pending/name).write_text('new-'+name)
                with patch.object(manage,'reload_services',side_effect=[RuntimeError('failed health'),None]) as reload:
                    with self.assertRaises(RuntimeError):manage.activate(c)
                    self.assertEqual(reload.call_count,2)
                self.assertEqual(os.readlink(directory/'live'),old)
                self.assertEqual((directory/'fullchain.pem').read_text(),'old-fullchain.pem')
                self.assertEqual((directory/'privkey.pem').read_text(),'old-privkey.pem')
        with tempfile.TemporaryDirectory() as folder:
            c=cfg();c['deployment_dir']=folder;directory=Path(folder)/'certs';directory.mkdir()
            (directory/'privkey.pem').write_text('existing-unmanaged-key')
            with patch.object(manage,'verify_certificate'):
                with self.assertRaises(ValueError):manage.activate(c)
            self.assertEqual((directory/'privkey.pem').read_text(),'existing-unmanaged-key')


def run(args, env=None, success=True):
    result = subprocess.run(args, env=env, capture_output=True, text=True, timeout=30)
    if (result.returncode == 0) != success:
        raise AssertionError(result.stderr[-3000:])
    return result


def relay_application(host, root, context, request, token, processes, log):
    seen = []
    class App(http.server.BaseHTTPRequestHandler):
        def log_message(self, *args): pass
        def do_GET(self):
            seen.append(dict(self.headers))
            if self.path == '/ws':
                accept=base64.b64encode(hashlib.sha1((self.headers['Sec-WebSocket-Key']+'258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()).digest()).decode()
                self.send_response(101); self.send_header('Upgrade','websocket'); self.send_header('Connection','Upgrade'); self.send_header('Sec-WebSocket-Accept',accept); self.end_headers()
                self.connection.settimeout(5)
                frame=self.rfile.read(2); size=frame[1]&127; mask=self.rfile.read(4); data=self.rfile.read(size)
                decoded=bytes(v^mask[i%4] for i,v in enumerate(data))
                self.wfile.write(bytes([0x81,len(decoded)])+decoded);self.wfile.flush();return
            if self.path == '/relative':
                self.send_response(302);self.send_header('Location','login?from=relay');self.end_headers();return
            self.send_response(200);self.send_header('Set-Cookie','app_session=ok; Domain='+host);self.end_headers();self.wfile.write(b'generic application through FRP')
    app=http.server.ThreadingHTTPServer(('127.0.0.1',0),App)
    thread=threading.Thread(target=app.serve_forever,daemon=True);thread.start()
    def api(path, data, headers=None, method='POST'):
        body=json.dumps(data,separators=(',',':')).encode()
        status,payload,_=request(host,'/api'+path,{'Content-Type':'application/json',**(headers or {})},method,body)
        assert status in (200,201), (path,status,payload)
        return json.loads(payload)
    try:
        enrollment=api('/v1/enrollments',{}, {'Authorization':'Bearer '+token})['enrollment_token']
        key=Ed25519PrivateKey.generate()
        enc=lambda b:base64.b64encode(b).decode().rstrip('=')
        identity=api('/v1/devices/enroll',{'enrollment_token':enrollment,'name':'controlled NAS',
                     'public_key':enc(key.public_key().public_bytes(Encoding.Raw,PublicFormat.Raw)),
                     'proof':enc(key.sign(('mfconnect-enroll\n'+enrollment+'\ncontrolled NAS').encode()))})
        data={'transport':'frp','targets':[{'key':'generic','name':'Generic WebUI','url':'http://127.0.0.1:'+str(app.server_port)}]}
        body=json.dumps(data,separators=(',',':')).encode();stamp=str(int(time.time()));nonce=secrets.token_urlsafe(32)
        message='\n'.join(['POST','/api/v1/device/sync',stamp,nonce,hashlib.sha256(body).hexdigest()]).encode()
        headers={'X-MF-Device':identity['device_id'],'X-MF-Time':stamp,'X-MF-Nonce':nonce,'X-MF-Signature':enc(key.sign(message))}
        synced=api('/v1/device/sync',data,headers);assignment=synced['assignments'][0];service=assignment['service_id']
        api('/v1/services/'+service,{'published':True},{'Authorization':'Bearer '+token},'PATCH')
        config={'serverAddr':'127.0.0.1','serverPort':17000,'user':identity['device_id'],'metadatas':{'mf_token':identity['tunnel_token']},'loginFailExit':False,
                'transport':{'tls':{'enable':True,'trustedCaFile':str(root/'fixture/ca.crt'),'serverName':host}},
                'log':{'to':'console','level':'warn','disablePrintColor':True},
                'proxies':[{'name':service,'type':'tcp','localIP':'127.0.0.1','localPort':app.server_port,'remotePort':assignment['remote_port']}]}
        file=root/'frpc.json';file.write_text(json.dumps(config));file.chmod(0o600)
        processes.append(subprocess.Popen([str(ROOT/'dist/tools/frpc'),'-c',str(file)],stdout=log,stderr=log))
        nonce=secrets.token_urlsafe(32)
        launch=api('/v1/services/'+service+'/launches',{'nonce_sha256':hashlib.sha256(nonce.encode()).hexdigest(),'path':'/'},{'Authorization':'Bearer '+token})
        app_host=launch['origin'].removeprefix('https://')
        assert app_host.endswith('.'+host) and len(app_host.split('.')[0])==36
        from urllib.parse import urlencode
        status,_,result=request(app_host,'/_mf/bootstrap',{'Content-Type':'application/x-www-form-urlencoded'},'POST',urlencode({'ticket':launch['ticket'],'nonce':nonce}).encode())
        assert status==303 and 'Domain=' not in result['Set-Cookie']
        cookie=result['Set-Cookie'].split(';')[0]
        deadline=time.monotonic()+10
        while True:
            status,payload,result=request(app_host,'/',{'Cookie':cookie,'Authorization':'Bearer '+token})
            if status==200:break
            assert time.monotonic()<deadline,(status,payload)
            time.sleep(.1)
        assert payload==b'generic application through FRP' and 'Domain=' not in result['Set-Cookie']
        assert request(app_host,'/relative',{'Cookie':cookie})[2]['Location']=='login?from=relay'
        assert all(h.get('Authorization')!='Bearer '+token and '__Host-mf_' not in h.get('Cookie','') for h in seen)
        with context.wrap_socket(socket.create_connection(('127.0.0.1',18444),timeout=5),server_hostname=app_host) as ws:
            ws.sendall(('GET /ws HTTP/1.1\r\nHost: '+app_host+'\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nOrigin: https://'+app_host+'\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nCookie: '+cookie+'\r\n\r\n').encode())
            reader=ws.makefile('rb');assert reader.readline().startswith(b'HTTP/1.1 101')
            while reader.readline()!=b'\r\n':pass
            mask=b'abcd';data=b'ping';ws.sendall(bytes([0x81,0x84])+mask+bytes(v^mask[i%4] for i,v in enumerate(data)))
            assert reader.read(2)==bytes([0x81,4]) and reader.read(4)==b'ping'
        print('PASS: signed /api enrollment+sync, real frpc/frps app transfer, launch, cookie/token isolation, relative redirect and WebSocket, '+host,flush=True)
    finally:
        app.shutdown();app.server_close();thread.join(timeout=5)


def native():
    server = Path(os.environ.get('MF_TEST_SERVER', ROOT / 'dist/unified-test/mfconnect-server'))
    nginx = Path(os.environ.get('MF_TEST_NGINX', ROOT / 'dist/test-runtime/root/usr/sbin/nginx'))
    frps = ROOT / 'dist/tools/frps'
    for host, migrating in [('example.test',False), ('connect.example.test',False), ('example.test',True)]:
        for port in [8443, 9091, 17000, 18080, 18444]:
            with socket.socket() as s:
                s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
                s.bind(('127.0.0.1', port))
        with tempfile.TemporaryDirectory(prefix='mfconnect-unified-') as temporary:
            root = Path(temporary); fixture = root / 'fixture'; state = root / 'state'; certs = root / 'certs'
            certs.mkdir()
            legacy = ['--api-host','api.old.test','--base-domain','apps.old.test','--connect-host','browser.old.test','--tunnel-host','relay.old.test'] if migrating else []
            run([str(server), 'init', '--dir', str(fixture), '--entry', 'https://' + host, '--frps', str(frps)] + legacy)
            shutil.copyfile(fixture/'server.crt', certs/'fullchain.pem')
            shutil.copyfile(fixture/'server.key', certs/'privkey.pem')
            c = cfg(host); c.update(deployment_dir=str(root), trust_bundle=str(fixture/'ca.crt'))
            downloads = root/'public/downloads/mfconnect'; downloads.mkdir(parents=True)
            (downloads/'fixture.plg').write_text('release fixture')
            c['downloads_root'] = str(root/'public')
            if migrating: c.update(legacy_api='api.old.test',legacy_apps='apps.old.test',legacy_browser='browser.old.test',tunnel_host='relay.old.test'); c=manage.configuration(c)
            manage.verify_certificate(c, certs)
            # Prove *.D is insufficient for a different entry or a second level.
            bad = dict(c, sans=['other.test', '*.other.test'])
            try: manage.verify_certificate(bad, certs)
            except ValueError: pass
            else: raise AssertionError('wrong SAN accepted')
            env = {k:v for k,v in os.environ.items() if not k.startswith('MF_')}
            env.update(MF_ENTRY_ORIGIN='https://' + host, MF_STATE_DIR=str(state), MF_TLS_DIR=str(certs),
                       MF_SERVER_BIN=str(server), MF_FRPS_BIN=str(frps), SSL_CERT_FILE=str(fixture/'ca.crt'), CURL_CA_BUNDLE=str(fixture/'ca.crt'))
            if migrating: env.update(MF_API_HOST='api.old.test',MF_BASE_DOMAIN='apps.old.test',MF_CONNECT_HOST='browser.old.test',MF_TUNNEL_HOST='relay.old.test')
            cmd = ['sh', str(ROOT/'deploy/docker/entrypoint.sh')]
            run(cmd + ['init'], env)
            saved = json.loads((state/'server.json').read_text())
            assert saved['entry_origin'] == 'https://' + host
            assert saved['api_host'] == ('api.old.test' if migrating else host) and saved['base_domain'] == ('apps.old.test' if migrating else host)
            run(cmd + ['init'], env, success=False)
            run(cmd + ['serve'], dict(env, MF_ENTRY_ORIGIN='https://other.test'), success=False)
            run([str(server), 'check', '--dir', str(state)], env)
            site = manage.nginx(c).replace('listen 80;', 'listen 18080;').replace('listen 443 ssl;', 'listen 18444 ssl;').replace('127.0.0.1:18443', '127.0.0.1:8443')
            conf = root/'nginx.conf'
            conf.write_text(f'pid {root}/nginx.pid; error_log stderr; events {{}} http {{ access_log off; client_body_temp_path {root}/body; proxy_temp_path {root}/proxy; fastcgi_temp_path {root}/fastcgi; uwsgi_temp_path {root}/uwsgi; scgi_temp_path {root}/scgi;\n' + site + '\n}')
            run([str(nginx), '-p', str(root), '-c', str(conf), '-t'])
            context = ssl.create_default_context(cafile=str(fixture/'ca.crt'))
            def request(name, path, headers=None, method='GET', body=None):
                conn = http.client.HTTPConnection('127.0.0.1', 18444, timeout=2)
                conn.sock = context.wrap_socket(socket.create_connection(('127.0.0.1',18444),timeout=2), server_hostname=name)
                try:
                    conn.request(method, path, body=body, headers={'Host': name, **(headers or {})})
                    response=conn.getresponse(); data=response.read()
                    return response.status, data, dict(response.headers)
                finally: conn.close()
            with (root/'process.log').open('wb') as log:
                processes = [subprocess.Popen(cmd+['serve'],env=env,stdout=log,stderr=log),
                             subprocess.Popen([str(nginx),'-p',str(root),'-c',str(conf),'-g','daemon off;'],stdout=log,stderr=log)]
                try:
                    deadline = time.monotonic()+15
                    while True:
                        assert all(p.poll() is None for p in processes), (root/'process.log').read_text()[-3000:]
                        try:
                            if request(host,'/api/healthz')[0] == 200 and (state/'run/frps-status.json').is_file(): break
                        except (OSError,http.client.HTTPException): pass
                        assert time.monotonic()<deadline, (root/'process.log').read_text()[-3000:]
                        time.sleep(.1)
                    run(['sh',str(ROOT/'deploy/docker/healthcheck.sh')],env)
                    status, data, _ = request(host,'/.well-known/mfconnect')
                    assert status==200 and json.loads(data)['api_base_url']=='https://'+host+'/api'
                    assert request(host,'/api/v1/devices')[0]==401
                    token=(state/'admin-token').read_text().strip()
                    assert request(host,'/api/v1/devices',{'Authorization':'Bearer '+token})[0]==200
                    assert request('unknown.'+host,'/')[0]==404
                    assert request(host,'/downloads/mfconnect/fixture.plg')[:2]==(200,b'release fixture')
                    assert request('unknown.'+host,'/downloads/mfconnect/fixture.plg')[0]==404
                    assert request(host,'/api/v1/devices',{'Origin':'https://unknown.'+host,'Authorization':'Bearer '+token})[0]==403
                    if not migrating:
                        relay_application(host,root,context,request,token,processes,log)
                    else:
                        assert request('api.old.test','/healthz')[0]==200
                        assert request('api.old.test','/downloads/mfconnect/fixture.plg')[:2]==(200,b'release fixture')
                        status,data,result=request('api.old.test','/v1/devices',{'Authorization':'Bearer '+token})
                        assert status==200 and 'Location' not in result
                        assert request('browser.old.test','/')[0]==200
                        assert request('unpublished.apps.old.test','/')[0]==404
                        print('PASS: old API/browser/app aliases and new /api simultaneously served through nginx TLS, no token redirect',flush=True)
                    print('PASS: native Docker entrypoint + real nginx + verified TLS/SNI + same-origin /api + FRP health, '+host,flush=True)
                finally:
                    for p in reversed(processes): p.terminate()
                    for p in reversed(processes): p.wait(timeout=20)
    print('LIMIT: controlled private CA and native subprocesses; no Docker daemon, public DNS/ACME or production changes',flush=True)


if __name__ == '__main__':
    if '--native' in sys.argv: native()
    else: unittest.main()
