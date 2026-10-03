# Codex Agent 管理 API 契约

版本：2026-09-17。管理 API 以 `/api` 为前缀，现有路径和响应字段保留；新增字段可被旧客户端忽略。账号路径参数优先使用稳定 `id`，同时兼容旧名称。名称碰撞返回明确冲突结果，改名为现有名称返回 `409`；后续操作统一使用 ID。账号名称只是展示字段，不应作为前端行键或关联键。

## 通用规则

- 请求/响应使用 JSON，UTF-8；时间为 Unix 秒，`latency_ms` 为毫秒。
- 自动化客户端统一使用 `Authorization: Bearer <admin_key>`，可调用本文全部管理端点。`POST /api/session` 是登录入口，通过 JSON 的 `admin_key` 换取浏览器 cookie；脚本无需先登录。
- 浏览器会话使用 HttpOnly/SameSite cookie；HTTPS 下设置 Secure。显式 `Authorization` 优先于 cookie；错误、空白或重复的认证头直接拒绝，不退回 cookie。有效 Admin Key 不依赖 Redis cookie 服务，旧 cookie 不会阻止 Bearer 请求。Redis 会话验证失败返回 `503`，绝不放行。
- 除登录外，所有 `/api` 需要认证。响应 `Cache-Control: no-store`。
- 普通网关 API Key 即使启用、拥有全部分组，也没有管理权限；管理端点返回 `401 unauthorized`。`admin_api` 只控制 Admin Key 能否用于模型网关，不关闭管理 API。关闭后仍可用 Admin Key 查看、编辑设置和轮换密钥。
- 显式 Bearer 请求不使用 cookie 来源校验；cookie 写请求与登录请求若携带 `Origin`，必须是单个合法 HTTP(S) origin 且 host/port 与请求一致。来源不匹配返回 `403 origin_mismatch`。不接受 URL 查询参数充当管理员凭据。
- 账号令牌、刷新令牌、代理密码不在任何账号/代理响应中返回；分别使用 `has_refresh_token`、`has_password`。代理密码未传或空字符串表示保持原值，显式 `clear_password:true` 才清除。
- 通用错误：`{"error":{"type":"proxy_error","code":"version_conflict","message":"..."}}`；旧客户端可继续读取 `error.message`。`code` 是兼容新增字段。
- 状态：`400` 参数/格式错误；`401` 未认证/会话过期；`403` cookie 写入或登录来源不匹配；`404` 实体或路由不存在；`405` 方法不支持并带 `Allow`；`409` 重复/名称歧义/版本冲突；`413` 请求过大；`500` 持久化失败；`503` 会话或必要存储不可用。404/405 也使用 JSON 错误结构；认证先于路由检查，未认证的未知路径返回 401。
- 已注册的 `GET` 也支持 `HEAD`；`Allow` 由实际路径匹配结果给出。非 GET 请求体必须是单个 JSON 对象，未知字段、错误类型和拼接多个对象均返回 `400`。无请求体的删除接口不需要发送 `{}`。
- 编辑请求传入读取到的 `version`；匹配后提交并递增版本，不匹配返回 `409`。旧客户端省略版本兼容提交，但新界面必须传入。冲突后重新读取详情，不自动覆盖。
- 导入请求上限 32 MiB；其余请求上限 2 MiB。批量编辑每次最多 100 个账号；账号分页 `limit` 默认 50、最大 500。列表按 `(created_at DESC,id DESC)` 游标继续，不使用大 OFFSET。

## 现有端点与兼容扩展

