# API 与客户端接入

本文适用于 0.7.0。`endpoint_contract=2` 是地址协议版本，与软件版本分别管理。所有域名、标识和地址均为示例。

## 地址模型

| 用途 | 主域入口 | 子域入口 |
|---|---|---|
| 用户唯一输入 `entry_origin` | `https://example.com` | `https://connect.example.com` |
| `api_origin`（纯 origin） | `https://example.com` | `https://connect.example.com` |
| `api_base_url`（含 API 前缀，不含尾斜线） | `https://example.com/api` | `https://connect.example.com/api` |
| 用户命名连接 ID 的浏览器入口 | `https://example.com/home` | `https://connect.example.com/home` |
| NAS 原生访问来源 | `https://home.example.com` | `https://home.connect.example.com` |
| 自动应用来源 | `https://app-<32位hex>.example.com` | `https://app-<32位hex>.connect.example.com` |

没有固定 `connect` 前缀，没有固定域名层级，没有 `<应用>.<ID>.D`。入口使用标准 HTTPS 443；统一部署配置只接受 HTTPS DNS origin，不接受端口、路径、账号、query 或 fragment。

连接 ID 仍由用户在 App 输入，1–32 位，保留原有保留字（包括 `api`）。新增应用 HostKey 为 `app-` + 32 位随机 hex，36 位，不与用户 ID 命名空间重叠。HostKey 在服务创建时持久保存，并在事务中检查唯一。已有 HostKey、service_id、Docker application_id、target_key 和发布状态保留。Docker application_id 与 HostKey 是两个字段，客户端不得相互推导。多个 NAS 上同名容器拥有各自的服务和应用来源。

迁移预检及配置连接 ID 时检查整个实例的主机归属：NAS、应用、API、浏览器、隧道及兼容域名。冲突时拒绝配置，不静默改动已有地址。已撤销 NAS 的 ID 继续保留，避免旧地址归属易主。

## 能力探测与信任来源

新安装：对用户输入的入口 E 请求 `GET E/.well-known/mfconnect`，验证 HTTPS、禁止重定向。响应 `endpoint_contract=2`，提供完整 API base。新入口对应的 `api_origin` 必须等于用户输入 origin，`api_base_url` 必须等于 E + `/api`。公共发现只能说明服务器能力，不代表已获得某台 NAS 的管理权限。

已有插件：优先通过用户**已经登录的 NAS**读取 `GET /plugins/mfconnect/Connect.php?endpoint_contract=2`。此接口仍要求 Unraid root 原生会话。插件用保存的 controller 配置、可信 CA 和设备签名请求云端 `action=endpoints`，校验返回的 active API base 与本地配置一致，以及浏览器地址与声明的域名、ID 一致后，才返回 `endpoint_verified=true`。配置私钥、隧道令牌和云端管理员凭据不返回 App。

示例（新统一安装，Docker/service 列表仅示意）：

```json
{
  "schema_version": 2,
  "endpoint_contract": 2,
  "configuration_source": "plugin",
  "endpoint_verified": true,
  "entry_origin": "https://example.com",
  "api_origin": "https://example.com",
  "api_base_url": "https://example.com/api",
  "canonical_api_base_url": "https://example.com/api",
  "browser_domain": "example.com",
  "apps_domain": "example.com",
  "device_id": "dev_example",
  "csrf_token": "仅用于该NAS原生接口",
  "capabilities": {
    "unified_entry": true,
    "api_base_url": 1,
    "docker_discovery": 1,
    "self_install": 1,
    "legacy_api": false,
    "app_host_layout": "flat",
    "host_bound_sessions": true
  },
  "connect": {
    "id": "home", "enabled": true, "online": true,
    "entry_url": "https://example.com/home",
    "origin": "https://home.example.com"
  },
  "services": [
    {"id":"svc_example","key":"docker_example","name":"WebUI","origin":"https://app-0123456789abcdef0123456789abcdef.example.com","published":true}
  ],
  "docker": {"schema_version":1,"device_id":"dev_example","state":"ready","online":true,"fresh":true,"scanned_at":0,"applications":[]}
}
```

在旧 API 上，`GET /v1/endpoints` 或 `GET /.well-known/mfconnect` 也可探测地址能力。契约 2 客户端向旧 API 请求时必须发送 `X-MF-Endpoint-Contract: 2`，才能获得规范的新 NAS/应用地址。统一 `/api` 本身隐含选择规范地址；未知契约值返回 400。若新入口恰好复用了旧 API 主机，服务端须显式配置 `legacy_api_root=true` 才保留该主机的旧 `/v1/...` 与 `/healthz`；旧请求仍返回旧地址，新的 `/api` 返回规范地址。缺少此标记时预检拒绝可能破坏旧入口的迁移配置。

