# Q01：请求语义、CLI 条件分支与账号质量归因

检查日期：2026-09-24。对象为当前工作区源码和 `.local/codex-source-0.155.0`，不是已部署进程。本文未读取账户令牌、未改变生产账户、未调用远端模型。官方源码引用均指该固定本地版本；运行时配置和多账号糖果题由主任务另行取证。

## 当前结论

没有发现网关自动把 model、reasoning.effort 或 service_tier 改成更低档值的代码。新增 loopback HTTP/WS 抓包回归证明，显式 xhigh/priority、opaque reasoning/compaction、工具历史、输出格式、大整数和客户端 metadata 在本次覆盖的链路中保留；指纹开关不改变这些正文控制。此结果不证明所有生产请求无问题，也不证明服务端实际执行了指定推理预算。

发现四个应优先分离的差异：默认 prepend 提示词、原生 CLI 与 custom provider 在进入网关前的正文差异、缺失的 routing hint，以及 native helper 进程级基础设施路由 cookie 在多账号之间共享。前三项是明确代码行为；最后一项同时是官方库行为与网关多账号架构带来的作用域差异。尚无证据证明任何一项导致“账号被标记后路由到较弱算力集群”。

答错糖果题是质量回归信号，可以触发抓包与配对复测；它不能单独识别“账号标记”“集群算力”或“网关缺陷”。不要在题目、instructions 或期望解析器中注入 21 来取得通过结果。

## 已检查边界

| 边界 | CLI 0.155.0 实际行为 | 当前网关行为 | 对质量分析的含义 |
|---|---|---|---|
| model | `core/src/client.rs:871` 使用解析后的 `model_info.slug` | `proxy.go:105` 验证非空，RawMessage 保留；不解析别名，不自动降级 | 对比 ingress、sent、returned 三个值；客户端展示名称不能代替 sent model |
| reasoning.effort | `core/src/client.rs:763` 取显式 effort 或 model catalog 默认，再 resolve | policy 和 prepareBody 不填默认，也不降档；absent/null/none/low/xhigh 均保留 | 无 effort 的手写题目请求不等同原生 CLI 请求；必须固定实际发送值 |
| reasoning.summary/context | CLI 按模型能力决定 summary；Responses Lite 设置 all_turns | 原样保留完整 reasoning 对象 | 不能只比 effort 而遗漏 context 或 Lite 模式 |
| service_tier | `protocol/src/openai_models.rs:924` 删除显式 default 和 catalog 不支持的档位 | 原样保留，不做 catalog 判定 | “传 priority”不证明上游使用该档位；优先采集 response 元数据 |
| instructions | CLI 取 prompt.base_instructions，Lite 转入 developer input | `store.go:638` 默认启用内置提示词并 prepend；`gateway_policy.go:35` 对显式 instructions 也追加前缀 | 默认配置确实改变模型输入，是必须关闭后再比较的变量；先读部署实际设置 |
| input 内容与顺序 | CLI 格式化历史；非 OpenAI provider 删除内部消息 metadata 与 encrypted_function_args（`client.rs:831`） | RawMessage 保存输入，兼容模式把 system 改 developer；不删除 encrypted history | 客户端 custom provider 在 ingress 前已丢弃的字段，网关无法恢复；多轮对比要抓客户端出站 |
| tools/text/数字 | CLI 按模型能力选择 tools 和 verbosity/schema | RawMessage 保留；未走 float64 重编码 | 新增 wire 测试含 9007199254740993/9007199254740995，防止数字精度损失影响工具约束 |
| store/stream | CLI 常规 HTTP 请求 store=false、stream=true | HTTP 强制二者与 CLI 相同；WS 保留调用者字段 | HTTP JSON 输出是网关聚合 SSE；对比需确认完整终态，不使用半段 delta 判分 |
| cache key | `client.rs:497` 默认为 session_id，内部子任务可用 source:parent_thread_id | 显式 key 保留；缺失时按 key+session 或静态前缀生成摘要 | 生成 cache key 不是会话 ID；不把它当“降智标记”；同题不同账号需固定策略 |
| session/account/proxy | 原生单账号客户端的会话自然使用该 auth | 显式会话按 API key+session 绑定 owner；多代理按 session rendezvous；无 session 时代理轮换 | 同账号多次测试也可能换出口；要记录最终 account 与 proxy，不能只记录推送目标 |
| previous_response_id/turn state | WS 续写及当前 turn 状态在官方 client 中传递 | HTTP/WS 保留；WS 连接固定账号，opaque turn-state 转发 | 质量单题最好新 session；多轮质量另设专项，不混用别人的 previous_response_id |
| routing hint | `client.rs:1023` 在 uses_codex_backend 时生成 `model=<model>[;tier=<tier>]`；HTTP `:1547`、WS `:1701` 使用 | `proxy.go:144` 允许透传；当前不存在从 prepared body 补齐的逻辑 | 原生 ChatGPT CLI 与 custom provider/header-less 请求有明确头差异；缺少该头是否影响质量待实测 |
| user-agent/originator/version | 官方 default_client 根据进程、OS、终端生成 | 原生启发式保留；普通 SDK 请求生成缩略 CLI 标识；开启 profile 时覆盖三者 | profile 是确定性 header 配置，不是完整设备身份；改 profile 不等于更换 TLS 栈 |
| attestation | `client.rs:732` 只有 supports_attestation 且 host provider 提供值才附加，源码不提供通用生成器 | 保留未改身份的 opaque 值；指纹重写或补身份时删除 | 缺失不自动证明非官方请求；不能伪造/静态复制一条 proof 来解释质量 |
| ingress Cookie | CLI HTTP 内部 cookie store 管理基础设施 cookies | 头白名单不转发客户端 Cookie，也不把下游登录态交给上游账号 | 排除普通 ingress Cookie 跨账号的直接通道 |
| native cookie 作用域 | `http-client/src/chatgpt_cloudflare_cookies.rs:9` 明确声明 process-global、可跨 auth；只允许 CF 基础设施及 __oailb | 同源、同代理、同 timeout 共用 native helper；key 不含账号；cli_request 重建 HTTP client 仍共用静态 jar | __oailb 是路由 cookie，源码不足以说明算力分档；隔离账号/重启 helper 是值得实验的路由变量，不是已证根因 |
| HTTP/WS cookie 差异 | websocket-client 无 HTTP cookie jar 注入 | HTTP helper 自动 cookie，WS connector 无该注入 | HTTP 与 WS 结果差异可能混合传输、连接和 cookie 状态；要分层实验 |
| 压缩 | CLI ChatGPT auth 请求按自身条件选择 zstd | 仅保留客户端已选择的 zstd；非压缩入口不强制压缩 | TLS/压缩字节不同不等于语义不同；比较解压后完整 JSON 同时记录 wire 特征 |
| compact | CLI 0.155 用 Responses 流式 compaction_trigger | backend-api/codex 的旧 /compact 转换成 trigger，保留输入，再返回 compact 对象 | 首轮题目不涉及该分支；长会话退化不能用首轮通过排除 |
| 401/retry | auth 恢复可重建请求但不换用户 | `gateway_auth.go` 保持 owner、proxy，拒绝身份变化；WS 仅握手未发送时恢复 | 记录 attempts 和 refresh；不要把不同 owner 的请求算成配对样本 |
| 观测可信度 | 返回的模型与 reasoning metadata 才是服务端回显 | `gateway_observation.go` 已区分 RequestedReasoningEffort/ReasoningEffortSource 及 model 来源 | 服务端回显仍不是内部硬件证据；有 reasoning_tokens 才可对预算执行作有限判断 |

