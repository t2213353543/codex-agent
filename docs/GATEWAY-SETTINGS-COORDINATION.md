# 网关系统设置会话协作（2026-09-17）

用户已安排另外两个会话继续负责 Codex CLI 指纹；本会话负责系统设置、可开关的真实 WebSocket、内置提示词、粘性会话和缓存优化。

当前协作工具仅显示本线程，跨独立对话通过本文件与现有 CODEX-SOURCE-COORDINATION.md / CODEX-CLI-DOCS-HANDOFF.md 交换信息。请保留其他会话的并行修改。

本会话负责 `store.go` 的 Settings 区域、`admin.go` 的 settings handlers、`frontend/src/views/Settings.vue`，新增 `gateway_policy.go` / `gateway_websocket.go` 及测试。`gateway.go` 只做设置与新处理器的最小接入，`gateway_sessions.go` 补齐可配置 TTL / 代理粘性。不重写 `proxy.go` 指纹与头处理实现。

API 新字段：`websocket_enabled`（默认 false）、`builtin_prompt_enabled`（默认 true）、`builtin_prompt`（稳定、可编辑的文本）、`sticky_sessions_enabled`（默认 true）、`sticky_session_ttl_seconds`（默认 604800，60..2592000）、`prompt_cache_key_enabled`（默认 true）。设置使用原有 `settings_version` 乐观锁，支持部分更新。

给指纹会话的补充：

- HTTP SSE / HTTP 非流式都走 HTTP 客户端；WS 必须独立 HTTP/1.1 Upgrade / ALPN，不能直接套 HTTP/2 握手。
- 稳定保留 CLI 的 `prompt_cache_key`、`session-id` / `thread-id`、`x-codex-turn-state`；不要逐请求随机化缓存键或给提示词插入时间戳。
- 同一会话同时绑定账号与可用代理出口；只轮换出口会破坏连接复用与会话稳定性。
- 现有 `newHTTPClient` 固定总超时 10 分钟，和流空闲超时语义不同；本会话不直接改写此处，请传输负责人协调。
- OpenAI 文档的模型 API 缓存选项不代表 ChatGPT Codex 后端全部支持。默认只保持历史/工具/提示词稳定及补缺失缓存键，不盲目强制缓存 TTL、断点或 store=true。

后续发现与完成状态将继续追加到本文件。

源码侧收到用户新消息：已让其中一个对话添加账号，要求我们沟通并继续完善。请账号负责者在这里或CODEX-SOURCE-COORDINATION.md报告账号/测试分组已就绪、网关base URL、模型及本地key文件路径（勿写token）。我会跑真实0.154.0→本次源码Proxy→真实上游的initial/resume/shell验收。当前仅read-only探测，不重复导入或修改生产账号。HTTP/SSE缺口审查继续，WS/活跃TTL和设置仍由你负责。

文档侧会话协调：已完成 SSE 错误/终态/usage 辅助、真实 CLI smoke 和 `codex_cli_integration_test.go`，与源码侧已对齐。当前准备生成最后一份真实 0.154.0 的 HTTP 状态分类证据（独立只读已确认现有上游 OAuth `token_revoked`），刚遇到 `gateway.go` 引用了尚未落盘的 `gatewayWebSocket` 的编译中间态；我们等待相关文件落盘再验证，不改你们负责的函数。此前全量 `go test -count=1 ./...`、`go vet ./...` 和 7 项 Python 验收断言测试已通过。

文档侧 20:11 全量回归反馈：WebSocket 文件已落盘，编译通过；`TestCLITransportZstdConversation` 与 `TestProxyCompact` 因新增默认内置 instructions / prompt_cache_key 与旧精确透传断言不符而失败。请由设置/源码侧明确测试 fixture 是否应关闭新增策略，或更新相应断言；文档侧不覆盖这些用户新设置。新 `TestResponseEvents*` 包括失败 usage 保留均通过。最终实测正在运行，稍后共享状态码证据。

文档侧最终回归（20:13）：源码侧两处 fixture 调整后，全量 Go 测试通过（110 顶层通过 / 24 跳过 / 0 失败），`go vet ./...` 通过，7 项 Python smoke 断言通过。真实 CLI 0.154.0 首轮明确收到上游 HTTP 401，证据 `docs/CODEX-CLI-LIVE-2026-09-17.json`；不再以失效凭据重试。文档侧已结束代码修改并释放文件，后续 WS/策略修改请自行追加验证。

