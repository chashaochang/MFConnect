#!/usr/bin/env python3
"""Local three-process EasyTier test. Does not claim WAN NAT or mobile coverage."""
import argparse
import http.client
import http.server
import json
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import threading
import time

ROOT = Path(__file__).resolve().parents[1]

def port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]

def wait(fn, label, timeout=35):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            result = fn()
            if result:
                return result
        except (OSError, ValueError, subprocess.SubprocessError, http.client.HTTPException):
            pass
        time.sleep(.3)
    raise AssertionError(label)

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--core', type=Path, default=ROOT / 'dist/tools/easytier-core')
    parser.add_argument('--cli', type=Path, default=ROOT / 'dist/tools/easytier-cli')
    args = parser.parse_args()
    class App(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass
        def do_GET(self):
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b'easytier-application-fixture')
    app = http.server.ThreadingHTTPServer(('127.0.0.1', 0), App)
    threading.Thread(target=app.serve_forever, daemon=True).start()
    try:
        for direct in (True, False):
            with tempfile.TemporaryDirectory(prefix='mfconnect-et-probe-') as tmp:
                work = Path(tmp)
                relay, nas, forwarded = port(), port(), port()
                rpc = [port() for _ in range(3)]
                secret = secrets.token_hex(32)
                procs, logs = [], []
                try:
                    for i, name in enumerate(('relay', 'nas', 'client')):
                        listeners = [f'tcp://127.0.0.1:{relay if i == 0 else nas}'] if i < 2 else []
                        peers = [] if i == 0 else [f'tcp://127.0.0.1:{nas if direct and i == 2 else relay}']
                        cfg = f'instance_name = "{name}"\nhostname = "{name}"\nipv4 = "10.144.0.{i+1}"\nlisteners = {json.dumps(listeners)}\nstun_servers = []\nstun_servers_v6 = []\n'
                        cfg += f'tcp_whitelist = ["{app.server_port if i == 1 else 1}"]\nudp_whitelist = ["1"]\n'
                        if i == 2:
                            cfg += f'[[port_forward]]\nbind_addr = "127.0.0.1:{forwarded}"\ndst_addr = "10.144.0.2:{app.server_port}"\nproto = "tcp"\n'
                        for peer in peers:
                            cfg += f'[[peer]]\nuri = "{peer}"\n'
                        cfg += f'[network_identity]\nnetwork_name = "probe"\nnetwork_secret = "{secret}"\n'
                        cfg += '[flags]\nno_tun = true\nuse_smoltcp = true\nprivate_mode = true\nrelay_network_whitelist = "probe"\ndisable_p2p = true\nneed_p2p = false\ndisable_tcp_hole_punching = true\ndisable_udp_hole_punching = true\ndisable_sym_hole_punching = true\ndisable_upnp = true\nenable_ipv6 = false\naccept_dns = false\nbind_device = false\n'
                        path = work / f'{name}.toml'
                        path.write_text(cfg)
                        path.chmod(0o600)
                        log = (work / f'{name}.log').open('wb')
                        logs.append(log)
                        procs.append(subprocess.Popen([str(args.core), '-c', str(path), '--rpc-portal', f'127.0.0.1:{rpc[i]}', '--console-log-level', 'error', '--disable-env-parsing'], stdout=log, stderr=subprocess.STDOUT))
                    def request():
                        c = http.client.HTTPConnection('127.0.0.1', forwarded, timeout=2)
                        try:
                            c.request('GET', '/')
                            r = c.getresponse()
                            return r.status == 200 and r.read() == b'easytier-application-fixture'
                        finally:
                            c.close()
                    wait(request, 'application traffic failed')
                    routes = json.loads(subprocess.check_output([str(args.cli), '-p', f'127.0.0.1:{rpc[2]}', '-o', 'json', 'route'], timeout=5))
                    route = next(r for r in routes if r['ipv4'].split('/')[0] == '10.144.0.2')
                    assert route['path_len'] == (1 if direct else 2), route
                    assert route['next_hop_hostname'] == ('DIRECT' if direct else 'relay'), route
                    print(f'PASS: actual HTTP over {"direct" if direct else "relay"} route, verified {route["path_len"]} hops', flush=True)
                finally:
                    for p in reversed(procs):
                        p.terminate()
                        try:
                            p.wait(timeout=5)
                        except subprocess.TimeoutExpired:
                            p.kill(); p.wait()
                    for log in logs:
                        log.close()
    finally:
        app.shutdown(); app.server_close()

if __name__ == '__main__':
    main()