| 方法与路径 | 请求 | 响应与说明 |
| --- | --- | --- |
| `GET /api/overview` | `from,to,step` | 保留 `range,cards,series,account_top,status,recent_errors`；新增统计口径信息，见后文 |
| `GET /api/diagnostics` | 无 | `version,checked_at,uptime_seconds,storage_mode,status,dependencies,capabilities`；只读依赖探测，见后文 |
| `GET /api/audit-logs` | `limit,cursor,action,entity_id,from,to` | `supported,storage_mode,items,has_more,next_cursor,limit`；审计记录游标分页，见后文 |
| `GET /api/accounts` | 新增 `limit,cursor,search,group_id,proxy_id,status,enabled` | 保留 `accounts` 数组，新增 `has_more,next_cursor,total_kind` |
| `POST /api/accounts` | 旧 `{name,credentials,defaults}` 或 `{accounts:[{name,credentials}],defaults}`；新增 `{raw,replace,dry_run}` | 保留 `imported`；新增 `added,duplicate,replaced,failed,results,dry_run` |
| `PUT /api/accounts/{name}` | 保留 `enabled,concurrency,priority,weight,groups,proxies,quota,window_days,reset_window`；新增 `version,name,notes,tags` | 保留 `ok:true`；新增 `id,version` |
| `DELETE /api/accounts/{name}` | 可选 query `version` | `{ok:true}`；保留历史日志 |
| `POST /api/accounts/refresh` | 旧 `{name}`，新增 `{id}` | `{ok:true,id,version,expires_at}`；同账号刷新串行，不阻塞其他账号 |
| `GET /api/accounts/{name}/requests` | `limit`，新增 `cursor` | 保留 `items`，新增 `has_more,next_cursor,total,total_kind` |
| `GET /api/groups` | 无 | `{groups:[{id,name,description,created_at,version,accounts,keys}]}` |
| `POST /api/groups` | `{name,description}` | 分组对象 |
| `PUT /api/groups/{id}` | `{name,description,version?}` | 分组对象 |
| `DELETE /api/groups/{id}` | 无 | `{ok:true}`；删除分组与解除账号/密钥关联在同一 PG 事务中完成 |
| `GET /api/keys` | 无 | 保留 `keys` 脱敏列表，新增每项 `version` |
| `POST /api/keys` | `{name,groups}` | 保留新建密钥响应，创建时需保存一次性密钥 |
| `PUT /api/keys/{id}` | `{name?,groups?,enabled?,version?}` | 返回密钥元数据与 `key_masked`，不回显完整 `key`；禁用后不能调用网关 |
| `DELETE /api/keys/{id}` | 无 | `{ok:true}` |
| `GET /api/proxies` | 无 | `{proxies:[代理元数据]}`；增加 `has_password,version`，移除明文 `password` |
| `POST /api/proxies` | `{name,type,host,port,username?,password?,enabled?}` | 代理元数据，含 `has_password`，不回显密码 |
| `PUT /api/proxies/{id}` | 上述可编辑字段及 `version?,clear_password?` | 代理元数据，含 `has_password` |
| `DELETE /api/proxies/{id}` | 无 | `{ok:true}`；删除与解除账号关联在同一 PG 事务中完成 |
| `POST /api/proxies/test` | `{id}` 或内联代理字段 | 诊断响应，见后文；被禁用的已存代理也可检测 |
| `GET /api/logs` | 保留 `from,to,account,key_name,status,page,page_size,size`；新增 `cursor,account_id,model` | 保留 `items,total,page,page_size`；新增 `has_more,next_cursor,total_kind` |
| `GET /api/settings` | 无 | 保留 `admin_key` 为空字符串及 `admin_api,version,uptime_seconds`；新增 `admin_key_masked,settings_version`，常规读取不返回长期密钥；`version` 为程序版本，不是实体乐观锁版本 |
| `POST /api/settings` | 设置字段部分更新及 `settings_version?`，或单独 `{regenerate:true,request_id?}`；兼容 `regenerate_key` 别名 | `{ok:true,settings_version}`；轮换另返回 `admin_key,request_id`；轮换混合修改其他设置、普通修改携带非空 `request_id` 均返回 `400` |

