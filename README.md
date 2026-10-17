# MF Connect

为 Unraid 提供自部署的 HTTPS 中继和 Docker WebUI 访问，供 RaidMate 接入。手机使用普通 HTTPS，无需额外隧道客户端或 VPN。

## 使用已有中继

在 RaidMate 或 Unraid 的 **Settings → MF Connect** 中填写部署者提供的 HTTPS 服务入口，再设置自己的连接 ID。使用者不需要云账号、DNS 凭据或另购服务器。

## Docker 部署中继

以下操作由**中继部署者**在自己的服务器上完成，云厂商不限。准备 Docker Engine、Compose V2、域名，以及覆盖入口 `D` 和 `*.D` 的证书。当前镜像为 Linux amd64。

在空目录复制执行：

```bash
curl -fLO --proto '=https' --proto-redir '=https' --retry 3 \
  https://github.com/chashaochang/MFConnect/releases/download/v0.7.2/mfconnect-0.7.2-docker-amd64.tar.gz
curl -fLO --proto '=https' --proto-redir '=https' --retry 3 \
  https://github.com/chashaochang/MFConnect/releases/download/v0.7.2/SHA256SUMS
sha256sum -c SHA256SUMS && tar -xzf mfconnect-0.7.2-docker-amd64.tar.gz
cd mfconnect
docker load --input images.tar.gz
cp .env.example .env
chmod 600 .env
```

编辑 `.env`，将 `MF_ENTRY_ORIGIN` 改成自己的入口，例如 `https://example.com` 或 `https://connect.example.com`。将证书完整链和私钥分别放入 `config/fullchain.pem`、`config/privkey.pem`。入口及其通配子域解析到服务器，放行 TCP 80、443、17000，然后启动：

```bash
docker compose up -d
docker compose ps
docker compose logs --tail=50 gateway
```

**网关、证书管理和云端程序全部运行在容器里。** 无需在宿主机安装 Nginx、Python、Go 或续期任务。预编译镜像已随部署包提供。

没有现成证书时，可选择 [自动申请和续期](docs/deployment.md#自动申请和续期可选)。目前支持 Cloudflare、阿里云 DNS、腾讯云 DNSPod；按域名实际使用的 DNS 服务商配置，不要求使用腾讯云。凭据只由部署者保存在自己的服务器上。

也可使用可选的 [配置向导](docs/deployment.md#可选配置向导) 生成配置。它只下载镜像和配置 Compose，不安装宿主机服务。

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

- [部署与维护](docs/deployment.md)：DNS、证书、排错、备份和旧部署迁移。
- [API 与客户端接入](docs/api.md)：端点契约、鉴权、应用目录、安装及更新接口。
- [开发说明](docs/development.md)：构建、测试和发布范围。
