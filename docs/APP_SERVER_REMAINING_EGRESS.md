# App Server CLI：剩余出站差异审计

2026-09-27，仅审阅固定 `codex-rs 0.155.0` 源码和当前网关路由，未调用任何新增模型、账号或插件 API，未修改 Go/Rust。源码根目录为 `.local/codex-source-0.155.0/codex-rs`。本文是下一阶段的缺口清单，不把源码中存在的接口写成已经在线实测。

## 首要结论

**`chatgpt_base_url` 只改变目的地址，不改变客户端的认证类型，也不会让 provider 的 `env_key` 自动成为系统 API 的凭据。** `protocol/src/auth.rs:54` 明确将 `ApiKey` 判为不使用 Codex backend；`Chatgpt`、`ChatgptAuthTokens`、`Headers`、`AgentIdentity`、`PersonalAccessToken` 则属于该 backend。很多调用者在构造 HTTP 请求前就据此返回空结果、禁用能力或报错。因此，“补几个 `/backend-api/...` GET 路由”无法独立完成 API-key 客户端与账号登录 App Server 的全部行为等价。

根字段 `chatgpt_base_url` 默认值为 `https://chatgpt.com/backend-api/`（`core/src/config/mod.rs:4311`，定义见 `config/src/config_toml.rs:389`），与 `[model_providers.<id>].base_url` 分开。若后续做显式系统 API 桥，应使用类似 `https://example.invalid/backend-api` 的根配置：`backend-client/src/client.rs:126` 按地址是否含 `/backend-api` 选择 `/wham/...` 或 `/api/codex/...`。自定义域不会自动补 `/backend-api`。这个示例仅说明路径选择；当前尚无该系统桥，不是可用配置承诺。

用户级 provider key 正常进入 `model_provider.api_auth()`；下面多数系统调用却读取 `AuthManager` 的 Codex 登录状态。单改 URL 后还可能继续携带客户本机真实 ChatGPT bearer，或完全不带 Authorization，不能假定所有流量都自动采用网关 key。下一步必须先用合成 key 对固定官方二进制验证“客户端确实发请求及带何种凭据”，再实现消费者真正需要的精确转发。

## 已共享的行为，不应重复生成

官方 CLI 与 App Server 复用 `core`、`codex-api`、backend-client 和扩展客户端。普通推理、预热、压缩及工具调用的条件差异，需按同配置、同认证、同能力开关比较；接口在 App Server 捕获中出现，并不证明它是 App Server 独有的增强推理路径。

当前轻量网关已有 Responses/compact、Guardian、alpha/search、images、memory summary 和 Realtime 的固定处理入口；证据分别见 [接口清单](APP_SERVER_EGRESS_INVENTORY.md)、[Realtime 报告](APP_SERVER_REALTIME_CAPTURE_20260927.md) 及各冻结运行报告。这里的“有路由”不表示客户端一定暴露对应工具，也不表示所有真实服务的条件分支都验收完成。现有普通生成测试不能覆盖下表的客户端认证检查。

仅转发消费者实际发来的请求：不要在每轮生成前人工增加 settings、插件同步、云任务、遥测、登录或诊断请求；不要因为清单里有写操作就自动执行它。

## 八项真实缺口

