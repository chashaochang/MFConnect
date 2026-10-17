#!/bin/bash
set -euo pipefail
if [ "$(id -u)" != 0 ]; then echo 'Run as root on the Docker host.' >&2; exit 1; fi
release=$(cd -- "$(dirname -- "$0")/../.." && pwd)
root=/srv/mfconnect
mkdir -p "$root/ops" "$root/updates"
chmod 700 "$root/updates"
chown 10001:10001 "$root/updates"
install -m 755 "$release/dist/mfconnect-updater" "$root/ops/mfconnect-updater"
python3 - "$release" "$root/ops/updater.json" <<'PY'
import json, os, sys
from pathlib import Path
from urllib.parse import urlsplit
config = {'component':'server','control_dir':'/srv/mfconnect/updates','runtime_dir':'/srv/mfconnect/updates/runtime',
          'compose_dir':sys.argv[1], 'container':'mfconnect-mfconnect-1','project':'mfconnect'}
# 同机 Nginx 经 loopback 下载仍校验正式域名证书，避免公网回环故障。
values = dict(line.split('=',1) for line in (Path(sys.argv[1])/'.env').read_text().splitlines() if '=' in line and not line.startswith('#'))
download_host=urlsplit(values.get('MF_ENTRY_ORIGIN','')).hostname or values.get('MF_API_HOST')
if download_host:
 config.update(download_host=download_host,download_ip='127.0.0.1')
fd = os.open(sys.argv[2], os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(fd,'w') as f: json.dump(config,f,indent=2)
PY
install -m 644 "$release/deploy/docker/mfconnect-updater.service" /etc/systemd/system/mfconnect-updater.service
systemctl daemon-reload
systemctl enable --now mfconnect-updater.service
echo 'Update worker enabled. Configure MF_UPDATE_DIRECTORY and MF_UPDATE_OWNER in the deployment .env.'
