# 直连官方 HTTP / WebSocket 对照：预登记与独立审核

日期：2026-09-25。审核范围：3 个既定账号 `q0/g2/g7`，固定 Codex CLI `0.155.0` 新捕获的原题完整首轮请求，`gpt-5.6-sol / medium`，各账号 HTTP 与 WS 各 2 次，总共 12 格。审核者不执行真实调用，只审源码、冻结产物和合成测试。

本轮回答的问题是：**相同账号、相同完整语义输入，在规定的官方 HTTP 与 WS 请求构造差异下，答案和可见 usage 是否出现一致的 transport 关联差异。** 请求由新 native helper 直接送往官方，绕过 Go 推理转发。本轮不能证明生产 Go 网关质量恢复，也不能把一次正确答案解释为修复已恢复全部能力。

## 1. 固定的输入与可变项

- 原题来自既定 `semantics-inputs/baseline.json` 的 `input`，原题 SHA-256 为 `cd771ba38f8f75696ba703c7d3e91e5273588c91cb8c4145e3ce4b077c075c3f`。必须在捕获时验证其对应的文本字节，说明取值和编码；不能拿整个 JSON 文件的哈希代替该 input 的哈希。
- 用户验收答案为 **21**。这个值只在离线评分器/计划里出现，不送进请求、CLI 指令、上下文或工具结果。题目、提示词、tools、effort、预算不能为得到 21 而修改。
- 固定一次新捕获的完整 HTTP 首轮语义输入：instructions、全部 input items、tools、reasoning、model、service tier、cache/session 信息。每格都重发完整输入，无 `previous_response_id`，不把前格的输出变成下一格历史。
- HTTP 与 WS 不是整段 body 字节相同：WS 有官方 `response.create` envelope、官方 metadata/header 投影和握手差异。所有允许差异必须列成白名单，转换前后严格核验，白名单外变化立即停止。`stream:true` 是此版本官方 WS 可保留字段，不能凭印象删掉。
- WS 的 dispatch timestamp 若按官方源码要求随实际发送更新，须从“固定字节”声明中明确排除，单独保存实际值或摘要及采集时钟。允许时间变化不允许修改 session、cache key、题目或工具。不要伪称 HTTP/WS 的 wire hash 必须相同。
- 首轮不继承已有 turn-state：固定源码 `core/src/client.rs:1087` 的 `connect_websocket` 首轮传入 `None`，不把刚收到的 101 turn-state 注入这一帧。转换器因此要求捕获中没有 `x-codex-turn-state`、`previous_response_id`、`generate`，不凭空添加 turn-state；握手响应若有该字段，仅保留当前格的观测摘要，不移入其他账号或下一格请求。
- 每格重新创建 helper 与连接，CLI/native binary 的完整 SHA-256、配置与源码清单冻结。`9110…` 只作沟通缩写；执行必须验证完整哈希。每账号实际 AT、稳定 user、workspace 与预登记 epoch 绑定；令牌只在私有输入/内存，报告用哈希。

官方固定版本源码依据：`codex-rs/codex-api/src/common.rs` 的 `ResponseCreateWsRequest::from`，以及 `codex-rs/codex-api/src/endpoint/responses_websocket.rs` 的首轮序列化。具体字段和行号由 [转换契约](C:/Users/Administrator/Desktop/项目/codex-agent/docs/quality-20260925/transport-pair-contract.md) 的冻结源码审核记录承担。

## 2. 建议执行顺序

保留主任务给出的严格镜像顺序，不在看到结果后改序。

| 格次 | 账号 | transport | 该条件重复 |
|---:|---|---|---:|
| 01 | q0 | HTTP | 1 |
| 02 | g2 | WS | 1 |
| 03 | g7 | HTTP | 1 |
| 04 | q0 | WS | 1 |
| 05 | g2 | HTTP | 1 |
| 06 | g7 | WS | 1 |
| 07 | g7 | WS | 2 |
| 08 | g2 | HTTP | 2 |
| 09 | q0 | WS | 2 |
| 10 | g7 | HTTP | 2 |
| 11 | g2 | WS | 2 |
| 12 | q0 | HTTP | 2 |

每个账号、transport、账号×transport 条件的位置均值都是 6.5，可平衡按格次近似线性变化的因素。各格用时不等，因此仍须保存真实开始/结束时间，不能声称严格消除了真实时间漂移。

偶数长度的完全镜像序列必然有中心相邻同格，本方案是两次 `g7/WS`。它可能增强短时间状态与 cache 的相关性，但不是执行错误。为消除此相邻重复而交换第 7/8 格，会破坏每条件的精确镜像；在仅两次重复时没有证据表明该改动整体更好。推荐保持既定序列，明确这两次并非独立样本，不额外插入模型请求、缓存清理或结果驱动等待。