| # | 缺口与固定源码证据 | 普通 API-key/custom-provider 实际条件 | 下一步最便宜的决定性验证或实现 |
| --- | --- | --- | --- |
| 1 | **用户设置与组织配置的客户端认证门槛。** `ext/git-attribution/src/policy.rs:70-92` 才是启动 `GET B/wham/settings/user` 的调用者，它读取 `commit_attribution_enabled`；`cloud-config/src/service.rs:50-58` 再限制组织配置计划类型，`backend.rs:62` 读 `GET B/wham/config/bundle`。 | 非 backend auth 的 git attribution 直接得到 `false`，不发请求；组织配置还要求 Business/Education/Enterprise 类 plan。`chatgpt_base_url` 不改变这两道门槛。settings 请求在此处影响提交归属，不是已证实的模型强弱开关。 | 离线二进制 fixture 分别注入 synthetic ApiKey/backend auth，记录是否请求、归属字段及缓存行为。若要向客户端提供同等设置，需显式客户端能力/认证适配和 key→owner 固定绑定；仅增加 GET 路由不足。不要把上游组织配置应用到另一个随机池账号的客户端。 |
| 2 | **账号额度、用户资料和云任务不是 provider 推理接口。** `app-server/src/request_processors/account_processor.rs:1127-1149` 在 `account/rateLimits/read` 前检查 backend auth；`chatgpt/src/chatgpt_client.rs:70-80` 还要求 account id；`cloud-tasks/src/lib.rs:88-109` 对 API key 直接退出。路径族包括 `B/wham/usage`、`profiles/me`、`tasks/list`/task detail 等。 | 使用 provider `env_key` 不会成为本机 ChatGPT 登录，也不会自然触发这些账号功能。完整 hosted App Server 中的账号 RPC 与仅模型 API 的客户端可用性确有差异。 | 先捕获客户端实际调用的只读 RPC/HTTP；桥接时保持客户端 key→账号/工作区归属和调用方权限，不能将整个池的 usage 当成某个账户的官方 usage。云任务创建、重置券消费等写操作只在用户真实操作时转发，不能作为“补齐启动请求”自动发起。 |
| 3 | **远程插件目录与匿名 featured 是两种不同认证路径。** `core-plugins/src/remote.rs:2296` 的 `ensure_chatgpt_auth` 在请求前拒绝 API key，`remote.rs:1019` 用于 suggested；`manager.rs:1848` 不满足条件时改走 legacy。`remote_legacy.rs:123-150` 的 `/plugins/featured` 仍会发送，但只有 backend auth 才附认证。 | API-key 客户端通常不会发 `/ps/plugins/suggested/codex` 或 installed 请求；legacy featured 则可能匿名发送，**不会附 provider 的网关 key**。把其 base 改到鉴权网关后会得到 401，不能把这个 401 解释成推理降级。 | 先以合成 `env_key` 捕获 suggested/installed 的缺席和 featured 的无 Authorization。若提供插件系统桥，必须明确客户端认证注入与 capability adapter，然后只开放固定目录/详情接口；保留 `OAI-Product-Sku`、query、账号作用域。目录成功不等于安装/工具可执行成功。 |
| 4 | **插件归档出站不全受 `chatgpt_base_url` 控制。** `core-plugins/src/startup_sync.rs:23-30,77-92` 固定 GitHub API/git 与 `https://chatgpt.com/backend-api/plugins/export/curated` 备用地址；远程 catalog 还可能返回动态 download URL。该同步函数不接受账号 auth。 | GitHub/归档同步是共享客户端行为；它不因 provider 指向网关而转移，也不会因网关补 `/plugins/export/curated` 路由就自动使用新地址。已有缓存、git 可用性和功能开关会改变实际出站数量。 | 离线验证空缓存/已缓存及 git/HTTP 分支，并标明哪条 URL 固定、哪条来自响应。需要统一出口时应在客户端共享网络配置或下载器中做适配；服务器不应代客户再下载一份插件来凑请求数。 |
| 5 | **Apps/连接器目录与真实工具认证。** `chatgpt/src/connectors.rs:35-55` 先做 `apps_enabled_for_auth` 和 backend auth 检查；`chatgpt_client.rs:75` 还检查 account id。目录路径含 `/connectors/directory/list`、`list_workspace`，后续 MCP 工具调用使用独立服务/凭据。 | 非 backend auth 会得到空目录或认证错误；普通 `env_key` 和 `chatgpt_base_url` 不足以启用连接器。即使补目录 GET，也不能由此推断 MCP 工具调用已可用。 | 先 fixture 验证工具发现是否发出，再逐一核对目录→工具列表→单个工具调用的认证交接。客户端拥有工具执行，网关只桥接真实消费的固定服务接口；任意 MCP 目的地址不属于一个有限 provider URL 清单。 |
| 6 | **History/Notes 路由缺失且扩展不会在普通 custom provider 启用。** `ext/history-notes/src/extension.rs:46-66` 同时要求 `token_budget.use_history_notes_extension`、`provider.is_openai()`、`current_auth_uses_codex_backend()`。`tools.rs:84-96` 包含 `alpha/history/v2/{list_windows,list_items,read_item,search_contents}` 与 `alpha/notes/v2/{list_files_by_prefix,read_file,search_contents,append_to_file,write_file}`，另有 `thread_hint`。 | 当前网关没有这组精确路由；当前普通 custom-provider 配置也不会调用它们。不能只在服务端加路径后宣称 Notes 已适配，更不能为启用它恢复用户已删除的本地 token 上限。 | 先按固定 extension 接口做离线消费者测试，再补透明路径和客户端能力适配。保留 `x-openai-encrypted-tool-arguments` 与 `x-openai-tool-output-truncation-policy`（`backend.rs:55-81`）；append/write 不跨账号自动重放。 |
| 7 | **图像路径已接入，但官方客户端工具暴露门槛仍不同。** `ext/image-generation/src/extension.rs:41-43` 的 available 要求 `is_openai || requires_openai_auth || uses_openai_actor_authorization`。`supports_standalone_web_search` 是独立搜索能力位，不解决图像启用。 | 常见 `requires_openai_auth=false` + `env_key` custom provider 不满足此门槛；网关 `/images/*` 可转发不等于客户 CLI 自动获得 image_generation 工具。 | 固定官方二进制 fixture 查看最终工具列表；再验证支持的 provider 认证配置能否启用且始终携带网关 key，或在客户端能力层作显式适配。不要为了过 gate 冒充一个未验证的 ChatGPT 登录态。 |
| 8 | **按 turn 查询官方费用的独立接口缺失。** `app-server/src/turn_cost_worker.rs:101-126` 仅在 OTLP log/metrics exporter 配置时启动；`:429-454` 的 custom-provider 分支真实使用 `P/analytics/codex/turn-costs` 和 provider auth。ChatGPT 分支使用 `B/wham/usage/thread-estimates/query`（`turn_cost_worker_chatgpt.rs:35-40`）。 | 与上面 auth-gated 系统接口不同，这条 custom-provider 查询确实可带网关 key 到达 `/v1/analytics/codex/turn-costs`，当前无对应路由。它读取费用结算信息，不是模型响应 `usage`，也不是无条件遥测 POST。 | 先启用合成 exporter 捕获真实请求/响应形状。若桥接费用查询，按真实 turn→owner/response 关联转换并保留未结算/不可见状态，不把未知金额当 0、不用本地估算冒充官方账单。不启用 exporter 时不补发请求。 |

## 验收边界与下一阶段顺序

优先验证会真实到达当前网关的费用查询，以及图像/History/Notes 的能力暴露；这些能以合成官方二进制消费者和固定 loopback fixture 得到决定性证据。系统账号、插件和 Apps 随后要先证明认证桥可行，不能先部署“没有客户端能调用”的路由并把它计作完成。

以上八项不能归因为“官方 CLI 使用了另一个低智模型”。它们说明**使用 ChatGPT 登录的完整运行时**与**使用网关 API key 的 custom provider 客户端**存在产品能力、状态归属与出站条件差异；CLI 与 App Server 共享底层实现，不会因名字不同自动绕过这些检查。登录、设备配对、发布插件、邮件、Sentry/OTLP 及任意用户工具网络继续按实际调用和各自协议处理，不人工复制到每轮推理链。

完成“所有差异”仍需逐项把认证前的客户端行为、线上实际请求、返回字段、账号归属与最终能力验收关联起来。本文没有把条件未触发、源码推断或路由存在当成全功能验收通过。
