#!/usr/bin/env python3
"""Build an Unraid .plg and checksum-pinned payload from static Linux binaries."""
import argparse
import gzip
import hashlib
import html
import io
import json
from pathlib import Path
import re
import shlex
import subprocess
import tarfile
from urllib.parse import urlparse

root = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser()
parser.add_argument('--version', default='0.9.0', help='Unraid plugin package version')
parser.add_argument('--connector-version', default='0.9.0', help='Bundled connector binary version')
parser.add_argument('--updater', type=Path, default=root / 'dist/mfconnect-updater')
parser.add_argument('--frp-version', default='0.71.1')
parser.add_argument('--binary', type=Path, required=True)
parser.add_argument('--frpc', type=Path, required=True)
parser.add_argument('--frp-license', type=Path, required=True)
source = parser.add_mutually_exclusive_group(required=True)
source.add_argument('--release-base', help='HTTPS directory hosting mfconnect.plg and versioned payloads')
source.add_argument('--local', action='store_true', help='Offline test package with no download or update URL')
parser.add_argument('--output', type=Path, help='Default: dist/plugin/<version>')
args = parser.parse_args()
if not re.fullmatch(r'\d+\.\d+\.\d+(?:[.-][A-Za-z0-9]+)*', args.version):
    parser.error('invalid version')
if args.output is None:
    args.output = root / 'dist/plugin' / args.version
base = (args.release_base or '').rstrip('/')
url = urlparse(base)
if not args.local and (url.scheme != 'https' or not url.hostname or url.username or url.password or url.query or url.fragment or any(c in base for c in '\r\n\t<>')):
    parser.error('--release-base must be an HTTPS directory URL')
for binary in (args.binary, args.frpc, args.updater):
    header = binary.read_bytes()[:20]
    if header[:6] != b'\x7fELF\x02\x01' or header[18:20] != b'\x3e\x00':
        parser.error(f'{binary} is not a Linux amd64 ELF executable')
if subprocess.check_output([str(args.binary.resolve()), 'version'], text=True).strip() != args.connector_version:
    parser.error('connector version differs from --connector-version')
try:
    capabilities = json.loads(subprocess.check_output([str(args.binary.resolve()), 'capabilities'], text=True))
    if 2 not in capabilities.get('endpoint_contracts', []):
        parser.error('connector does not implement endpoint contract 2; refusing to label an old binary')
except (subprocess.CalledProcessError, ValueError):
    parser.error('connector does not provide endpoint capabilities; rebuild before packaging')
if args.frp_version != '0.71.1' or subprocess.check_output([str(args.frpc.resolve()), '--version'], text=True).strip() != args.frp_version:
    parser.error('FRP version mismatch')
args.output.mkdir(parents=True, exist_ok=True)
payload_name = f'mfconnect-{args.version}-linux-amd64.tar.gz'
payload = args.output / payload_name
files = {
    'mfconnectd': (args.binary.read_bytes(), 0o755),
    'frpc': (args.frpc.read_bytes(), 0o755),
    'mfconnect-updater': (args.updater.read_bytes(), 0o755),
    'VERSION': ((args.version + '\n').encode(), 0o644),
    'FRP_VERSION': ((args.frp_version + '\n').encode(), 0o644),
    'FRP-LICENSE': (args.frp_license.read_bytes(), 0o644),
}
for name in ('Connect.php', 'MFConnect.page', 'Settings.php', 'register-api.js', 'api/index.js', 'api/package.json', 'assets/settings.css',
             'assets/settings.js', 'include/Settings.php', 'install.sh', 'rc.mfconnect'):
    path = root / 'plugin' / name
    files[name] = (path.read_bytes(), 0o755 if name in ('install.sh', 'rc.mfconnect') else 0o644)
with payload.open('wb') as output:
    with gzip.GzipFile(filename='', fileobj=output, mode='wb', mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode='w') as archive:
            for path, (data, mode) in sorted(files.items()):
                info = tarfile.TarInfo(path)
                info.size, info.mode, info.mtime = len(data), mode, 0
                archive.addfile(info, io.BytesIO(data))
