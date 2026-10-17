# MF Connect

为 Unraid 提供自部署的 HTTPS 中继和 Docker WebUI 访问，供 RaidMate 接入。手机使用普通 HTTPS，无需额外隧道客户端或 VPN。

## 使用已有中继

在 RaidMate 或 Unraid 的 **Settings → MF Connect** 中填写部署者提供的 HTTPS 服务入口，再设置自己的连接 ID。普通使用者不需要云账号、DNS 凭据或另购服务器。

## 自己部署中继

支持**普通 Docker** 和 **Docker Compose**，任选一种。云端、网关、证书管理都运行在容器内，镜像从 GitHub 容器仓库 GHCR 拉取；宿主机不需要安装 Nginx、Python 或 Go。

开始前准备：

- 一台安装了 Docker Engine 的 Linux amd64 服务器，云厂商不限；选择 Compose 时还需 Compose V2。
- 自己的域名：入口可以是 `example.com` 或 `connect.example.com`，将入口和 `*.入口` 解析到服务器。
- 放行 TCP **80、443、17000**。
- 覆盖入口和 `*.入口` 的已有证书，或者域名 DNS 服务商的授权，用于自动申请证书。

先在服务器准备部署目录和配置，将下方入口改为自己的域名：

```bash
mkdir -p mfconnect/config
cd mfconnect
cat > .env <<'ENV'
MF_ENTRY_ORIGIN=https://connect.example.com
MF_TLS_MODE=provided
ENV
chmod 600 .env
```

