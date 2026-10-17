#!/usr/bin/env python3
"""Real-process TLS/frp integration. No Docker, internet or Unraid is simulated.

The application fixture is local; this verifies real relay processes, not NAT,
the Unraid plugin manager, or a mobile WebView.
"""
import argparse
import base64
import hashlib
import http.client
import http.server
import json
import os
from pathlib import Path
import signal
import socket
import ssl
import sqlite3
import subprocess
import tempfile
import threading
import time
from urllib.parse import urlencode, urlsplit

root = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser()
parser.add_argument('--transport', choices=('frp', 'easytier'), default='frp')
parser.add_argument('--frpc', type=Path, default=root / 'dist/tools/frpc')
parser.add_argument('--frps', type=Path, default=root / 'dist/tools/frps')
parser.add_argument('--easytier', type=Path, default=root / 'dist/tools/easytier-core')
args = parser.parse_args()
args.frpc, args.frps = args.frpc.resolve(), args.frps.resolve()

def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]

def wait_for(fn, message, timeout=30):
    until = time.monotonic() + timeout
    last = None
    while time.monotonic() < until:
        try:
            value = fn()
            if value:
                return value
        except (OSError, ValueError, KeyError, http.client.HTTPException) as exc:
            last = exc
        time.sleep(0.2)
    raise AssertionError(f'{message}: {last}')

