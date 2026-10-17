#!/bin/bash
# Download a versioned deployment bundle, verify it, then start the local wizard.
set -euo pipefail
umask 077
if [ "${1:-}" = --help ]; then
  cat <<'HELP'
MF Connect cloud installer
Usage: sudo bash install.sh [--plan | --resume | --config /root/mfconnect-install.json]
Interactive installation asks for the entry domain, DNS zone, public IPv4 and
Tencent DNS credentials. Existing MF Connect deployments are never overwritten.
HELP
  exit 0
fi
[ "$(id -u)" = 0 ] || { echo '请使用 sudo bash install.sh 运行。' >&2; exit 1; }
[ "$(uname -s)" = Linux ] && [ "$(uname -m)" = x86_64 ] || { echo '当前安装包支持 Linux amd64。' >&2; exit 1; }
if ! command -v curl >/dev/null || ! command -v python3 >/dev/null; then
  if command -v apt-get >/dev/null; then
    apt-get update
    apt-get install -y curl ca-certificates python3
  elif command -v dnf >/dev/null; then
    dnf install -y curl ca-certificates python3
  else
    echo '请先安装 curl、CA 证书和 Python 3。' >&2
    exit 1
  fi
fi
python3 -c 'import sys; sys.exit(0 if sys.version_info >= (3, 9) else "需要 Python 3.9 或更新版本。")'
version=0.7.1
base="https://github.com/chashaochang/MFConnect/releases/download/v${version}"
archive="mfconnect-${version}-linux-amd64-deploy.tar.gz"
work=$(mktemp -d /tmp/mfconnect-install.XXXXXX)
trap 'rm -rf -- "$work"' EXIT
fetch() { curl --fail --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 --retry 3 --connect-timeout 15 --max-time 900 "$1" -o "$2"; }
echo "下载 MF Connect ${version} 部署包……"
fetch "$base/SHA256SUMS" "$work/SHA256SUMS"
fetch "$base/$archive" "$work/$archive"
python3 - "$work" "$archive" <<'PY'
from pathlib import Path, PurePosixPath
import hashlib,re,sys,tarfile
work=Path(sys.argv[1]);name=sys.argv[2]
matches=[]
for line in (work/'SHA256SUMS').read_text().splitlines():
    fields=line.split()
    if len(fields)==2 and fields[1].lstrip('*')==name: matches.append(fields[0])
if len(matches)!=1 or not re.fullmatch(r'[0-9a-f]{64}',matches[0]): raise SystemExit('发行包摘要记录无效。')
digest=hashlib.sha256()
with (work/name).open('rb') as source:
    for block in iter(lambda:source.read(1024*1024),b''):digest.update(block)
if digest.hexdigest()!=matches[0]:raise SystemExit('发行包 SHA-256 不匹配，已停止。')
target=work/'package';target.mkdir()
with tarfile.open(work/name) as bundle:
    members=bundle.getmembers()
    if len(members)>3000 or sum(m.size for m in members)>2*1024**3:raise SystemExit('发行包大小超出限制。')
    for member in members:
        path=PurePosixPath(member.name)
        if path.is_absolute() or '..' in path.parts or not member.isfile():raise SystemExit('发行包包含非普通文件或无效路径。')
        destination=target.joinpath(*path.parts);destination.parent.mkdir(parents=True,exist_ok=True)
        with bundle.extractfile(member) as source,destination.open('wb') as output:
            while True:
                block=source.read(1024*1024)
                if not block:break
                output.write(block)
        destination.chmod(member.mode & 0o777 & ~0o022)
PY
python3 "$work/package/deploy/install/cloud.py" --package "$work/package" "$@"
