# Q13：真实 CLI 与质量探针的请求一致性复核

2026-09-24。只读检查当前网关、`gateway_quality_live_test.go`、官方参考 probe，以及固定 `.local/codex-source-0.155.0/codex-rs/`；执行无凭据的本地 loopback 回归。未 SSH、未调用外部模型、未更改生产代码或题目。下列 CLI 文件路径以固定快照为根。

**本次没有确认新的必须修复的生产网关 bug。确认了三个会影响实验归因的具体差异，其中第一项是当前 direct/transparent 配对仍存在的明确头差异。** 这些差异不能证明账号被标记、模型预算下降或服务端集群不同；也不能靠补齐字段就宣称原题质量恢复。

| 项目 | 分类 | 对当前证据的影响 |
| --- | --- | --- |
| direct 缺 routing hint，而 transparent 自动补齐；实验身份与 `codex exec` 不同 | 仅参考不完整 | 即使正文摘要相同，仍不是全部上游请求条件相同的对照 |
| CLI 目录驱动的正文与手写题目正文不同；candidate 内置提示词仍是额外策略 | 前者仅参考不完整，后者有意 policy | official CLI、direct/transparent、candidate 是不同的正文条件，不能把差值全部归于 TLS |
| CLI 默认 WS、HTTP fallback、压缩与传输状态不等同单次 native HTTP | 仅参考不完整；网关按入站协议转发是有意 policy | 同库源码不能证明同一次请求走了相同传输与连接状态 |

## 1. 当前 direct/transparent 配对仍有确定的 routing hint 差异

`gateway_quality_live_test.go:161` 的 direct 用原生 HTTP client 直达 Codex；`:180` 只补账号认证头，`:183` 仅在独立 `routing_hint` arm 添加 `X-Codex-Routing-Hint`。direct 并不进入该分支。

transparent 关闭的是内置提示词、指纹 profile、cache 补齐和兼容归一化。它仍进入 `gateway.go:730` 的 `applyGatewayRoutingHint`。`gateway_routing_hint.go:95` 限定第一方 HTTPS Codex `/responses`、AT 和 AccountID 后，从实际 prepared model/tier 生成缺失头；这一步不依赖 `CodexFingerprintEnabled`。因此按现有配置，direct 没有此头，transparent 有 `model=gpt-5.6-sol`。

CLI `core/src/client.rs:1023` 的 `build_routing_hint_header`、`:1547` 的 HTTP 调用点支持透明路径现在补头的行为。不是应该删除网关补齐。正文 SHA 相同只能证明已比较层面的正文相同；历史结果不能改写成已经发过相同请求头。

其他已定位的身份差异也应保留：探针 `:174` 固定 `codex_cli_rs/0.155.0 (Linux 6.8.0; x86_64) unknown`，而 `exec/src/lib.rs:260` 设置 `codex_exec`；真实 UA 在 `login/src/auth/default_client.rs:166` 根据运行 OS/架构/终端生成。CLI `codex-api/src/endpoint/responses.rs:119` 把 thread ID 同时用作 `x-client-request-id`，`:122` 添加 session/thread 头；探针仅有独立的 Session-Id 和每次生成的 X-Client-Request-Id，没有 Thread-Id。网关 `proxy.go:145` 允许这些 CLI 头透传，不负责猜造线程身份。

还需区分 cookie 状态：managed transparent dispatch 在 `gateway_upstream.go:36` 附带 owner scope；direct 没有 managed account context，`gateway_native_identity.go:32` 保守按 Authorization/Account-ID 摘要隔离。因此两条路径不会自动复用同一个 native helper cookie jar。这是此前跨账号状态隔离的正确边界，不是新的凭据丢失。首次请求和后续复用应分别观察，不能仅用相同 helper 文件 SHA 宣称状态相同。

**可验证边界：** test-only IPC 捕获应记录实际 routing hint、受限身份头、认证摘要、scope 和正文摘要。后续若确实需要匹配头的 direct 条件，应在新计划中显式按 CLI 规则添加 hint，不能改变历史 arm 的意义。无需为这项已被源码确定的差异单独重跑模型。本次没有修改 live harness，避免与父任务 IPC 工作冲突。

## 2. 手写题目正文没有重建 CLI catalog 与工具上下文

`gateway_quality_live_test.go:146` 只构造 model、单条 user input、实验 instructions、stream/store 和 reasoning.effort。CLI `core/src/client.rs:783` 的 `build_responses_request` 还有明确不同的请求上下文：

