# NAS 原入口应用通道 v1

状态：协议 v1 已冻结（本轮协作修订 2：按客户端约定传原始 TCP，容器 TLS 端到端）；尚未发布实现，先前 HTTP/1.1 解密草案作废。实现与现场验证进度单独报告。它不是云端 endpoint_contract 1/2，也不改变云中继接口。只有当前 NAS 入口实际返回本协议的 capability 才能启用；不能按插件版本猜测可用。

## 地址和边界

固定基路径 **`/graphql/api/mfconnect/v1`**。客户端只将下列相对路径附加到已登录、已信任 NAS 的原 `scheme://host:port`，保留 HTTP/HTTPS、自定义端口和 IPv6。所有请求禁止自动跟随重定向；HTML 登录页、302、非协议响应均不是认证成功。不得改为其它域名、云入口或将 NAS 凭据交给 WebView。

Unraid API 扩展模块负责在已有 `/graphql` 代理范围注册精确接口，再经本地 Unix socket 访问 daemon。无需外层 WebSocket Upgrade、新公网监听或用户 nginx 配置。只允许精确 `/graphql` 而禁止子路径的上游仍可能阻断本通道；客户端报告 `ENTRY_ROUTE_UNAVAILABLE`，不暗改入口。

所有数据面是有界 JSON HTTP 请求，内容类型 `application/json`；响应 `Cache-Control: no-store`。GET 仅 capability/targets，其余 POST。除 auth 外，私有操作使用 `Authorization: Bearer <access_token>`，不能使用 NAS key 充当通道 token。不接受查询串凭据。不提供浏览器 CORS；本接口仅供 App 原生网络层调用，带 `Origin` 的请求拒绝。

## capability

`GET /graphql/api/mfconnect/v1/capability`，不要求凭据，不返回应用目录、NAS 身份或机密。

```json
{
  "protocol": 1,
  "capability": "existing_origin_webui",
  "transport": "http_push_pull",
  "auth_types": ["unraid_api_key", "unraid_webgui_session"],
  "authorization_policy": "nas_admin_only",
  "wire_protocol": "tcp",
  "limits": {
    "max_request_bytes": 98304,
    "max_chunk_bytes": 65536,
    "buffer_bytes_per_direction": 262144,
    "max_streams_per_session": 8,
    "max_streams_total": 64,
    "max_poll_ms": 20000,
    "stream_idle_seconds": 90,
    "session_ttl_seconds": 1800,
    "auth_recheck_seconds": 5
  }
}
```

`auth_types` 允许实现仅声明已支持子集；客户端不得使用未声明类型。`authorization_policy` v1 只接受 NAS 管理员。能读 Docker 不代表可获得任意应用的管理页面；受限 key 返回权限错误，不自动创建更强的 key，也不要求重复登录来掩盖缺失权限。

插件组件版本为 `0.9.0`，测试发布通过 GitHub prerelease 标记区分，不在组件版本字符串附加 alpha 后缀，以兼容已有云端更新协议的数字版本校验。云服务器仍保持其原版本。

## auth 和续期

`POST /auth`：

```json
{"protocol":1,"auth_type":"unraid_api_key"}
```

`unraid_api_key` 从外层 `X-API-Key` 读取已保存 NAS key；`unraid_webgui_session` 从外层 `Cookie` 和 `X-CSRF-Token` 读取现有 NAS 会话。两种凭据不可混用，不能提供任意身份验证 URL。服务端通过固定本机 Unraid API 的受保护 `me { id roles }` 获取真实主体，只接受有效管理员角色；GraphQL errors、空 me、无权限均失败。禁止用 `__typename` 或 HTTP 200 代替校验。

成功 200：

```json
{
  "protocol":1,
  "nas_id":"nas_<persistent-random-id>",
  "principal_id":"<opaque-principal-id>",
  "access_token":"<opaque-random-secret>",
  "expires_at":1800001800,
  "authorization_policy":"nas_admin_only"
}
```

时间均为 Unix 秒。`nas_id` 为插件在该 NAS 持久化的独立随机身份，非云 device_id；重启不变。token 只在 daemon 内存存活，重启失效。原始 NAS 凭据仅保存在会话内存中供重新核验，不写日志、磁盘、URL或容器。

续期调用同一 `/auth`，使用通道 Bearer，body `{"protocol":1,"auth_type":"session"}`。重新确认原始 NAS 授权后延长 1800 秒，返回相同 token，**不旋转、不重放原始应用请求**。建议剩余 300 秒时续期；过期后重新 auth，旧流不可迁移。auth 失败不触发自动 NAS 登录。

最多 5 秒重新核验授权；撤销、降权、会话失效或持续无法确认身份时关闭所属流并使会话失效。通道自身 close-all 立即撤销。

## targets

`GET /targets`，通道 Bearer。返回自动发现的 Docker；不要求 publish 操作，不包括手工任意 URL。