## 本轮新增回归及结果

`gateway_quality_q01_test.go` 新增 3 个顶级测试，HTTP 和 WS 各有指纹开/关子例：

- `TestQualityQ01HTTPWirePreservesReasoningAndHistory`：真正经过 ServeHTTP 到 loopback upstream 的最终正文；同时检查 routing hint、turn state 和 ingress cookie 隔离。
- `TestQualityQ01WebSocketWirePreservesReasoningAndHistory`：真正经过 WS frame 转发到 upstream 的最终正文；覆盖 xhigh/priority 和多类型 opaque 历史。
- `TestQualityQ01EffortAndTierAreNeverInvented`：覆盖 missing/null/none/low/xhigh 与 flex/default/priority，不允许网关凭空补 effort/tier。

比较使用 UseNumber 的 canonical JSON，故不会用 float64 近似掩盖大整数变化。合成模型和 opaque 数据仅用于本地传输检查，不声称会被远端模型接受。此回归专门关闭 BuiltinPromptEnabled，保留其余默认兼容/cache 设置；不掩盖默认提示词差异。

2026-09-24 执行通过：

```powershell
go test ./... -run '^TestQualityQ01' -count=1 -timeout=90s
```

结果：`ok codex-agent 0.087s`；native-transport/tests 无测试文件。这不是生产 TLS 或模型质量验证。

## 最有鉴别力的实测顺序

