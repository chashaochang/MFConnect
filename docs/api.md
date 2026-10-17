# API 与客户端接入

本文适用于 **服务端 0.8.1、插件 0.8.0 及以上**。本文是客户端最终契约，设置能力版本为 1。`endpoint_contract=2` 是地址协议版本，与软件版本分别管理。所有域名、标识和地址均为示例。

插件 0.9.0 的无云、原 NAS 入口应用通道使用独立的 [TCP 通道 v1 契约](existing-origin-v1.md)。须以实际 capability 探测启用；不改变本文云中继会话和地址协议。

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

连接 ID 仍由用户在 App 输入，1–32 位，保留原有保留字（包括 `api`）。新增应用 HostKey 为 `app-` + 32 位随机 hex，36 位，不与用户 ID 命名空间重叠。HostKey 在服务创建时持久保存，并在事务中检查唯一。已有 HostKey、service_id、Docker application_id、target_key 保留；0.8.0 起 Docker 发布状态迁移为跟随设备总开关，手动应用的原有选择保留。Docker application_id 与 HostKey 是两个字段，客户端不得相互推导。多个 NAS 上同名容器拥有各自的服务和应用来源。

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
    "docker_auto_publish": 1,
    "browser_public_direct": 1,
    "launch_navigation": 1,
    "updates": 1,
    "webdav": false,
    "self_install": 1,
    "legacy_api": false,
    "app_host_layout": "flat",
    "host_bound_sessions": true
  },
  "connect": {
    "id": "home", "enabled": true, "online": true,
    "browser_public_direct": false,
    "web_access": {"schema_version":1,"mode":"relay_only","public_ingress_configured":false,"status":"disabled"},
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

### session 与 refresh 的版本选择

0.8.1 修复旧 session／refresh 被无条件标记为契约 2 的兼容问题。插件 0.8.0 可直接配合修复后的服务端，无需修改 NAS 配置。

| 请求 | 返回契约 |
|---|---|
| 插件保存旧 API 配置，GET Connect.php 未指定契约、POST `action=session` 未指定契约 | `schema_version:1`；session 保持旧 API 与 NAS 地址 |
| 旧 API `/v1/connect/refresh`，不传 Header 或传 `X-MF-Endpoint-Contract: 1` | `schema_version:1`，响应 Header 为 1 |
| 旧 API 的 session／refresh，传 `X-MF-Endpoint-Contract: 2` | 完整契约 2，含 `capabilities.launch_navigation:1` |
| 统一 `/api/v1/...` 的 session／refresh | 隐含选择契约 2，不降级为 1 |

契约 1 的 session／refresh 仅包含 `schema_version`、`api_origin`、`device_id`、`access_token`、`expires_at`、`refresh_token`、`refresh_expires_at`、`connect`；不添加 `endpoint_contract` 或契约 2 的地址描述。缺失 `endpoint_contract` 按 1 处理。旧 ID resolve 也保持契约 1。纯统一插件配置仍拒绝本地契约 1，返回 409 `endpoint_contract_2_required`。

公共 `/.well-known/mfconnect`、`/v1/endpoints` 及设备 `action=endpoints` 仍提供契约 2 的能力描述；发现能力不等于升级既有会话。客户端在旧 API 上选定契约 2 后，每次请求（包括刷新）都须继续发送 Header 2。未知契约在轮换 refresh token 前拒绝。

刷新仍会原子轮换 access／refresh token，旧 refresh 不可重放。若旧版本已经完成轮换，但客户端因响应协议错误丢弃新令牌，服务端修复不会恢复已消费的旧令牌；应通过同一已登录 NAS 的插件重新取得 MF 会话，不清除 NAS 登录或改变可信 API 范围。

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
    "published": true,
    "available": true,
    "relay_enabled": true
  }]
}
```

`container_id` 用于匹配当前 Docker 列表，`application_id` 是持久身份，不能通过名称或端口猜测 `service_id`。可用 `container_id`（完整 ID 或至少 12 位前缀）或 `application_id` 查询，二者不能同时指定；前缀存在歧义时返回 409。

目录状态为 `ready/unavailable/disabled/unsupported`。应用状态为 `ready/stopped/no_webui/invalid_webui/network_unresolved/identity_conflict/unreachable`；无有效 WebUI 时 `origin` 和 `service_id` 可能为空。设备心跳超过 45 秒或扫描超过 90 秒，`fresh=false`、`available=false`。0.8.0 插件在 NAS 上对声明的完整 WebUI 地址发送 HEAD 探测；证书验证失败、无法连接或 HTTP 5xx 标记 `unreachable`，不会自动开放。401/403/405 仍说明 HTTP 服务存在，具体登录和页面功能由应用负责。旧插件未升级时，`available` 仍只表示地址解析及运行状态。

**0.8.0 起自动发布，无需应用管理或逐应用 publish。** 设备 `connect.enabled=true` 时，服务端在每次设备同步及总开关保存时自动设置 Docker 服务可访问；新发现、重建均适用。目录增加 `publication_mode:"automatic"`。`published` 保留兼容，表示总开关允许；`relay_enabled` 还要求设备在线、发现及目标可用。实际通道尚未建立时仍可能返回 503。

旧 `PATCH /v1/services/{id}` 和设备 `action=publish` 对 Docker 服务返回 409 `docker_automatically_managed`，对手动内网应用保留原行为。客户端探测 `docker_auto_publish:1` 后不显示逐应用发布控制，也不能用循环调用旧 API 实现自动发布。关闭总开关会立即使既有网页会话失效，停止／删除／目标变化会撤销对应应用访问；恢复后使用相同持久服务 ID 和域名。

关联保存在插件私有 `docker-discovery.json`。容器重建后按 Compose 身份、Unraid 模板、容器名恢复关联；只有名字可用且同时改名重建时无法保证原身份。停止、删除或发现失败保留服务身份，但拒绝访问并使旧会话失效。

## 浏览器公网直连设置（能力版本 1）

`GET /v1/devices/{device_id}/connect` 读取；`PUT` 同一路径保存，并返回完整最终设置。请求示例：

```json
{"id":"home","enabled":true,"browser_public_direct":true}
```

`browser_public_direct` 为严格 JSON boolean，默认 **false**。旧请求不传该字段时保留当前值；不会重置用户偏好。它与 `enabled` 分开：总开关关闭时所有 MF 远程访问关闭，偏好可保留。保存成功后应使用返回值，再 GET 回读确认。NAS 本地契约 2 的 `connect`、云端 session、ID resolve 和设备签名 `status/overview/endpoints` 都返回同一设置。插件设置页也可修改。仅当前 NAS 的会话、所有者或管理员可保存；这不影响普通 RaidMate LAN/DDNS 原生登录。

```json
{
  "id":"home", "enabled":true, "online":true,
  "entry_url":"https://example.com/home",
  "origin":"https://home.example.com",
  "browser_public_direct":true,
  "web_access":{
    "schema_version":1,
    "mode":"public_preferred",
    "public_ingress_configured":false,
    "status":"not_configured"
  }
}
```

`web_access.mode` 为 `relay_only/public_preferred`；`status` 为 `disabled/not_configured/verify_on_access`。`public_ingress_configured=true` 仅表示 NAS 声明了完整网关，**不是可达验证通过**。状态不会伪装成实时探测结果。

公网地址来自 NAS 管理员保存的 `public_gateway.origin`，经设备签名同步；不取 `network.public_ip`，不猜 Docker 端口、不采信容器随意提供的外域。该可选网关需要独立 DNS 命名空间、可信 HTTPS 通配符证书和真实公网 443 转发，部署要求见 deployment.md。它仍运行 MF 会话鉴权，不直接暴露裸应用。每次准备切换时，云端解析公网 DNS（拒绝内网／保留地址、防 DNS 重绑定）、校验 HTTPS 证书且不跟随重定向，并挑战已登记 NAS 的 Ed25519 身份。随后浏览器进行无凭据可达探测，超时使用中继。

开启时，浏览器入口和 App WebUI 使用相同选路：正常入口和应用 `origin/bootstrap_url` **仍是云端中继来源**；App launch 必须声明 `navigation_contract:1` 才允许跨 origin 交接，旧客户端保持中继。云端通过检测后由网关导航到 NAS 公网来源。应用使用 30 秒、单次、绑定 NAS/应用/目标 Host/来源 Origin 的表单票据换 host-only Cookie；控制 Bearer、刷新令牌和 NAS 原生 Cookie不跨域转发。关闭公网偏好会撤销已有直连应用会话，活动直连请求每 2 秒复核授权（请求超时上限 3 秒），NAS 网页仍要求 Unraid 原生登录。

客户端不得拼接公网 URL，也不应给任意新域发送 Authorization。原 bootstrap POST 流程不变；WebView 只允许本次受认证 launch 返回的确切 `navigation_origins`，不得用 suffix／通配符放行其他 NAS 或应用。每次 launch 替换旧允许列表。Cookie、Storage 和下载凭据保持来源隔离。直连 NAS 原生来源与中继来源的登录 Cookie 隔离，首次直连可能需要原生登录。无公网网关、云端探测失败或浏览器探测失败时均使用原中继链路；探测成功后网络再中断不保证当前页面无缝恢复，可重新从中继入口打开。浏览器禁用 JavaScript 时，选路页面提供“使用中继”链接。

`/_mf/public-probe`、`/_mf/bootstrap`、`/_mf/relay` 是网页网关内部路径，App 不调用它们替代控制 API。WebDAV 没有独立配置、鉴权或转发能力契约，能力固定 `webdav:false`，本轮不提供开关；普通 HTTP WebUI 代理不能据此宣称支持 WebDAV 客户端。

## 应用启动

1. 客户端生成 32–128 字符随机 nonce，保留原值。
2. 向 `POST /v1/services/{service_id}/launches` 发送 `{"path":"/web/","nonce_sha256":"<nonce的SHA-256小写hex>"}`。路径使用目录返回的 `entry_path`，可以带 query，不能是外域 URL 或 `/_mf/` 保留路径。
3. 校验返回的 `origin` 和完整 `bootstrap_url` 属于已确认的服务来源。票据有效 30 秒；不要把票据放进 URL。
4. WebView 以表单 POST `ticket=<票据>&nonce=<原nonce>` 到返回的 bootstrap URL，不携带控制 Bearer。

支持 `launch_navigation:1` 的服务端接受以下扩展请求：

```json
{"path":"/web/","nonce_sha256":"<SHA-256小写hex>","navigation_contract":1}
```

响应扩展（下列是结构示例，不表示当前实例已配置公网）：

```json
{
  "mode":"relay",
  "origin":"https://app-0123456789abcdef0123456789abcdef.example.com",
  "bootstrap_url":"https://app-0123456789abcdef0123456789abcdef.example.com/_mf/bootstrap",
  "bootstrap_method":"POST",
  "bootstrap_path":"/_mf/bootstrap",
  "ticket":"<一次性票据>",
  "expires_in":30,
  "service_id":"svc_example",
  "navigation_contract":1,
  "navigation_origins":[
    "https://app-0123456789abcdef0123456789abcdef.example.com",
    "https://app-0123456789abcdef0123456789abcdef.direct.example.net"
  ],
  "public_gateway_origin":"https://app-0123456789abcdef0123456789abcdef.direct.example.net",
  "navigation_expires_at":1790000030
}
```

`mode:"relay"` 表示 bootstrap 从中继开始，不能据此判断最终数据路径。只有开关开启且云端在 **launch 时** 完成目标 HTTPS／NAS 身份验证，响应才带第二个 origin。否则 `public_gateway_origin:null`、`navigation_origins:[origin]`，`navigation_expires_at` 仍为票据到期 Unix 秒。这不是公共发现页面提供的外域列表，而是当前应用经过鉴权的单次导航授权。

该允许列表最多两个标准 HTTPS origin；第二个与第一应用共享同一个稳定 HostKey，但属于管理员明确配置的公网命名空间。不同应用使用不同 Host，不共享 Cookie 或 Storage。客户端应验证 `navigation_contract===1`、首项等于既有 `origin`、第二项（若有）严格等于非空 `public_gateway_origin`，拒绝路径、账号、端口、重复项及其他元素。不能从这些地址建立新的 API 信任或向其发送控制 Token。

首次跨域交接须在 `navigation_expires_at` 前开始；服务端把精确公网 origin 和期限绑定在原票据／中继会话中，准备交接时再次验证当前设备、应用、配置、开关和可达性，并原子消费一次。实际交接票据的有效期不得超过该期限。随后网关先做浏览器侧无凭据可达探测，成功才表单 POST 到允许的公网 `/_mf/bootstrap`，失败则在原中继 origin 的 `/_mf/relay?path=...` 回退，短期固定中继防循环。已成功进入公网来源后的同源浏览不受首次交接期限影响，但所有请求继续校验服务端授权。未声明导航契约的旧 launch 不携带跨域授权，也不会在后续应用 GET 中突然切换来源。

bootstrap 原子消费票据，设置 host-only `__Host-mf_session`，返回 303 到同源路径。票据重放、缺失或过期会话返回 401，跨源请求返回 403，上游不可达返回 503。Cookie 默认有效一小时，带 Secure、HttpOnly、SameSite=Lax、Path=/，不带 Domain。

中继 bootstrap 要求 POST，不接受 query；请求 Origin 和浏览器 Fetch Metadata 必须通过来源校验。手机平台需要实际验证原生 WebView 的 POST 导航行为，不能靠放宽来源校验适配。

## 自助安装

先检查 `GET /v1/installation-policy`，再以 `{"request_id":"<随机32至64位标识>"}` 调用 `POST /v1/installations/claim`。该接口不需要控制 Token，也不接受用户自选 owner。

首次返回 201，相同请求重试返回 200，包含 `install_url`、`expires_at`、`replayed`。链接有效 30 分钟，GET/HEAD 可以重试，设备成功登记后才消费；之后返回 404。客户端通过已登录 NAS 的原生安装流程提交该链接，完成后读取插件契约并获取设备会话。

安装只能登记新的独立 NAS，不授予其他 NAS 或云端的管理权限。已绑定 NAS 重复安装保留原身份。客户端不记录安装链接或 request_id，不复用已结束的请求。

常见错误：403 `self_install_disabled`，409 `installation_consumed/device_limit_reached`，410 `installation_expired`，429 `installation_rate_limited`，503 `installers_not_configured/installer_contract_2_required`。过期或已消费前先核对 NAS 当前安装状态，再决定是否申请新链接。

## 更新

`GET /v1/updates/server` 或 `GET /v1/devices/{device_id}/updates` 返回 `schema_version`、`component`、`current_version`、`latest`、`update_available`、`can_update`、`executor_online`、`check_error`、`checked_at`、`latest_job`。

`latest` 提供 `release_id`、`version`、`notes`、`size` 和 `requires_restart`，也可能为 null。`latest_job` 也可能为 null；任务版本可以早于 `current_version`，例如后来通过其他合法渠道更新。`can_update` **仅表示该调用者有更新权限**，不是“现在可点更新”。按钮还须检查 `executor_online && update_available && check_error==""` 且没有冲突任务。`check_error` 非空不能显示为“已是最新版”。只有有权限、执行器在线、存在更新且无冲突任务时才允许提交。

在同一路径 POST `{"release_id":"<检查接口返回的ID>","request_id":"<随机8至64位标识>"}`。只接受发行 ID，不接受客户端提供的 URL 或命令。首次返回 202；相同请求幂等重试返回 200 和原任务。它们均不代表安装成功。

使用 `GET /v1/update-jobs/{id}` 查询任务。状态依次包括 `queued/downloading/installing/verifying`，失败回退时为 `rolling_back`；终态为 `succeeded/rolled_back/failed`。保留 job ID 和 request_id，应对更新期间短暂断线；超时不能直接判断更新失败。

任务返回 `id`、`component`、可选 `device_id`、`release_id`、`version`、`previous_version`、`state`、`error`、`created_at`、`updated_at`、`expires_at`。建议每 3–5 秒轮询；重新进入页面通过 `latest_job` 恢复进度。

云端更新权限仅属于管理员或部署时指定的身份；NAS 会话默认只允许更新当前插件。401 表示中继会话失效，不应清空 NAS 登录凭据。409 `update_in_progress` 应恢复现有任务，409 `release_changed` 应重新检查发行信息。未配置发行目录或旧端点不支持更新时，应明确显示不可用。

## 更新响应字段与线上核对样本

更新协议为 `schema_version:1`，独立于 `endpoint_contract:2`。两组件必须独立加载和重试；HTTP 200 的更新状态不是连接配置保存结果。如下是升级前实际 HTTPS 响应的脱敏样本（2026-10-17，旧 API 与统一 /api 均为 200）；版本和发行信息是采样快照，不能硬编码：

server：

```json
{
  "can_update": true,
  "check_error": "",
  "checked_at": 1792233365,
  "component": "server",
  "current_version": "0.7.0",
  "executor_online": true,
  "latest": {
    "notes": "统一 HTTPS 入口及同源 /api，兼容旧 API 和应用域名，支持主域或子域部署。",
    "release_id": "server-0.7.0",
    "requires_restart": true,
    "size": 82692699,
    "version": "0.7.0"
  },
  "latest_job": {
    "component": "server",
    "created_at": 1792077971,
    "device_id": "",
    "error": "",
    "expires_at": 1792079771,
    "id": "upd_example_server",
    "previous_version": "0.4.0",
    "release_id": "server-0.4.1",
    "state": "succeeded",
    "updated_at": 1792077983,
    "version": "0.4.1"
  },
  "schema_version": 1,
  "update_available": false
}
```

plugin：

```json
{
  "can_update": true,
  "check_error": "",
  "checked_at": 1792233366,
  "component": "plugin",
  "current_version": "0.7.1",
  "device_id": "dev_example",
  "executor_online": true,
  "latest": {
    "notes": "通用配置示例及明确的插件文件清单，保留统一入口和现有设备配置。",
    "release_id": "plugin-0.7.1",
    "requires_restart": true,
    "size": 2145,
    "version": "0.7.1"
  },
  "latest_job": {
    "component": "plugin",
    "created_at": 1792226765,
    "device_id": "dev_example",
    "error": "",
    "expires_at": 1792228565,
    "id": "upd_example_plugin",
    "previous_version": "0.7.0",
    "release_id": "plugin-0.7.1",
    "state": "succeeded",
    "updated_at": 1792226833,
    "version": "0.7.1"
  },
  "schema_version": 1,
  "update_available": false
}
```

| 字段 | 类型与含义 |
|---|---|
| `schema_version` | integer，当前为 1 |
| `component` | `server` 或 `plugin` |
| `device_id` | 插件检查为 string；服务端检查不带此字段；服务端 job 内是空字符串 `""`（客户端可兼容 null 并规范化）；插件 job 必须是实际 NAS 的非空 device_id |
| `current_version` | string；旧插件不报告版本时可为空字符串，应显示未知 |
| `latest` | object 或 null；`size` 为整数 byte，插件为清单大小，服务端为更新归档大小 |
| `update_available` | boolean；仅在版本已知、发行检查成功且最新版本更高时为 true |
| `can_update` | boolean，调用者权限 |
| `executor_online` | boolean，执行器心跳在线，不等于上游 Docker WebUI 可达 |
| `check_error` | string，成功为 `""`；未配置为 `updates_not_configured`，不得把非空值视为已是最新 |
| `checked_at` | integer，Unix 秒；不是 ISO 字符串或毫秒 |
| `latest_job` | object 或 null；不是数组，不保证其版本与当前安装版本相同 |

`latest` 的 `release_id/version/notes` 都是 string，`requires_restart` 是 boolean。job 的 `error` 成功时为空字符串，三个 `*_at` 均为 Unix 秒。HTTP 非 2xx 用 `{"error":"code"}` 错误对象，不按成功 schema 解析。未配置、无权限、执行器离线是分别可表示的正常检查结果；服务端检查不可用不能阻止插件检查。

## NAS 地址信息

`GET /v1/devices` 的 `network` 提供接口地址 `interface_ips`、采集时间 `interfaces_updated_at` 和服务端观察到的 `public_ip/public_ip_observed_at`。仅设备所有者或管理员能读到；已撤销设备不返回网络信息。

这些字段只报告地址，不保证手机能直接访问。手机自行决定 LAN、DDNS 或中继连接策略，普通 LAN/DDNS 登录不依赖 MF Connect。