## 3. 必须明确的混杂与范围

1. **transport 是一组路径差异。** HTTP/zstd 与 WS/deflate、握手、连接建立、合法 header 投影可能一起变化。可称“源码规定的 HTTP/WS 路径对照”，不能只归因于 TCP、TLS、UA 或某一 header。
2. **新 helper 不清除服务端缓存。** 固定 session/cache key 会保留本轮希望控制的上下文，但同账号跨格仍可能有缓存或短时路由记忆；跨账号身份隔离也不证明服务端不共享任何缓存。应报告服务端实际 `cached_tokens`，不要把“未提供”填成零。
3. **账号是按既往结果挑选的 3 个对象。** 不能推广成所有账号或账号固有属性；同 workspace 的不同 user 仍是不同身份。目录一致只说明当前公开目录一致，不证明内部调度/预算一致。
4. **重放不是完整 CLI 自主运行。** CLI 提供真实请求构造，native helper负责实际传输，探针负责单次解析；没有完整 CLI 的恢复、工具执行和后续 turn。必须同时保存这些层各自的版本/哈希。
5. **两次重复只用于发现可复现方向。** 报告逐格结果和每账号 H/W 的 `correct/eligible_completed`，不做小样本显著性、稳定恢复率或内部“低算力池”推断。reasoning token 数是上游可见计数，不能当作隐藏推理内容或内部预算证明。

## 4. 发送、停止和评分规则

- 最多 12 格，每格最多一次推理 dispatch；HTTP 禁重试，WS 仅一次握手、一次 `SEND`，不 fallback、不换账号补发、不刷新 token。失败的格保留，不追加第 13 格。
- 一般格失败按预登记 `continue` 进入下一格。身份 epoch、CLI/helper/工具/capture hash、转换契约不一致属于全批停止条件。quota 不允许则该格 `skipped_quota`，没有 dispatch，不算错答。
- 每格先读 quota/usage eligibility 和身份 epoch，结束后再读 usage/epoch；超时失败也尽量在预留时间内完成 after 观测。若 after 观测失败，保留其失败，不能声称 epoch 始终不变。模型 usage、账户 quota 快照和网关指标是不同证据，分字段记录。
- 一次绝对 deadline 包含 helper 启动、握手、发送、读取和清理，并预留 after 观测与清理时间。所有超时分类都固定命名，不把异常字符串或 credential-bearing response 原样输出。
- WS 的 ACK 只证明该 SEND 完成写入；101 只证明握手成功；都不能单独视为答案成功。允许 `MESSAGE` 先于 ACK 到达，必须分别记录二者。完整 completed 已到、之后连接关闭时，保留答案和 closing error 两个事实。
- 工具调用不执行、不伪造结果、不提交第二轮；该格标记 `tool_requested/no_final` 等协议结果，不能归为答案错误或以额外提示“修正”。完整 capture 中已有工具声明保持不变。
- 只对有完整最终 assistant 答案的 completed 响应评分。`correct=21`、`wrong_answer`、`ambiguous_final`、`missing_final`、`incomplete`、`protocol_error`、`timeout`、`skipped_quota` 分开。最终消息、逐项事件重建和 terminal output 不一致时标 ambiguity，不静默选有利的那份。
- 保留原始 final 的安全脱敏文本和 SHA；额外记录公开解释是否明确利用按形状取糖的策略，仅作描述，不改变用户的 21 判据。不能从 summary/encrypted_content 还原或声称知道隐藏推理。

## 5. 每格报告最低字段

| 类别 | 要求 |
|---|---|
| 登记 | experiment/attempt ID、格次、账号 alias、transport、重复号、固定顺序、计划 SHA、失败策略、最大 dispatch=12 |
| 身份与执行来源 | user/workspace/token/identity epoch hashes 的前后值；CLI/helper/工具/配置/capture 完整 hashes；direct official 路由；无 Go inference hop；egress 观测来源；helper 退出与进程清理 |
| 请求不变量 | 原题 input hash、原 capture body/head hashes、实际 HTTP body 或 WS event hash、去除允许 transport 投影后的语义 hash；model/effort/tier/明确预算字段；session/cache/turn-state hashes；允许差异清单和核验结果 |
| 实际发送 | dispatch count、HTTP method/path/compression 或 WS握手状态及 SEND sequence、ACK sequence/observed、terminal-before-ACK、首字节/首事件/terminal/结束时刻；重试/fallback=false |
| 响应 | HTTP status 或 WS101；created/terminal response ID hash、请求关联 ID hash；全部事件 type/count；terminal status、response model/tier/reasoning controls；error 分类、连接结束分类；完整流/事件摘要 hashes |
| usage | terminal 原始嵌套数字结构，字段缺失保留缺失；input/output/total、cached/reasoning 计数；before/after quota 快照和 eligibility 结果；各字段来源 |
| 答案 | 完整安全 final、final SHA、final来源、一致性/歧义、解析答案、score；是否请求工具，是否存在最终 completed |
| 有界性 | case/batch 绝对 deadline、elapsed、after观测状态、无重试计数、helper reaped、暂存清理、残留进程检查 |