1. 固定一份未经答案提示的糖果题原文及 SHA-256，明确模型 ID、effort、summary、tier、instructions、输出解析规则。把异常答案保留为结果，不把非 21 都归类为某个根因。
2. 每个账号独立建立新 session、同一个已知出口、同一 model/effort/body；先比较原生 CLI 直连、相同请求通过网关。采集 ingress JSON 与 final upstream JSON 的字段差异、最终 request headers 名单和非敏感值、request ID、response model/tier/reasoning/source、reasoning tokens、完整终态、出口、候选 binary/hash。
3. 按账号配对、交错顺序运行，避免“先正常后异常”与时段或集群漂移混淆。先小批量发现边界，再对有差异路径增加重复；单次通过不能证明账号恢复，单次失败不能证明网关导致。
4. 若同账号原生直连也失败，优先验证题目、模型设置、账号与出口；若只经网关失败，先清零默认 prompt 变量，再比较 routing hint；每次仅改一个因素。
5. 对 HTTP native cookie 另建独立 helper/同账号重复基线，与共享 helper 按 A→B→A 和 B→A→B 顺序对比；只记录 cookie 名和摘要，不能输出原始值。必须固定 proxy 和 native lifecycle，重建 client 本身并不会清空静态 jar。
6. HTTP 与 WS 分开取得完整闭环，再测试跨协议；固定指纹关闭与 CLI 原始身份作为基线，确定性 profile 开启作为实验组，不同时更换 UA、出口、TLS、prompt 与 cache。
7. 增加若干独立且可判定的题目与同义变体，保留糖果题原本判据。单题用来复现，题组用来验证修复是否改善整体质量，避免对一个固定答案过拟合。

## 修复取舍

- 请求字段已保留的部分继续保持透明，不强制所有调用者 high/xhigh 或 priority。这会改变用户请求、费用/时延及对照条件，也不能证明“满血集群”。
- 默认提示词属于既有产品策略；应在诊断对照中显式禁用并记录其实际值摘要。是否调整默认值由主任务结合生产配置与产品预期决定，不能把改完后的通过结果与旧 baseline 混算。
- routing hint 是最便宜的源码一致性补齐候选。应只从实际 prepared model/tier 生成，限定 Codex backend Responses；WS 需处理预热时尚无正文、连接级 hint 与后续变更不一致的问题；未知/不支持 provider 不宜随意加头。
- cookie 隔离值得作为网关多账号健壮性改进，但不要声称官方禁止共享或共享一定导致降智；官方恰好明确允许基础设施 cookie process-global。若要隔离应覆盖 HTTP、WS、OAuth route/pool 及刷新后 owner 连续性，不能只换 UA。
- attestation 是 host integration 提供的 opaque 边界，不能从 CLI 开源正文推导有效 proof。保留有来源的值，避免身份重写后携带不匹配 proof；缺失证据应写明，而非生成假值。

尚待主任务给出运行中网关配置、28765 原始题目和多账号实测。本文的源码、合成回归证据不足以宣布原始目标完成。

## Q01 第二专项：28765 CPA 桥凭据状态映射修复

这一缺陷影响失效 AT 的状态同步，与糖果题非 21 的根因分别记录。修改文件为 `deploy/loophub_codex_bridge.py`，没有修改 28765 源码、数据库、推送配置或生产进程。

### 已复现的旧行为

旧 `Bridge.list_files` 只检查 enabled、mapping.initialized、本地 mapping.expires_at 和 group。即使当前 agent summary 已 status=error，或当前凭据已被只读 upstream snapshot 确证返回 401，只要本地记录的 exp 在未来，桥仍返回 `status=active, disabled=false, unavailable=false`，而且没有 `needs_reauth`。反向也会发生：本地 mapping 过期但当前 agent AT 已换新时，桥仍报 error。

已读取的真实契约：

- `accounts_pg.go:415`：summary error 同时包括 credential error 和任意最近请求 error。因此 429、容量不足、网络失败都可能让 summary=error；这不是凭据失效的充分证据。
- `admin_extra.go:168`：GET account summary 不包含 token fingerprint 或 credential_version。它提供当前 expires_at、credential_status、LastError 和派生 Status。
- `admin_account_upstream.go:61`：GET `/api/accounts/{id}/upstream` 只读缓存，不触发模型/额度探测。PostgreSQL 的 stale 比较当前 credential_version；文件存储模式保守比较 account.Version。快照超过 300 秒也会标记 stale。不能把 metadata version 误当成 PG credential_version。
- `loophub-mother-checkout-20260920/webui/team_rotation.py:2761`：`_monitor_cpa_repair_reason` 读取远端 auth-file，将 status_message 转作 last_error，然后调用 `sub2api_oauth_repair_reason`。后者在 `:1151` 直接接受 `needs_reauth=true`，随后还有宽文本匹配。因此桥不能把任意 upstream message 原样送入 status_message。

