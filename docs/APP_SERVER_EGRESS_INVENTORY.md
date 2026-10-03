# Codex 0.155.0 出站接口与中转边界

审查版本：本地固定源码 .local/codex-source-0.155.0/codex-rs，对应 rust-v0.155.0。2026-09-27。

本清单区分静态接口、触发条件和实际捕获。它覆盖下列官方 Rust 网络客户端的接口类别；可配置 MCP、插件下载 URL、用户工具和第三方 provider 的任意目的地址没有有限静态全集。源码存在接口不等于本轮执行过，更不等于每个客户端版本都会调用。桌面端未公开的前端网络不在这份 Rust 源码证明范围内。

实际捕获基准为已审阅完整原始证据的服务器隔离 pilot：

- run：20260927T101011.868409Z。
- official_app_server：25 个 HTTP 请求、19 次 CONNECT；模型推理使用一个 WSS 连接及预热/生成/续接消息。
- official_codex_cli（本项目旧直连路径）：3 个 HTTP 请求、3 次 CONNECT。
- 已经产出的后续 full/lightweight runs 有各自冻结源码；不能用它们的成功摘要代替逐接口原始证据。本文没有将那些摘要自动升级为“所有条件已实测”。
- strace -f connect 在上述 pilot 中没有观测到非 loopback 连接；两个测试进程树除代理外各连接一次本地测试网关。这不证明未连接 UDP、预存进程、io_uring 等全部可能出网方法。

以下 P 表示当前模型 provider 的 base_url。ChatGPT OAuth 常见 P 为 https://chatgpt.com/backend-api/codex；自定义 Responses provider 可为中转的 /v1。B 表示 chatgpt_base_url 对应的 backend-api 前缀；A 表示 auth issuer（默认 https://auth.openai.com）。Provider 的 url_for_path 仅连接 base/path，见 codex-api/src/provider.rs:53；不会把自定义 provider 的搜索自动切回官方地址。

## 已确认的 alpha/search 缺口

原生客户端的独立 web.run 是客户端执行器发出的单独 POST，不是主 Responses 请求里的服务端 web_search 内置工具，两者都可能存在。

决定性链路：

1. ext/web-search/src/extension.rs:43：OpenAI、OpenAI actor authorization 或显式 supports_standalone_web_search 的 provider 可用，且 web_search_mode 不能 Disabled。具体每轮工具暴露还受 feature/model metadata 选择。
2. ext/web-search/src/tool.rs:103：执行器调用当前 provider.api_provider()；:107 获取同一 provider 的认证；:111 创建 SearchClient。
3. ext/web-search/src/tool.rs:116：请求含 session id、model、最近输入、commands、settings、max_output_tokens；:148 实际调用 search。输入包括上轮搜索状态时必须保留原文，不能转成一次普通模型提问。
4. codex-api/src/endpoint/search.rs:31：路径固定 alpha/search；:44 使用 POST；响应按 SearchResponse JSON 解码。
5. app-server/tests/suite/v2/web_search.rs:53：官方专门测试自定义 provider；:78–79 明确 ChatGPT 测试路径 /api/codex/alpha/search 和自定义 /v1/alpha/search；:120–125 设置 supports_standalone_web_search=true。

因此客户把 provider base_url 指向本项目 /v1，且该工具被启用时，会请求本项目 /v1/alpha/search。原来的 404 是明确协议缺口，不是必须在服务器重跑官方工具循环的证据。

本轮新增 gateway_appserver_auxiliary.go 已提供精确 POST /v1/alpha/search 与 /alpha/search：只在启用 lightweight official_codex_cli 的入口生效，鉴权/账号准入/出口代理/401 恢复/账号切换/owner CAS 复用现有边界，向已选账号的 P/alpha/search 转发原始 JSON，响应原字节返回；不改写 instructions、stream、工具声明或搜索状态，不缓存搜索会话。合成测试已通过；最终轻量捕获 20260927T105836.078019Z 已验证该端点 HTTP 200、12 条实际结果，见 final-functional.json。

## 模型 provider 接口

