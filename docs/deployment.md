# 部署

文中的 `example.com`、`203.0.113.10` 均为示例，部署时使用自己的配置。实际配置、凭据和备份存放在服务器私有目录。

## 一键安装（新服务器）

### 准备

| 需要的东西 | 要求 |
| --- | --- |
| 云服务器 | Linux amd64，systemd；Ubuntu 22.04/24.04、Debian 12/13、OpenCloudOS 9 或 RHEL 9 系列 |
| 域名 | DNS 在腾讯云 DNSPod 托管，可使用主域名或任意层级的子域名 |
| 云安全组 | 放行入站 TCP 80、443、17000；服务器能访问 GitHub、软件源、DNSPod 和 Let's Encrypt |
| DNS 凭据 | 腾讯云 API SecretId / SecretKey，具备目标域名的 DNS 记录查询、创建和删除权限 |

DNS 凭据可在腾讯云控制台的 **访问管理 → 访问密钥 → API 密钥管理** 获取。使用专用子用户并授权目标域名的 DNSPod 权限即可；不需要服务器管理权限。证书签发和续期需要添加、删除 `_acme-challenge` TXT 记录，不能只授予只读权限。这里使用腾讯云 API 密钥，不是旧版 DNSPod ID/Token。

### 复制执行

通过 SSH 登录服务器，在 root 终端执行：

```bash
curl -fL --proto '=https' --proto-redir '=https' --retry 3 \
  https://raw.githubusercontent.com/chashaochang/MFConnect/v0.7.1/install.sh \
  -o mfconnect-install.sh && bash mfconnect-install.sh
```

如果当前是普通用户，最后的 `bash mfconnect-install.sh` 改为 `sudo bash mfconnect-install.sh`。如果提示没有 curl，Ubuntu/Debian 执行 `apt-get update && apt-get install -y curl ca-certificates`，RHEL/OpenCloudOS 执行 `dnf install -y curl ca-certificates`，然后重新执行上面的命令。

依次填写入口域名、托管域名、公网 IPv4、SecretId 和 SecretKey。例如入口用 `connect.example.com`，托管域名填 DNSPod 列表中的 `example.com`。凭据隐藏输入，不显示在命令参数里；不需要安装 TCCLI。

安装脚本会：

1. 下载 GitHub Release 的部署包，验证 SHA-256；无需 Go 或本机编译。
2. 安装缺少的 Docker、Compose V2 和 Nginx；已有 Docker 不会被替换。
3. 创建入口和通配子域的 DNS A 记录，申请两者共同使用的 HTTPS 证书。
4. 加载镜像、初始化私有数据卷、启动服务、接入 Nginx，并验证 HTTPS 健康检查。
5. 发布插件下载文件、设置每日续期检查，显示插件安装链接。

下载包包含云端 0.7.0、插件 0.7.1、FRP 0.71.1，以及固定版本的 acme.sh 源码。部署包版本为 0.7.1，各组件不要求版本号相同。acme.sh 的源码和许可证保留在包内 `third_party/acme.sh/`。

### 完成安装

将脚本输出的 `.plg` 链接粘贴到 **Unraid → Plugins → Install Plugin**。例如入口为 `https://example.com`，链接就是：

```text
https://example.com/downloads/mfconnect/mfconnect.plg
```

打开 **Settings → MF Connect** 或 RaidMate 的 MF Connect 设置，填写同一个 HTTPS 入口，再设置连接 ID。无需单独填写 API 域名、购买每应用证书或创建云端账号。

Docker WebUI 默认发现但不自动发布。独立 IP 网络中的应用必须允许 Unraid 主机访问；插件不会替用户更改 Docker 网络设置。

### 中途失败或已有站点

排查失败原因后，在保存安装脚本的目录执行：

```bash
bash mfconnect-install.sh --resume
```

恢复安装只识别该安装器创建的状态和相同发行包，不重新初始化数据。已经完成的安装只显示地址；**这个命令不是版本升级命令**。

| 提示或现象 | 处理 |
| --- | --- |
| DNS 记录冲突 | 检查入口和通配记录是否仍指向旧服务；脚本不会擅自覆盖 A / AAAA / CNAME / NS |
| 证书申请失败 | 检查 DNSPod 权限、托管 zone 和网络；修正后 `--resume` |
| 已有 Nginx 站点冲突 | 选择未占用的域名范围，或按下方高级说明手工合入路由 |
| 已有 Docker 缺少 Compose V2 | 补装 Compose V2；安装器不替换已有 Docker |
| 80/443 被其他服务占用 | 自动接入支持系统 Nginx 和宝塔 Nginx；其他反向代理需手工接入 |
| 本机健康但外网打不开 | 检查云安全组 TCP 443/17000、DNS 是否已生效；云安全组不会由脚本修改 |
| `/srv/mfconnect` 或数据卷已经存在 | 不删除数据，不用新安装覆盖；按维护或迁移流程操作 |