### 修复后的行为

- 当前 agent `expires_at` 替代本地 mapping expiry 作为有效期依据；未知/缺失 expiry 不声称 active，也不假定凭据过期。
- 普通 summary error / credential error 只表示 unavailable，不自动要求刷新。
- `needs_reauth=true` 只来自当前已过期时间、精确允许的 token failure code、严格结构化 HTTP 401，或当前凭据的非 stale 快照。自由文本 message/output/reason 里的 `token_revoked`、`invalid`、`401` 等均不参与判定。
- 结构化 429/403/5xx 不因嵌套 message 或 code 命中而变成凭据失效。输出 status_message 仅使用固定代码；不输出上游自由文本、令牌或凭据信息。
- 快照需匹配 account ID，显式 `stale=false`，正 credential_version 和合理 checked_at。更新的成功快照覆盖旧拒绝；相同秒的成功/拒绝矛盾视作无确证。
- 将快照 401 反馈给源前，再读 account version；并发更换凭据/修改记录时取消旧失败归因，回报 observation unavailable。身份 tag/name/RT 所有权变化继续抛出冲突，不被辅助读取错误吞掉。
- GET upstream 缓存不可用时只报 observation unavailable，不请求 OAuth refresh，不调用 upstream POST/test-stream，不操作源。

### 回归证据

1. `tools/test_loophub_bridge_quality.py`：13 个 Python 测试通过。覆盖旧 active 误报复现、当前 expiry 替代本地 expiry、精确码/结构化401、429/403/网络/容量错误、自由文本伪信号、stale/错账号/坏版本/未来时间快照、成功覆盖旧401、相同秒矛盾、并发替换、RT 所有权变化、辅助读取失败、withdrawn/filename过滤。
2. `loophub_bridge_q01_test.go` + `tools/loophub_bridge_contract_q01.py`：当前 Go 管理 API → 真正 loopback HTTP → Python urllib → 修改后的桥完整契约，共 5 子场景通过。覆盖 fresh401、stale401、429、过期/换新 metadata，并在 Go HTTP handler 强制只允许两个 GET 路径。合成快照直接写临时管理状态，不触发任何外部请求。
3. 既有 `integration/gateway_environment/bridge_smoke.py` 18 项通过；它属于 workbench 桥，只作为邻近契约回归，不能冒充 Loophub 专项验证。
4. 将未修改的 28765 `sub2api_oauth_repair_reason` AST 函数单独载入，6 种桥输出信号全部符合预期：expired/401/revoked 可触发修复，destination unavailable/observation unavailable/空消息不触发修复。未导入整个服务、未写源文件。
5. Python compile 通过；Q01 前一专项 HTTP/WS 质量字段回归与本专项 Go 契约一起复验通过，`ok codex-agent 0.625s`。

复验命令：

```powershell
python tools/test_loophub_bridge_quality.py
go test ./... -run '^(TestLoophubBridgeQ01RealManagementContract|TestQualityQ01)' -count=1 -timeout=90s
```

桥候选 SHA-256：`c880b9e4cbed0b669b0899f80b74fa028c8be84b766d6d89555a023ed28bfce8`。本专项已准备好供主任务审核后更新生产桥，尚未部署。

### 保留的证据限制

业务请求 401 不一定写入 account.LastError，业务日志也没有可直接关联当前 credential_version 的字段。此次桥不拿“最近任意 error”猜测凭据失效；若不存在可信当前快照，summary error 会变成 unavailable，但不会 needs_reauth。若要求每次业务 401 都能自动触发修复，应由网关提供与当前 credential_version 关联的结构化状态，不能用旧请求文本或模糊词匹配补洞。

summary 不暴露 token fingerprint，bridge mapping 中的 token_sha256 是上次源推送的 checkpoint，不是对 destination 当前 AT 的证明。外部操作者保留同 tag/name 且替换为另一个 AT 时，旧桥 upload 的同-token shortcut 无法仅凭 summary 检测该漂移。本次未为比较 hash 导出凭据、未扩张管理 API，也未擅自用源旧 AT 覆盖外部更新；现已避免使用旧 mapping expiry，快照判断依据网关当前凭据 stale 状态。进一步强一致同步需要受版本保护的非敏感 credential metadata 契约。

## Q01 第三专项：补齐缺失的 Codex routing hint