| 方法 / 相对路径 | 源码证据（codex-rs 下） | 触发条件与运行时归属 | pilot 观察 | 纯客户端中转相关性 |
| --- | --- | --- | --- | --- |
| POST P/responses | codex-api/src/endpoint/responses.rs:40、:170；core/src/client.rs | 正常生成；CLI/App Server 共用 core | CLI 2 次，均有模型完成事件 | 必需；保留请求/响应语义 |
| WSS GET P/responses；帧 response.create | codex-api/src/endpoint/responses_websocket.rs:239；core/src/client.rs:1767 | provider 支持 WS、功能启用、无已选 HTTP fallback；共用 | App Server 已见预热、首轮、续接 | 必需的传输变体；仅保连接状态即可，不等于需要第二个 agent |
| response.create，generate=false | core/src/client.rs:1900、:1934 | 初次网络预热；共用 | 已观察，并以 exact previous_response_id 展开 | 无生成预热；需区分失败是否已发生成 |
| POST P/guardian；可用同 WS endpoint 选择 | codex-api/src/endpoint/responses.rs:31–44 | 完整 Guardian 审核推理；功能/审核路径启用；共用 | 未触发 | 客户端启用该能力时是独立推理面，不应伪装普通 Responses |
| POST P/guardian-classifier | codex-api/src/endpoint/responses.rs:34–44；ext/guardian-v2/src/async_scorer/sampler/connection_pool.rs:247 | 异步风险分类器；共用 | 未触发 | 可透明转发；服务器不必另外启 Guardian 循环 |
| GET P/models?client_version=... | codex-api/src/endpoint/models.rs:31、:41；model-provider/src/models_endpoint.rs:87 | 模型目录加载/刷新；共用；部分官方鉴权有目录 base 选择规则 | App 2、CLI 1；两路径对应目录响应 SHA 相同 | 直接影响能力/defaults/context，不能凭模型名猜测 |
| POST P/alpha/search | codex-api/src/endpoint/search.rs:31、:44；ext/web-search/src/tool.rs:103 | 客户端 web.run 独立执行器；共用 | 此 pilot 未触发 | 明确需要补的透明辅助接口；与内置 web_search 区分 |
| POST P/memories/trace_summarize | codex-api/src/endpoint/memories.rs:32、:43；core/src/client.rs:633 | 非空原始记忆摘要输入；共用 | 未触发 | 客户端启用记忆时需要；不是每轮生成必要请求 |
| POST P/images/generations | codex-api/src/endpoint/images.rs:41、:69；ext/image-generation/src/backend.rs:65 | 客户端图像生成扩展 | 未触发 | 单独内容协议；不能只覆盖文本 /responses 就宣称全能力 |
| POST P/images/edits | codex-api/src/endpoint/images.rs:54、:69 | 客户端图像编辑扩展 | 未触发 | 同上，保留图像输入及输出 |
| POST P/realtime/calls | codex-api/src/endpoint/realtime_call.rs:62、:111、:155 | WebRTC/实时会话，可能 SDP 或 multipart；共用 core，App RPC 可触发 | 未触发 | 实时产品范围内相关，不能用普通 JSON rewrite |
| POST P/live（非 backend-api frameless bidi） | codex-api/src/endpoint/realtime_call.rs:66–78 | FramelessBidi 且非 backend 请求形状 | 未触发 | 条件分支，不是所有实时调用都走此路径 |
| WSS /v1/realtime 或 provider/realtime，model/intent query | codex-api/src/endpoint/realtime_websocket/methods.rs:1150–1195 | 启用实时语音；服务/配置选择 host/path | 未触发 | 需独立双向流，普通 Responses 中转不自动覆盖 |
| WSS /v1/live 或 provider/live | 同上 :1170–1172 | FramelessBidi 实时协议 | 未触发 | 同上 |
| POST P/alpha/notes/v2/list_files_by_prefix、read_file、search_contents、append_to_file、write_file | ext/history-notes/src/tools.rs:90–94；backend.rs:55 | history-notes 扩展实际调用；共用扩展层 | 未触发 | 客户端启用才需辅助接口；后两项有真实写入语义，不应自动重试 |
| POST P/alpha/notes/v2/thread_hint | ext/history-notes/src/extension.rs:122；backend.rs:55 | 扩展线程提示加载 | 未触发 | 条件能力；保留接口清单，不在每轮合成执行 |

