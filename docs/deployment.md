# 部署与维护

普通 App / NAS 使用者只需填写已有中继的 HTTPS 入口。本文供自行部署服务器的人使用。域名解析、SSL 证书的申请和续期由部署者使用自己的平台或工具管理；MF Connect 的部署不绑定云厂商或证书服务商。

## 安装方式

首次安装按 [README 的标准 Docker 或 Compose 命令](../README.md#自己部署中继) 操作即可。仓库根目录的 `compose.yaml` 是完整新部署模板。本文使用用户自行准备的 SSL 证书，不需要填写云账号或 DNS API 密钥。

两种方式运行相同的服务：`mfconnect-server` 提供 API 和 NAS 中继连接，`mfconnect-gateway` 读取你提供的 SSL 证书，提供 HTTPS 网关和插件下载。0.8.1 包含服务端及插件 0.8.1、FRP 0.71.1，仅支持 Linux amd64。

### 配置域名

将入口及其一层通配子域解析到服务器公网 IP：

| 服务入口 D | 在 `example.com` 下的 A 记录名 | 证书必须覆盖 |
| --- | --- | --- |
| `example.com` | `@`、`*` | `example.com`、`*.example.com` |
| `connect.example.com` | `connect`、`*.connect` | `connect.example.com`、`*.connect.example.com` |

填写实际 DNS zone 下对应的记录名。通配符证书不覆盖入口本身，也不覆盖两层子域。NAS 与应用均采用 D 下的一层子域，无需逐应用申请证书。

按实际端口映射放行服务器防火墙和云安全组。标准示例使用 TCP 80、443、17000：80 用于跳转 HTTPS，443 用于网页和 API，17000 用于 NAS 的加密中继连接。

## 下载完整配置包

除 README 的直接命令外，也可一次下载 Compose、可选管理脚本和说明。在空目录执行：

```bash
curl -fLO --proto '=https' --proto-redir '=https' --retry 3 \
  https://github.com/chashaochang/MFConnect/releases/download/v0.8.1/mfconnect-0.8.1-deploy.tar.gz
curl -fLO --proto '=https' --proto-redir '=https' --retry 3 \
  https://github.com/chashaochang/MFConnect/releases/download/v0.8.1/SHA256SUMS
awk '$2 == "mfconnect-0.8.1-deploy.tar.gz" {print; n++} END {if (n != 1) exit 1}' SHA256SUMS > selected.sha256
sha256sum -c selected.sha256 && tar -xzf mfconnect-0.8.1-deploy.tar.gz
cd mfconnect
cp .env.example .env
chmod 600 .env
```

编辑 `.env`，将 `MF_ENTRY_ORIGIN` 改为自己的入口，按下节配置你准备好的 SSL 证书，再选择启动命令：

| 运行方式 | 启动命令 | 查看状态 |
| --- | --- | --- |
| 普通 Docker | README 中的 `docker run`，或可选的 `sh docker-run.sh up` | `docker ps` |
| Compose | `docker compose up -d --wait --wait-timeout 660` | `docker compose ps` |

首次启动自动拉取镜像。**同一个实例只选一种运行方式**；两者的数据卷命名不同，不能直接交替运行来迁移数据。

## 配置你自己管理的 SSL 证书

SSL 证书用于让手机、浏览器与中继建立可信的 HTTPS 连接。申请渠道和续期工具由你选择。MF Connect 需要以下对接信息：

- 证书由系统信任的机构签发，在有效期内。
- 同一张证书同时包含服务入口和其通配子域名；具体域名见上表。
- 证书文件包含站点证书及中间证书（完整证书链），另有配套的无密码私钥，均为 PEM 格式。

在 `.env` 中填写自己的入口，并保留读取外部证书的配置：

```dotenv
MF_ENTRY_ORIGIN=https://connect.example.com
MF_TLS_MODE=provided
```

将文件复制到部署目录，或通过你自己的证书管理工具更新到以下位置：

```text
mfconnect/
├── .env
└── config/
    ├── fullchain.pem   # SSL 证书及完整证书链
    └── privkey.pem     # 该证书配套的私钥
```

原始文件的名称可以不同，复制时使用上述目标名称。文件格式必须是 PEM；仅修改扩展名不能把其他格式转换为 PEM。在 `mfconnect` 目录执行 `chmod 600 config/privkey.pem`，限制私钥文件的读写权限。

README 的 Docker 命令和 Compose 配置都会将整个 `config` 目录以只读方式挂载给网关。网关检查证书包含的域名、有效性和私钥是否匹配，再提供 HTTPS 服务。

### 证书续期后怎么更新

用你自己的工具完成续期后，将新的证书和私钥更新到上述两个文件。网关会定期检查文件变化，校验后自动加载；中继连接可能短暂重连。校验失败时继续使用仍有效的旧证书，错误可从网关日志查看。

**证书续期需要你自己的工具或流程负责。** 自动加载文件表示 MF Connect 使用更新后的证书，不会替你延长证书有效期。更新目录中的文件即可，不要替换整个 `config` 挂载目录。

## 接入自己的反向代理

你可以按自己的运维方式管理域名、SSL 证书和前置反向代理。前置代理接入 MF Connect 时，需要满足以下要求：

- 同时转发入口域名和其下一层 NAS／应用子域名，保留原始 `Host`。
- 保留完整请求路径；例如 `/api/v1/...` 不能去掉 `/api` 再转发。
- 启用 WebSocket 转发，并按所用应用设置上传大小和连接超时。
- 转发到 MF Connect 的 HTTPS 网关时，保留对应的 TLS 主机名（SNI）并验证上游证书。
- TCP 17000 是 NAS 中继端口，需要单独可达，不能按普通网页路径代理。

如果前置代理已经占用 80／443，调整 MF Connect 网关的端口映射，避免冲突。普通 Docker 修改 `-p` 参数；Compose 修改 `.env` 中的 `MF_HTTP_BIND`、`MF_HTTPS_BIND`，再将前置代理指向调整后的网关地址。

**当前 0.8.1 的网关、中继服务和 NAS 隧道仍需要读取 TLS 证书。** 使用前置代理时，也要将相应证书提供给 MF Connect。仅在前置代理配置证书、让 MF Connect 后台完全使用 HTTP 的部署方式目前不支持。证书由谁申请、用什么工具续期，不受此限制。

## 日常管理与更新

在 `mfconnect` 部署目录执行：

| 操作 | 普通 Docker | Docker Compose |
| --- | --- | --- |
| 状态 | `docker ps -a --filter name=mfconnect-docker` | `docker compose ps` |
| 网关日志 | `docker logs --tail=100 mfconnect-docker-gateway` | `docker compose logs --tail=100 gateway` |
| 服务端日志 | `docker logs --tail=100 mfconnect-docker-server` | `docker compose logs --tail=100 mfconnect` |
| 停止并保留数据 | `docker stop mfconnect-docker-server mfconnect-docker-gateway` | `docker compose down` |
| 重新启动 | `docker start mfconnect-docker-gateway mfconnect-docker-server` | `docker compose up -d` |

升级前备份，查看新版说明。普通 Docker 先 `docker pull` 两个新版镜像，再停止并删除**两个容器**，用新版镜像标签重新执行 README 的两个 `docker run` 命令；不要删除或重建数据卷。首次创建网络和卷的命令无需重复执行。

Compose 在 `.env` 中新增或修改 `MF_SERVER_IMAGE`、`MF_GATEWAY_IMAGE` 为对应新版镜像，然后执行：

```bash
docker compose pull
docker compose up -d --wait --wait-timeout 660
```

使用固定版本标签，不依赖 `latest`。如果使用完整配置包，普通 Docker 还可以运行 `sh docker-run.sh update`：先在 `.env` 中设置两个新镜像地址，它会拉取并重建容器、保留数据。已有镜像的离线重建使用 `sh docker-run.sh recreate`。这些脚本只是标准 Docker 命令的可选封装。

若新版修改了配置模板，按该版迁移说明更新，保留 `.env`、`config/` 和数据卷。普通 Docker 的端口、网络由 `docker run` 参数指定；调整网关 IP 时，还需同步修改服务端的 `MF_TRUSTED_PROXY_IPS`。Compose 和管理脚本可通过 `.env` 的端口、网段字段配置。

普通 Docker 使用 `mfconnect-docker-*` 容器、网络和数据卷，Compose 默认项目名为 `mfconnect`。两者各有业务数据、网关状态、证书和更新目录四个数据卷。**不要删除数据卷或执行 `docker compose down -v`**。备份配置、密钥和一致性的业务数据库，备份不放入公开下载目录。

App 内自动更新需要部署者预先配置更新频道；普通使用者不需要处理签名密钥或发行目录。

## 离线镜像包

无法访问 GHCR 时，可从 GitHub Release 下载含镜像的包。

完全离线的服务器可在其他设备上下载 `mfconnect-0.8.1-docker-amd64.tar.gz` 和 `SHA256SUMS`，传入服务器后执行：

```bash
awk '$2 == "mfconnect-0.8.1-docker-amd64.tar.gz" {print; n++} END {if (n != 1) exit 1}' SHA256SUMS > selected.sha256
sha256sum -c selected.sha256 && tar -xzf mfconnect-0.8.1-docker-amd64.tar.gz
cd mfconnect
docker load --input images.tar.gz
cp .env.example .env
chmod 600 .env
```

填写入口并放入证书，按 README 的 Docker 或 Compose 命令启动，即可使用已导入的镜像。离线更新导入新版镜像后，普通 Docker 使用 `sh docker-run.sh recreate`，Compose 使用 `docker compose up -d --pull never`，避免尝试联网拉取。

## 常见问题

| 现象 | 检查 |
| --- | --- |
| 镜像拉取失败 | 到 GHCR 的网络，或使用离线镜像包 |
| 缺少或不接受 SSL 证书 | 检查 `config/fullchain.pem` 和 `config/privkey.pem`，确认 PEM 格式、证书链完整、私钥匹配且证书包含入口及其通配子域名 |
| 80 / 443 已占用 | 避免重复入口，或在 `.env` 修改 `MF_HTTP_BIND` / `MF_HTTPS_BIND` 接入自己的前置代理 |
| Docker 网络地址冲突 | 首次部署前同时修改 `MF_NETWORK_SUBNET` 和其中的 `MF_GATEWAY_IP`；已有实例需迁移网络 |
| 容器正常但外网打不开 | 入口与通配解析、主机防火墙、云安全组及端口映射 |
| Existing entry differs | 配置与已有数据不符；恢复原入口或执行明确迁移，不删除数据重新安装 |

安装插件使用 `https://入口/downloads/mfconnect/mfconnect.plg`，设置位于 **Settings → MF Connect**。App 可请求一次性安装链接。开启 MF Connect 后，已声明 WebUI 且目标有效的 Docker 应用自动接入，每 30 秒重新扫描；新增或重建应用自动关联，无需逐个发布。关闭 MF Connect 后远程访问关闭。独立 IP 容器需允许 Unraid 主机访问。

## 已有部署迁移

已有实例先备份，不要把新安装流程直接套在原数据上。保留连接 ID、设备身份及旧入口，在新入口和证书验证通过、客户端迁移完成前继续提供旧 API。

从宿主机 Nginx 迁到网关容器时，需要安排 80/443 端口交接，并明确导入原业务数据和证书。本文的新安装命令不负责搬迁现有站点或 Docker／Compose 数据。接口兼容和客户端迁移字段见 [API 文档](api.md)。

## 可选：NAS 公网网页网关

默认不需要公网直连配置，浏览器和应用使用中继。只有 NAS 确实具有公网可达 HTTPS 入口时，才配置本节；此设置不会为普通 LAN/DDNS 原生登录增加依赖。

NAS 管理员在插件私有 `/boot/config/plugins/mfconnect/config.json` 中加入以下字段，保留原设备身份和其他配置：

```json
{
  "public_gateway": {
    "origin":"https://direct.example.net",
    "listen":"0.0.0.0:18744",
    "certificate":"/boot/config/plugins/mfconnect/public/fullchain.pem",
    "private_key":"/boot/config/plugins/mfconnect/public/privkey.pem"
  }
}
```

这是一份字段片段，不能覆盖整个配置文件。`origin` 必须是完整 HTTPS DNS origin，当前公网端口固定 443，不接受路径或 IP。该域及子域不能与云端 API／应用／NAS 网页域名空间重叠，例如云端为 `connect.example.com`，可用独立的 `direct.example.net` 或 `direct.example.com`。DNS `*.direct.example.net` 指向 NAS 的真实公网地址，路由器将公网 TCP 443 转到 NAS 的 18744；只开放此认证网关，不开放 Docker 内部端口。

网关使用 `*.direct.example.net` 证书：应用使用 `<既有HostKey>.direct.example.net`，NAS 使用 `nas-<设备摘要>.direct.example.net`；均为单层子域，不需要嵌套通配符。`origin` 是域名空间配置，本版本不提供该根域的网页入口。证书及私钥保存在 NAS，申请和续期使用部署者自己的工具；公网 DNS 和路由器端口映射也由部署者管理。更换证书文件后下次 TLS 握手自动读取；无有效证书或监听失败时云端验证失败并保留中继。

运行插件连接程序的 `check` 校验配置，再重新连接插件。在 RaidMate 或插件设置页开启“浏览器访问使用公网直连”。开关默认关闭，公网入口未经 HTTPS 和 NAS 身份验证时不会启用直连。此开关与 MF Connect 总开关都保存在服务端，并由插件签名接口回读；公网监听本身不授予任何应用访问权限。