系统设置包含 `admin_api`、`websocket_enabled`、`builtin_prompt_enabled`、`builtin_prompt`、`sticky_sessions_enabled`、`sticky_session_ttl_seconds`、`prompt_cache_key_enabled`、`codex_fingerprint_enabled`、`codex_fingerprint_profile`，GET 返回平铺字段，POST 可单独提交任一字段。省略字段保留原值，显式 `false` 生效；必须至少提交一个设置字段。提示词最多 32768 UTF-8 字节，允许明确保存空文本；会话 TTL 范围 60..2592000 秒。指纹 profile 以 GET 返回的 `codex_fingerprint_profiles` 为准；应用头模板不代表 TLS/操作系统指纹。详见 [默认值、传输和缓存行为](GATEWAY-SETTINGS.md)。

## 新端点

| 方法与路径 | 请求 | 响应 |
| --- | --- | --- |
| `POST /api/session` | `{admin_key}` | 登录并设置 cookie；返回 `authenticated,expires_at`，不回显密钥 |
| `GET /api/session` | 无 | `{authenticated:true,expires_at}`；Bearer 认证没有 cookie 到期时间，`expires_at:0` |
| `DELETE /api/session` | 无 | `{ok:true,authenticated:false}` 并清除 cookie；cookie 认证还删除对应 Redis 会话。Bearer 调用不撤销 Admin Key |
| `GET /api/accounts/{name}` | ID 或兼容名称 | `{account:账号详情}`；不返回凭据 |
| `POST /api/accounts/import/preview` | 与导入相同 | 与导入同结构，强制 `dry_run:true`，不写入 |
| `POST /api/accounts/batch` | `{items:[{id,version?,patch:{enabled?,notes?,tags?,groups?,proxies?}}]}` | `{results:[{id,ok,version?,error?,code?}],succeeded,failed}`；每项原子，项间独立，失败不伪报成功 |
| `POST /api/settings/rotate-key` | `{request_id?}` | `{ok:true,admin_key,request_id}`；独立持久化，成功后旧密钥退出普通管理认证；有界幂等重放规则见后文 |

## Admin Key 调用与轮换

每次请求发送当前 Admin Key 即可，账号凭据和普通网关密钥均不能代替它。以下 PowerShell 示例从进程环境获取密钥；`CODEX_ADMIN_BASE_URL` 是网关根地址，不带 `/api` 或 `/v1`。

```powershell
$adminApiBase = $env:CODEX_ADMIN_BASE_URL.TrimEnd('/')
$adminApiHeaders = @{ Authorization = "Bearer $env:CODEX_ADMIN_KEY" }
$adminApiSettings = Invoke-RestMethod -Uri "$adminApiBase/api/settings" -Headers $adminApiHeaders
$adminApiBody = @{
  settings_version = $adminApiSettings.settings_version
  websocket_enabled = $true
} | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri "$adminApiBase/api/settings" -Headers $adminApiHeaders -ContentType 'application/json' -Body $adminApiBody
```

轮换前生成并保存唯一 `request_id`，建议 UUID；长度为 1..128 字节，不能包含空白或控制字符。省略时服务端生成 ID。仅轮换响应返回完整新 `admin_key`，常规设置读取只返回空 `admin_key` 和掩码；调用方应保存新值后继续请求。轮换是独立操作，不能夹带设置编辑。

网络中断丢失轮换响应时，可在 24 小时内用原 Admin Key、相同 `request_id`、相同轮换端点重放。只有该轮换结果仍为当前有效密钥时才返回相同结果，不再次生成密钥。原密钥不能用于其他端点、其他 ID 或跨端点恢复。PostgreSQL 保存轮换记录；文件兼容模式保存最近一次轮换的恢复记录。后续轮换使之前结果失效，不能用过期结果替换当前密钥。认证与提交之间发生其他轮换时返回冲突，客户端重新读取自身已保存的认证状态后处理。

轮换不会撤销此前已建立的合法浏览器会话；会话按原到期时间或显式注销结束。Bearer 调用注销只清理响应 cookie，不删除未参与本次认证的浏览器 Redis 会话。