**迁移中的 active 与 canonical 不一定相同。** 例如插件仍配置 `server=https://api.old.example.net`：

```json
{
  "entry_origin": "https://example.com",
  "api_origin": "https://api.old.example.net",
  "api_base_url": "https://api.old.example.net",
  "canonical_api_base_url": "https://example.com/api",
  "browser_domain": "example.com",
  "apps_domain": "example.com"
}
```

App 继续使用 `api_base_url`，同时发送契约 2 Header。`canonical_api_base_url` 是迁移信息，不是自动发送 Bearer 的授权。插件不会自动改写保存的 server，不通过 301/302/307/308 搬运设备签名、Bearer 或 refresh token。没有完成单独的可信配置迁移前，旧 API 必须保留。上述地址已足够支持新浏览器/应用来源，插件本身不必立即迁移。

## 客户端适配

1. `apiOrigin` 保持纯 origin，新建独立 `apiBaseUrl`。不能把 `/api` 塞进旧字段，也不能继续强制 API 与浏览器域分离。
2. 契约 1 继续按原分域规则校验。契约 2 按已确认的 active base、入口、单层应用域校验；允许 API origin 与浏览器入口相同，但 API 与 NAS/app **访问来源**仍不同。
3. 新增相对 API 路径拼接函数：base 无尾斜线，追加 `/v1/...`。不要使用会丢弃 `/api` 的 URL origin-relative resolve（例如把 `/v1/...` 直接传给 `new URL(path, base)`）。公共响应的任意外域不能自行成为可信 controller。
4. 应用使用目录返回的 `service_id` 和 `origin`。launch 响应提供完整 `bootstrap_url`，必须匹配已确认的应用来源；不可通过容器名、ID、端口自行拼主机名。
5. 旧插件不认识 `endpoint_contract=2` 时可能忽略参数并返回 schema 1；检查返回契约，再进入旧流程。新版插件连接旧云端、不支持端点描述时返回 409 `endpoint_contract_2_unavailable`，可请求本地契约 1 继续兼容流程；不要把鉴权失败当成可随意切外域的理由。不要将“没有配置 MF Connect”作为普通 LAN/DDNS 登录失败的条件。
6. 新插件面对纯统一配置，未指定契约或指定 1 的本地 Connect GET/session POST 返回 409 `endpoint_contract_2_required`。面对保存的旧配置，未指定契约仍返回 schema 1，原 App 流程保留。未知本地契约返回 400。

## 会话、安装与路径清单

本地获取 session：向 `/plugins/mfconnect/Connect.php` POST form：`action=session`、`endpoint_contract=2`、原生 `csrf_token`、`mf_csrf`。复用 NAS 登录 Cookie，不将 NAS Cookie 发到云 API。返回的 session 包含同一端点描述及 access/refresh 字段，令牌仍只允许当前 NAS 范围；云端更新权限未扩大。

下表路径均相对于 **api_base_url**。新入口加 `/api`，旧 API 仍原地处理裸路径，不重定向。

| 操作 | 相对 API 路径 |
|---|---|
| 健康、端点探测 | `GET /healthz`、`GET /v1/endpoints` |
| 安装策略、自助申请 | `GET /v1/installation-policy`、`POST /v1/installations/claim` |
| 管理员安装链接（旧工具保留） | `POST /v1/installations` |
| 一次性安装清单 | `GET/HEAD /v1/install/<token>/mfconnect.plg` |
| 设备登记、同步、转发鉴权 | `POST /v1/devices/enroll`、`POST /v1/device/sync`、`POST /v1/device/access` |
| 插件管理 action | `POST /v1/device/connect`，新增 `endpoints`，原 `status/session/configure/overview/docker_apps/services/publish/portal_access` 保留 |
| ID 解析、刷新 session | `GET /v1/connect/resolve?id=<ID>`、`POST /v1/connect/refresh` |
| 设备、连接设置 | `GET /v1/devices`、`GET/PUT /v1/devices/<id>/connect`、`DELETE /v1/devices/<id>` |
| Docker 目录 | `GET /v1/devices/<id>/docker-apps`，原 container_id/application_id 过滤不变 |
| 服务、发布、启动 | `GET /v1/services`、`PATCH /v1/services/<id>`、`POST /v1/services/<id>/launches` |
| 云端更新 | `GET/POST /v1/updates/server` |
| 插件更新 | `GET/POST /v1/devices/<id>/updates` |
| 更新任务 | `GET /v1/update-jobs/<id>` |
| 管理员 enrollment（旧工具保留） | `POST /v1/enrollments` |