- `:817` 使用 `prompt.base_instructions`；Responses Lite 分支会把指令/工具放到 developer 前缀中。官方 probe 的 `developer_instructions` 是附加开发者消息，不等同把请求 `instructions` 替换成糖果题解释要求。
- `:763` 对 reasoning effort 使用显式值或目录默认，然后 resolve；summary 依模型能力和配置生成，Lite 可设置 `context=all_turns`。`protocol/src/config_types.rs:65` 的默认 summary 是 `auto`。探针只有 effort，不是相同 reasoning 对象。
- `:852` 总是请求 `include=["reasoning.encrypted_content"]`；`:853` 依据能力决定 text verbosity；`:869` 添加 prompt_cache_key，普通来源取 session ID（`:497`）；`:885` 添加 client_metadata。probe 没有这些字段。
- tools、tool_choice、parallel_tool_calls 由实际 Prompt 构造。`core/src/tools/mod.rs:68` 优先采用模型目录的 tool_mode；关闭 `features.code_mode` 不能覆盖 `code_mode_only`。完整 host 包解决缺文件警告，不会使 CLI 变成 `tools=[]`。
- `protocol/src/openai_models.rs:924` 过滤显式 default 和不支持的 service tier；网关只保留调用者的原值，不假定自己具有当前账号目录的全部能力信息。

q0/q1/q4 目录相同的已保存证据排除了当次目录可见字段差异，但不使上述手写正文自动等同 CLI 请求。当前网关 `gateway_policy.go`/`prepareBody` 使用 RawMessage 保留 include、reasoning、tools、text、client_metadata 及不透明历史。现有 Q01 真实 loopback HTTP/WS 测试已经覆盖这些字段，包含 float64 无法精确表示的大整数。没有找到“CLI 发了但网关删除 encrypted reasoning/include、降低 effort 或 tier”的新代码路径。

candidate/default 的 `BuiltinPromptEnabled=true`、`BuiltinPromptMode=prepend`（`store.go:637`、`gateway_policy.go:33`）则确实添加了内容；关闭这个开关后才能作为正文透明条件。它是现有产品策略，不宜伪装成 CLI 固有指令，也不能用错误答案反推它就是根因。本次不追加以取得 21 为目的的指令、catalog 替换或隐藏答案。

**可验证边界：** 抓取实际官方 CLI 出站后，只比较字段 presence、类型、长度及内容摘要；对工具/目录指令不保存全文。分别标明 CLI 上游正文、探针输入、网关实际 IPC，不把完整 CLI 答案当成只改变 TLS 的实验。已存在的字段保留回归足以覆盖本次查到的网关边界，不新增镜像实现的测试。

## 3. 默认 CLI 是 WS 客户端，native HTTP probe 没有同等状态机

CLI `core/src/client.rs:926` 根据 provider capability 与 session fallback 状态选择 WS；内置 OpenAI provider `model-provider-info/src/lib.rs:476` 支持 WS。`:1332` 可以在模型正文存在前 preconnect；`:1729` 遇 426 可进入 HTTP fallback。`core/src/responses_retry.rs:101` 在允许重试耗尽后切换传输，`:115` 明确 release 会隐藏第一次 WS 重试通知。probe 的 direct/transparent 从头到尾是一次 HTTP POST，既没有同一 WS 握手，也没有 CLI 的重连/fallback 历史。

官方 HTTP 副参考的 `tools/quality_official_cli_probe.py:236` 使用另一个 provider ID 明确关闭 WS 和重试。这里不能简单归类成“非 OpenAI provider 会清除内部字段”：它保留 `name="OpenAI"`，而 `model-provider-info/src/lib.rs:526` 的 `is_openai()` 判断的是 name。源码中的非 OpenAI正文清理分支，不能只根据 provider ID 不同就断言会触发。

HTTP 压缩也有条件：CLI `core/src/client.rs:1450` 要求 enable_request_compression、Codex backend auth 和 is_openai 才用 zstd。网关 `gateway_request_compression.go:39` 按调用者已有 Content-Encoding 保留选择；手写探针没有声明 zstd，不能因为最终都是 JSON 就声称 wire 一致。这不要求网关无条件强制所有 SDK 请求压缩。

TLS 环境须对实际进程取证。`http-client/src/custom_ca.rs:395` 的非空 `CODEX_CA_CERTIFICATE` 优先于 `SSL_CERT_FILE`，`:307` 选择 custom CA 时显式改用 rustls。helper `native-transport/src/main.rs:338` 复用该 builder。生产自动注入 CA 已由前一专项修复，但不能追溯替代旧独立进程环境，也不能把 HTTP 默认后端等同 WS 后端。官方 probe `isolated_environment` 只继承白名单；live process 的实际环境必须独立保存。本次没有运行时访问，未把源码重新审阅写成新的 TLS runtime 验证。