```json
{
  "protocol":1,
  "nas_id":"nas_<id>",
  "state":"ready",
  "scanned_at":1800000000,
  "targets":[{
    "application_id":"<stable-app-id>",
    "container_id":"<64-hex-docker-id>",
    "name":"Media",
    "state":"ready",
    "target_revision":"<sha256>",
    "target":{
      "upstream_origin":"http://192.0.2.20:8096",
      "entry_path":"/web/",
      "scheme":"http",
      "host":"192.0.2.20",
      "port":8096,
      "wire_protocol":"tcp",
      "tls_mode":"client_end_to_end"
    }
  }]
}
```

`application_id` 跨容器重建尽量保持稳定；`container_id` 用于与当前 Docker 列表匹配。修订号绑定容器身份及目标；目标改变后旧流关闭，客户端重新读目录。`target` 仅在有明确目标时出现，只有 state=ready 可 open。

目录状态 `ready | unavailable`。应用状态沿用发现结果 `ready | stopped | no_webui | invalid_webui | network_unresolved | identity_conflict | unreachable`，本通道还可返回 `target_forbidden`（管理入口/非容器服务）、`tls_untrusted`。未知状态必须按不可打开处理。目录不可用时旧缓存不能作为授权依据。

**target 是 NAS 允许连接的服务端目标。** App 可在独立 Profile/incognito 下使用真实 upstream_origin+entry_path 作为展示地址；这不是新增公网入口，所有目标 TCP 仍经原 NAS 通道。scheme 仅为 http/https；host 不含 IPv6 方括号、port 为已解析的有效数字（含默认80/443），upstream_origin 按 URL 规则为 IPv6 加方括号；entry_path 是含可选 query 的 origin-form 路径。响应不提供客户端可改的任意 dial 参数。

`wire_protocol=tcp`、`tls_mode=client_end_to_end` 表示 daemon 不解析 HTTP、不改 Host/Cookie、不终止容器 TLS。HTTPS CONNECT 后的 TLS ClientHello 与后续密文原样转发；证书验证/容器单独信任由 WebView/App承担，原 NAS 的证书例外绝不复制给容器。目录就绪只证明目标归属与 TCP 可达，不证明容器 TLS 已被手机信任。HTTP absolute-form→origin-form 以及本机 CONNECT 白名单由 App代理处理。

每个 NAS/应用必须用独立浏览器存储环境；相同主机不同端口不能单靠 origin 隔离 Cookie。客户端负责防止管理页面、另一个应用或其它进程进入该应用的本机代理能力。

## open

`POST /open`，通道 Bearer：

```json
{"protocol":1,"request_id":"<16-64 chars [A-Za-z0-9_-]>","application_id":"<id>","target_revision":"<sha256>"}
```

成功 200：

```json
{"protocol":1,"stream_id":"<opaque-id>","application_id":"<id>","target_revision":"<sha256>","send_offset":0,"receive_offset":0}
```

一个 stream 对应到已授权应用目标的一条独立 TCP 连接。可承载 HTTP、端到端 HTTPS、应用 WebSocket 或连接复用；外层始终不需要 Upgrade。open 在成功建立目标 TCP 连接后才返回。服务端仅接受 application_id/revision，不接受 host、端口、URL、DNS覆盖或任意 TCP目的地。并行资源请求分别 open，不共享 offset。

外层 NAS key、Cookie、通道 token 从不写入内层 TCP。App 只能把应用连接字节交给 push，不把 NAS 管理凭据混入应用 HTTP 请求。daemon 不解析或重写密文/HTTP，因此不能替客户端实现 Cookie隔离、Host重写或 TLS验证；也不会因内部重定向自动开另一目标。

相同 session 内相同 request_id 和相同参数重试返回同一 stream；参数冲突返回 `REQUEST_ID_CONFLICT`。每 session 最多记住 256 次 open，直到 session 结束；超过后 `SESSION_REQUEST_LIMIT`，先结束旧会话再 auth。关闭后重试相同 open 返回 `STREAM_CLOSED`，绝不悄悄重建连接重放非幂等请求。

## push

`POST /push`，通道 Bearer：

```json
{"protocol":1,"stream_id":"<id>","offset":0,"data":"R0VUIC8gSFRUUC8xLjENCg==","eof":false}
```

`data` 为标准 RFC4648 base64（带 padding，无空白），解码后最多 65536 字节。offset 是该方向从 0 开始的原始字节计数，不是 base64 长度；所有计数整数在 0..9007199254740991。空 data 仅允许 eof=true。eof=true 为 half-close：已排队字节全部送出后，daemon 对目标 TCP 执行 CloseWrite，仍可持续 pull；它不是全连接 close。最后一块也可同时有 data 和 eof。重复完全相同的最终 push 幂等；其他后续 push 返回 STREAM_CLOSED。

成功 200：`{"protocol":1,"next_offset":16,"eof":false}`。响应表示整块已进入该流的有界输入队列，不保证应用已经执行；不可据此重发业务操作。每方向最多一个正在执行的调用。