启用 firewalld / UFW 的主机会添加所需端口规则。SELinux 保持开启，脚本设置 Nginx 代理所需网络权限及公开文件/证书的文件标签。已有 Nginx 配置先校验再加载，失败时还原站点文件。

### 查看状态与维护

```bash
cd /srv/mfconnect/current
docker compose ps
docker compose logs --tail=100 mfconnect
systemctl status mfconnect-tls.timer
```

程序与 Compose 文件位于 `/srv/mfconnect/current`，私有输入及 DNS 凭据位于 `/srv/mfconnect/secrets`（仅 root 可读）。公开下载只包含 `/srv/mfconnect/public/downloads/mfconnect` 内的插件文件。数据卷是 `mfconnect_data`；保留绑定和连接 ID 时不要删除它。

安装器自动配置证书续期；**App 内自动更新频道需要另外配置**，见下方“更新、续期和备份”。没有发行频道时，不能把“未配置更新”当作“已是最新版本”。

## 高级部署：自行构建、其他 DNS 或已有代理

以下适用于需要自己管理镜像、DNS 或 Nginx 的部署。使用上面的安装器时无需重复执行。

### 构建

需要 Go 1.26、Python 3、Docker Compose，以及 Nginx、OpenSSL。当前产物目标为 Linux amd64。

```bash
make build
make frp
cp .env.example .env
```

编辑 `.env`，设置 `MF_ENTRY_ORIGIN=https://example.com`。入口也可以是子域名；新部署不需要填写 `MF_API_HOST`、`MF_BASE_DOMAIN`、`MF_CONNECT_HOST`、`MF_TUNNEL_HOST`。默认隧道端口为 TCP 17000，HTTPS 后端只绑定 `127.0.0.1:18443`。

### DNS、证书和入口路由

| 入口 D | DNS A 记录 | 必须覆盖的证书名称 |
| --- | --- | --- |
| `example.com` | `@`、`*` 指向云服务器 | `example.com`、`*.example.com` |
| `connect.example.com` | `connect`、`*.connect` 指向云服务器 | `connect.example.com`、`*.connect.example.com` |

通配符证书不覆盖入口本身或多层子域。应用和 NAS 都使用 D 下的一层子域，不需要每个应用单独申请证书。

`deploy/unified/manage.py` 管理 DNS、证书和 Nginx 配置。手工调用它时，需要事先安装 acme.sh（含 `dns_tencent`）；一键安装器已提供固定版本。目前自动 DNS 管理仅支持腾讯云 DNSPod，其他服务商需自行添加上述 DNS 记录并提供匹配证书。

将 `deploy/unified/config.example.json` 复制到服务器的 `/etc/mfconnect/unified.json`，填写入口、DNS zone、公网 IPv4、部署目录、系统 CA bundle 和 acme.sh 路径。`zone` 是实际托管的 DNS zone，不能简单取入口的最后两段。

DNS 凭据由云服务器环境中的 `Tencent_SecretId`、`Tencent_SecretKey` 提供。配套 systemd 单元从 `/etc/mfconnect/dns.env` 读取它们，该文件仅允许 root 读取。

```bash
python3 deploy/unified/manage.py plan --config /etc/mfconnect/unified.json
python3 deploy/unified/manage.py render --config /etc/mfconnect/unified.json --apply
python3 deploy/unified/manage.py dns --config /etc/mfconnect/unified.json --apply
python3 deploy/unified/manage.py certificate --config /etc/mfconnect/unified.json --apply
python3 deploy/unified/manage.py activate --prepare --config /etc/mfconnect/unified.json --apply
```

没有 `--apply` 时只输出计划。DNS 操作保留匹配记录，遇到冲突拒绝更改。证书签发使用 DNS-01；必要时可设置 `dns_sleep=60` 等待 DNS 传播，取值范围 30–600 秒。

`render` 生成候选环境变量和 Nginx 配置，不覆盖现有配置。审核后合入 `.env`，并将候选站点装入 Nginx 的 http context。Nginx 保留原 Host、WebSocket Upgrade 和 `/api` 路径，以 `$host` 作为后端 TLS SNI 并验证证书。

证书目录固定为 Compose 目录下的 `certs`，内部通过 `live` 链接选择成对的证书和私钥。激活前验证证书链、SAN 和私钥匹配。已有普通 PEM 文件须先私密备份，再建立管理工具要求的布局。