常见错误码包括 `invalid_request`、`unauthorized`、`session_expired`、`origin_mismatch`、`not_found`、`method_not_allowed`、`version_conflict`、`conflict`、`association_conflict`、`body_too_large`、`storage_error`、`storage_unavailable`、`session_unavailable`。客户端按 HTTP 状态及 `error.code` 分支，展示 `error.message`；不要解析底层存储错误或假定重试可解决版本冲突。

## 账号、导入与版本

账号列表保留原有配置、用量及展示字段，增加 `id,version,updated_at,notes,tags,credential_status,last_checked_at`。`status` 取 `disabled,expired,error,unverified,ok`；未实际验证的账号不展示正常。`credential_status` 是持久化凭据维护状态，不能把“令牌尚未过期”解读为“已经请求成功”。

导入 `raw` 是包含完整 JSON / JSON 数组 / JSONL / 纯 token 的字符串，后端统一识别 `tokens`、`credentials` 和平铺凭据。损坏 JSON 返回格式错误或逐行失败，绝不当作 token。结构化 `accounts`、单账号输入和 `raw` 互斥。`replace` 默认 `false`，只有显式为 `true` 才允许替换冲突账号。重名与重复凭据分别报告，绝不静默覆盖。

```json
{"imported":1,"added":1,"duplicate":1,"replaced":0,"failed":0,"dry_run":false,"results":[{"index":1,"id":"a_...","name":"example","status":"added"},{"index":2,"name":"example","status":"duplicate","code":"duplicate"}]}
```

`results.index` 从 1 开始，对应解析后的输入项；JSONL 格式错误另给原始行号。`imported=added+replaced`；预览报告是预测结果，不保证后续提交仍无并发变化，提交会重新校验。失败项不含输入凭据。新界面保存账号 ID，后续编辑、删除、刷新与查看日志均使用 ID。

## 分页、日志与统计

账号分页格式：`{"accounts":[],"has_more":false,"next_cursor":"","total_kind":"not_computed"}`。游标是不透明字符串，客户端原样传回；筛选条件改变后清空游标。

日志按 `(created_at DESC,id DESC)` 稳定倒序。`page_size` 默认 50、最大 500。`total=-1,total_kind="not_computed"` 表示没有执行全表计数；界面显示“更多”而不是 `-1 条`。旧 `page` 兼容有界翻页，超过 100 页返回 `400`；新界面统一使用 `cursor`。`from` 包含、`to` 不包含，均为 Unix 秒。

`cards.cache_rate` 固定为 `0..1` 比率，前端乘 100 展示百分比。分母为零时为 0，并提供 `cache_rate_has_samples:false`。`tokens_total` 是自持久事件起累计（包括导入时明确标记的旧统计基线）；`tokens_retained` 为当前保留期聚合。响应提供 `timezone`、`retention_days`、`totals_scope`；今日边界使用部署配置时区，默认 UTC。概览将 `from` 向下、`to` 向上取整到分钟，并在 `range` 返回实际范围；趋势、卡片、排行榜和最近错误使用同一区间。范围最大 366 天、最多 600 个趋势点，超出范围返回 `400`。无查询参数的概览缓存 10 秒；修改后失效，Redis 故障自动回源 PostgreSQL。持久聚合查询失败返回 `503 statistics_unavailable`，不会缓存伪造的零用量。

## 代理检测

保留 `ok,latency_ms,ip,http_code,error`，增加 `checked_at,stage,error_code,connection,authentication,http,exit_ip`。阶段状态使用 `ok,failed,not_checked`。连接阶段仅描述已经观测到的结果；不能从 TCP 成功推断鉴权通过。只有目标 HTTP 为 2xx 且响应含合法 IPv4/IPv6 时 `ok:true`。HTTP 403/407/429 均失败，407 定位为代理鉴权失败；连接/超时/HTTP 状态/响应读取/IP 解析分别返回稳定 `error_code`。错误文本固定脱敏，不返回底层含密码的 URL 或原始响应正文。

## 依赖诊断与审计

