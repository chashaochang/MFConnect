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

下面的命令在服务器的空目录执行。向导会询问入口、证书方式，然后拉取镜像并启动。选择已有证书时输入完整链和私钥的**绝对文件路径**；选择自动申请时输入 DNS 服务商及凭据。凭据仅保存在你自己的服务器上。

### 方式一：普通 Docker

**不需要 Compose。** 复制整个代码块执行：

```bash
curl -fL --proto '=https' --proto-redir '=https' --retry 3 \
  https://raw.githubusercontent.com/chashaochang/MFConnect/v0.7.2/install.sh \
  -o mfconnect-install.sh && sh mfconnect-install.sh --docker
```

之后在生成的 `mfconnect` 目录管理：

```bash
cd mfconnect
sh docker-run.sh status
sh docker-run.sh logs
```

启动脚本实际使用 `docker run`，负责网络、数据卷、容器启动顺序和健康检查。停止服务用 `sh docker-run.sh down`，保留数据；重新启动用 `sh docker-run.sh up`。

### 方式二：Docker Compose

复制整个代码块执行：

```bash
curl -fL --proto '=https' --proto-redir '=https' --retry 3 \
  https://raw.githubusercontent.com/chashaochang/MFConnect/v0.7.2/install.sh \
  -o mfconnect-install.sh && sh mfconnect-install.sh --compose
```

之后使用标准 Compose 命令管理：

```bash
cd mfconnect
docker compose ps
docker compose logs --tail=100
docker compose up -d
```

部署目录内有 `compose.yaml`、`.env` 和 `config/`。也可以[手动下载配置部署](docs/deployment.md#手动部署)，不使用向导。

自动签证支持 Cloudflare、阿里云 DNS 和腾讯云 DNSPod，按**域名的 DNS 服务商**选择，与服务器在哪里买无关；其他服务商可使用已有证书。镜像仓库无法访问时可用[离线镜像包](docs/deployment.md#离线镜像包)。

## 安装 Unraid 插件

1. 在 Unraid 7+ 打开 **Plugins → Install Plugin**。
2. 输入 `https://你的入口/downloads/mfconnect/mfconnect.plg`，点击安装。
3. 打开 **Settings → MF Connect**，或在支持本协议的 RaidMate 中配置同一个入口和连接 ID。

支持自动安装的客户端也可取得一次性安装链接，无需邀请码或云账号。Docker WebUI 会自动发现，由用户选择发布哪些应用；应用本身的登录仍然有效。

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
