# MF Connect

MF Connect 为 Unraid 提供自部署的 HTTPS 中继和 Docker WebUI 访问，供 RaidMate 客户端接入。

项目包含云端服务、Unraid 插件和部署工具。手机使用普通 HTTPS，无需安装额外的隧道客户端或启用 VPN。

## 一键部署云端

准备一台 **Linux amd64** 云服务器和一个托管在**腾讯云 DNSPod** 的域名。支持 Ubuntu 22.04/24.04、Debian 12/13、OpenCloudOS 9 / RHEL 9 系列；需要 systemd。云安全组放行 **TCP 80、443、17000**。

在服务器的 **root 终端**复制执行：

```bash
curl -fL --proto '=https' --proto-redir '=https' --retry 3 \
  https://raw.githubusercontent.com/chashaochang/MFConnect/v0.7.1/install.sh \
  -o mfconnect-install.sh && bash mfconnect-install.sh
```

随后按提示输入：

1. 入口域名，例如 `example.com` 或 `connect.example.com`。
2. DNSPod 中托管的域名，例如 `example.com`。
3. 服务器的公网 IPv4。
4. 腾讯云 API 的 SecretId、SecretKey（隐藏输入）。

脚本下载并校验预编译发行包，安装 Docker / Compose / Nginx，配置 DNS，申请入口及通配符证书，启动云端并配置续期。**不需要自己编译、制作镜像或逐个申请应用证书。** DNS 凭据只保存在这台服务器的私有目录。已有 MF Connect 部署不会被覆盖。

安装成功后会显示服务入口和插件安装链接。如果失败，解决屏幕提示的问题后执行 `bash mfconnect-install.sh --resume` 继续。系统要求、DNS 凭据、已有站点和故障处理见 [部署说明](docs/deployment.md)。

## 安装 Unraid 插件

1. 在 Unraid 7+ 打开 **Plugins → Install Plugin**。
2. 粘贴云端安装脚本最后显示的 `.plg` 链接，点击安装。
3. 打开 **Settings → MF Connect**，或在支持本协议的 RaidMate 中设置 MF Connect，填写上面同一个 HTTPS 服务入口，再设置自己好记的连接 ID。

支持自动安装的客户端也可以取得一次性安装链接，无需邀请码或云账号。Docker WebUI 会自动发现，由用户选择发布哪些应用；应用本身的登录仍然有效。

## 能做什么

- 一个 HTTPS 入口，同时提供浏览器入口与同源 `/api`。
- 用户自定义连接 ID，通过独立 NAS 子域名访问 Unraid 原生登录页。
- 自动发现 Docker WebUI，保持容器与服务的稳定关联。
- 每个应用使用独立子域名，通过短期启动票据建立浏览器会话。
- 配置签名发行频道后，App 可检查和执行云端、插件更新，保留设备身份和配置。
- 保留旧双域名部署的兼容接口，支持逐步迁移。

例如入口为 `https://example.com`：

| 用途 | 地址 |
| --- | --- |
| 控制 API | `https://example.com/api` |
| 连接 ID 入口 | `https://example.com/home` |
| NAS 原生登录 | `https://home.example.com` |
| Docker WebUI | `https://app-<随机标识>.example.com` |

也支持 `https://connect.example.com` 这样的子域入口。上述地址仅为示例。

未配置 MF Connect 时，RaidMate 的普通 LAN/DDNS 登录仍可独立使用。

## 开发与接入

- [API 与客户端接入](docs/api.md)：端点契约、鉴权、应用目录、安装及更新接口。
- [部署说明](docs/deployment.md)：一键安装、排错、维护和旧入口迁移。
- [开发说明](docs/development.md)：源码结构、构建、测试和发布范围。

默认使用 FRP 作为云端与 NAS 之间的隧道。云端控制 Token 不进入应用 WebView；应用会话绑定各自来源，应用自身的登录仍然有效。
