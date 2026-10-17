#!/bin/bash
set -euo pipefail
root=${MFCONNECT_TEST_ROOT:-}
if [ -z "$root" ] && [ "$(id -u)" != 0 ]; then echo "Run through the Unraid plugin manager as root." >&2; exit 1; fi
payload=$(cd -- "$(dirname -- "$0")" && pwd)
version=$(cat "$payload/VERSION")
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][a-zA-Z0-9]+)*$ ]] || exit 1
base="$root/usr/local/lib/mfconnect"
config="$root/boot/config/plugins/mfconnect"
run="$root/var/run/mfconnect"
mkdir -p "$base/versions" "$config/packages" "$run" "$root/etc/rc.d" "$root/usr/local/emhttp/plugins/mfconnect"
chmod 700 "$config" "$run"
exec 8>"$run/install.lock"
flock -x 8

"$payload/mfconnectd" version >/dev/null
[ "$("$payload/frpc" --version)" = "$(cat "$payload/FRP_VERSION")" ]
if [ -f "$config/config.json" ]; then "$payload/mfconnectd" check --config "$config/config.json" --require-transport frp; fi

destination="$base/versions/$version"
previous=$(readlink -f "$base/current" 2>/dev/null || true)
if [ -d "$destination" ]; then
  if ! diff -qr "$destination" "$payload" >/dev/null; then
    echo "Version already exists with different contents; publish a new version." >&2
    exit 1
  fi
else
  stage=$(mktemp -d "$base/versions/.stage.XXXXXX")
  cp -a "$payload/." "$stage/"
  mv "$stage" "$destination"
fi

activate() {
  local selected=$1
  ln -sfn "$selected" "$base/current.next"
  mv -Tf "$base/current.next" "$base/current"
  install -m 755 "$selected/rc.mfconnect" "$root/etc/rc.d/rc.mfconnect"
  install -m 644 "$selected/MFConnect.page" "$root/usr/local/emhttp/plugins/mfconnect/MFConnect.page"
  # 回退到不带设置页的旧包时，清理新版入口，避免 UI 与程序版本混用。
  local web="$root/usr/local/emhttp/plugins/mfconnect"
  rm -rf "$web/include" "$web/assets"
  rm -f "$web/Settings.php" "$web/Connect.php"
  if [ -f "$selected/Settings.php" ]; then
    cp -a "$selected/include" "$selected/assets" "$web/"
    install -m 644 "$selected/Settings.php" "$web/Settings.php"
    if [ -f "$selected/Connect.php" ]; then install -m 644 "$selected/Connect.php" "$web/Connect.php"; fi
  fi
}

if [ -x "$root/etc/rc.d/rc.mfconnect" ]; then "$root/etc/rc.d/rc.mfconnect" stop; fi
activate "$destination"
healthy=true
"$root/etc/rc.d/rc.mfconnect" start || healthy=false
if [ -f "$config/config.json" ] && [ ! -f "$config/disabled" ] && $healthy; then
  sleep 1
  "$root/etc/rc.d/rc.mfconnect" status >/dev/null || healthy=false
fi
if ! $healthy; then
  "$root/etc/rc.d/rc.mfconnect" stop || true
  if [ -n "$previous" ] && [ -d "$previous" ]; then
    activate "$previous"
    "$root/etc/rc.d/rc.mfconnect" start || true
  fi
  echo "New version did not start; restored the previous version when available." >&2
  exit 1
fi
if [ -n "$previous" ] && [ "$previous" != "$destination" ]; then ln -sfn "$previous" "$base/previous"; fi
# 更新执行器独立于连接程序，安装过程中不会杀死正在执行升级的进程。
if [ -z "$root" ] && [ -x "$base/current/mfconnect-updater" ]; then
  updater_running=false
  if [ -r "$run/updater.pid" ]; then
    read -r updater_pid < "$run/updater.pid"
    if [[ "$updater_pid" =~ ^[0-9]+$ ]] && kill -0 "$updater_pid" 2>/dev/null; then
      updater_exe=$(readlink "/proc/$updater_pid/exe" 2>/dev/null || true)
      if [[ "$updater_exe" == "$base/versions/"*/mfconnect-updater ]]; then updater_running=true; fi
    fi
  fi
  if ! $updater_running; then
    nohup "$base/current/mfconnect-updater" --plugin >"$run/updater.log" 2>&1 8>&- &
    echo "$!" > "$run/updater.pid"
  fi
fi
echo "mfconnect $version installed. Device configuration has been preserved."
cat <<'NOTICE'

MF Connect 网络提示：
  中继转发前，Unraid 主机必须能够访问应用的 IP 和端口。
  Docker 应用使用 br0、ipvlan 或 macvlan 独立 IP 时，请检查：
  设置 → Docker → 高级视图 → 主机访问自定义网络
  （Host access to custom networks），需要设为“启用 / Enabled”。
  手机或电脑能在内网打开应用，不代表 Unraid 主机也能访问它。
  修改 Docker 设置可能需要先停止 Docker，请安排好容器停机时间。
NOTICE
docker_config="$root/boot/config/docker.cfg"
if [ -r "$docker_config" ]; then
  if grep -Eq "^[[:space:]]*DOCKER_ALLOW_ACCESS=['\"]?yes['\"]?[[:space:]]*$" "$docker_config"; then
    echo '  当前主机访问自定义网络：已启用。'
  else
    echo '  当前主机访问自定义网络：未启用，使用独立 IP 的应用需要先处理此项。'
  fi
fi
echo '  安装完成后，在 RaidMate 的此 NAS → MF Connect 中填写连接 ID、开启远程访问。'
echo '  点击“插件”列表里的 MF Connect 图标，或进入“设置 → MF Connect”，可查看状态并管理应用地址。'