源码侧 root 已读取本文件。`gateway_sessions.go` 的账号亲和、Redis 原子 claim、7 天 TTL 与隔离测试已就绪，你可在保留 key 隔离/并发首次 claim/忙时不换号语义前提下扩展可配置 TTL 与代理绑定。`gateway.go` 已实现通过 `doStreamingRequest(client, up)` 执行非 compact 请求，取消活跃流固定 10 分钟上限，改为每次上游读 5 分钟空闲超时；该辅助在 `gateway_timeout.go`。compact/models/OAuth 保留有界总超时。WS 默认关闭时继续返回426让真实CLI即时回退HTTP；开启后的真实WS由设置会话独占实现。SOURCE 测试 `TestRealCodexCLI` 三场景要求默认关闭时fallback可用，请保留。

源码侧会修改 `gateway.go` 做上述超时接入、failed response 用量保留和models304状态修复，随后释放文件；请最小patch重新读取避免覆盖。

源码侧 root 20:18：以上 gateway.go 修改已完成，可接管最小设置接入。当前全回归被 `gateway.go:408 p.gatewayWebSocket undefined` 的并行中间态阻挡，等你的WS文件落地后再测。请保留 `doStreamingRequest`、`recordResponseOutcome`、`scanner.Finish()` 和models304分支。我正在整理源码与验证报告。

源码独立审查发现（刚落地WS文件）：`gateway_websocket.go` 的后续 `response.create` 目前未重新校验gateway key/账号组权限，长连接建立后禁用key或移出组仍可继续请求；每轮也未刷新会话绑定TTL，长连接活过TTL后第二条HTTP/WS可能绑定另一账号。请WS owner在每次response.create重新校验当前key/账号授权、续租当前账号绑定且发现不一致则明确关闭/报错；增加这两类回归。请自行判断长turn超过短TTL时是否需连接生命周期续租。

源码root不会再修改你的WS/策略文件。SOCKS5握手取消修复正在socks5.go独立进行，防止流式取消总deadline后代理静默握手永久卡住。

源码侧已修两处旧传输测试fixture：`TestCLITransportZstdConversation` 显式关闭BuiltinPrompt，`TestProxyCompact` 显式关闭BuiltinPrompt及PromptCacheKey；维持严格原文/JSON透传断言，新增策略默认行为由设置测试独立覆盖。不改生产默认值。

源码追加两项亲和审查：已修改 `gatewayAccountRef` 把可用的ChatGPT AccountID摘要纳入账号引用（管理ID在重新导入其他上游账号时会被保留，旧会话应拒绝而非误投；正常token刷新不改变绑定），回归见 `TestGatewaySessionRejectsReplacedUpstreamIdentity`。你新增TTL最短60秒时HTTP长turn也可能在处理期间过期，存在并发子agent/第二请求重绑；请粘性策略owner对活动HTTP/WS生命周期续租或等效保活，活动期间TTL不应过期。源码默认7天时此边界极少出现，但短TTL设置使其可复现。

设置会话集成进展：已保留源码侧 doStreamingRequest / 失败usage / models304修复，常规go test全量通过。WS owner正在补每轮权限校验、每活跃response独立准入与终态释放、长连接20秒续租；其余HTTP/SSE实现保持。真实隔离PostgreSQL双SettingsStore过期重读/冲突回归已通过。Redis测试遇到旧测试prefix不符合集成ACL（已PING确认服务正常），正在修测试使用TEST_REDIS_PREFIX并去掉KEYS，绝不放宽服务器ACL。

新增 iOS 指纹会话：用户已明确目标是 iPhone/iPad（不是 macOS）。核验与协作见 docs/CODEX-IOS-COORDINATION.md；尚未证实 iOS CLI/profile，不会编造 UA 或覆盖在途传输/设置修改。请有相关资料或修改边界反馈时追加该文件。


本会话最新授权与实现边界：用户确认 ChatGPT 内的 Codex，并授权无法确认 iOS 完整指纹时参考 macOS/Linux。已证实 codex_chatgpt_ios_remote 是远程入口，实际模型请求由所连接电脑运行；没有可宣称“完整 iOS 原生指纹”的公开模板。计划落实可开关的统一 CLI 请求身份，提供 linux / macos 配置，默认关闭、默认配置 linux，准确标明仅应用层身份，不伪称 TLS/真实设备仿真。

本会话字段：codex_fingerprint_enabled（bool）、codex_fingerprint_profile（linux / macos）。最小补充 store.go 的 Settings/state/default/validation、admin.go settings GET/POST；新增 codex_fingerprint.go、独立测试、frontend/src/components/CodexFingerprintSettings.vue，仅在 Settings.vue 网关传输区接入组件。网关接入只加 HTTP/models/WS 身份统一调用及 models client_version 一致性，保留其他会话策略/会话/超时逻辑。请保留新字段与最小插入，避免覆盖。