**可验证边界：** 将 clean 官方完整 CLI 作为独立参考；将显式 HTTP 副参考作为另一条件；分别记录 WS/HTTP、fallback、压缩及进程 CA 环境。网关按入站 WS/HTTP 转发符合代理边界，不能为了“看起来像 CLI”悄悄把 HTTP 客户端改成 WS 或在已发送请求后重放。

## 本次验证与交付范围

本地执行以下已有回归成功（`ok codex-agent 0.151s`）；它包含 Q01 字段透传、routing hint scope/HTTP/WS、原生 exec 头缺省和身份/attestation 边界。均为合成数据与 loopback，不提供真实模型恢复证据。

```powershell
go test ./... -run '^(TestQualityQ01.*|TestSourceNativeExecHeaderAbsence|TestGatewayHeadersAuditIdentityAndAttestationPolicy|TestCodexFingerprintDisabledPreservesIdentityAndProof)$' -count=1 -timeout=90s
```

初始只读复核阶段仅新增本报告。没有确认新生产缺陷，因此未添加一个必然失败但与产品契约无关的测试，也未改动生产代码。父任务继续推进实际 IPC 捕获；没有理由据此猜测 UA、cookie 或内部集群标签。后续针对真实结果发现的离线评分器问题另记如下。

## 本轮新增官方参考证据复核

报告收尾时已直接读取 `.local/quality-20260924/official-q4-fresh/result/report.json`、`events.safe.jsonl`、计划、执行回执及前后身份快照：完整官方 CLI 0.155.0 在 q4 原题上 clean completed，最终回答 **29**，65.573 秒，进程返回 0。启动警告、error、可见工具事件和 reroute 均为 0。前后 identity/token 摘要、assigned_at、oauth_updated_at 相同；官方 provider 为 `openai`，目录正常包含请求模型且 tool mode 为 `code_mode_only`，没有替换 catalog 或注入答案。

这条结果补齐了此前“异常账号缺 clean 完整 CLI 有效答案”的缺口，并证明 **绕过本次网关仍能产生 29**。它不证明该账号恢复了原题答 21，不排除更早使用历史，也不证明具体内部集群。报告仍将 `model_returned` 和 `transport_observed` 记为 null，`tool_surface_zero_verified=false`；不能把没有可见工具事件等同完整工具审计，也不能把默认 WS 配置等同实际握手观测。此处是新的有效参考，不是与手写 HTTP 的逐字段纯传输 A/B。

## 后续修复：离线评分器遗漏末尾 boxed 数值

真实官方参考最终文本以 `\[\boxed{29\text{个}}\]` 结束。旧 `tools/quality_probe_report.py::parse_answer` 删除 LaTeX 标点后剩下 `boxed29text个`，既不匹配明确答案标签，也不是纯数字最后一行，返回 `(None, "no_unambiguous_final_answer")`。这会遗漏一条本应计作原题错答的结果，属于诊断工具缺陷，**不是网关生产质量修复**。

先加入真实输出形态及边界测试，旧实现的 terminal boxed 两项测试产生 6 个失败断言，再实现修复。现在只接受整段输出末尾完整、唯一的 boxed 整数块，支持 `\[...\]`、`\(...\)`、美元数学分隔符及独立 boxed 行，可带糖果单位。只识别整数，不执行 LaTeX 表达式。多个 box、条件引导、明确答案冲突、分数/小数/算式和不完整末尾格式仍拒判；前文明确答案不能覆盖一个不支持的最后公式。推理中间的 box 不参与取值，后面有明确最终答案时只采用该明确答案。已有完成状态、协议错误和最终消息选择逻辑不变。

测试新增 5 个有针对性的方法，覆盖上述实际漏判、格式、非终末值、多个/条件/冲突，以及中间 box 不覆盖后续明确结论。评分回归 **50/50 通过**；相关 semantics 题面隔离、原题/控制判分与完整证据报告回归 **45/45 通过**。原题答案键仍为 21。

```powershell
python -m unittest discover -s tools -p 'test_quality_probe_report.py' -q
python -m unittest discover -s tools -p 'test_quality_semantics*.py' -q
```

对现有完整官方结果离线重读得到 `answer=29, expected_answer=21, category=wrong_answer, extraction=terminal_boxed`，没有新增模型调用。派生评分存于 `.local/quality-20260924/official-q4-fresh-boxed-score.json`；源 report SHA-256 为 `1f6315b0c08b9772c268a12d2f43299a0afead34209b28b1a055961a7f78314e`，重读确认原文件未改动。修复文件仅为 `tools/quality_probe_report.py`、`tools/test_quality_probe_report.py` 及本报告。