超时/断网后仅可用**完全相同 offset/data/eof**重试最新 push；服务端保留最近一块的指纹与回执，保证不重复写入。旧于最近一块、重叠、跳号或同 offset 不同内容均返回 409 `OFFSET_MISMATCH`（含 expected_offset），不能自行跳 offset 继续。应用级执行结果不确定时绝不新建流自动重放 POST。

队列不足时返回 429 `BACKPRESSURE`，整块未接受，`retry_after_ms:100`；按相同 offset 重试。客户端同时维持 pull，不能等整个上传完成才开始读响应。

## pull

`POST /pull`，通道 Bearer：

```json
{"protocol":1,"stream_id":"<id>","offset":0,"max_bytes":65536,"wait_ms":20000}
```

max_bytes=1..65536，wait_ms=0..20000。成功 200：

```json
{"protocol":1,"offset":0,"next_offset":128,"data":"<base64>","eof":false}
```

响应是有界 JSON 长轮询：有数据立即返回，等待到期返回空 data、相同 offset、eof=false。不依赖前置代理支持 chunked 实时 flush。客户端 HTTP 读取期限至少 wait_ms+10000ms；上游超时更小时可降低 wait_ms。

下一次请求 offset=上次 next_offset 表示确认收到该块。服务端在确认前保存最新非空块；丢失响应时重试原 offset 得到同一块。确认后不能回退读取旧 offset。重复 pull 可能返回原块长度，即使新的 max_bytes 更小，因此客户端始终能接收 capability 最大块。并发 pull 返回409 `STREAM_BUSY`；空响应不是 EOF。eof=true 表示目标发送方向 FIN，先消费同响应的数据；这是独立的 receive half-close，目标仍可能接收 push。两方向结束或客户端不再使用时调用 close；异常流终止返回410错误，不把截断伪装为正常 EOF。TLS close_notify 属于客户端/应用协议，TCP FIN 不能代替它。

输出队列满后暂停读取应用，形成背压；不会无界缓存媒体。基础实现每方向 256 KiB，加最近响应的有限重试缓存。流连续90秒没有有效传输/轮询关闭；等待中的合法 pull 计入活动。流过期后不能靠旧 offset 复活。

## close

`POST /close`，通道 Bearer：`{"protocol":1,"stream_id":"<id>"}`。成功 `{"protocol":1,"closed":true}`；同 session 重复关闭已不存在的流也成功，不泄露其他主体的流。取消立即关闭上游并释放等待者。

关闭整个 session：`{"protocol":1,"all":true}`。返回成功后 token 和全部流失效；重试可能401，客户端按已结束处理。App 退出对应访问环境或 NAS 注销时应关闭，不将 token 存到网页。

## 错误与重试

错误 JSON：`{"protocol":1,"error":{"code":"OFFSET_MISMATCH","retryable":false,"expected_offset":65536}}`。仅在适用时含 expected_offset/retry_after_ms；不含原始凭据、内部异常堆栈。客户端允许新增响应字段；未知错误按失败处理。请求未知字段拒绝，未来扩展先探测新协议能力。

| HTTP | code | 处理 |
|---|---|---|
| 400 | INVALID_REQUEST / UNSUPPORTED_PROTOCOL | 修正请求，不重试 |
| 401 | AUTHENTICATION_REQUIRED / SESSION_EXPIRED | 使用仍有效的已保存 NAS 授权重新 auth；不重放旧流 |
| 403 | PERMISSION_DENIED / ORIGIN_FORBIDDEN / TARGET_FORBIDDEN | 不提权，不跨域绕过 |
| 404 | NOT_FOUND / APPLICATION_NOT_FOUND | 刷新目录 |
| 409 | TARGET_CHANGED / REQUEST_ID_CONFLICT / OFFSET_MISMATCH / STREAM_BUSY | 前三种结束当前操作；busy可在原调用结束后重试 |
| 410 | STREAM_CLOSED / STREAM_EXPIRED / UPSTREAM_IO_ERROR | 关闭本地连接；不自动重放业务请求 |
| 413 | REQUEST_TOO_LARGE | 按块长度限制发送 |
| 429 | BACKPRESSURE / STREAM_LIMIT / SESSION_LIMIT / SESSION_REQUEST_LIMIT / AUTH_RATE_LIMIT | 有界退避；不得无限创建会话绕过配额 |
| 502 | TARGET_UNREACHABLE / TLS_UNTRUSTED | 目标或TLS失败；不静默降级验证 |
| 503 | AUTH_BACKEND_UNAVAILABLE / DISCOVERY_UNAVAILABLE / DAEMON_UNAVAILABLE | 显示具体状态；有限重试，不声明已连接 |

`retryable=true` 只说明可重试相同协议请求，不允许重放已经发给应用的业务请求。无 JSON 的404/502/HTML由客户端归为入口或协议错误。HTTP 自身的状态码与 JSON错误必须一起检查。

## 验收范围

必须分别报告：daemon/fixture 测试、真实 NAS API身份验证、实际入口注册、真实 Docker TCP/HTTP/Range/端到端TLS/WS、手机 WebView 联调。一个层次通过不能代替另一个层次。普通 NAS LAN/DDNS 管理、既有云中继与更新协议保持独立。