`claim_path` 保留原 `/v1/installations/claim`，明确相对于 API base；策略新增完整 `claim_url`、`api_base_url`。`install_url` 使用请求所在的 active base。自助安装不需要邀请、云账号或管理员令牌，幂等/有效期/限额与原协议相同。

统一安装清单调用 `mfconnectd provision --entry E`；旧入口生成的清单仍调用 `--server OLD_ORIGIN`。新 CLI 保留 `--server` 语义，新增 `--entry` 自动保存：

```json
{"server":"https://example.com","entry_origin":"https://example.com","api_base_url":"https://example.com/api"}
```

既有配置优先，provision 遇到不同 origin 或不同 API base 拒绝覆盖。新安装只需要用户提供入口，不另填 API 域名或浏览器域名。

**旧 0.6.0 包不包含契约 2 实现。** 0.7.0 插件清单添加 `<!-- mfconnect-endpoint-contract:2 -->`。新统一入口要求该标识，否则策略 `configured=false`、申请/取清单返回 503 `installer_contract_2_required`，避免把不支持 `--entry` 的旧安装包发给用户。发布必须同时更新程序和清单，不能只手工添加标识。打包脚本会先调用程序的 `capabilities` 子命令确认契约 2，拒绝拿旧二进制配新清单。

## 认证与真实路由约束

- 控制 API 的 Bearer 只发 active API base 的已定义控制路径。应用 WebView 只通过短期、单次、绑定应用主机的启动票据换 host-only `__Host-mf_session`；控制 Bearer 不传 WebView。代理额外剥离本服务器识别出的控制 access/refresh Bearer，同时保留应用自身 Authorization 的使用。
- bootstrap ticket 和新 session 都绑定实际 Host。旧无 Host session 只在旧应用域名有效，新旧来源不会共享 Cookie。应用 Set-Cookie 去掉 Domain；NAS 原生会话保持来源隔离。
- 新统一模式校验 TLS SNI 与 HTTP Host 一致；Nginx 上游必须 `proxy_ssl_name $host`，并保持证书验证。反代不去除 `/api`：服务端在验证原始签名 URI 的前提下分派路径。
- `/api` 是保留路由，优先于连接 ID。拒绝含编码路径分隔、重复斜线、点路径等有歧义的 API 请求。只认实际登记的单层 NAS/app 主机，不以宽松 suffix 接受外域或多层域名。
- 应用域名上的 `/api` 是应用自己的路径，永远不会进入控制 API。API 不接受来自 NAS/app 来源的跨源请求，不依赖控制 Cookie 鉴权。
- WebSocket Upgrade 双向转发；相对 Location 保留；指向原应用内部主机的绝对 Location 改写到实际访问的应用主机。
- Docker 发现及自动关联对所有有有效 WebUI 声明的容器通用；未声明 WebUI 显示原因，仍支持手动添加其他内网应用。Jellyfin 没有专用路由。

## Docker WebUI 目录

插件每 30 秒读取 Docker 容器清单和 Unraid 的用户模板，使用 WebUI 标签或模板声明解析地址。不会猜测没有 WebUI 声明的端口，不上传容器环境变量或挂载。

`GET /v1/devices/{device_id}/docker-apps` 使用当前 NAS 的设备会话。响应示例：

```json
{
  "schema_version": 1,
  "device_id": "dev_example",
  "state": "ready",
  "online": true,
  "fresh": true,
  "scanned_at": 1790000000,
  "applications": [{
    "application_id": "app_example",
    "container_id": "完整的64位Docker容器ID",
    "name": "webapp",
    "identity_kind": "unraid_template",
    "state": "ready",
    "running": true,
    "target_key": "docker_example",
    "service_id": "svc_example",
    "origin": "https://app-0123456789abcdef0123456789abcdef.example.com",
    "entry_path": "/web/",
    "published": false,
    "available": true,
    "relay_enabled": false
  }]
}
```

`container_id` 用于匹配当前 Docker 列表，`application_id` 是持久身份，不能通过名称或端口猜测 `service_id`。可用 `container_id`（完整 ID 或至少 12 位前缀）或 `application_id` 查询，二者不能同时指定；前缀存在歧义时返回 409。

目录状态为 `ready/unavailable/disabled/unsupported`。应用状态为 `ready/stopped/no_webui/invalid_webui/network_unresolved/identity_conflict`；无有效 WebUI 时 `origin` 和 `service_id` 可能为空。设备心跳超过 45 秒或扫描超过 90 秒，`fresh=false`、`available=false`。`available` 不代表 HTTP 健康检查通过。

新发现服务默认不发布。用户通过 `PATCH /v1/services/{service_id}`、`{"published":true}` 开启。`relay_enabled` 还要求设备在线、发现可用和远程开关允许；通道尚未建立时仍可能返回 503。

