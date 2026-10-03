# App Server 全目标完成审计

## 2026-09-27 后续实现状态

下列编号审计保留的是改造前发现的缺口，不是当前所有代码都仍有该缺陷。随后已实现并验证：轻量入口、JSON准备、账号缓存改写、压缩、SSE/JSON输出和 WS 取消本地总量限制；原生传输协商无总量限制的 HTTP 及 64 位长度分块 WS v2；保留账号并发。33 MiB HTTP plain/zstd、TLS WS text/binary、33 MiB 单事件与 34 MiB 聚合均有回归。WS 待完成/会话数、长 ID、256 项响应引用淘汰等本地限制也在新 profile 下取消。

Realtime 已有七条官方运行时到隔离 fixture 的请求捕获，轻量 call-create/sideband 转发已实现，包含 API multipart→ChatGPT JSON、固定 sideband URL、账号归属、不同压缩协商、令牌刷新和真实用量观察。实际 Rust 助手的组合测试验证了该转发。仍未据此声称真实音频、账户权益或 ICE/媒体路径已验证。

当前实际使用的最终候选 Go test artifact 为 `af327b3a0ec3295db7fa3d7e93149fa8ca0738a8b617ec7a6807d35d33ae22f9`，Rust helper 为 `361b0df32ce0e74d5a07bad697abf49cb59e295eb1795797ae4c2a18548948ee`。真实模型 run `20260927T123407.756914Z` 的 14 项内容审计通过；该组合的 13 项实际 native 互操作全部通过、无跳过。Go 全套测试与 vet 通过；当前源码 Linux race 168 项通过，无跳过、无缺失、冻结后未变化。统一证据索引为 `.local/appserver-parity/reports/latest-evidence.json`。

因此编号 1、2 的主要模型载荷回归已修；编号 7 的 Realtime 网关传输缺口已有实现。编号 3 的完整官方 sidecar 保留语义、编号 5 的严格配置 profile、编号 6 的执行器能力、编号 7 的账户/插件等非 provider 生命周期和编号 8 的其它独立边界，仍须分别审计。目标仍未标记完成，也没有切换生产模式。

审计对象：当前工作区候选源码、固定 Codex 0.155.0 源码、已保存的 native/bridge/lightweight captures。只读核查，不改冻结 Go/Rust。目标同时包含“所有出站差异补齐”“客户端保有工具与会话管理”“删除应用上限但保留账号并发”。因此现有 14 场景与 128 race 测试通过不能自动代表全目标完成。

## 1. HTTP 请求大小：contradiction，确定回归

原 hosted 路径 `gateway_official.go:150` 使用 `readOfficialBody`，无应用 body ceiling，zstd memory/window 显式放开。轻量路径 `gateway.go:674` 与辅助 JSON `gateway_appserver_auxiliary.go:66` 仍调用 `proxy.go:636` 的 `readBody`：wire 与解压后 JSON 均 32 MiB，zstd window 也 32 MiB。随后 `prepareHTTPBody`（`proxy.go:109`）、`prepareGatewayRequestBody`（`gateway_request_compression.go:78`）、prompt-cache 改写大小检查（`gateway_prompt_cache.go:279`）还会独立拒绝大请求。`officialRequestUnbounded` 目前仅被 policy 等局部调用，未贯穿这条路径。

下一步：建立仅对已启用 lightweight 官方通道生效的 request-size policy；贯穿读取、JSON准备、account cache rewrite、压缩、compact，旧 legacy 分支维持原行为。用 33 MiB 合成字符串与 zstd 高压缩数据分别跑真实 HTTP，确认没有任一层重新拒绝；账号并发逻辑不动。

## 2. Native HTTP/WS IPC 与响应聚合：contradiction，不能只改入口

Go `gateway_native_http.go:32/717/723` 和 Rust `native-transport/src/main.rs:30/352` 同时固定 32 MiB。Rust compression 的 `request_compression.rs:20/34` 限原文及压缩体。WS request 指定 `max_message_bytes=maxBody`（`gateway_native_ws.go:148`），helper `websocket.rs:246` 再钳到 MAX_BODY；Go `SetReadLimit(-1)` 甚至会重置为 32 MiB（`:662`）。轻量 pool/一次性 transport 的 SetReadLimit、mapped SSE scanner（`gateway_appserver_cli.go:562`）、通用 collector（`proxy.go:371`）及 `response_output.go:168/320/345/358/421` 均仍有限制。它们会把大 tool output/image/context 变成错误，直接违反已有要求。

