# 网关系统设置

2026-09-18。管理页可独立保存设置；每次更新携带 `settings_version`，冲突返回 409。失败不会显示已保存。旧配置缺少的新字段按下表加载；显式关闭或空文本会持久保留。PostgreSQL 模式的其他进程最多约 2 秒读取到新设置。开关和模式选择即时保存；文本、会话期限和三个超时需点击对应保存按钮，刷新和版本冲突会保留未保存草稿。

| API 字段 | 默认值 | 行为 |
| --- | --- | --- |
| `websocket_enabled` | `false` | 接受新的 Responses WS 连接；关闭时返回 HTTP 426，支持回退的客户端改用 HTTP。已建立的连接不因关闭开关强制断开。 |
| `builtin_prompt_enabled` | `true` | 在每轮 Responses / compact 的 instructions 按选定模式加入稳定内置文本，保留客户端原 instructions。 |
| `builtin_prompt` | 简短的准确、协助用户、如实描述行动的提示词 | 可编辑，最多 32768 UTF-8 字节。空文本表示不附加；不含动态日期、时间戳或随机值。 |
| `sticky_sessions_enabled` | `true` | 按网关键和原始会话身份固定账号，确定性选择可用代理出口。 |
| `sticky_session_ttl_seconds` | `604800` | 滑动绑定期限，范围 60..2592000 秒；请求及活跃连接续期。这不是上游提示缓存 TTL。 |
| `prompt_cache_key_enabled` | `true` | 补充缺失或空缓存键；CLI 默认会话键在同一网关键及账号内稳定复用，显式自定义键保留。 |
| `http_stream_enabled` | `true` | 允许 HTTP Responses SSE；关闭返回 403，不影响原生 WS。 |
| `http_json_enabled` | `true` | 允许 HTTP Responses 非流式 JSON；关闭返回 403。 |
| `compact_enabled` | `true` | 允许 `/responses/compact`；关闭返回 403。 |
| `models_enabled` | `true` | 允许模型列表；关闭返回 403。 |
| `compatibility_normalization_enabled` | `true` | 移除顶层 temperature / max_output_tokens / user 及 background=false/null，将 input 中 system 消息就地转为 developer，清理已有标准 role 的 message.author 元数据；background=true 明确返回不支持，不降级为同步请求。关闭跳过这些转换。基础协议处理仍生效。 |
| `automatic_token_refresh_enabled` | `true` | 自动刷新接近过期的令牌；关闭后未过期令牌仍可使用，已过期则拒绝。管理端手动刷新不受影响。 |
| `upstream_401_recovery_enabled` | `true` | 上游明确 401 时同账号至多一次刷新恢复；关闭直接返回原始 401。与临期自动刷新独立控制。 |
| `builtin_prompt_mode` | `prepend` | `prepend` 前置、`append` 后置、`if_empty` 仅当 instructions 缺失、null 或仅空白时补充。重复处理不重复注入。 |
| `upstream_stream_idle_timeout_seconds` | `300` | 10..3600 秒，仅 HTTP 上游读取等待计时，不限制持续有数据的整轮回复。 |
| `upstream_response_header_timeout_seconds` | `90` | 5..600 秒，HTTP Responses / models 等待响应头。 |
| `upstream_compact_timeout_seconds` | `1200` | 30..3600 秒，compact 的响应头等待及整体期限。 |

例如只切换 WS：

```json
{"settings_version": 4, "websocket_enabled": true}
```

`GET /api/settings` 返回这些平铺字段；`POST /api/settings` 返回 `{"ok":true,"settings_version":5}`。未提交的字段保持不变，密钥轮换仍单独提交。

## HTTP、TLS 与 WebSocket

- HTTP 流式请求返回 SSE；非流式请求收集上游 SSE 后返回终态 JSON。compact 返回 JSON。以上不依赖 WS 开关。
- 各 HTTP 入口开关在选取账号前校验，关闭时不发上游请求；管理员网关 key 同样受限。三个 HTTP 超时不改变原生 WS 超时。请求采用开始时的配置，后续修改不会中断正在执行的 HTTP 请求。
- HTTPS/WSS 的 TLS 配置独立于请求的 `stream` 字段。WS 专用 transport 克隆账号的 HTTP/SOCKS5 代理和 TLS 配置，并使用 HTTP/1.1 Upgrade；普通 HTTP/SSE 的 HTTP/2 配置不会被修改。
- WS 是到上游的真实持久连接，保留 `previous_response_id`、`stream_id`、`generate:false`、工具和客户端元数据。每个生成请求重新校验权限、计入账号并发，终态或断连释放；空闲 socket 不占用生成名额。
- 内置提示词和缓存键设置在已有 WS 的下一轮生效。更改需要重新握手的 CLI 身份配置时，已有连接会要求重连。每连接最多保存 128 个会话绑定和 128 个待结束响应，已结束响应会从跟踪表移除。
- 发生网络故障不会自动重放上游生成；客户端应按协议恢复。WS 缺少会话头时，需要先读取 `response.create` 才能用其中的显式缓存键选账号，这种情况下上游握手错误通过 WS error 事件返回，上游握手响应头无法事后补发到客户端。
- CLI 使用 WS 时还需在自定义 provider 配置 `supports_websockets = true`。是否流式不代表是否使用 WS。
- 服务停机主动取消 WS，结束上游连接、释放账号名额并保存最终记录；普通 HTTP 请求保留原有优雅退出等待。