压缩不是仅凭一个 URL 名字可确定的行为。0.155.0 的远端上下文压缩可表现为 Responses 的 compaction 控制/输入，并受 provider 和配置影响；本项目 /responses/compact 兼容入口也会映射到 /responses。不能把客户端输入的 /responses/compact 和上游实际出站路径混为同一个已捕获事实。

## ChatGPT 后端账户、云任务与使用量

backend-client/src/client.rs:185 会为常见 ChatGPT host 规范化 backend-api；PathStyle 另有 /api/codex/... 变体。下表写常见 B/wham 路径，不表示所有配置都只有一个绝对 URL。

| 方法 / 路径 | 源码 | 触发条件与归属 | pilot 观察 / 中转处理 |
| --- | --- | --- | --- |
| GET B/wham/settings/user | backend-client/src/client.rs:485、:673 | 用户设置加载；CLI/App Server 共用 backend client | App 1 次；可能影响能力，因果尚未判定，不默认删 |
| GET B/wham/config/bundle | backend-client/src/client.rs:468–476 | 拉管理配置 bundle | 未触发；独立配置面 |
| GET B/wham/accounts/check | backend-client/src/client.rs:341–346 | 帐户检查 | 未触发；鉴权/管理用途，不是推理回合 |
| GET B/wham/profiles/me | backend-client/src/client.rs:351–361 | token usage profile | 未触发；管理/展示 |
| GET B/wham/usage | backend-client/src/client/rate_limit_resets.rs:74、:127 | 额度/速率窗口查询 | 未触发；中转已有账户用量管理应独立映射 |
| GET B/wham/rate-limit-reset-credits | 同上 :84、:137 | 可用额度重置券查询 | 未触发；不为生成自动调用 |
| POST B/wham/rate-limit-reset-credits/consume | 同上 :113、:151 | 明确消耗重置额度 | 未触发；有状态变更，不能自动模拟 |
| POST B/wham/usage/thread-estimates/query | backend-client/src/client/chatgpt_turn_cost.rs:55–59 | ChatGPT 线程费用估算 | 未触发；账单面 |
| POST B/wham/usage/thread_usage/query | backend-client/src/client/thread_usage.rs:52、:80 | 线程使用量查询 | 未触发；不能代替模型返回的 usage |
| POST /v1/analytics/codex/turn-costs | backend-client/src/client/turn_usage.rs:62、:89 | API 账单/turn cost，使用解析后的 API URL | 未触发；非生成必需 |
| GET B/wham/tasks/list；GET B/wham/tasks/{id} | backend-client/src/client.rs:379–439 | 云任务列表/详情；CLI cloud 与 App 对应功能 | 未触发；纯本地客户端中转无需替客户建云任务 |
| GET B/wham/tasks/{id}/turns/{id}/sibling_turns | backend-client/src/client.rs:445–460 | 云任务分支/兄弟 turn | 未触发；独立云工作流 |
| POST B/wham/tasks | backend-client/src/client.rs:517–523 | 创建云任务 | 未触发；副作用，不自动重放 |
| GET B/wham/workspace-messages | backend-client/src/client.rs:502、:667 | 工作区消息查询 | 未触发；与模型主协议独立 |
| POST B/wham/accounts/send_add_credits_nudge_email | backend-client/src/client.rs:365–371、:657 | 用户明确触发额度提醒邮件 | 未触发；绝不作为请求等价化补齐动作 |

## 登录、令牌与身份

