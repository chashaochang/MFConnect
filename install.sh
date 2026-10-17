#!/bin/sh
# Optional downloader/configuration wizard. The runtime is Docker Compose only.
set -eu
umask 077
if [ "${1:-}" = --help ]; then
    echo 'Run from an empty directory: sh install.sh'
    echo 'Requires Docker Compose V2, curl, tar and sha256sum. Installs no host services.'
    exit 0
fi
docker compose version >/dev/null
docker info >/dev/null
for command in curl tar sha256sum; do command -v "$command" >/dev/null; done
[ ! -e mfconnect ] || { echo 'mfconnect directory already exists; use its Compose configuration.' >&2; exit 1; }
version=0.7.2
base="https://github.com/chashaochang/MFConnect/releases/download/v${version}"
name="mfconnect-${version}-docker-amd64.tar.gz"
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
fetch() { curl -fL --proto '=https' --proto-redir '=https' --retry 3 --connect-timeout 15 --max-time 900 "$1" -o "$2"; }
fetch "$base/$name" "$work/$name"
fetch "$base/SHA256SUMS" "$work/SHA256SUMS"
(cd "$work" && sha256sum -c SHA256SUMS)
tar -tzf "$work/$name" | while IFS= read -r path; do
    case "$path" in mfconnect/*) ;; *) exit 1;; esac
    case "/$path/" in */../*|*/./*|*//* ) exit 1;; esac
done
tar -tvzf "$work/$name" | awk 'substr($0,1,1) != "-" {exit 1}'
tar -xzf "$work/$name" --no-same-owner
cd mfconnect
sha256sum -c CONTENTS.sha256
docker load --input images.tar.gz
docker run --rm -it --network none -v "$PWD:/output" "mfconnect-gateway:$version" configure
echo '准备好域名解析和证书后，在 mfconnect 目录执行：docker compose up -d'