`GET /api/diagnostics` 在一个 2 秒探测期限内并行检查已配置的 PostgreSQL、Redis。`storage_mode` 为 `file` 或 `postgres`，总体 `status` 为 `ok` 或 `degraded`；每个 `dependencies.postgres/redis` 返回 `configured,status,latency_ms,error_code?`，状态取 `not_configured,ok,unavailable,timeout`。响应不包含数据库地址、代理地址、驱动错误、环境变量或凭据。依赖故障体现在诊断结果中，成功生成报告仍为 HTTP 200；认证存储故障可能在进入诊断前返回 503。

`capabilities.audit_logs` 表示是否支持 PostgreSQL 审计查询。`GET /api/audit-logs` 在 PostgreSQL 模式返回 `supported:true`；文件兼容模式返回 `supported:false,storage_mode:"file",items:[]`，不能把空数组解读为“从未发生过管理操作”。

审计 `limit` 默认 50、最大 200；`action` 和 `entity_id` 为精确匹配，最长分别 100、256 字节，不能包含控制字符。`from` 包含、`to` 不包含，使用正整数 Unix 秒；两者同时提供时 `to` 必须大于 `from`。未知或重复查询参数返回 400。每项包含 `id,created_at,actor,action,entity_id,result`，`id` 是十进制字符串，避免 JavaScript 整数精度丢失。

审计按 `(created_at DESC,id DESC)` 翻页，`next_cursor` 是不透明游标，客户端原样传回；修改 `action,entity_id,from,to` 时清空游标，否则返回 400。查询期限为 5 秒；存储不可用返回 `503 audit_unavailable`，不会伪造空成功结果。审计接口返回已有持久化操作记录，不承诺记录每一个被认证拒绝的 HTTP 请求。

## 自动化验收

`go test ./... -run '^TestAdminKeyAPI' -count=1` 使用临时文件存储与合成凭据，覆盖全部 32 个注册管理路由的 Admin Key 访问、普通网关 Key 拒绝、JSON 错误和 `Allow`、账号/分组/密钥/代理生命周期、部分设置与版本冲突、轮换后旧凭据失效、存储错误脱敏。刷新请求和代理探测只访问本地测试服务器，不调用真实账号或外部网络。Cookie、跨实例轮换与 PostgreSQL/Redis 行为另由对应集成测试验证；跳过外部依赖测试不能当作已通过。

## 运行诊断与既有操作审计查询

新增两个只读管理接口，使用相同的管理认证与 `Cache-Control: no-store`：

| 路径 | 返回与约束 |
| --- | --- |
| `GET /api/diagnostics` | `version,checked_at,uptime_seconds,storage_mode,status,dependencies,capabilities`。依赖 `postgres/redis` 含 `configured,status,latency_ms,error_code?`；状态为 `ok/not_configured/unavailable/timeout`。两项共用 2 秒检查预算，故障详情脱敏，不返回连接配置；依赖故障仍返回 200 及局部状态，管理认证失败保留 401/503。 |
| `GET /api/audit-logs` | `supported,storage_mode,items,has_more,next_cursor,limit`。查询支持 `action,entity_id,from,to,limit,cursor`；from 含、to 不含，时间为正 Unix 秒；limit 默认 50，上限 200。记录字段 `id` 为十进制字符串、`created_at` 为 Unix 秒，另含 `actor,action,entity_id,result`。内部游标保留微秒并绑定筛选；错误参数、重复参数、跨筛选游标返回 400。 |

审计读取现有 PostgreSQL 事务记录，不新增事件写入或 schema。文件模式 `supported:false`、空 items；不伪造持久历史。存储查询有 5 秒期限，故障返回 503 且不暴露底层 SQL/驱动错误。

## 网关边界

本契约只覆盖管理 API 与存储，不修改 Codex 转发协议、账号调度、负载均衡、计费、限流或模型路由。数据库 migration、导入 CLI、环境变量与切换/回退步骤另见 `docs/BACKEND-HANDOFF.md`。禁止直接对生产自动迁移。
