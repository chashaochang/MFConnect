# 部署与维护

普通 App / NAS 使用者只需填写已有中继的 HTTPS 入口。以下服务器操作由中继部署者完成；不要求腾讯云账号，也不限定服务器供应商。

## Docker Compose 快速开始

按 [README 的命令](../README.md#docker-部署中继) 下载部署包并加载镜像，编辑 `.env`，然后启动 `docker compose up -d`。

部署包包含两个预编译镜像：`mfconnect-server:0.7.2-docker.1` 和 `mfconnect-gateway:0.7.2`。网关内提供 Nginx、证书管理和插件 0.7.2；连接程序为 0.7.2，FRP 为 0.71.1。宿主机只运行 Docker / Compose，不安装 Nginx、Python、Go 或 systemd 续期单元，不挂载 Docker socket。

### 配置域名

将入口及其一层通配子域解析到服务器公网 IP：

| 服务入口 D | 在 `example.com` 下的 A 记录名 | 证书必须覆盖 |
| --- | --- | --- |
| `example.com` | `@`、`*` | `example.com`、`*.example.com` |
| `connect.example.com` | `connect`、`*.connect` | `connect.example.com`、`*.connect.example.com` |

填写实际 DNS zone 下对应的记录名。通配符证书不覆盖入口本身，也不覆盖两层子域。NAS 与应用均采用 D 下的一层子域，无需逐应用购买或申请证书。

服务器防火墙和云安全组放行 TCP 80、443、17000；脚本不会修改它们。证书模式为自动申请时，网关还需能访问证书机构和所选 DNS API。

### 使用已有证书（默认）

`.env` 中配置：

```dotenv
MF_ENTRY_ORIGIN=https://connect.example.com
MF_TLS_MODE=provided
```

将完整证书链放入 `config/fullchain.pem`，对应未加密私钥放入 `config/privkey.pem`。证书应由系统信任的机构签发，并同时覆盖入口与通配域名。

```bash
chmod 600 config/privkey.pem
docker compose up -d
```

此模式**不读取任何云服务商凭据**。证书的申请和续期可由你已有的证书工具完成；替换这两个文件后，网关会校验并自动加载。更换文件内容即可，不要单独替换整个 `config` 挂载目录。

### 自动申请和续期（可选）

如果希望由网关申请通配符证书，在 `.env` 中设置：

```dotenv
MF_TLS_MODE=acme
MF_DNS_PROVIDER=cloudflare
```

`MF_DNS_PROVIDER` 选择**域名实际使用的 DNS 服务商**，与服务器在哪里买没有关系。将对应凭据保存在 `config/dns.env`，每行 `字段=值`，不加引号：

| DNS 服务商 | 配置值 | `config/dns.env` 字段 |
| --- | --- | --- |
| Cloudflare | `cloudflare` | `CF_Token`，可选 `CF_Zone_ID` / `CF_Account_ID` |
| 阿里云 DNS | `alidns` | `Ali_Key`、`Ali_Secret` |
| 腾讯云 DNSPod | `dnspod` | `Tencent_SecretId`、`Tencent_SecretKey` |

凭据需有目标域名 DNS 查询及 TXT 记录新增、删除权限；不需要服务器管理权限。Cloudflare 可使用限定目标 zone 的 API Token；腾讯云这里使用云 API 密钥，不是旧版 DNSPod ID/Token。

```bash
chmod 600 config/dns.env
docker compose up -d
```

入口的 A 记录由部署者预先配置。网关通过 DNS-01 自动添加/清理验证 TXT 记录并申请入口及通配证书，随后每 12 小时检查续期。凭据只挂载给网关，不传给云端业务容器、NAS 插件或手机，也不发送给 MF Connect 项目维护者。其他 DNS 服务商可以使用已有证书模式。

网关使用固定版本 acme.sh，完整源码及 GPL 许可证位于网关镜像的 `/opt/acme/`。证书和私钥经校验后成对切换；证书变化时 API / FRP 在自身容器内重载，现有中继连接会短暂重连。无证书变化时不重启。校验失败保留原证书。

### 可选配置向导

在空目录运行：

```bash
curl -fL --proto '=https' --proto-redir '=https' --retry 3 \
  https://raw.githubusercontent.com/chashaochang/MFConnect/v0.7.2/install.sh \
  -o mfconnect-install.sh
sh mfconnect-install.sh
```

向导检查现有 Docker / Compose、下载并校验部署包、加载镜像，然后在临时容器里生成 `.env`。默认选择已有证书；选自动申请时才询问你的 DNS 服务商和对应凭据。不会安装或替换宿主机软件，也不会覆盖已有配置。完成域名解析、证书准备后，在生成的 `mfconnect` 目录执行 `docker compose up -d`。

## 状态、故障与维护

在部署目录执行：

```bash
docker compose ps
docker compose logs --tail=100 gateway mfconnect
docker compose restart
```

| 现象 | 检查 |
| --- | --- |
| 缺少或不接受证书 | `config` 中文件名、私钥匹配、完整链，以及入口和 `*.入口` 两项覆盖 |
| 自动申请失败 | 所选 DNS 服务商、凭据权限、DNS 传播及出站网络；修复后 `docker compose restart gateway` |
| 80 / 443 已占用 | 关闭重复入口，或在 `.env` 修改 `MF_HTTP_BIND` / `MF_HTTPS_BIND` 接入自己的前置代理 |
| Docker 网络地址冲突 | 同时修改 `.env` 的 `MF_NETWORK_SUBNET` 和其中的 `MF_GATEWAY_IP` |
| 容器正常但外网打不开 | 入口与通配解析、主机防火墙、云安全组及端口映射 |
| Existing entry differs | 配置与已有数据不符；恢复原入口或执行明确迁移，不删除数据重新安装 |

Compose 管理业务数据、网关状态、证书和更新目录四个命名卷。配置在部署目录的 `.env` 与 `config/`。**保留绑定和连接 ID 时不要执行 `docker compose down -v`。** 备份配置、密钥和一致性的业务数据库，备份不放入公开下载目录。

安装插件使用 `https://入口/downloads/mfconnect/mfconnect.plg`。插件设置位于 **Settings → MF Connect**；App 可以请求一次性安装链接。Docker WebUI 默认自动发现，由用户选择是否发布；独立 IP 容器需允许 Unraid 主机访问。

首次启动不要求填写软件更新签名频道。App 更新接口继续提供，但“未配置更新频道”不等于“已是最新”。容器部署的发行升级应加载新镜像并更新 `.env` 对应标签后执行 `docker compose up -d`；已有宿主机更新执行器的配置和迁移说明见下方。

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

完整 Compose 部署由网关容器处理证书检查和加载。保留宿主机 Nginx 的手工部署可使用 `deploy/unified/mfconnect-tls.service` 和 `.timer`，按服务器实际路径调整后启用。证书变更会重启 Go/FRP 容器并重新加载 Nginx，现有中继连接会短暂中断；未到续期时间不重启。不要同时运行另一套任务修改同一证书。

备份包含私有配置、证书、插件身份和一致性的数据库副本。停止写入后复制 SQLite，或使用其备份机制。备份、操作日志和实际验证结果均不放入公开下载目录。

## 从旧双域名部署迁移

1. 备份配置、数据和证书，保留旧发布目录。不要重新登记 NAS 或重置连接 ID。
2. 保留旧 `api_host`、`base_domain`、`connect_host`、`tunnel_host` 和对应环境变量，新增 `entry_origin` / `MF_ENTRY_ORIGIN`。如果新入口就是旧 API 主机，显式设置 `legacy_api_root=true`。
3. 在管理工具配置中填写 `legacy_api`、`legacy_apps`、`legacy_browser`、`tunnel_host`。先取得覆盖新旧主机的证书，再启用新路由；后端证书也必须覆盖全部名称。
4. 使用候选配置和状态副本执行 `mfconnect-server check --dir <候选目录>`。它检查主机归属冲突、TLS 和持久配置。状态副本不能指向生产数据库。
5. 验证旧 API、旧应用和旧 NAS 登录，再验证新入口 `/api`、安装、应用启动和插件契约。保留旧 API 原路径，不能用跨域重定向搬运 Bearer。
6. 插件返回保存的 active `api_base_url`，`canonical_api_base_url` 仅作迁移信息。全部客户端和设备完成可信配置迁移前，不撤销旧入口。

新旧应用来源需要各自的启动票据和 Cookie，服务 ID 与发布选择保持不变。契约字段和兼容行为见 [API 文档](api.md)。