不能只保存 `correct=true` 或 CLI 累积 token 总数；也不能让报告转换器把缺失 nested usage 默认写为零。响应模型/tier缺失应明确标 `not_reported`，不能用请求值冒充回显。

## 6. 预定结果解释

- 某账号两次 WS 都 21、两次 HTTP 都非 21：是本轮条件下与 transport 相关的可复现方向，值得另登记确认；仍不足以定位具体 header、TLS 或内部机制。
- 同账号 H/W 都呈同一结果，账号间不同：本轮未显示 transport 足以解释差异，继续保留账号、服务端状态等未观测因素，不能直接认定风控。
- 结果随前后位置、cache计数变化，或同条件两次不一致：报告不稳定和相关性，不挑单格给“恢复”结论。
- 全部 21：证明这 12 个直接官方请求通过该题验收，不证明 Go 网关恢复，也不否定先前失败证据。
- 发生协议错误或拒绝：先判断测量链是否有效；不可把探针处理不完整造成的错误当模型能力下降。quota skip 与非 completed 都不进入错答分母，但所有 12 个计划格必须保留在总表。

要判断网关质量恢复，需后续另登记同账号、同捕获、同 helper/transport 的 **direct official ↔ Go gateway** 有界 AB/BA，对比实际出口 body/header/terminal/usage，不能从本批直接外推。

## 7. 工具独立审核状态

设计已核对固定 0.155.0 源码与既有采集器。当前 WS 执行探针和 HTTP↔WS 转换工具由另两名代理实现，实际冻结哈希、独立合成测试和发现项将在此节追加。此状态不表示工具已通过，也不授权审核者真实调用。

首项独立复现并已修复：v5 `inventory_remote.py::snapshot` 原先使用 `chatgpt_user_id or claims.sub`，未遵循官方 `chatgpt_user_id` 缺失/null 时取 `auth.user_id` 的规则。仅 `auth.user_id` 时稳定用户 hash 取错 `sub`，仅 `sub` 时未拒绝；两项 AST 隔离合成测试先失败，主任务修正后独立复跑均通过。测试只执行该函数和注入的合成 adapter，不导入 driver、不读 credential 文件、不联网。实际所选账号有主字段时，不据此宣称历史结果已受影响；旧 inventory 保留，本轮捕获/模型计划使用修正后的 snapshot 重验 epoch。

复跑：`python .local/research-20260925/http-ws-controls-v5/review/check_inventory_identity.py`。红测证据位于同目录 `inventory-identity-red.log`。

原始捕获链已只读审核，无阻断项：`build_capture.py` 按冻结清单核验并复制源码，只变更捕获 wrapper 与对应 fixture；`raw_capture_test.go` 在任何 gateway policy 前记录解压后的实际请求实体；`capture_remote.py` 校验 CLI/server/helper hashes、3 个账号 epoch、原题 SHA、单次 capture 和解析后的 model/effort，并在 finally 删除临时捕获凭据。底层 exact capture POST 进入没有 outbound transport 的 synthetic helper，额外 POST 拒绝。这里“模型 POST=0”不等于零网络：模型目录 GET 使用真实官方路径。

捕获限制已明确：header来自 Go HTTP parser，经过排序、大小写规范化，原 wire 顺序/大小写未观测；本地 `/v1/responses` 在声明中规范化为官方路径；真正对照输入采用 `raw-request.body`，不使用经 gateway serializer 处理的备用 `request.body`。

转换器独立发现并修复 `_Number(str)` 类型混淆：原先数字 `model=123`、数字 `prompt_cache_key=123` 可以通过字符串校验。两项合成子场景先红，改为严格字符串类型后通过；另一项直接把转换后的 body/head 交给真实 WS 探针 validator，确认大整数 `9007199254740993`、原 input/cache 字节与 `stream:true` 保持，禁止的 previous/generate/background/未知字段仍拒绝。独立脚本 `review/check_transport_pair.py` 三项通过。

WS 证据独立发现并修复：握手→完整 completed→transport error 且未收到 ACK 时，原 `_invalidate` 清空已经观察到的 final。现在保持本格 `protocol_success=false / eligible_for_scoring=false / answer=null`，另存 `observed_completion` 的实际 final、答案、usage 和响应控制字段；这不会把协议无效格加入正确率分母。独立 native-IPC 脚本 `review/check_ws_completed_evidence.py` 验证 terminal-before-ACK 正常单次发送，以及 missing-ACK 异常下保留 observed completion，两项通过。