关联保存在插件私有 `docker-discovery.json`。容器重建后按 Compose 身份、Unraid 模板、容器名恢复关联；只有名字可用且同时改名重建时无法保证原身份。停止、删除或发现失败保留服务身份和发布选择，但拒绝访问并使旧会话失效。

## 应用启动

1. 客户端生成 32–128 字符随机 nonce，保留原值。
2. 向 `POST /v1/services/{service_id}/launches` 发送 `{"path":"/web/","nonce_sha256":"<nonce的SHA-256小写hex>"}`。路径使用目录返回的 `entry_path`，可以带 query，不能是外域 URL 或 `/_mf/` 保留路径。
3. 校验返回的 `origin` 和完整 `bootstrap_url` 属于已确认的服务来源。票据有效 30 秒；不要把票据放进 URL。
4. WebView 以表单 POST `ticket=<票据>&nonce=<原nonce>` 到返回的 bootstrap URL，不携带控制 Bearer。

bootstrap 原子消费票据，设置 host-only `__Host-mf_session`，返回 303 到同源路径。票据重放、缺失或过期会话返回 401，跨源请求返回 403，上游不可达返回 503。Cookie 默认有效一小时，带 Secure、HttpOnly、SameSite=Lax、Path=/，不带 Domain。

bootstrap 要求 POST，不接受 query；请求 Origin 和浏览器 Fetch Metadata 必须通过来源校验。手机平台需要实际验证原生 WebView 的 POST 导航行为，不能靠放宽来源校验适配。

## 自助安装

先检查 `GET /v1/installation-policy`，再以 `{"request_id":"<随机32至64位标识>"}` 调用 `POST /v1/installations/claim`。该接口不需要控制 Token，也不接受用户自选 owner。

首次返回 201，相同请求重试返回 200，包含 `install_url`、`expires_at`、`replayed`。链接有效 30 分钟，GET/HEAD 可以重试，设备成功登记后才消费；之后返回 404。客户端通过已登录 NAS 的原生安装流程提交该链接，完成后读取插件契约并获取设备会话。

安装只能登记新的独立 NAS，不授予其他 NAS 或云端的管理权限。已绑定 NAS 重复安装保留原身份。客户端不记录安装链接或 request_id，不复用已结束的请求。

常见错误：403 `self_install_disabled`，409 `installation_consumed/device_limit_reached`，410 `installation_expired`，429 `installation_rate_limited`，503 `installers_not_configured/installer_contract_2_required`。过期或已消费前先核对 NAS 当前安装状态，再决定是否申请新链接。

## 更新

`GET /v1/updates/server` 或 `GET /v1/devices/{device_id}/updates` 返回 `schema_version`、`component`、`current_version`、`latest`、`update_available`、`can_update`、`executor_online`、`check_error`、`checked_at`、`latest_job`。

`latest` 提供 `release_id`、`version`、`notes`、`size` 和 `requires_restart`，也可能为 null。`check_error` 非空不能显示为“已是最新版”。只有有权限、执行器在线、存在更新且无冲突任务时才允许提交。

在同一路径 POST `{"release_id":"<检查接口返回的ID>","request_id":"<随机8至64位标识>"}`。只接受发行 ID，不接受客户端提供的 URL 或命令。首次返回 202；相同请求幂等重试返回 200 和原任务。它们均不代表安装成功。

使用 `GET /v1/update-jobs/{id}` 查询任务。状态依次包括 `queued/downloading/installing/verifying`，失败回退时为 `rolling_back`；终态为 `succeeded/rolled_back/failed`。保留 job ID 和 request_id，应对更新期间短暂断线；超时不能直接判断更新失败。

任务返回 `id`、`component`、可选 `device_id`、`release_id`、`version`、`previous_version`、`state`、`error`、`created_at`、`updated_at`、`expires_at`。建议每 3–5 秒轮询；重新进入页面通过 `latest_job` 恢复进度。

云端更新权限仅属于管理员或部署时指定的身份；NAS 会话默认只允许更新当前插件。401 表示中继会话失效，不应清空 NAS 登录凭据。409 `update_in_progress` 应恢复现有任务，409 `release_changed` 应重新检查发行信息。未配置发行目录或旧端点不支持更新时，应明确显示不可用。

## NAS 地址信息

`GET /v1/devices` 的 `network` 提供接口地址 `interface_ips`、采集时间 `interfaces_updated_at` 和服务端观察到的 `public_ip/public_ip_observed_at`。仅设备所有者或管理员能读到；已撤销设备不返回网络信息。

这些字段只报告地址，不保证手机能直接访问。手机自行决定 LAN、DDNS 或中继连接策略，普通 LAN/DDNS 登录不依赖 MF Connect。