需要提供下载时，配置 `downloads_root`，将已审查的发布产物放入其 `downloads/mfconnect/` 子目录。该目录只用于明确发布的文件，不能放入整个工作目录、Git bundle、配置或运维记录。

### 启动云端

准备可由容器 UID/GID 10001 读取的证书和更新目录。默认 `.env` 使用相对更新目录 `./updates`；若更改 `MF_UPDATE_DIRECTORY`，同步创建对应目录。

```bash
sudo install -d -o 10001 -g 10001 -m 0700 updates
docker compose config --quiet
docker compose build
docker compose run --rm mfconnect init
docker compose up -d --no-build
docker compose ps
```

初始化创建私有数据卷和管理凭据，重复初始化拒绝覆盖已有状态。环境配置与持久配置不一致时拒绝启动。云端已管理 frps，不需要再运行独立 frps。

公网放行 TCP 443 和 TCP 17000。容器内的应用转发端口和鉴权端口不应发布到公网。数据卷名称为 `mfconnect_data`，保留设备绑定时不要删除它。

如需采集 NAS 公网地址，Nginx 必须覆盖 `X-MF-Client-IP` 为实际客户端地址，`MF_TRUSTED_PROXY_IPS` 只填写后端实际看到的直接代理 IP。不要照抄其他服务器的 Docker bridge 地址。

### 自行构建和发布插件

以自己的下载目录构建清单和固定版本载荷：

```bash
make plugin RELEASE_BASE=https://example.com/downloads/mfconnect
```

发布 `dist/plugin/<版本>/` 中的 `.plg`、载荷和对应摘要。Unraid 安装清单 URL 示例为 `https://example.com/downloads/mfconnect/mfconnect.plg`。普通清单安装程序；自动登记使用客户端申请的一次性安装链接。

将经过审查的普通清单作为更新目录中的 `plugin-install.plg`，确保容器 UID 10001 可读。客户端请求 `/api/v1/installations/claim` 即可取得一次性 URL；安装后仍须通过 NAS 原生身份获取管理会话。

Docker WebUI 默认发现但不自动发布。独立 IP 网络中的应用必须允许 Unraid 主机访问；插件不会替用户更改 Docker 网络设置。

## 更新、续期和备份

更新发行物由发布方签名，客户端验证内置公钥、HTTPS、SHA-256、组件和版本。`scripts/sign_release.py` 用于签名，私钥只留在发布端。自建发行渠道需要配置自己的签名身份和发行目录，不复用他人的私钥。

云端更新由宿主机执行器完成，业务容器不挂载 Docker socket。`deploy/docker/install-updater.sh` 使用 `/srv/mfconnect` 布局；使用其他布局时应调整单元和执行器配置。`MF_UPDATE_OWNER` 或 `MF_UPDATE_DEVICE` 指定可更新云端的身份，其他 NAS 不因此获得权限。

一键安装已经配置 `mfconnect-tls.timer`。手工部署使用 `deploy/unified/mfconnect-tls.service` 和 `.timer`，按服务器实际路径调整后启用。证书变更会重启 Go/FRP 容器并重新加载 Nginx，现有中继连接会短暂中断；未到续期时间不重启。不要同时运行另一套任务修改同一证书。

备份包含私有配置、证书、插件身份和一致性的数据库副本。停止写入后复制 SQLite，或使用其备份机制。备份、操作日志和实际验证结果均不放入公开下载目录。

## 从旧双域名部署迁移

1. 备份配置、数据和证书，保留旧发布目录。不要重新登记 NAS 或重置连接 ID。
2. 保留旧 `api_host`、`base_domain`、`connect_host`、`tunnel_host` 和对应环境变量，新增 `entry_origin` / `MF_ENTRY_ORIGIN`。如果新入口就是旧 API 主机，显式设置 `legacy_api_root=true`。
3. 在管理工具配置中填写 `legacy_api`、`legacy_apps`、`legacy_browser`、`tunnel_host`。先取得覆盖新旧主机的证书，再启用新路由；后端证书也必须覆盖全部名称。
4. 使用候选配置和状态副本执行 `mfconnect-server check --dir <候选目录>`。它检查主机归属冲突、TLS 和持久配置。状态副本不能指向生产数据库。
5. 验证旧 API、旧应用和旧 NAS 登录，再验证新入口 `/api`、安装、应用启动和插件契约。保留旧 API 原路径，不能用跨域重定向搬运 Bearer。
6. 插件返回保存的 active `api_base_url`，`canonical_api_base_url` 仅作迁移信息。全部客户端和设备完成可信配置迁移前，不撤销旧入口。

新旧应用来源需要各自的启动票据和 Cookie，服务 ID 与发布选择保持不变。契约字段和兼容行为见 [API 文档](api.md)。
