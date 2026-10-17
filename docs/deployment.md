# 部署与维护

普通 App / NAS 使用者只需填写已有中继的 HTTPS 入口。本文供自行部署服务器的人使用；不要求腾讯云账号，也不限定服务器供应商。

## 安装方式

首次安装推荐使用 [README 的配置向导](../README.md#自己部署中继)，普通 Docker 和 Docker Compose 都支持。向导只下载配置和镜像，不安装宿主机服务；已存在 `mfconnect` 目录时拒绝覆盖。

两种方式运行相同的服务：`mfconnect-server` 提供 API 和 FRP，`mfconnect-gateway` 提供 HTTPS 网关、证书管理和插件下载。0.7.2 包含服务端及插件 0.7.2、FRP 0.71.1，仅支持 Linux amd64。宿主机不挂载 Docker socket，不运行证书续期任务。

### 配置域名

将入口及其一层通配子域解析到服务器公网 IP：

| 服务入口 D | 在 `example.com` 下的 A 记录名 | 证书必须覆盖 |
| --- | --- | --- |
| `example.com` | `@`、`*` | `example.com`、`*.example.com` |
| `connect.example.com` | `connect`、`*.connect` | `connect.example.com`、`*.connect.example.com` |

填写实际 DNS zone 下对应的记录名。通配符证书不覆盖入口本身，也不覆盖两层子域。NAS 与应用均采用 D 下的一层子域，无需逐应用申请证书。

服务器防火墙和云安全组放行 TCP 80、443、17000；向导不会修改它们。自动签证还需要能访问证书机构和所选 DNS API。

## 手动部署

不使用向导时，在空目录复制执行以下命令下载配置：

```bash
curl -fLO --proto '=https' --proto-redir '=https' --retry 3 \
  https://github.com/chashaochang/MFConnect/releases/download/v0.7.2/mfconnect-0.7.2-deploy.tar.gz
curl -fLO --proto '=https' --proto-redir '=https' --retry 3 \
  https://github.com/chashaochang/MFConnect/releases/download/v0.7.2/SHA256SUMS
awk '$2 == "mfconnect-0.7.2-deploy.tar.gz" {print; n++} END {if (n != 1) exit 1}' SHA256SUMS > selected.sha256
sha256sum -c selected.sha256 && tar -xzf mfconnect-0.7.2-deploy.tar.gz
cd mfconnect
cp .env.example .env
chmod 600 .env
```

编辑 `.env`，将 `MF_ENTRY_ORIGIN` 改为自己的入口，按下面一种方式准备证书，然后选择启动命令：

| 运行方式 | 启动命令 | 查看状态 |
| --- | --- | --- |
| 普通 Docker | `sh docker-run.sh up` | `sh docker-run.sh status` |
| Compose | `docker compose up -d --wait --wait-timeout 660` | `docker compose ps` |

首次启动自动拉取镜像。**同一个实例只选一种运行方式**；两者的数据卷命名不同，不能直接交替运行来迁移数据。

### 使用已有证书（默认）

`.env` 中配置：

```dotenv
MF_ENTRY_ORIGIN=https://connect.example.com
MF_TLS_MODE=provided
```

将完整证书链放入 `config/fullchain.pem`，对应未加密私钥放入 `config/privkey.pem`。证书应由系统信任的机构签发，并同时覆盖入口与通配域名。

```bash
chmod 600 config/privkey.pem
```

此模式不读取任何云服务商凭据。证书的申请和续期可由已有的证书工具完成；替换这两个文件后，网关会校验并自动加载。更换文件内容即可，不要单独替换整个 `config` 挂载目录。

### 自动申请和续期（可选）

在 `.env` 中设置：

```dotenv
MF_TLS_MODE=acme
MF_DNS_PROVIDER=cloudflare
```

按域名实际使用的 DNS 服务商选择，将对应凭据保存在 `config/dns.env`，每行 `字段=值`，不加引号：

| DNS 服务商 | 配置值 | `config/dns.env` 字段 |
| --- | --- | --- |
| Cloudflare | `cloudflare` | `CF_Token`，可选 `CF_Zone_ID` / `CF_Account_ID` |
| 阿里云 DNS | `alidns` | `Ali_Key`、`Ali_Secret` |
| 腾讯云 DNSPod | `dnspod` | `Tencent_SecretId`、`Tencent_SecretKey` |

凭据需有目标域名 DNS 查询及 TXT 记录新增、删除权限，不需要服务器管理权限。Cloudflare 可使用限定目标 zone 的 API Token；腾讯云这里使用云 API 密钥，不是旧版 DNSPod ID/Token。

```bash
chmod 600 config/dns.env
```

入口的 A 记录由部署者预先配置。网关通过 DNS-01 添加、清理验证 TXT 记录，申请入口及通配证书，随后每 12 小时检查续期。凭据只挂载给网关，不传给业务容器、NAS 插件或手机，也不发送给项目维护者。其他 DNS 服务商可使用已有证书模式。

证书和私钥经校验后成对切换，API / FRP 在自身容器内重载，现有中继连接会短暂重连。无证书变化时不重启；校验失败保留原证书。

## 日常管理与更新

在 `mfconnect` 部署目录执行：

| 操作 | 普通 Docker | Docker Compose |
| --- | --- | --- |
| 状态 | `sh docker-run.sh status` | `docker compose ps` |
| 日志 | `sh docker-run.sh logs` | `docker compose logs --tail=100` |
| 停止并保留数据 | `sh docker-run.sh down` | `docker compose down` |
| 重新启动 | `sh docker-run.sh up` | `docker compose up -d` |
| 拉取并应用更新 | `sh docker-run.sh update` | `docker compose pull && docker compose up -d --wait --wait-timeout 660` |

升级前备份；查看新版说明，将 `.env` 的 `MF_SERVER_IMAGE`、`MF_GATEWAY_IMAGE` 改为对应的新版本，再执行更新。使用固定版本标签，不依赖 `latest`；只运行更新命令不会自动跳到其他版本。若新版修改了配置模板或启动脚本，按该版迁移说明一并更新，保留 `.env`、`config/` 和数据卷。

普通 Docker 使用 `mfconnect-docker-*` 容器、网络和数据卷，Compose 默认项目名为 `mfconnect`。两者各有业务数据、网关状态、证书和更新目录四个数据卷。**不要删除数据卷或执行 `docker compose down -v`**。备份配置、密钥和一致性的业务数据库，备份不放入公开下载目录。

App 内自动更新需要部署者预先配置更新频道；普通使用者不需要处理签名密钥或发行目录。

## 离线镜像包

无法访问 GHCR 时，可从 GitHub Release 下载含镜像的包。向导也支持离线镜像模式，将安装命令改成 `sh mfconnect-install.sh --docker --offline` 或 `sh mfconnect-install.sh --compose --offline`；仍需能访问 GitHub 下载文件。

完全离线的服务器可在其他设备上下载 `mfconnect-0.7.2-docker-amd64.tar.gz` 和 `SHA256SUMS`，传入服务器后执行：

```bash
awk '$2 == "mfconnect-0.7.2-docker-amd64.tar.gz" {print; n++} END {if (n != 1) exit 1}' SHA256SUMS > selected.sha256
sha256sum -c selected.sha256 && tar -xzf mfconnect-0.7.2-docker-amd64.tar.gz
cd mfconnect
docker load --input images.tar.gz
cp .env.example .env
chmod 600 .env
```

按手动部署步骤填写入口和放入证书，启动即可使用已导入的镜像。离线更新导入新版镜像后，普通 Docker 使用 `sh docker-run.sh recreate`，Compose 使用 `docker compose up -d --pull never`，避免尝试联网拉取。

## 常见问题

| 现象 | 检查 |
| --- | --- |
| 镜像拉取失败 | 到 GHCR 的网络，或使用离线镜像包 |
| 缺少或不接受证书 | `config` 中文件名、私钥匹配、完整链，以及入口和 `*.入口` 两项覆盖 |
| 自动申请失败 | DNS 服务商、凭据权限、DNS 传播及出站网络；修复后重新启动 |
| 80 / 443 已占用 | 避免重复入口，或在 `.env` 修改 `MF_HTTP_BIND` / `MF_HTTPS_BIND` 接入自己的前置代理 |
| Docker 网络地址冲突 | 首次部署前同时修改 `MF_NETWORK_SUBNET` 和其中的 `MF_GATEWAY_IP`；已有实例需迁移网络 |
| 容器正常但外网打不开 | 入口与通配解析、主机防火墙、云安全组及端口映射 |
| Existing entry differs | 配置与已有数据不符；恢复原入口或执行明确迁移，不删除数据重新安装 |

安装插件使用 `https://入口/downloads/mfconnect/mfconnect.plg`，设置位于 **Settings → MF Connect**。App 可请求一次性安装链接。Docker WebUI 默认自动发现，由用户选择是否发布；独立 IP 容器需允许 Unraid 主机访问。

## 已有部署迁移

已有实例先备份，不要把新安装流程直接套在原数据上。保留连接 ID、设备身份及旧入口，在新入口和证书验证通过、客户端迁移完成前继续提供旧 API。

从宿主机 Nginx 迁到网关容器时，需要安排 80/443 端口交接，并明确导入原业务数据和证书。通用向导不会改动现有站点，也不会自动搬迁 Docker 与 Compose 的数据。接口兼容和客户端迁移字段见 [API 文档](api.md)。
