#!/bin/sh
# Downloader/configuration wizard. All services run in Docker containers.
set -eu
umask 077
runtime=compose
offline=0
for option in "$@"; do
    case "$option" in
        --docker) runtime=docker;;
        --compose) runtime=compose;;
        --offline) offline=1;;
        --help)
            echo 'Run from an empty directory: sh install.sh [--docker|--compose] [--offline]'
            echo 'Requires Docker, curl, tar and sha256sum; --compose also needs Compose V2. Installs no host services.'
            exit 0;;
        *) echo "Unknown option: $option" >&2; exit 1;;
    esac
done
if [ "$runtime" = compose ]; then docker compose version >/dev/null; fi
docker info >/dev/null
for command in curl tar sha256sum awk; do command -v "$command" >/dev/null; done
[ ! -e mfconnect ] || { echo 'mfconnect directory already exists; use its Compose configuration.' >&2; exit 1; }
version=0.7.2
base="https://github.com/chashaochang/MFConnect/releases/download/v${version}"
name="mfconnect-${version}-deploy.tar.gz"
if [ "$offline" = 1 ]; then name="mfconnect-${version}-docker-amd64.tar.gz"; fi
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
fetch() { curl -fL --proto '=https' --proto-redir '=https' --retry 3 --connect-timeout 15 --max-time 900 "$1" -o "$2"; }
fetch "$base/$name" "$work/$name"
fetch "$base/SHA256SUMS" "$work/SHA256SUMS"
awk -v name="$name" '$2 == name {print; count++} END {if (count != 1) exit 1}' "$work/SHA256SUMS" > "$work/selected.sha256"
(cd "$work" && sha256sum -c selected.sha256)
tar -tzf "$work/$name" | while IFS= read -r path; do
    case "$path" in mfconnect/*) ;; *) exit 1;; esac
    case "/$path/" in */../*|*/./*|*//* ) exit 1;; esac
done
tar -tvzf "$work/$name" | awk 'substr($0,1,1) != "-" {exit 1}'
tar -xzf "$work/$name" --no-same-owner
cd mfconnect
sha256sum -c CONTENTS.sha256
gateway="ghcr.io/chashaochang/mfconnect-gateway:$version"
if [ "$offline" = 1 ]; then docker load --input images.tar.gz; else docker pull "$gateway"; fi
docker run --rm -it --network none --user "$(id -u):$(id -g)" \
    -v "$PWD:/output:z" "$gateway" configure
if grep -qx 'MF_TLS_MODE=provided' .env; then
    printf '已有证书完整链的文件路径：'
    IFS= read -r certificate
    printf '对应私钥的文件路径：'
    IFS= read -r private_key
    [ -r "$certificate" ] && [ -r "$private_key" ] || { echo '文件不可读；配置已保留，请将证书放入 mfconnect/config 后启动。' >&2; exit 1; }
    cp -- "$certificate" config/fullchain.pem
    cp -- "$private_key" config/privkey.pem
    chmod 600 config/privkey.pem
fi
if [ "$runtime" = docker ]; then
    sh docker-run.sh up
    sh docker-run.sh status
else
    docker compose up -d --wait --wait-timeout 660
    docker compose ps
fi
echo '部署完成。配置与数据已保留；以后在 mfconnect 目录管理服务。'