下一步：按下方 IPC 设计分工，加入协商能力并消除该通道的总消息/body ceiling；chunk buffer 大小与 backpressure 不属于总 payload 拒绝，保留。除了 33 MiB request，再测 33 MiB 单 SSE event、多 event 聚合大于 32 MiB、双向大 WS message 与取消/断线；不把诊断采样丢弃误当实际转发丢弃。

## 3. 旧 `/responses/compact` 兼容适配：incomplete，保留策略明确不同

`prepareResponsesCompaction`（`gateway_compact_v2.go:56`）追加唯一 trigger、强制 stream/store；`responsesCompactionResult`（`:106`）保留 user/developer/system 后附 compaction，并有 `64000*4` 字节预算。固定官方 `compact_remote_v2_attempt.rs:41/79` 还会依据 context window 修剪工具输出、使用当前工具并清 output_schema；`compact_remote_v2.rs:491/542/604` 按用户可见消息、client_authored sidecar、agent-message类别、图片与 token 预算选择保留，并可截断边界文本。当前 byte suffix 算法不等同该算法；成功回忆 marker 仅证明测试案例可用。当前 adapter 还可保留 caller text.format/tool_choice，官方 compact prompt 明确 output_schema=None，需单独比较带这些控制的请求。

下一步：捕获带 developer/system、工具输出、结构化 schema、图片与大消息的 compact 输入/输出 oracle。客户端原生 `compaction_trigger` 路径继续原样由客户端管理历史；旧 JSON compact API 应给完整、明确的保留策略配置，删除未经用户选择的本地固定 byte ceiling。要重现官方 retention，需客户端提供 client_authored/图片预算等缺失 sidecar；不能复制猜测值后称完全一致。

## 4. 已测 request writer、身份、metadata、socket：proved，但证明范围有限

已有固定 executable 捕获 oracle 证明 Lite 前缀 UUID、默认 controls、原始 body/header writer；真正 App Server/`codex exec` 归一化后确认 thread_source 差异，native mirror 有成对 fixture。lightweight 当前补齐显式 prewarm metadata、目标 App Server UA/originator、输出前 owner CAS，以及账号/key/proxy授权和严格 history hash 检查后的跨 HTTP socket reuse。其执行行为仍是无 hosted runtime 的请求适配；旧文档“每HTTP独立socket”已被新 pool 替代，不能沿用旧结论。

下一步：用当前冻结 full-run 最终报告确认 first/resume/function-result/compact-reset 的握手数、前缀连续性及未发生隐式 replay；同时保留 TLS interception 不能证明 TLS fingerprint 的事实。不要用较早的 49 race/14case报告覆盖新 pool 的证据。

## 5. 客户端 controls 与模型默认：incomplete，profile 差异应显式

实际 catalog 的 defaults 已读取，缺 instructions 与显式空值有 provenance 测试。客户端明确 reasoning/strict schema/tool_choice 等原文保留；官方配置 writer 却固定 tool_choice=auto、将 ultra/persistent 别名规范化、按模型 capability 丢不支持 summary/verbosity/tier。真实上游曾拒绝新增 tool_choice.namespace，目前已修。这说明保留客户端契约与复制官方配置语义是两个可检验 profile，不能隐式混用。

下一步：做纯 profile 控制矩阵并补真实最小用例：缺省 vs 显式 none/ultra/persistent、支持/不支持 summary/verbosity、default/priority service tier、required/指定custom。将“不支持但原样转交得到真实错误”与“已成功执行”分开记录，不自动重试已发出的生成。

## 6. 完整工具/上下文镜像与客户端执行权：存在具体约束冲突

真实 bridge 即使客户端无 tools，也暴露 functions/clock/collaboration；native catalog 默认 CodeMode 则暴露 exec/wait，并在 runtime 执行动态工具和 alpha/search。轻量当前仅声明客户端工具，不注入服务端 cwd/seccomp/root-agent。精确镜像上述全部内容要求客户端确实实现相应工具执行器、CodeMode JS runtime、agent-tree通信/goal状态，并提供执行器 sandbox/network/approval、安装/窗口/turn生命周期。仅复制工具 schema/提示词会产生客户端不能执行的调用。

下一步无需等待权限确认：实现显式 capability manifest 与 profile resolver，客户端能提供的 executor/context 则按 official catalog/source渲染并测试；缺失项作为可机器读取能力缺口，继续使用声明工具profile。可另提供客户端侧官方 executor shim，让 runtime在客户端本地执行；服务器仍仅转发请求。不能把隐式服务器执行 builtin 当解决方式。

## 7. 条件客户端 surface：incomplete，Realtime 与系统 API 仍需接入/验证