## 会话与缓存

身份优先级为 `session-id`、`session_id`、`thread-id`、`conversation_id`、请求中原有的 `prompt_cache_key`。根 session 能覆盖 CLI 子线程与恢复会话。自动生成的缓存键不会反向变成会话身份，避免将匿名独立对话绑到一个账号。

Redis 原子建立共享绑定，键按网关键隔离，只保存账号引用。无 Redis 的兼容文件模式使用本进程有界表。已绑定账号忙时返回可重试错误；账号禁用、撤权、替换成其他上游身份或凭证失效时不默默换号。关闭粘性会话可能让后续请求换号，带账号加密历史的对话应维持开启。绑定过期后允许重新选择账号，应据会话恢复周期配置期限。

代理选择不受绑定列表次序变化影响；删除/停用代理仅重分配受影响会话，加入代理会重新分配部分会话。网络失败不会自动换出口重发。WS 已建立连接固定出口；新连接使用当前代理配置。

缓存键缺失时，有会话标识则按网关键 + 会话生成稳定散列；无会话则按网关键 + 模型/指令/工具等稳定字段生成。用户消息的动态后缀不参与此键。已有工具、历史、reasoning、text 和缓存选项保持原样，不强制 `24h`、显式断点或存储策略。改变提示词开关/内容会改变缓存前缀。

2026-09-25 起，若客户端的非空 `prompt_cache_key` 与 `client_metadata.session_id` 精确相等，则按已选定账号的所有者身份、身份版本和网关键生成稳定的 64 字符缓存键，供同账号不同默认会话复用。此规则参考 Sub2API 的默认会话缓存键收敛；只改该 JSON 标量，不改真实会话标识、历史、提示词或账号绑定。自定义 override、内部任务的 `source:parent_thread_id`、缺失 metadata 或不匹配的键保持原样。关闭 `prompt_cache_key_enabled` 同时关闭补齐及默认键收敛。账号所有者更换或无法确认所有者的凭据轮转会分隔缓存命名空间，已知所有者的常规 token 刷新不分隔。详见 [本次复现与验证](CACHE-OPTIMIZATION-2026-09-25.md)。

实现依据为 [OpenAI 提示缓存文档](https://developers.openai.com/api/docs/guides/prompt-caching)、[Responses WebSocket 文档](https://developers.openai.com/api/docs/guides/websocket-mode) 和官方 Codex 0.154.0 本地源码。具体证据与兼容边界见 [缓存核查](GATEWAY-CACHE-RESEARCH.md)。`x-codex-turn-state` 只在同一 turn 内透传，不做跨轮会话缓存。

实际缓存率仍由上游返回的 `cached_tokens / input_tokens` 衡量。这些改动减少网关引入的变化，不承诺命中率。2026-09-17 已使用用户新授权账号和 `gpt-5.6-luna` 完成真实 HTTP JSON/SSE/WS 验收：长参考前缀首轮之后五次加权命中率 77.64%，基础短前缀六次零命中；这是小样本实际用量，不是生产优化前后对照。详细输入规模和逐轮数据见[真实账号验收](GATEWAY-SETTINGS-LIVE.md)。

## 验证

官方未修改 Codex CLI 0.154.0 配合本地协议服务器已验证 HTTP 工具调用与恢复、WS 关闭时 HTTP 回退、自动 compact 与恢复，以及 WS 开启时的真实工具调用、预热与增量续接。WS 场景观察到 2 条连接、3 次生成、2 次 `generate:false` 预热、3 次 `previous_response_id` 续接，没有 HTTP 回退。这是协议兼容验证，不是生产推理或生产缓存率测量。

```powershell
go test ./... -count=1
go vet ./...
npm --prefix frontend test
node frontend/scripts/build-settings-ui.mjs
node frontend/tests/legacy-settings-runtime-browser.mjs
```

真实 CLI 测试需将 `CODEX_REAL_CLI_BIN` 指向官方可执行文件后运行 `go test -run 'TestRealCodexCLI' -count=1 -v .`。隔离数据库回归需配置 `TEST_DATABASE_URL` / `TEST_REDIS_URL`；Redis ACL 前缀通过 `TEST_REDIS_PREFIX` 指定。无配置时相关测试跳过。

此 Windows 环境未配置 CGO/C 编译器，`go test -race` 无法执行；并发行为由原子绑定、并发准入及生命周期测试覆盖，不能代替 race detector。

当前恢复包只替换旧界面的 Settings 路由，输出 `.local/settings-restore/release-dist`。构建逐项校验旧界面及 Logs 补丁的 47 项基线资源，新增 Settings JS/CSS，沿用同一 Vue/Element 运行时。不能将当前整套前端构建直接覆盖到已回退的界面。