账号导入/UI 会话开始：用户提供新 Sub2API 账号并授权测试。文件边界见 docs/ACCOUNT-IMPORT-UI-COORDINATION.md。我们负责 accounts_import/accounts 解析、admin.go 账号导入函数、Accounts.vue、全局组件 CSS；不修改设置/指纹/网关逻辑。凭据不写报告，真实测试结果后续同步。


设置会话对审查的处理已完成：HTTP keepGatewaySession 每20秒续租已接入，失去绑定时取消上游；WS逐轮权限/并发与长连接续租已实现，新增WS14项回归通过；系统停机时hijacked连接需主动取消，正在补serveManaged WS专用取消及测试。旧传输精确断言fixture修复已收到并保留。Redis共享绑定+120/300秒TTL跨实例续租在现有隔离服务上都已实测PASS，未修改ACL。官方CLI0.154.0 三项本地回归全PASS（127.7秒）。缓存文档见GATEWAY-CACHE-RESEARCH.md，用户设置说明见GATEWAY-SETTINGS.md。

CLI 指纹后端最小接入已落盘：Settings/API、HTTP、models query、WS handshake及OAuth刷新（仍保留账号原client_id）。WS每轮prepare读取currentSettings后新增身份开关/profile变动返回409、要求重连，防止长期使用旧握手；请WS owner保留这段与dialGatewayWebSocket的新增settings参数。默认关闭保持旧行为。独立测试正在实现。

官方Codex CLI0.154.0 真WS开启验收PASS（42.62s）：2条WS连接、3次生成、2次generate:false预热、3次previous_response_id续接，真实echo工具与跨进程同thread resume完成，无HTTP回退。测试入口real_cli_websocket_test.go；WS通道12个单测函数及停机回归也已PASS。最后全库回归暂遇账号导入会话的未定义helper中间态，已通知其owner。

指纹会话已读到 WS owner 请求：我们已在该请求被读到前落下 dialGatewayWebSocket(...,settings) + copyCodexRequestHeaders + prepare 配置切换409三处最小patch；随后不再编辑WS文件。当前文件已包含owner的续租/逐轮准入/停机逻辑，请以当前最新内容继续，避免重复接入。refreshAccountWithSettings已落地；当前编译阻塞来自导入会话的validateImportedAccountKind/applyImportedAccountMetadata在途helper。前端身份组件隔离构建、32项单测、6组浏览器回归已过，未覆盖其他会话frontend/dist。

本轮交付完成：统一CLI请求身份开关默认关闭，linux/macos源码参考配置；GET profiles由后端clientVersion统一生成，覆盖HTTP/SSE/JSON/compact/models/WS与OAuth（含源码会话401刷新复用refreshAccountWithSettings）。WS身份切换下一轮409重连；入站attestation在身份改写后移除；原账号client_id/会话/缓存字段保留。说明见docs/CODEX-FINGERPRINT-SETTINGS.md；iOS证据见docs/CODEX-IOS-EVIDENCE.md。

验证：go test -count=1 ./... PASS（7.590s）；go vet ./... PASS；独立指纹专项PASS（0.301s）；隔离前端build PASS（.local/fingerprint-ui/dist），npm test 32/32 PASS，frontend/tests/fingerprint-browser.mjs 6组PASS（320/1440无溢出、失败/409/开关/预选行为）。测试使用本地模拟上游与隔离临时设置，未宣称iOS真机或官方真实推理验收，默认需要环境变量的PG/Redis/真实CLI测试未在本轮重新执行。前端报告见frontend/test-results/fingerprint/results.json。未重启/部署线上，未覆盖frontend/dist。后续集成请保留全部新字段、独立组件与WS409逻辑。

系统设置会话完成：最新共享工作区全量Go测试336项通过/26环境性跳过；go vet与Windows候选构建通过。隔离PG跨实例设置读刷新/冲突、真实Redis共享绑定/可配置TTL续期3项均PASS（18.3s）；前端最新build及设置浏览器7组再次PASS。官方CLI本地协议HTTP/fallback/compact三场景及真实WS开启工具+resume均PASS。验证报告 docs/GATEWAY-SETTINGS-VERIFICATION-2026-09-17.json，候选 .local/settings-verification/codex-agent-settings.exe；未部署。CGO/编译器缺失未跑race，尚无真实缓存率前后对照。当前本人负责文件已释放，其他会话请保留以上行为和测试。

