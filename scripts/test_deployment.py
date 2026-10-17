#!/usr/bin/env python3
"""Test the deployed HTTPS/FRP path with a temporary local connector and app.

Requires private operator/test-user token files. The test binding is revoked in
finally. This does not claim Unraid or mobile WebView validation.
"""
import argparse
import base64
import hashlib
import http.client
import http.server
import ipaddress
import json
import os
from pathlib import Path
import socket
import ssl
import subprocess
import tempfile
import threading
import time
from urllib.parse import urlencode, urlsplit

ROOT = Path(__file__).resolve().parents[1]


class App(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *_):
        pass

    def do_GET(self):
        if self.headers.get('Upgrade', '').lower() == 'websocket':
            accept = base64.b64encode(hashlib.sha1(
                (self.headers['Sec-WebSocket-Key'] + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()
            ).digest()).decode()
            self.send_response(101)
            self.send_header('Upgrade', 'websocket')
            self.send_header('Connection', 'Upgrade')
            self.send_header('Sec-WebSocket-Accept', accept)
            self.end_headers()
            while True:
                frame = self.rfile.read(2)
                if len(frame) != 2:
                    break
                length = frame[1] & 127
                mask, payload = self.rfile.read(4), self.rfile.read(length)
                if len(mask) != 4 or len(payload) != length:
                    break
                decoded = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
                self.wfile.write(bytes([0x81, len(decoded)]) + decoded)
                self.wfile.flush()
            self.close_connection = True
            return
        data = json.dumps({'cookie': self.headers.get('Cookie', ''), 'path': self.path}).encode()
        if self.path == '/range':
            assert self.headers.get('Range') == 'bytes=2-5'
            data = b'2345'
            self.send_response(206)
            self.send_header('Content-Range', 'bytes 2-5/10')
        elif self.path == '/redirect':
            data = b''
            self.send_response(302)
            self.send_header('Location', f'http://{self.headers["Host"]}/after')
        else:
            self.send_response(200)
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        data = self.rfile.read(int(self.headers['Content-Length']))
        result = hashlib.sha256(data).hexdigest().encode()
        self.send_response(200)
        self.send_header('Content-Length', str(len(result)))
        self.end_headers()
        self.wfile.write(result)


def wait_for(fn, label, timeout=45):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            result = fn()
            if result:
                return result
        except (OSError, http.client.HTTPException):
            pass
        time.sleep(0.5)
    raise AssertionError(label)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--api-host', default='api.connect.example.com')
    parser.add_argument('--owner', default='deploymentcheck')
    parser.add_argument('--admin-file', type=Path, required=True)
    parser.add_argument('--user-file', type=Path, required=True)
    parser.add_argument('--result', type=Path, required=True)
    args = parser.parse_args()
    admin, user = args.admin_file.read_text().strip(), args.user_file.read_text().strip()
    context = ssl.create_default_context()

    def request(method, path, data=None, token=None, host=None, cookie=None, form=False, headers=None):
        h = dict(headers or {})
        if token:
            h['Authorization'] = 'Bearer ' + token
        if cookie:
            h['Cookie'] = cookie
        if isinstance(data, dict):
            data = (urlencode(data) if form else json.dumps(data)).encode()
            h['Content-Type'] = 'application/x-www-form-urlencoded' if form else 'application/json'
        conn = http.client.HTTPSConnection(host or args.api_host, timeout=10, context=context)
        try:
            conn.request(method, path, data, h)
            response = conn.getresponse()
            return response.status, dict(response.getheaders()), response.read()
        finally:
            conn.close()

    assert request('GET', '/healthz')[0] == 200
    assert request('GET', '/v1/devices')[0] == 401
    app = http.server.ThreadingHTTPServer(('127.0.0.1', 0), App)
    app.daemon_threads = True
    threading.Thread(target=app.serve_forever, daemon=True).start()
    proc, device_id, ws, wsfile = None, None, None, None
    passed = False
    with tempfile.TemporaryDirectory(prefix='mfconnect-public-e2e-') as temporary:
        work = Path(temporary)
        log = (work / 'connector.log').open('wb')
        try:
            status, _, raw = request('POST', '/v1/enrollments', {'owner': args.owner}, admin)
            assert status == 201
            invite = work / 'enrollment'
            invite.write_text(json.loads(raw)['enrollment_token'])
            invite.chmod(0o600)
            config = work / 'config.json'
            daemon = ROOT / 'dist/mfconnectd'
            def command(*items):
                subprocess.run([str(i) for i in items], stdout=log, stderr=subprocess.STDOUT,
                               check=True, timeout=30)
            command(daemon, 'init', '--config', config, '--server', 'https://' + args.api_host,
                    '--ca', '/etc/ssl/certs/ca-certificates.crt', '--name', 'Deployment verification')
            local = json.loads(config.read_text())
            with socket.socket() as sock:
                sock.bind(('127.0.0.1', 0))
                local['gateway_port'] = sock.getsockname()[1]
            local['targets'] = [{'key': 'fixture', 'name': 'Deployment test fixture',
                                 'url': f'http://127.0.0.1:{app.server_port}'}]
            config.write_text(json.dumps(local))
            command(daemon, 'enroll', '--config', config, '--enrollment-file', invite)
            device_id = json.loads(config.read_text())['device_id']
            proc = subprocess.Popen([str(daemon), 'run', '--config', str(config),
                                     '--run-dir', str(work / 'run'), '--frpc', str(ROOT / 'dist/tools/frpc'),
                                     '--interval', '1s'], stdout=log, stderr=subprocess.STDOUT)

            def service():
                status, _, raw = request('GET', '/v1/services', token=user)
                assert status == 200
                return next((s for s in json.loads(raw) if s['device_id'] == device_id), None)
            svc = wait_for(service, 'Connector did not register through public API')
            status, _, raw = request('GET', '/v1/devices', token=user)
            device = next(d for d in json.loads(raw) if d['id'] == device_id)
            assert device['network']['interfaces_updated_at'] > 0
            assert ipaddress.ip_address(device['network']['public_ip']).is_global
            sid, host = svc['id'], urlsplit(svc['origin']).hostname
            assert request('GET', '/', host=host)[0] == 404
            assert request('PATCH', f'/v1/services/{sid}', {'published': True}, user)[0] == 200
            assert request('GET', '/', host=host)[0] == 401
            nonce = base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip('=')
            status, _, raw = request('POST', f'/v1/services/{sid}/launches',
                                    {'path': '/', 'nonce_sha256': hashlib.sha256(nonce.encode()).hexdigest()}, user)
            assert status == 201
            form = {'ticket': json.loads(raw)['ticket'], 'nonce': nonce}
            status, headers, _ = request('POST', '/_mf/bootstrap', form, host=host, form=True)
            assert status == 303 and headers['Location'] == '/'
            cookie = headers['Set-Cookie'].split(';', 1)[0]
            assert request('POST', '/_mf/bootstrap', form, host=host, form=True)[0] == 401
            wait_for(lambda: request('GET', '/', host=host, cookie=cookie)[0] == 200,
                     'Public FRP tunnel did not forward the application', timeout=60)
            status, _, raw = request('GET', '/', host=host, cookie=cookie + '; app_session=fixture')
            assert status == 200 and json.loads(raw)['cookie'] == 'app_session=fixture'
            data = os.urandom(256 * 1024)
            status, _, raw = request('POST', '/upload', data, host=host, cookie=cookie)
            assert status == 200 and raw.decode() == hashlib.sha256(data).hexdigest()
            status, headers, raw = request('GET', '/range', host=host, cookie=cookie,
                                           headers={'Range': 'bytes=2-5'})
            assert status == 206 and raw == b'2345' and headers['Content-Range'] == 'bytes 2-5/10'
            status, headers, _ = request('GET', '/redirect', host=host, cookie=cookie)
            assert status == 302 and headers['Location'] == 'https://' + host + '/after'
            print('PASS: public TLS, Nginx, container FRP, NAS gateway, source IP, authorization, upload, Range and redirect', flush=True)

            ws = context.wrap_socket(socket.create_connection((host, 443), timeout=5), server_hostname=host)
            wsfile = ws.makefile('rwb', buffering=0)
            key = base64.b64encode(os.urandom(16)).decode()
            handshake = (f'GET /ws HTTP/1.1\r\nHost: {host}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n'
                         f'Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: {key}\r\nCookie: {cookie}\r\n\r\n')
            wsfile.write(handshake.encode())
            assert b' 101 ' in wsfile.readline()
            for _ in range(100):
                line = wsfile.readline()
                assert line, 'WebSocket closed during handshake'
                if line == b'\r\n':
                    break
            else:
                raise AssertionError('Too many WebSocket response headers')
            message, mask = b'ping', os.urandom(4)
            wsfile.write(b'\x81\x84' + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(message)))
            assert wsfile.read(2) == b'\x81\x04' and wsfile.read(4) == message
            assert request('PATCH', f'/v1/services/{sid}', {'published': False}, user)[0] == 200
            assert wsfile.read(1) == b''
            assert request('GET', '/', host=host, cookie=cookie)[0] == 404
            print('PASS: public WebSocket and revocation closes the active stream', flush=True)
            passed = True
        finally:
            if wsfile:
                wsfile.close()
            if ws:
                ws.close()
            try:
                if device_id:
                    status, _, _ = request('DELETE', '/v1/devices/' + device_id, token=user)
                    assert status == 200, 'Test device revocation failed'
            finally:
                if proc:
                    proc.terminate()
                    try:
                        proc.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        proc.kill()
                        proc.wait(timeout=5)
                app.shutdown()
                app.server_close()
                log.close()
        assert passed
        args.result.parent.mkdir(parents=True, exist_ok=True)
        args.result.write_text(json.dumps({'version': '0.3.0', 'passed': True,
            'scope': 'local temporary connector through public Nginx/TLS and real cloud container FRP; no Unraid/phone',
            'public_ip_reported': True, 'test_device_revoked': True,
            'checked_at': int(time.time())}, indent=2) + '\n')
        print('Revoked temporary test binding and stopped local fixture/connector.', flush=True)


if __name__ == '__main__':
    main()
