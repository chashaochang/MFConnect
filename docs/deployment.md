# 部署

文中的 `example.com`、`203.0.113.10` 均为示例，部署时使用自己的配置。实际配置、凭据和备份存放在服务器私有目录。

## 1. 构建

需要 Go 1.26、Python 3、Docker Compose，以及 Nginx、OpenSSL。当前产物目标为 Linux amd64。

```bash
make build
make frp
cp .env.example .env
```

编辑 `.env`，设置 `MF_ENTRY_ORIGIN=https://example.com`。入口也可以是子域名；新部署不需要填写 `MF_API_HOST`、`MF_BASE_DOMAIN`、`MF_CONNECT_HOST`、`MF_TUNNEL_HOST`。默认隧道端口为 TCP 17000，HTTPS 后端只绑定 `127.0.0.1:18443`。

## 2. DNS、证书和入口路由

| 入口 D | DNS A 记录 | 必须覆盖的证书名称 |
| --- | --- | --- |
| `example.com` | `@`、`*` 指向云服务器 | `example.com`、`*.example.com` |
| `connect.example.com` | `connect`、`*.connect` 指向云服务器 | `connect.example.com`、`*.connect.example.com` |

通配符证书不覆盖入口本身或多层子域。应用和 NAS 都使用 D 下的一层子域，不需要每个应用单独申请证书。

`deploy/unified/manage.py` 管理 DNS、证书和 Nginx 配置。目前自动 DNS 管理支持腾讯云 DNSPod；需要事先安装并审查 acme.sh（含 `dns_tencent`），工具不会自动下载执行安装脚本。

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

## 3. 启动云端

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

## 4. 安装 Unraid 插件

以自己的下载目录构建清单和固定版本载荷：

```bash
make plugin RELEASE_BASE=https://example.com/downloads/mfconnect
```

发布 `dist/plugin/<版本>/` 中的 `.plg`、载荷和对应摘要。Unraid 安装清单 URL 示例为 `https://example.com/downloads/mfconnect/mfconnect.plg`。普通清单安装程序；自动登记使用客户端申请的一次性安装链接。

将经过审查的普通清单作为更新目录中的 `plugin-install.plg`，确保容器 UID 10001 可读。客户端请求 `/api/v1/installations/claim` 即可取得一次性 URL；安装后仍须通过 NAS 原生身份获取管理会话。

Docker WebUI 默认发现但不自动发布。独立 IP 网络中的应用必须允许 Unraid 主机访问；插件不会替用户更改 Docker 网络设置。

## 5. 更新、续期和备份

更新发行物由发布方签名，客户端验证内置公钥、HTTPS、SHA-256、组件和版本。`scripts/sign_release.py` 用于签名，私钥只留在发布端。自建发行渠道需要配置自己的签名身份和发行目录，不复用他人的私钥。

云端更新由宿主机执行器完成，业务容器不挂载 Docker socket。`deploy/docker/install-updater.sh` 使用 `/srv/mfconnect` 布局；使用其他布局时应调整单元和执行器配置。`MF_UPDATE_OWNER` 或 `MF_UPDATE_DEVICE` 指定可更新云端的身份，其他 NAS 不因此获得权限。

续期使用 `deploy/unified/mfconnect-tls.service` 和 `.timer`，按服务器实际路径调整后启用。证书变更会重启 Go/FRP 容器并重新加载 Nginx，现有中继连接会短暂中断；未到续期时间不重启。不要同时运行另一套任务修改同一证书。

备份包含私有配置、证书、插件身份和一致性的数据库副本。停止写入后复制 SQLite，或使用其备份机制。备份、操作日志和实际验证结果均不放入公开下载目录。

## 6. 从旧双域名部署迁移

1. 备份配置、数据和证书，保留旧发布目录。不要重新登记 NAS 或重置连接 ID。
2. 保留旧 `api_host`、`base_domain`、`connect_host`、`tunnel_host` 和对应环境变量，新增 `entry_origin` / `MF_ENTRY_ORIGIN`。如果新入口就是旧 API 主机，显式设置 `legacy_api_root=true`。
3. 在管理工具配置中填写 `legacy_api`、`legacy_apps`、`legacy_browser`、`tunnel_host`。先取得覆盖新旧主机的证书，再启用新路由；后端证书也必须覆盖全部名称。
4. 使用候选配置和状态副本执行 `mfconnect-server check --dir <候选目录>`。它检查主机归属冲突、TLS 和持久配置。状态副本不能指向生产数据库。
5. 验证旧 API、旧应用和旧 NAS 登录，再验证新入口 `/api`、安装、应用启动和插件契约。保留旧 API 原路径，不能用跨域重定向搬运 Bearer。
6. 插件返回保存的 active `api_base_url`，`canonical_api_base_url` 仅作迁移信息。全部客户端和设备完成可信配置迁移前，不撤销旧入口。

新旧应用来源需要各自的启动票据和 Cookie，服务 ID 与发布选择保持不变。契约字段和兼容行为见 [API 文档](api.md)。