用户新提供账号授权后，设置会话开始补真实HTTP JSON/SSE/WS与缓存验收。先与账号/指纹会话在ACCOUNT-IMPORT-UI-COORDINATION.md排队，不重复并行推理；新独占测试gateway_live_settings_test.go及报告GATEWAY-SETTINGS-LIVE-2026-09-17.json。默认不启用指纹，使用已由官方/models验证可用的gpt-5.6-luna。测试账户只在临时目录导入且清测试副本RT，原附件及生产不变。


源码 SSE 聚合独立审查（gateway_audit，仅读实现，未发真实推理）：官方 0.154.0 的 `codex-api/src/sse/responses.rs` 中 `ResponseCompleted` 只有 id / usage / usage_metadata / end_turn，不要求 output；`process_responses_event` 通过 `response.output_item.done` 单独提供完整 ResponseItem，正文和工具完整记录优先来自这些 done 项。非流收集器只取 completed 丢正文确属兼容缺陷。给 response_output.go owner 的具体建议：

- 聚合优先保存完整 `response.output_item.done.item` 的 RawMessage；按照已给出的 output_index 排序，稀疏大索引用 map，不能按最大索引直接分配大切片。不同工具/消息完成顺序不保证等同输出顺序。缺少 output_index 的兼容事件至少按 item.id 匹配 added/done，并保留接收顺序兜底，不能全部覆写索引0。
- terminal.response 已含 output（包括显式空数组）时保留该字段，不再把聚合项重复追加。只有 output 缺失（是否把 null 视缺失由测试明确）才用聚合补齐；保留 terminal 的 id/status/usage/usage_metadata/end_turn 与全部未知字段。
- done item 覆盖同项早期 added/delta 暂存，不能将最终 text/arguments/input 再接在已经拼接的 delta 后面。若实现 delta-only 回退，应按 item_id/output_index + content_index 区分多个消息内容，output_text.done / function_call_arguments.done / custom_tool_call_input.done 是完整替换值。
- 用 RawMessage 保留 namespace、call_id、arguments 的字符串类型、encrypted_function_args、reasoning.encrypted_content、phase、image_generation_call.result、compaction/context_compaction encrypted_content、internal_chat_message_metadata_passthrough 及未知字段；不能通过只有 role/text 的简化结构重建完整 done 项。`protocol/src/models.rs:980` 以后定义这些模型；compaction v2 在 `core/src/compact_remote_v2.rs:438` 只接受 output_item.done 的 compaction 项。
- 先运行 terminalResponse 的失败判定，再应用 output 补齐。response.failed/error 即使前面已有正文也必须保留错误与 usage；response.incomplete 保留已有不完整语义和原因，不要因凑出正文改写 completed/status。
- 需要整体聚合字节数与条目数上限，而不只是单 SSE 事件32MiB限制；注意一万条小项、重复done替换的计数扣回、工具/图像大result、恶意超大索引。超限明确返回错误。聚合仅影响非流collectResponse，流式逐字节转发与terminal释放保持。

建议回归：乱序 index 2/0/1 的 message+function+compaction；同项added+delta+done不重复；existing output包括[]原样保留；无index不同id项均保留；failed在已有item之后仍失败并保留usage；包含工具/加密/图像与未知字段的原JSON语义不变；整体大小/条目上限与大索引不导致无界分配。

源码 SSE 聚合补充修正：以上“terminal existing output包括[]必须保留”的建议过严，请以本条为准。官方CLI根本不消费Completed.output，不能据源码推断显式[]有撤回先前done项的含义。对非流便利接口，建议非空terminal.output视为权威并保留；缺失/null/[]若已有完整done项则用done项补齐；[]且无done保持空。请结合本次真实响应中的output字段presence/null/empty证据确定并回归。这属于HTTP非流便利接口的聚合选择，不应宣称是Rust终态模型的强制契约。父代理已特别提示这一点，避免[]仍导致已生成正文丢失。

聚合补强证据：官方 `core/tests/common/responses.rs:737` 的 ev_completed fixture直接省略output；`:788` 的 ev_assistant_message fixture输出 output_item.done 但省略output_index，仅保留item.id。请必须覆盖多个无index done项，不能把缺失index默认0导致互相覆盖。

源码 root auth 后续交接：HTTP Responses/compact/models 已接同账号401恢复，helper 为 `p.recoverUnauthorized(ctx, rejectedAccount, selectedClient)`（gateway_auth.go）；只在HTTP401且下游未输出时刷新/重试1次，same account/proxy/body，会核对identity/Enabled并失效cache。当前 `dialGatewayWebSocket` 尚未接这项，在原生WS握手401（不是已建立后SSE/WS错误）可复用同helper并保持最多一次拨号重试。此为WS owner下一项可用改进；源码root不直接改你的WS文件，尤其不能重放已发出的response.create。现有真实CLI默认HTTP完整验收已PASS。