digest = hashlib.sha256(payload.read_bytes()).hexdigest()
cache = f'/boot/config/plugins/mfconnect/packages/{payload_name}'
download = f'''  curl --fail --location --proto '=https' --proto-redir '=https' --tlsv1.2 --connect-timeout 15 --max-time 300 --output "$cache.download" {shlex.quote(base + '/' + payload_name)}
  printf '%s  %s\\n' {shlex.quote(digest)} "$cache.download" | sha256sum -c -
  mv "$cache.download" "$cache"
'''
if args.local:
    download = '''  echo "Offline test package missing or checksum mismatch: $cache" >&2
  exit 1
'''
install = f'''set -euo pipefail
root=${{MFCONNECT_TEST_ROOT:-}}
cache="$root"{shlex.quote(cache)}
mkdir -p "$(dirname "$cache")"
if ! [ -f "$cache" ] || ! printf '%s  %s\\n' {shlex.quote(digest)} "$cache" | sha256sum -c - >/dev/null 2>&1; then
{download.rstrip()}
fi
stage=$(mktemp -d /tmp/mfconnect-install.XXXXXX)
trap 'rm -rf -- "$stage"' EXIT
tar -xzf "$cache" --no-same-owner --no-overwrite-dir -C "$stage"
bash "$stage/install.sh"
'''
remove = '''set -euo pipefail
if [ -x /etc/rc.d/rc.mfconnect ]; then /etc/rc.d/rc.mfconnect stop; fi
if [ -f /usr/local/lib/mfconnect/current/register-api.js ]; then
  node /usr/local/lib/mfconnect/current/register-api.js remove
  /etc/rc.d/rc.unraid-api archive-dependencies
  /etc/rc.d/rc.unraid-api restart
fi
if [ -r /var/run/mfconnect/updater.pid ]; then
  read -r pid < /var/run/mfconnect/updater.pid
  if [[ "$pid" =~ ^[0-9]+$ ]] && [[ "$(readlink "/proc/$pid/exe" 2>/dev/null || true)" == /usr/local/lib/mfconnect/versions/*/mfconnect-updater ]]; then kill -TERM "$pid"; fi
fi
rm -f /etc/rc.d/rc.mfconnect
rm -rf /usr/local/emhttp/plugins/mfconnect /usr/local/lib/mfconnect /var/run/mfconnect
echo 'mfconnect removed. Device configuration remains in /boot/config/plugins/mfconnect; revoke its authorization on the server if no longer needed.'
'''
update_attribute = '' if args.local else f' pluginURL="{html.escape(base + "/mfconnect.plg", quote=True)}"'
manifest = f'''<?xml version="1.0" standalone="yes"?>
<PLUGIN name="mfconnect" author="MF Connect" version="{html.escape(args.version, quote=True)}" launch="Settings/MFConnect" icon="exchange"{update_attribute} min="7.0.0">
<!-- mfconnect-endpoint-contract:2 -->
<CHANGES>{html.escape(args.version)}: Existing NAS origin application TCP channel v1 for compatible RaidMate clients. Native NAS administrator authentication; automatic Docker target allowlist; no cloud configuration required. Connector {html.escape(args.connector_version)}; FRP {html.escape(args.frp_version)}.</CHANGES>
<FILE Run="/bin/bash"><INLINE><![CDATA[{install}]]></INLINE></FILE>
<FILE Run="/bin/bash" Method="remove"><INLINE><![CDATA[{remove}]]></INLINE></FILE>
</PLUGIN>
'''
(args.output / 'mfconnect.plg').write_text(manifest)
(args.output / f'mfconnect-{args.version}.plg').write_text(manifest)
(args.output / 'SHA256SUMS').write_text(f'{digest}  {payload_name}\n')
(args.output / 'release.json').write_text(json.dumps({'version': args.version, 'connector_version': args.connector_version, 'frp_version': args.frp_version, 'transport': 'frp', 'payload': payload.name, 'sha256': digest, 'local_only': args.local, 'release_base': base or None}, indent=2) + '\n')
print(f'Built {args.output / "mfconnect.plg"} and {payload}')