将证书完整链放入 `config/fullchain.pem`，私钥放入 `config/privkey.pem`，执行 `chmod 600 config/privkey.pem`。没有已有证书时，可以先配置[自动申请证书](docs/deployment.md#自动申请和续期可选)。然后选择以下一种方式。

### 方式一：普通 Docker

直接使用 `docker run`，**不需要 Compose 或安装脚本**。两个容器分别负责 HTTPS 网关和中继服务，共享证书和插件下载目录。

首次安装创建专用网络和数据卷：

```bash
docker network create --subnet 172.31.253.0/28 --label io.mfconnect.docker-project=mfconnect-docker mfconnect-docker-network
docker volume create --label io.mfconnect.docker-project=mfconnect-docker mfconnect-docker-gateway
docker volume create --label io.mfconnect.docker-project=mfconnect-docker mfconnect-docker-tls
docker volume create --label io.mfconnect.docker-project=mfconnect-docker mfconnect-docker-updates
docker volume create --label io.mfconnect.docker-project=mfconnect-docker mfconnect-docker-data
```

启动网关：

```bash
docker run -d --name mfconnect-docker-gateway \
  --label io.mfconnect.docker-project=mfconnect-docker \
  --platform linux/amd64 --init --restart unless-stopped --stop-timeout 120 \
  --read-only --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
  --cap-add SETUID --cap-add SETGID --cap-add NET_BIND_SERVICE --security-opt no-new-privileges:true \
  --tmpfs /tmp:rw,noexec,nosuid,size=64m,mode=1777 --tmpfs /run:rw,noexec,nosuid,size=8m,mode=0755 \
  --network mfconnect-docker-network --ip 172.31.253.2 --network-alias gateway \
  -p 80:80 -p 443:443 --env-file .env \
  -v mfconnect-docker-gateway:/state -v mfconnect-docker-tls:/state/certs \
  -v mfconnect-docker-updates:/updates -v "$PWD/config:/config:ro,z" \
  --health-cmd 'python3 /opt/mfconnect/gateway.py health' \
  --health-interval 10s --health-timeout 5s --health-retries 60 \
  --log-driver json-file --log-opt max-size=10m --log-opt max-file=3 \
  ghcr.io/chashaochang/mfconnect-gateway:0.8.0
```

执行 `docker ps`，等待网关显示 **healthy** 后启动中继服务。如果未就绪，用 `docker logs --tail=100 mfconnect-docker-gateway` 检查证书或 DNS 配置。

```bash
docker run -d --name mfconnect-docker-server \
  --label io.mfconnect.docker-project=mfconnect-docker \
  --platform linux/amd64 --init --restart unless-stopped --stop-timeout 60 \
  --user 10001:10001 --read-only --cap-drop ALL --security-opt no-new-privileges:true \
  --tmpfs /tmp:rw,noexec,nosuid,size=16m,mode=1777 \
  --network mfconnect-docker-network --network-alias mfconnect \
  -p 17000:17000 --env-file .env \
  -e 'MF_TRUSTED_PROXY_IPS=["172.31.253.2"]' -e MF_WATCH_TLS=1 \
  -e MF_UPDATE_DIR=/var/lib/mfconnect-updates \
  -e MF_PLUGIN_MANIFEST=/var/lib/mfconnect-updates/plugin-install.plg \
  -e MF_INSTALL_MODE=self_service -e MF_INSTALL_DEVICE_LIMIT=10 \
  -v mfconnect-docker-data:/var/lib/mfconnect -v mfconnect-docker-tls:/etc/mfconnect/tls:ro \
  -v mfconnect-docker-updates:/var/lib/mfconnect-updates \
  --log-driver json-file --log-opt max-size=10m --log-opt max-file=3 \
  ghcr.io/chashaochang/mfconnect-server:0.8.0-docker.1 bootstrap

docker ps
```

### 方式二：Docker Compose

在准备好 `.env` 和 `config/` 的部署目录下载完整配置，然后启动：

```bash
curl -fL --proto '=https' --proto-redir '=https' --retry 3 \
  https://raw.githubusercontent.com/chashaochang/MFConnect/v0.8.0/compose.yaml \
  -o compose.yaml
docker compose up -d --wait --wait-timeout 660
docker compose ps
```

完整配置见 [compose.yaml](compose.yaml)，可直接查看或复制。首次启动自动拉取两个镜像，Compose 会等待网关就绪后启动服务端。查看日志使用 `docker compose logs --tail=100`。

### 可选：交互配置向导

如果希望自动填写配置、创建资源并启动容器，可在空目录运行下面的向导。它只调用 Docker，不安装宿主机服务：

```bash
curl -fL --proto '=https' --proto-redir '=https' --retry 3 \
  https://raw.githubusercontent.com/chashaochang/MFConnect/v0.8.0/install.sh \
  -o mfconnect-install.sh && sh mfconnect-install.sh --docker
```

选择 Compose 时，将最后的 `--docker` 改为 `--compose`。向导询问入口、已有证书的绝对路径或 DNS 授权；凭据只保存在自己的服务器上。自动签证支持 Cloudflare、阿里云 DNS 和腾讯云 DNSPod，按域名使用的 DNS 服务商选择，与服务器在哪里买无关。

镜像仓库无法访问时可用[离线镜像包](docs/deployment.md#离线镜像包)。更新、备份及两种方式的管理命令见[部署与维护](docs/deployment.md)。

## 安装 Unraid 插件

1. 在 Unraid 7+ 打开 **Plugins → Install Plugin**。
2. 输入 `https://你的入口/downloads/mfconnect/mfconnect.plg`，点击安装。
3. 打开 **Settings → MF Connect**，或在支持本协议的 RaidMate 中配置同一个入口和连接 ID。

支持自动安装的客户端也可取得一次性安装链接，无需邀请码或云账号。开启 MF Connect 后，所有已声明 WebUI 且目标有效的 Docker 应用会自动接入，新增或重建应用也会自动关联，无需逐个发布。关闭 MF Connect 后远程访问关闭；中继认证与应用本身的登录仍然有效。

## 地址与能力

| 用途 | 入口为 `https://example.com` 时 |
| --- | --- |
| 控制 API | `https://example.com/api` |
| 连接 ID 入口 | `https://example.com/home` |
| NAS 原生登录 | `https://home.example.com` |
| Docker WebUI | `https://app-<自动生成的稳定标识>.example.com` |

入口也可以是子域名。每个 NAS 和应用保持独立来源，控制 Token 不进入 WebView。保留旧双域名部署的兼容接口；配置签名发行频道后可通过 App 检查并执行更新。普通 LAN/DDNS 登录可独立使用。

- [部署与维护](docs/deployment.md)：手动部署、DNS、证书、更新、备份和迁移。
- [API 与客户端接入](docs/api.md)：端点契约、鉴权、应用目录、安装及更新接口。
- [开发说明](docs/development.md)：构建、测试和发布范围。