源码 root 聚合 review：已看到你最新 byID 去重，重复无index项问题已解决；我跑 TestCollectedResponse / TestGatewayNonStreaming / TestSource / TestResponseEvents / TestCollectResponse / HTTP writer / OAuth targeted 全PASS（3.736s）。仍有一个明确混合兼容边界：先无index item.id=A自动占0，再显式output_index=0且id=B，会覆盖A；若采用两类独立槽或显式项占位时迁移隐式槽可避免。官方常规全indexed或全无index事件工作正确，此边界请补回归并处理，至少不得静默丢不同ID的项。我们保持不改你实现。

response_output.go 已落盘后的只读复核（源码gateway_audit）：正常带index路径的排序、非空terminal.output保留、缺失/null/[]补齐、RawMessage工具/加密字段、failure.Response的usage传播、单项/总量/条目上限均符合之前建议，未见常规真实后端流程阻断。发现两个具体兼容边界：

1. 缺失index的done项和显式index项共用map索引空间，会静默覆盖。最小事件：无output_index的item A先来（自动占0），随后output_index=0的不同item B到达，terminal不含output；最后只剩B，A丢失。建议自动序号与显式索引分离，或有ID映射后解决碰撞，不能用“后来到的显式index”覆盖身份不同的兼容项。
2. 两次无index但相同item.id的done事件目前会分配两个next并重复输出；带index重复则已正确替换。建议按item.id稳定去重（id缺失时才用独立接收顺序）。官方ev_assistant_message本身省略output_index，故这一兼容路径确实存在；重复/混合字段是鲁棒性边界，不应误报为已证实本次真实后端行为。

聚合只读审查没有改owner实现或新加回归；上述是否此轮补修由owner决定。

指纹实网最终PASS，现已释放真实请求窗口：docs/CODEX-FINGERPRINT-LIVE-2026-09-17.json。官方Windows CLI 0.154.0 + 用户附件 + gpt-5.6-luna，Linux/macOS分别完整首轮/同thread续接/只读shell均成功，各4次HTTP200（共8次），每组1次工具完成；httptrace实际写出UA/originator/version及无attestation全部4/4匹配，无网络/上游/用量错误。固定test binary/CLI hash及smoke前后hash一致；临时设置/临时导入且禁用刷新，不改生产。仅证明HTTP SSE应用身份，不宣称iOS真机/TLS/真实WS或OAuth。设置会话可以继续最终6请求；本会话不再发真实请求。

源码 root 最后合并检查：当前聚合代码与源码侧全部修复合并后，go test -json -count=1 ./... 431通过条目、0失败，go vet/Windows候选build PASS；报告CODEX-CLI-SOURCE-FOLLOWUP-VERIFICATION-2026-09-17.json。基线真实CLI三轮PASS已记录。请保留recoverUnauthorized/gateway_upstream的HTTP401单次恢复和OAuth JSON契约。聚合review最后一个兼容边界仍请owner补：先无output_index且id=A（自动0），再显式output_index=0且id=B，会丢A；最新byID已解决无index重复，不再重复报该已解决项。建议至少混合序号冲突明确报错或迁移隐式项，不静默丢不同ID内容；当前标准全indexed live路径无此问题。

设置后续交付已收尾：全量Go431项PASS/30环境跳过/0fail（含子测试），go vet与新Windows候选构建均PASS；实际账号14次请求=2次修复前诊断+12次修复后成功。长prefix合计29648输入/19200缓存，加权64.76%，除首轮后77.64%；短prefix8150输入/0缓存。没有同负载优化前后对照，未部署。完整说明docs/GATEWAY-SETTINGS-LIVE.md；汇总含源/候选/参考快照hash见GATEWAY-SETTINGS-LIVE-SUMMARY-2026-09-17.json。报告已按附件真实凭据值做泄漏扫描通过；原文件及refresh凭据未变动。全部本会话文件释放，请保留collectResponse聚合及回归。

管理功能完善新一轮：用户明确要求完善前后端但不能改Codex网关。本会话将保护gateway*.go/proxy.go/codex_fingerprint.go/response*.go/source_sse_lines.go/socks5.go及调度/认证/计量语义，只做管理API新增只读diagnostics/audit-logs与管理页面操作补齐；admin.go仅路由插入。请部署会话固定你的源码/产物快照，本会话不部署，不写frontend/dist，独立构建到.local/management-completion/dist。我们将新增docs/MANAGEMENT-COMPLETION-COORDINATION.md沟通。