探针在 terminal 与 ACK 都到达后主动停止，不等待后续连接事件；报告必须明确 `later_transport_events=not_observed_after_terminal_and_ack`，不能把没有继续读取写成“对端正常关闭”。

握手后 timestamp factory 的独立补测已通过：只有收到有效 101 后调用一次，实际发送 body 等于 factory 返回的完整字节；更改题目而非 timestamp 会在 SEND 前拒绝；失败握手不调用 factory、不发送 create。工厂只计算，projection 落盘由外层 dispatch 在返回后处理，避免在记录 request-start timestamp 后额外插入磁盘 I/O。

工具测试复跑：转换器 13 项通过；WS 探针本机 20 项通过、1 项 POSIX native loopback 因 Windows 跳过。负责代理另报告 Linux `9110b869e65db6bf7664e39fcfe3b2ec17fa7a0207ddbeccf28d73dbc12ccce8` 的 21 项实际通过；这条 Linux执行事实须以主任务收集的 `ws-probe-loopback/tests.log` 为准。独立自有测试为身份 2 项、转换 4 项、WS IPC/factory 4 项，均本地通过；没有真实模型调用。

真实新捕获报告交叉核对：raw body `7e8d4732aaa166959a513be1c11c26459f5b4bbec220407c17e20b2c1434131b`，raw head `e7c4d9542c925b388a30c57c9bc44c41c13f78507ee493ba1086074a54516348`；备用经 gateway 处理的 body 为 `aae53b32…`，没有混用。原始 11 个顶层字段只有 client_metadata 变化；13 个捕获 header 的投影和 RuntimeMetrics=false 有明确源码依据。审核者尚未取得 raw 字节，故此时只声明报告间一致和算法验证，真实字节重算由独立脚本 `review/verify_remote_pair.py` 在捕获目录只读执行后追加。

外层 runner 已按 review 修正并完成独立执行验证：`persist()` 任意错误设置 fatal 使后续 epoch gate 停止；连续 management API 使用同一剩余绝对 deadline；HTTP 与 WS 对 helper cleanup 不完整采用相同不可评分处理，并保留 observed completion。helper/runtime hash、readiness、factory 字节边界错误设为全批停止，普通上游模型失败按登记继续。

`review/check_runner_callbacks.py` 用 AST 仅提取实际 `run_remote.py` 的 nested adapter，注入合成 HTTP/WS/管理存储，不导入 driver、不读取真实 KEY：5 项通过，分别验证上述持久化停止、共享 deadline、边界错误与普通错误分类、HTTP cleanup 证据保留，以及 dispatch 异常停止。自有独立测试总计 **15 项通过**（身份 2、转换 4、WS IPC/factory 4、runner 5）。

最终只读审核结论：当前转换工具、WS探针、预登记顺序与 runner 没有剩余阻断项；启动前仍以冻结完整 hashes、Linux工具实际测试、独立真实 raw proof 和新 epoch gate 通过为准，不用审核文字替代执行结果。此阶段 reviewer 发起的真实模型请求为 0。

已审核工具 hashes：

```text
quality_transport_pair.py
41c2a476a89bd6dd96807af74f8b35abb5f745bf1eb71a084bd2807ecc5c8791
quality_exact_websocket_probe.py
3b251075f7f590abb24d859f29f5c8d279d03bcbffdb22514135fc7b37501ab7
verify_remote_pair.py（独立真实捕获验证）
9985b12231b382ef3741a80c8b6c3a4ec07ae770b94f0a8a2413065e2fc497b5
```

## 8. 实际完成后的独立结论

主任务随后执行只读真实 raw proof，`review/remote-pair-verification.json` 全部通过；Linux 冻结工具 157 项通过、0 跳过/失败。预登记 12 格完整执行，没有新增或重试格。

独立 `review/review_results.py --require-complete` 对全部 12 格检查通过，`findings=[]`；服务器 execution/12 份 report 原始字节哈希均与本地相符，3 个 unit 退出、5 份私有 raw/credential 文件清理已实测。完整模型答案 10 份，人工均为 29；另 2 份 q0 HTTP 429 无模型答案，不计错答。3 份自动解析为 null 的文本保留原值，人工判断与 final SHA 单列在 `human-annotations.json`。结果没有显示 WS 达到用户的 21 判据。

逐格 usage、顺序、失败解释边界和可复跑证据见[最终结果报告](C:/Users/Administrator/Desktop/项目/codex-agent/docs/CODEX-QUALITY-HTTP-WS-2026-09-25.md)。本批仍只验证 direct official transport，不外推 Go 网关恢复；审核者真实模型调用始终为 0。