| 方法 / 路径 | 源码 | 触发条件 / 归属 | pilot 观察 / 中转处理 |
| --- | --- | --- | --- |
| 浏览器 GET A/oauth/authorize | login/src/server.rs:611 | 交互登录，CLI/App 登录流程共用；浏览器是另一进程 | 未触发，实验直接注入临时访问令牌 |
| POST A/oauth/token | login/src/server.rs:827、:1148；login/src/auth/manager.rs:200 | authorization-code 换 token、refresh、相关 OAuth grant | 未触发；中转账号刷新沿用已有单飞/CAS/持久化边界 |
| POST A/oauth/revoke | login/src/auth/revoke.rs:150；manager.rs:201 | 登出/撤销 | 未触发；不为普通生成调用 |
| POST A/api/accounts/deviceauth/usercode | login/src/device_code_auth.rs:68、:74、:170 | 设备码登录开始 | 未触发；登录面 |
| POST A/api/accounts/deviceauth/token | 同上 :107、:118 | 设备码轮询 | 未触发；登录面 |
| 浏览器 A/deviceauth/callback；本机 /auth/callback | device_code_auth.rs:202；server.rs:176、:346 | 浏览器回跳，后者是本地接收接口 | 未触发；不是中转应转发的出站模型接口 |
| GET A/api/accounts/v1/user-auth-credential/whoami | login/src/auth/personal_access_token.rs:11、:13、:85 | PAT 身份校验 | 未触发；条件认证方式 |
| GET B/wham/agent-identities/jwks | agent-identity/src/lib.rs:252、:486 | agent identity 令牌验证 | 未触发；不代表每个账号需要 attestation |
| POST A/api/accounts/v1/agent/{runtime_id}/task/register | agent-identity/src/lib.rs:328、:479 | workload/agent identity 任务注册 | 未触发；只有该身份模式才相关 |
| POST A/oauth/token（workload identity） | login/src/auth/workload_identity.rs:32 | 联合身份 token exchange | 未触发；与普通 ChatGPT OAuth 分开记录 |

## 插件、连接器和可配置外部服务

| 方法 / 路径 | 源码 | 触发条件 / 归属 | pilot 观察 / 中转相关性 |
| --- | --- | --- | --- |
| GET B/plugins/featured | core-plugins/src/remote_legacy.rs:129、:137 | 官方运行时插件发现；CLI/App 共用 core-plugins | App 3 次，401；不能称这三次请求成功 |
| GET B/ps/plugins/suggested/codex | core-plugins/src/remote.rs:1021、:1025 | 插件建议 | App 3 次 |
| GET B/ps/plugins/installed | core-plugins/src/remote.rs:2215、:2235 | 已安装插件同步，可带 download URLs | App 3 次 |
| GET api.github.com/repos/openai/plugins | core-plugins/src/startup_sync.rs:757 | curated marketplace 同步 | App 3 次 |
| GET api.github.com/repos/openai/plugins/git/ref/heads/{branch} | 同上 :770 | 取得远端 commit | App 3 次 |
| GET api.github.com/repos/openai/plugins/zipball/{sha} → codeload.github.com/openai/plugins/legacy.zip/{sha} | 同上 :792 | 归档下载及重定向 | App 各 3 次；codeload 三次未收到完整响应，保留失败，不解释为模型失败 |
| GET B/plugins/export/curated | core-plugins/src/startup_sync.rs:27、:353 | GitHub curated 归档的备用获取路径 | pilot 未触发；不是已证实必要的模型前置步骤 |
| GET B/ps/plugins/list、workspace/shared、{plugin_id} | core-plugins/src/remote.rs:2173、:2195、:2263 | 用户插件列表/详情 | 未触发；客户端已管理时服务器无需重复做，但须明确产品职责 |
| GET B/ps/plugins/search | core-plugins/src/remote/search.rs:44、:63 | 插件搜索 | 未触发；不是 web.run 搜索端点 |
| POST B/ps/plugins/{id}/install、uninstall | core-plugins/src/remote.rs:1594、:1691 | 用户插件安装/卸载 | 未触发；有账户副作用，不当作等价请求自动补齐 |
| GET B/ps/plugins/workspace/created | core-plugins/src/remote/share.rs:385、:393 | 用户创建插件列表 | 未触发 |
| PUT B/ps/plugins/{id}/shares | 同上 :320–321 | 修改共享范围 | 未触发；变更操作 |
| POST B/public/plugins/workspace/upload-url；PUT 动态 upload_url | 同上 :405、:423 | 插件包上传 | 未触发；动态 URL 不由固定 provider 路由涵盖 |
| POST B/public/plugins/workspace 或 /{id}；DELETE /{id} | 同上 :287–288、:455–459 | 发布/更新/删除共享插件 | 未触发；客户端管理面 |
| GET B/connectors/directory/list、list_workspace | connectors/src/lib.rs:191、:270；chatgpt/src/chatgpt_client.rs | Apps/连接器目录 | 未触发；与客户 App 功能相关，不为纯模型推理自动调用 |
| 可配置 MCP URL：HTTP POST/GET、SSE/streamable HTTP；OAuth metadata/token/registration URL | rmcp-client/src/perform_oauth_login.rs；oauth_http_client.rs；codex-mcp/src | 客户端已配置 MCP/Apps、认证及工具调用 | 未触发；目的地动态，不能静态声称“所有 URL 枚举完” |
| 可配置 git clone/HTTP 资源、插件/工具自身任意网络 | core-plugins/src/marketplace_add/source.rs:56、:185；remote_bundle.rs | 用户安装来源或工具运行 | 未触发；外部命令出网不由 provider base_url 接管 |