当前公开路由已有 Responses/compact/models、Guardian HTTP/WS 和 alpha/search/images生成编辑/memories JSON辅助路径。fixed source `codex-api/endpoint/realtime_call.rs:63/72/146/174` 明确 `/realtime/calls`、`/live`、ChatGPT JSON与API multipart两形态；`realtime_websocket/methods.rs` 还构造 realtime/live及call ID路径。现路由 switch未覆盖这些，不能由现auxiliary JSON通道声称已支持。startup真实捕获还有 `/backend-api/wham/settings/user`、plugins featured/installed/suggested/export 和 signed files下载；它们是实际 runtime HTTP surface，不是推测隐藏模型路由。

下一步：逐项做固定endpoint协议adapter与loopback oracle，Realtime先验证method/query/content-type/Location/WS双向binary+cancel，再补一次受控真实初始化；系统API按其真实host/path/auth boundary接固定路径（不做任意URL带账号凭据代理），signed asset URL维持已有签名且不重写内容。区分gateway转发API与client本地插件安装动作，不把本地安装搬到服务端。

## 8. 运维与请求数量上限：incomplete，需要清单而非泛称“无限”

当前 helper `max_active:0` 已不额外限制并发，账号 admission 必须保留。仍可见 model endpoint 8 MiB response cap（gateway.go:1233）、official catalog32MiB缓存预算、IPC header64KiB/128字段、WS max_frame_size继承fork默认、IPC/write/read超时。缓存淘汰/流控块大小和总请求拒绝不是同一类；需区分实际产品限制、内存缓存策略、协议长度编码与必要取消边界，不能一律删除校验或一律声称“本来必须”。

下一步：将每个上限登记为“reject total / chunk size / cache eviction / malformed-protocol validation / deadline”，优先移除 lightweight 的任意总 payload reject，缓存预算可改成不影响请求正确性的eviction；保持账号并发。HTTP/WS协议真实长度边界与实际可用内存仍客观存在，不能称数学意义无限。

## 下一阶段大小上限改造分工与 IPC 结论

1. **Go请求与JSON组装层**：`proxy.go`、`gateway_request_compression.go`、`gateway_prompt_cache.go`、`gateway_compact_v2.go`、`gateway.go`、`gateway_appserver_auxiliary.go`。用明确 lightweight context/profile选择unbounded实现，别直接把全局 maxBody改大。`gateway_content_session`/cache-affinity遇大body目前放弃观察，检查不会误失必要owner会话；已有客户显式session优先。
2. **HTTP native层**：`gateway_native_http.go`、Rust `main.rs`/`request_compression.rs`。RequestHead.body_length是JSON `usize`，随后body为exact长度裸bytes，不是u32 payload frame。因此HTTP去32MiB无需改整个body framing；response本来已分64KiB DATA chunk，u32只限制每chunk不限制总response。新增ready capability如 `unbounded_payload_v1` 和明确request opt-in，未协商旧helper不得误发；用usize/u64 checked conversion、try_reserve/分块读取或streaming body避免overflow。没有必要用4GiB总body限制冒充协议要求。
3. **WS native层**：`gateway_native_ws.go`、Rust `websocket.rs`。现在 SEND/MESSAGE是`u8 kind + u32 payload_len + u64 seq + u8 msgtype + full_message`，真正有`u32::MAX-9`单IPC消息编码边界。仅删除MAX_BODY会让`data.len() as u32`截断，是严重错误。应新增协商 `websocket_message_v2`，把一条逻辑WS消息拆为start(total u64/seq/type)、若干现有有限DATA chunk、end；ACK仅在完整上游send后发。Go/Rust按seq校验chunk顺序/精确总量，取消与backpressure保持。u32仍作为chunk编码长度，不再成为整条WS消息人为上限。若暂用v1扩到u32边界，必须标为过渡，不可声称已完全移除。
4. **WS/SSE与收集层**：`gateway_websocket.go`、`gateway_appserver_transport.go`、`gateway_appserver_connection.go`、`gateway_appserver_cli.go`、`proxy.go` collector、`response_output.go`。协商v2后SetReadLimit(-1)真无应用message限制；Rust WebSocketConfig max_message_size/max_frame_size明确设None。Scanner换增量行reader，合法大单event/总JSON不得32MiB失败；保失败完整event→EOF与未终态断线→UnexpectedEOF语义。
5. **验证与版本冻结**：先loopback old+new helper协商、33MiB HTTP plain/zstd、单WS/单SSE/聚合输出、分片乱序/长度不符/取消/slowreader、账号并发仍只受account限额；再冻结helper/build provenance，重跑现full functional+race。这里只给设计与文件归属，本审计未修改运行源码或生产配置。