class App(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'
    def log_message(self, *_):
        pass
    def do_GET(self):
        if self.headers.get('Upgrade', '').lower() == 'websocket':
            key = self.headers['Sec-WebSocket-Key']
            accept = base64.b64encode(hashlib.sha1((key + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()).digest()).decode()
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
                mask = self.rfile.read(4)
                data = self.rfile.read(length)
                if len(mask) != 4 or len(data) != length:
                    break
                decoded = bytes(b ^ mask[i % 4] for i, b in enumerate(data))
                self.wfile.write(bytes([0x81, len(decoded)]) + decoded)
                self.wfile.flush()
            self.close_connection = True
            return
        data = json.dumps({'cookie': self.headers.get('Cookie', ''), 'path': self.path}).encode()
        if self.path == '/range':
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
        self.send_response(200)
        response = hashlib.sha256(data).hexdigest().encode()
        self.send_header('Content-Length', str(len(response)))
        self.end_headers()
        self.wfile.write(response)

with tempfile.TemporaryDirectory(prefix='mfconnect-e2e-') as temporary:
    work = Path(temporary)
    processes, logs = [], []
    def start(command, name):
        log = (work / (name + '.log')).open('wb')
        logs.append(log)
        p = subprocess.Popen([str(v) for v in command], stdout=log, stderr=subprocess.STDOUT)
        processes.append(p)
        return p
    def command(*arguments):
        subprocess.run([str(v) for v in arguments], check=True, stdout=subprocess.DEVNULL)
    backend = http.server.ThreadingHTTPServer(('127.0.0.1', 0), App)
    backend.daemon_threads = True
    threading.Thread(target=backend.serve_forever, daemon=True).start()
    try:
        server, daemon = root / 'dist/mfconnect-server', root / 'dist/mfconnectd'
        state = work / 'server'
        command(server, 'init', '--dir', state, '--transport', args.transport, '--frps', args.frps, '--easytier', args.easytier.resolve())
        command(server, 'issue-user', '--dir', state, '--user', 'alice')
        command(server, 'issue-user', '--dir', state, '--user', 'bob')
        config = json.loads((state / 'server.json').read_text())
        api_port, hook_port, tunnel_port, proxy_port = [free_port() for _ in range(4)]
        config.update(listen=f'127.0.0.1:{api_port}', hook_listen=f'127.0.0.1:{hook_port}', tunnel_port=tunnel_port, port_start=proxy_port, port_end=proxy_port)
        if args.transport == 'easytier':
            config['easytier'].update(listen_host='127.0.0.1', port_start=tunnel_port, port_end=tunnel_port)
        else:
            config['frp']['listen_host'] = '127.0.0.1'
        (state / 'server.json').write_text(json.dumps(config))
        frps = json.loads((state / 'frps.json').read_text())
        frps.update(bindAddr='127.0.0.1', bindPort=tunnel_port, allowPorts=[{'start': proxy_port, 'end': proxy_port}])
        frps['httpPlugins'][0]['addr'] = config['hook_listen']
        (state / 'frps.json').write_text(json.dumps(frps))
        ctx = ssl.create_default_context(cafile=str(state / 'ca.crt'))
        admin = (state / 'admin-token').read_text().strip()
        alice = (state / 'alice-token').read_text().strip()
        bob = (state / 'bob-token').read_text().strip()
        def req(method, path, data=None, token=None, host='localhost', cookie=None, form=False, extra=None):
            headers = {'Host': host}
            if token:
                headers['Authorization'] = 'Bearer ' + token
            if cookie:
                headers['Cookie'] = cookie
            if isinstance(data, bytes):
                encoded = data
            elif data is not None:
                encoded = urlencode(data).encode() if form else json.dumps(data).encode()
                headers['Content-Type'] = 'application/x-www-form-urlencoded' if form else 'application/json'
            else:
                encoded = None
            headers.update(extra or {})
            connection = http.client.HTTPSConnection('localhost', api_port, context=ctx, timeout=5)
            try:
                connection.request(method, path, body=encoded, headers=headers)
                response = connection.getresponse()
                return response.status, dict(response.getheaders()), response.read()
            finally:
                connection.close()
        controller = start([server, 'serve', '--dir', state], 'controller')
        wait_for(lambda: req('GET', '/healthz')[0] == 200, 'controller did not start')
        code, _, raw = req('POST', '/v1/enrollments', {'owner': 'alice'}, admin)
        assert code == 201
        invite = work / 'invite'
        invite.write_text(json.loads(raw)['enrollment_token'])
        connector_config = work / 'device.json'
        command(daemon, 'init', '--config', connector_config, '--server', f'https://localhost:{api_port}', '--ca', state / 'ca.crt', '--transport', args.transport)
        local = json.loads(connector_config.read_text())
        local['gateway_port'] = free_port()
        local['targets'] = [{'key': 'fixture', 'name': 'Integration fixture', 'url': f'http://127.0.0.1:{backend.server_port}'}]
        connector_config.write_text(json.dumps(local))
        command(daemon, 'enroll', '--config', connector_config, '--enrollment-file', invite)
        run = work / 'runtime'
        connector = start([daemon, 'run', '--config', connector_config, '--run-dir', run, '--frpc', args.frpc, '--easytier', args.easytier.resolve(), '--interval', '1s'], 'connector')
        def get_service():
            code, _, raw = req('GET', '/v1/services', token=alice)
            services = json.loads(raw)
            return services[0] if code == 200 and services else None
        service = wait_for(get_service, 'device did not register its target')
        code, _, raw = req('GET', '/v1/devices', token=alice)
        reported_device = json.loads(raw)[0]
        assert code == 200 and reported_device['network']['interfaces_updated_at'] > 0
        assert isinstance(reported_device['network']['interface_ips'], list)
        assert req('GET', '/v1/devices', token=bob)[2].strip() == b'[]'
        service_id = service['id']
        host = urlsplit(service['origin']).hostname
        assert req('GET', '/', host=host)[0] == 404
        assert req('PATCH', f'/v1/services/{service_id}', {'published': True}, bob)[0] == 403
        assert req('PATCH', f'/v1/services/{service_id}', {'published': True}, alice)[0] == 200
        assert req('GET', '/', host=host)[0] == 401
        nonce = base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip('=')
        launch_body = {'nonce_sha256': hashlib.sha256(nonce.encode()).hexdigest(), 'path': '/'}
        assert req('POST', f'/v1/services/{service_id}/launches', launch_body, bob)[0] == 403
        code, _, raw = req('POST', f'/v1/services/{service_id}/launches', launch_body, alice)
        assert code == 201
        ticket = json.loads(raw)['ticket']
        code, headers, _ = req('POST', '/_mf/bootstrap', {'ticket': ticket, 'nonce': nonce}, host=host, form=True)
        assert code == 303 and headers['Location'] == '/'
        cookie = headers['Set-Cookie'].split(';')[0]
        assert req('POST', '/_mf/bootstrap', {'ticket': ticket, 'nonce': nonce}, host=host, form=True)[0] == 401
        wait_for(lambda: req('GET', '/', host=host, cookie=cookie)[0] == 200, 'real tunnel not ready')
        code, _, raw = req('GET', '/', host=host, cookie=cookie + '; app_session=fixture')
        assert code == 200 and json.loads(raw)['cookie'] == 'app_session=fixture'
        payload = os.urandom(256 * 1024)
        code, _, raw = req('POST', '/upload', payload, host=host, cookie=cookie)
        assert code == 200 and raw.decode() == hashlib.sha256(payload).hexdigest()
        code, headers, raw = req('GET', '/range', host=host, cookie=cookie, extra={'Range': 'bytes=2-5'})
        assert code == 206 and raw == b'2345' and headers['Content-Range'] == 'bytes 2-5/10'
        code, headers, _ = req('GET', '/redirect', host=host, cookie=cookie)
        assert code == 302 and headers['Location'] == f'https://{host}/after'
        print(f'PASS: signed device enrollment, real TLS/{args.transport} relay, service authorization, one-use ticket, cookies, upload, Range, redirects', flush=True)
        # An actual frps outage exercises frpc's network reconnection behavior.
        if args.transport == 'frp':
            relay_status = state / 'run/frps-status.json'
            previous = json.loads(relay_status.read_text())['process_id']
            os.kill(previous, signal.SIGKILL)
            assert req('GET', '/', host=host, cookie=cookie)[0] == 503
            wait_for(lambda: json.loads(relay_status.read_text()).get('process_id', previous) != previous, 'FRP server child not restarted')
        else:
            previous = json.loads((run / 'status.json').read_text())['process_id']
            os.kill(previous, signal.SIGKILL)
            wait_for(lambda: json.loads((run / 'status.json').read_text()).get('process_id', previous) != previous, 'EasyTier child not restarted')
        wait_for(lambda: req('GET', '/', host=host, cookie=cookie)[0] == 200, 'tunnel did not reconnect', timeout=40)
        print('PASS: transport process interruption and automatic reconnection', flush=True)
        if args.transport in ('frp', 'easytier'):
            # Bypass the public gateway and test the NAS's own boundary.
            # This test-only client verifies the exact certificate pin from the
            # signed enrollment/sync record, as the cloud gateway does.
            with sqlite3.connect(config['database']) as db:
                dbstate = json.loads(db.execute('SELECT payload FROM state WHERE id=1').fetchone()[0])
            device = next(iter(dbstate['devices'].values()))
            forwarded_gateway, blocked_app, wrong_network = free_port(), free_port(), free_port()
            def overlay_client(name, secret, forwards):
                cfg = f'instance_name = "{name}"\nhostname = "{name}"\nipv4 = "10.144.0.3"\nlisteners = []\nstun_servers = []\nstun_servers_v6 = []\n'
                cfg += f'[[peer]]\nuri = "tcp://127.0.0.1:{tunnel_port}"\n'
                for bind, target in forwards:
                    cfg += f'[[port_forward]]\nbind_addr = "127.0.0.1:{bind}"\ndst_addr = "10.144.0.2:{target}"\nproto = "tcp"\n'
                cfg += f'[network_identity]\nnetwork_name = "{device["id"]}"\nnetwork_secret = "{secret}"\n'
                cfg += '[flags]\nno_tun = true\nuse_smoltcp = true\nprivate_mode = true\ndisable_p2p = true\nneed_p2p = false\ndisable_tcp_hole_punching = true\ndisable_udp_hole_punching = true\ndisable_sym_hole_punching = true\ndisable_upnp = true\nenable_ipv6 = false\nbind_device = false\naccept_dns = false\n'
                path = work / f'{name}.toml'
                path.write_text(cfg)
                path.chmod(0o600)
                return start([args.easytier.resolve(), '-c', path, '--rpc-portal', '0', '--disable-env-parsing', '--console-log-level', 'error'], name)
            if args.transport == 'easytier':
                overlay_client('client', device['easytier_secret'], [(forwarded_gateway, local['gateway_port']), (blocked_app, backend.server_port)])
                overlay_client('wrong-secret', 'f' * 64, [(wrong_network, local['gateway_port'])])
            else:
                # The private FRP service forward must terminate at the pinned
                # NAS gateway, not at the raw backend application.
                forwarded_gateway = proxy_port
            pinctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
            pinctx.check_hostname = False
            pinctx.verify_mode = ssl.CERT_NONE
            class PinnedNAS(http.client.HTTPSConnection):
                def connect(self):
                    super().connect()
                    assert hashlib.sha256(self.sock.getpeercert(binary_form=True)).hexdigest() == device['gateway_pin']
            def nas_req(cookie_value=None, host_value=host):
                c = PinnedNAS('127.0.0.1', forwarded_gateway, context=pinctx, timeout=5)
                try:
                    headers = {'Host': host_value}
                    if cookie_value:
                        headers['Cookie'] = cookie_value
                    c.request('GET', '/', headers=headers)
                    response = c.getresponse()
                    response.read()
                    return response.status
                finally:
                    c.close()
            wait_for(lambda: nas_req() == 401, 'third node cannot reach NAS gateway')
            assert nas_req('__Host-mf_session=not-a-session') == 401
            assert nas_req(cookie) == 200
            assert nas_req(cookie, 'another.apps.localhost') == 401
            for port_to_test in ((blocked_app, wrong_network) if args.transport == 'easytier' else ()):
                connection = http.client.HTTPConnection('127.0.0.1', port_to_test, timeout=2)
                try:
                    connection.request('GET', '/')
                    connection.getresponse()
                except (OSError, http.client.HTTPException):
                    pass
                else:
                    raise AssertionError('Unapproved host port or foreign-network access was allowed')
                finally:
                    connection.close()
            if args.transport == 'frp':
                generated = json.loads((run / 'frpc.json').read_text())
                assert all(p['localIP'] == '127.0.0.1' and p['localPort'] == local['gateway_port'] for p in generated['proxies'])
                # Test independent NAS enforcement even after frps removes the
                # unpublished proxy or the controller is unavailable.
                forwarded_gateway = local['gateway_port']
            print('PASS: NAS gateway via real tunnel requires a valid host-bound session and verifies its enrolled certificate; no raw application port forwarding', flush=True)
        def open_websocket(port, context, pin=None):
            ws = context.wrap_socket(socket.create_connection(('127.0.0.1', port)), server_hostname='localhost' if pin is None else 'mfconnect-device')
            if pin is not None:
                assert hashlib.sha256(ws.getpeercert(binary_form=True)).hexdigest() == pin
            ws.settimeout(8)
            key = base64.b64encode(os.urandom(16)).decode()
            ws.sendall((f'GET /ws HTTP/1.1\r\nHost: {host}\r\nCookie: {cookie}\r\nOrigin: https://{host}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: {key}\r\n\r\n').encode())
            stream = ws.makefile('rb')
            assert b'101' in stream.readline()
            while True:
                line = stream.readline()
                assert line, 'EOF in WebSocket handshake'
                if line == b'\r\n':
                    break
            data, mask = b'hello', os.urandom(4)
            ws.sendall(bytes([0x81, 0x80 | len(data)]) + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(data)))
            assert stream.read(2) == b'\x81\x05' and stream.read(5) == b'hello'
            return ws, stream
        ws, stream = open_websocket(api_port, ctx)
        if args.transport in ('frp', 'easytier'):
            direct_ws, direct_stream = open_websocket(forwarded_gateway, pinctx, device['gateway_pin'])
        assert req('PATCH', f'/v1/services/{service_id}', {'published': False}, alice)[0] == 200
        assert stream.read(1) == b''
        stream.close()
        ws.close()
        assert req('GET', '/', host=host, cookie=cookie)[0] == 404
        if args.transport in ('frp', 'easytier'):
            assert nas_req(cookie) == 401
            assert direct_stream.read(1) == b'', 'NAS did not revoke a stream bypassing the public gateway'
            direct_stream.close()
            direct_ws.close()
            print('PASS: NAS revokes the third-node WebSocket independently of the public gateway', flush=True)
        print('PASS: WebSocket round trip; unpublishing closes the active stream and rejects further access', flush=True)
        if args.transport in ('frp', 'easytier'):
            assert req('PATCH', f'/v1/services/{service_id}', {'published': True}, alice)[0] == 200
            code, _, raw = req('POST', f'/v1/services/{service_id}/launches', launch_body, alice)
            assert code == 201
            code, headers, _ = req('POST', '/_mf/bootstrap', {'ticket': json.loads(raw)['ticket'], 'nonce': nonce}, host=host, form=True)
            assert code == 303
            cookie = headers['Set-Cookie'].split(';')[0]
            offline_ws, offline_stream = open_websocket(forwarded_gateway, pinctx, device['gateway_pin'])
            controller.send_signal(signal.SIGSTOP)
            try:
                assert offline_stream.read(1) == b'', 'NAS stream survived failed authorization renewal'
                assert nas_req(cookie) == 503, 'Control outage incorrectly granted access or appeared as a login failure'
            finally:
                controller.send_signal(signal.SIGCONT)
                offline_stream.close()
                offline_ws.close()
            wait_for(lambda: nas_req(cookie) == 200, 'authorization did not recover')
            assert req('DELETE', f'/v1/devices/{device["id"]}', token=alice)[0] == 200
            wait_for(lambda: json.loads((run / 'status.json').read_text())['state'] == 'authorization_required', 'revoked connector did not stop')
            assert 'process_id' not in json.loads((run / 'status.json').read_text())
            print('PASS: controller outage closes NAS streams, denies new access with 503, then recovers; device revocation stops the tunnel', flush=True)
    except Exception:
        for log in logs:
            log.flush()
        # Logs are private test artifacts. Print only the process exit status,
        # never credentials or request bodies.
        print('Process exit codes:', [p.poll() for p in processes], flush=True)
        raise
    finally:
        for process in reversed(processes):
            if process.poll() is None:
                process.send_signal(signal.SIGCONT)
                process.terminate()
                try:
                    process.wait(timeout=8)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
        backend.shutdown()
        backend.server_close()
        for log in logs:
            log.close()