上述插件 21 次 HTTP 请求完整计入 pilot 清单。它们是官方运行时启动/插件同步的可观测行为；客户端自己已经承担该功能时可以由架构明确不在中转服务器重复执行。不能仅凭删除它们就断言推理质量已等价，也不能为了“全部出站相同”偷偷替客户安装、发布或删除插件。

## 实时远程控制、遥测和诊断

| 方法 / 路径 | 源码 | 触发条件 / 归属 | pilot 观察 / 中转相关性 |
| --- | --- | --- | --- |
| WSS B/wham/remote/control/server | app-server-transport/src/transport/remote_control/protocol.rs:236 | App Server 远程控制/设备连接 | 未触发；属于远程控制服务，非模型 HTTP 代理 |
| POST B/wham/remote/control/server/enroll、refresh | protocol.rs:224–227；server_api.rs:97–158 | 远程设备注册、令牌续期 | 未触发；不可为普通代理生成虚拟设备 |
| POST B/wham/remote/control/server/pair、pair/status | protocol.rs:230–233；enroll.rs:66、:169 | 远程配对及状态轮询 | 未触发；与客户端 UI/账户操作相关 |
| POST B/codex/analytics-events/events | analytics/src/client.rs:141、:903 | 埋点队列实际 flush，受设置影响；共用 | 未触发；遥测，不是模型上下文 |
| POST https://ab.chatgpt.com/otlp/v1/metrics | otel/src/config.rs:9、:14–34 | release 默认 Statsig exporter/配置选择；debug 默认不发送 | 未触发；不能因源码有地址就声称已出网 |
| 配置的 OTLP HTTP/gRPC exporter | otel/src/config.rs | 显式遥测配置 | 未触发；动态地址，不归模型 provider 反代 |
| POST Sentry DSN 对应 /api/{project}/envelope/ | feedback/src/lib.rs:58；feedback/src/upload.rs；report_upload.rs | 崩溃诊断/用户反馈提交，受设置和用户操作影响 | 未触发；诊断附件不是推理请求，不作为等价化重放 |

## 范围与验收要求

1. 区分 provider 接口、账户后端、登录服务、插件/CDN、遥测、任意 MCP/工具网络。只有前者自然继承客户设置的 provider base_url。
2. 相同 HTTP 200、相同模型 slug、相同 effort 或相同 TLS 库都不足以证明回答语义等价；模型目录、请求上下文、工具命名空间、预热/previous_response_id 状态必须另比。
3. alpha/search 应以官方 SearchRequest/SearchResponse 原文、独立鉴权账号绑定、实际客户端 web.run 返回作为验收，不用“回复提到了联网”替代真实搜索。
4. 暂未触发的 guardian/memories/realtime/images/notes、登录及管理操作保留在清单；不得把普通文本/搜索用例通过总结成“所有官方行为全覆盖”。
5. 本文没有触发任何真实模型、登录、插件变更、邮件或发布请求。源码审查和本地合成 alpha 路由测试分别标识，原始凭据未进入文档。

## 2026-09-28 工作树补充

当前工作树已增加严格的 `backend-api/codex/realtime/*` 到 `/v1/realtime/*` alias，并以 `appServerRealtimePath` 校验 call-id；后端账号、任务、插件、额度固定路径也统一经过 delegated-owner allowlist。`CODEX_APP_SERVER_CLI=1` 可选启动 wiring 要求 native HTTP/WS helper 与 identity profile，但不启动官方 executable。

这些改动已通过当前 server 全套测试与 `go vet`；它们不把未触发的 Guardian、OAuth/device、agent identity、remote-control、telemetry、动态 MCP/plugin URL 或真实 provider 权益写成已验证。