这是固定 CLI 源码支持的请求一致性修复，不是“提高智力”的结论。主任务实测仍使用冻结二进制，本节修改仅进入本地候选源码。

### 源码与边界决策

- CLI `core/src/client.rs:1023` 按实际 request.model / request.service_tier 构建 `model=<model>[;tier=<tier>]`；HTTP `:1547` 和 WS `:1701` 使用该值。
- CLI `model-provider-info/src/lib.rs:351` 默认 OAuth 等 ChatGPT auth 使用 `https://chatgpt.com/backend-api/codex`，但内置 provider 允许 base_url override；`supports_codex_backend_routes:530` 甚至只检查路径后缀。网关不具备 CLI 内置 provider 的身份条件，不能把相同路径的任意自定义域名当成 ChatGPT。因此本次生成范围有意比 CLI override 更窄。
- CLI `core/src/client.rs:1332` 支持请求正文形成前的 WS preconnect；`:1396` 只因 endpoint 变化、连接关闭或 owner generation 变化重连，不因后续 model / tier 变化重连。网关继续遵守这一连接复用边界。

### 本地实现

新增 `gateway_routing_hint.go`，HTTP 集成于 `gateway.go:730`，WS 集成于 `gateway_websocket.go:75`：

1. 仅 HTTPS、精确 `chatgpt.com`（主机大小写等价）、无端口或 443、规范 `/backend-api/codex[/]`、无 userinfo/query/fragment，并且已选中账号同时有 AT 和 AccountID 时考虑生成。相似域名、子域、HTTP、其他端口、转义路径、自定义 API 上游均不生成。
2. 仅原始路由 `/responses`。不对 `/guardian`、`/guardian-classifier`、`/models` 或旧 `/responses/compact` 生成。即使旧 compact 经适配器转成上游 `/responses`，也按原路由保持本次兼容行为。原生 CLI 通过 `/responses` 发送 compaction_trigger 自然属于 Responses 范围。
3. 从最终 prepared JSON 读取 model / service_tier。缺失/null tier 只生成 model；显式 tier 按原值生成，不过滤 default，不替换为 priority，不改 reasoning。
4. 只为非空、长度有界、无空白/控制字符/非 ASCII/`;`/`=` 的 model 和 tier 生成可表示的头。不满足时跳过补齐，继续原请求，不新增 HTTP 400，不改变 upstream 对 body 的原有校验权。
5. 已有 hint 会检查已知 model/tier 是否匹配 prepared body，但匹配、冲突、重复字段、空值和未知扩展都保留原值，不重写、不拒绝。helper 返回有限状态（如 generated / preserved_match / preserved_unmatched / deferred）供回归诊断；当前未扩大 RequestRecord schema，状态没有被持久化为新遥测字段。
6. WS body-first 的首次 upstream handshake 已有 prepared frame，能够补齐缺失 hint。已有 Session-Id 的提前 handshake 仍保持 preconnect 行为：正文未知，不臆造 model，也不延迟握手丢失下游 101 中的 upstream turn-state。后续 model/tier 变化继续复用连接，不重写握手、不重放请求。

### 验证

新增 `gateway_routing_hint_q01_test.go` 共 5 个顶级测试：

- scope/control 表驱动 30 种分支（域名/路径/端口/endpoint/auth 元数据/空值/注入/非法类型等）。
- 显式 hint 的匹配、冲突、重复、未来扩展和非规范 casing 保持原值。
- 实际 ServeHTTP → loopback 出站，验证缺失 hint 补齐、显式冲突不变、xhigh/priority 正文不变。
- 实际旧 compact-v2 适配路由、models、两个 guardian 路由均不新增 hint。
- 实际 WS 首帧有/无提前 session 两种路径，第一轮 first/priority、第二轮 second/flex，确认只有一条 upstream 连接；body-first hint 取第一轮，preconnect 不生成未知 hint。

测试将 policy BaseURL 保持为生产形态，但通过显式 RoundTripper 将全部 I/O 路由到 loopback 测试服务，未进行外网 DNS/连接。

```powershell
go test ./... -run '^TestQualityQ01RoutingHint' -count=1 -timeout=90s
```

结果 `ok codex-agent 0.114s`。相关指纹、HTTP/WS、会话、策略、compaction、native WS 与前两专项回归也通过（`ok codex-agent 11.864s`；该关联运行早于最后新增的非 Responses 路由测试，后者随后单独通过）。

本修复不能保证上游采用某个集群，也不能依据 hint 是否存在预测糖果题答案。已知显式冲突不代表客户端必然错误；因此保留扩展和兼容行为，由上游解释原头。
