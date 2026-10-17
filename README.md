# MF Connect

MF Connect 为 Unraid 提供自部署的 HTTPS 中继和 Docker WebUI 访问，供 RaidMate 客户端接入。

项目包含云端服务、Unraid 插件和部署工具。手机使用普通 HTTPS，无需安装额外的隧道客户端或启用 VPN。

## 功能

- 一个 HTTPS 入口，同时提供浏览器入口与同源 `/api`。
- 用户自定义连接 ID，通过独立 NAS 子域名访问 Unraid 原生登录页。
- 自动发现 Docker WebUI，保持容器与服务的稳定关联。
- 每个应用使用独立子域名，通过短期启动票据建立浏览器会话。
- App 可检查和执行云端、插件更新；更新保留设备身份和配置。
- 保留旧双域名部署的兼容接口，支持逐步迁移。

例如入口为 `https://example.com`：

| 用途 | 地址 |
| --- | --- |
| 控制 API | `https://example.com/api` |
| 连接 ID 入口 | `https://example.com/home` |
| NAS 原生登录 | `https://home.example.com` |
| Docker WebUI | `https://app-<随机标识>.example.com` |

也支持 `https://connect.example.com` 这样的子域入口。上述地址仅为示例。

## 安装

云端需要 Linux amd64、Docker Compose、Nginx、一个可管理 DNS 的域名，以及覆盖入口和其通配子域的 HTTPS 证书。Unraid 插件需要 Unraid 7 或更新版本。

1. 按 [部署说明](docs/deployment.md) 配置域名、证书并启动云端服务。
2. 发布自己构建的插件清单和载荷，在 Unraid 的 **Plugins → Install Plugin** 中输入清单 URL。
3. 在插件设置或支持本协议的 RaidMate 中填写自己的服务入口，配置连接 ID 和需要发布的应用。

插件设置位于 **Settings → MF Connect**。支持自动安装的客户端可以从云端取得一次性安装链接，无需邀请码或云账号。

未配置 MF Connect 时，RaidMate 的普通 LAN/DDNS 登录仍可独立使用。

## 开发与接入

- [API 与客户端接入](docs/api.md)：端点契约、鉴权、应用目录、安装及更新接口。
- [部署说明](docs/deployment.md)：构建、DNS/TLS、启动、插件安装和旧入口迁移。
- [开发说明](docs/development.md)：源码结构、构建、测试和发布范围。

默认使用 FRP 作为云端与 NAS 之间的隧道。云端控制 Token 不进入应用 WebView；应用会话绑定各自来源，应用自身的登录仍然有效。
