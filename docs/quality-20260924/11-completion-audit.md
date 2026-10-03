# Q11：账号质量目标的独立完成审计

2026-09-24。此审计检查当前源码及已保存的原始结果、部署回执、测试日志；没有发送新模型请求、读取真实凭据、操作生产或修改通过标准。它审计的是账号质量目标，不重新执行上一阶段 20 项传输审计。**目标仍未完成：明确缺陷修复已得到证据支持，异常账号的原题稳定答 21 尚未得到证据支持。**

`proven` 只适用于表中限定命题；`unproven` 表示缺少能证明该要求的直接证据；`contradicted` 表示已有相反结果，不能把它重新表述成已通过。历史回执证明记录时刻，不替代下一轮请求前的实时身份/进程核验。

| 需求或必要命题 | 判定 | 已核验的证据与边界 |
| --- | --- | --- |
| 使用原糖果题、原答案 21 判据，不注入答案 | proven | `tools/mother_account_benchmark.py`、Q03 数学核验、live harness 构造及官方 probe 配置均未向模型填入期望答案。21/29 的形状选择语义已独立区分；其他题面变体不能替原题达标。 |
| 已用 28765 来源做多账号实验，并恢复已核验的临时推送状态 | proven | `post-experiment-source.json`、`push-control-restored.json`、`push-directfirst-restored.json`、`credential-cleanup.json`、`continuation-final-runtime.json`。本审计直接读取两份 restored 回执及清理结果；历史报告中的第三次临时实例不凭报告文字另计为一次独立复核。证明所记录窗口及清理时刻，不证明账号此后永不轮转。 |
| HTTP/WS、刷新与管理请求按 owner 隔离原生状态 | proven | 当前 `gateway_native_identity.go`、`gateway_upstream.go` 与 Q05 的真实 native A/B/A、同 workspace 不同 user、同 owner 换 AT、owner revision 场景；`go-quality-cookie-isolated/summary.json` 为 6 通过、0 失败、0 跳过。含 user/workspace 的可信 owner 正常刷新保留自己的状态，未知 owner 凭据变动保守隔离。 |
| routing hint 与推送桥状态缺陷已修复并上线 | proven | `gateway_routing_hint.go`、`bridge-deploy-v2.json`、`activation-result.json`；桥当前发布摘要为 `1cecef42…6fae1`，网关为 `84ca77af…85f0`。只证明限定路由条件和交付状态，不证明它们决定算力。 |
| 生产默认 HTTP TLS 不再被部署默认 SSL_CERT_FILE 切换 | proven | `runtime-tls-correction.json`、`tls-final-runtime.json`、`tls-runtime-probe.json`；同 UID 无凭据连接正常验证证书并收到 401。backend 判断来自真实环境、源码分支和协议观测三者，不是从 HTTP/1.1 单独猜测 TLS 库。Linux 37 项部署测试全过。 |
| HTTP/TLS 与 WSS/TLS 在所有平台/所有请求完全等同 CLI | unproven | `http-fingerprint.json` 与官方 CLI/WSS 测试只覆盖所记 Linux、信任模式、连接生命周期及字段。默认 native 未协商 h2；没有把它计为 h2 对照。WS 使用 `ExplicitCodexTls`，不能把 HTTP 环境修复描述为 WS 从 rustls 改到 OpenSSL。源码/vendor 相同不能替代所有 runtime 证据。 |
| 网关未降低所测请求的 model/effort/tier，且返回元数据被正确区分 | proven | Q01 的 HTTP/WS loopback 出站断言、`gateway_observation.go` 与 live 的 `response_model/reasoning/service_tier`。证明这些覆盖的字段保留与回显，不证明后台实际推理预算或硬件。 |
| q0/q1/q4 的实时目录与 plan 声明没有可见差异 | proven | `catalog-report.json` 为三个 GET 200、九个模型的完整字段摘要相同；`catalog_probe_remote.py` 不调用模型、不替换 catalog。plan 是 AT 里的声明；不是服务端集群分配证据。 |
| 官方完整 CLI 已在异常账号给出无告警、无错误的有效原题答案 | proven | 新 `official-q4-fresh/`：完整 0.155.0 包、同 q4 epoch，65.573 秒 clean completed，最终 29；无启动警告、回合错误或可见 reroute。11 项离线核对通过。该项证明参考有效，原题本身仍不通过；历史不完整或失败参考不被回写为有效。 |
| q4 同身份、同 AT epoch 的原题 direct/透明网关配对已执行 | proven | `semantics-live-q4/` 两条原题均完整返回 29；输入哈希和 predicted body 相同，前后身份、分配时间、OAuth 更新时间相同；进程显式环境无 CA/proxy 覆盖。证明该组在已记录层面的可比性，不是实际 wire 相同或多轮稳定率。 |
| 单次 q4 透明请求的真实 Go→Rust IPC 正文与认证和预期一致 | proven | 新 `dispatch-live-q4/` 与 `dispatch-live-q4-review.json`：925 字节完整写入本地 Rust socket，body 与输入/预测摘要相同；实际 AT、workspace、request ID、routing hint、model/effort 核对通过，HTTP 200 完成仍答 29。18 项离线检查通过。不是网络 wire、Go owner scope 或生产多账号调度的全面证明。 |
| 异常账号恢复原题答 21 | unproven | 新 q4 配对的原题结果仍为 29/29，恢复要求未满足。澄清形状能力后 21 仅是诊断控制，不计入原题通过。q1 仍无新的有效恢复证据；正常账号、变体、限流、重试后筛中的 21 均不能替代。 |
| 本次网关请求是产生 29 的必要条件 | contradicted | `isolated-q4.jsonl`：同 AT 摘要，先独立原生 direct 200/29，再 candidate 200/29；前者发生在进入本次网关质量路径前。它不排除更早账号历史、其他客户端、出口或上游状态。 |
| 29 已证明账号标记并分配到较弱内部集群 | unproven | 没有经过验证的内部 cluster ID、风险标记或路由决策记录。token 数、时延、CF-Ray、plan、目录一致、答案 21/29 都不独立提供这类证明。 |

## 三个关键证据边界及最新状态

### 1. 同身份、同凭据原题配对已补齐，质量恢复仍未达标

`isolated-q4.jsonl` 的两个请求确实具有相同 client body 和 credential 摘要；但 direct 的 predicted body 是 `763f3380…df7f5`，candidate 是 `69d3c94d…3a260`，所以这是不同出站正文策略的对照，不能当成纯 TLS A/B。q1 的后续 clean HTTP 429 也没有补上原题质量证据。

主任务随后执行了这个最小配对，本审计已逐项读取 `semantics-live-q4/` 的计划、执行回执、四条原始结果、输入证据、四份 process 和逐次 identity-after，而非只依赖汇总报告：

| 固定顺序 | 实际结果 | 原题判定 |
| --- | --- | --- |
| baseline-direct | HTTP 200 / completed / 29；16,035 ms | wrong_answer |
| baseline-transparent | HTTP 200 / completed / 29；33,901 ms | wrong_answer |
| clarified-transparent | HTTP 200 / completed / 21；98,780 ms | 独立诊断控制，不计原题通过 |
| blind-transparent | HTTP 200 / completed / 29；27,035 ms | 不同任务控制，不计原题通过 |

四次均为 `gpt-5.6-sol/medium`；每次开始前与结束后的 identity/token 摘要、assigned_at、oauth_updated_at 全相同，结果中的 credential 摘要也匹配。每行 attempts=1，三个透明网关请求的精确关联 gateway attempts=1。两个原题的 request 与 predicted body hash 都是 `763f3380…df7f5`，补足了旧 candidate 注入策略不同这一混杂；不因此宣称实际出站头、IPC 或 TLS wire 相同。

新结果使原题配对从“缺失”变为“有直接观察但两端均未通过”，无须为了取得已有事实再重复同一批调用。控制题支持“形状选择能力的明确表述影响本次解法”，不能作为原题已恢复的证明，也不能外推成账号间稳定能力相同。新 AT 即使 owner 相同也应分新 block，不能与旧 epoch 拼成配对。四次计划已结束，执行回执及 `semantics-final-runtime.json` 确认 AT 快照删除、现网 ready、in-flight=0，无临时 receiver 凭据残留。

生产 service 的 `SSL_CERT_FILE` 与独立 `systemd-run` probe 的环境是两个来源；采样早于生产环境修复，不能据此认定 q4 当时也走 custom CA/rustls。主任务的历史 unit 查询现已保存为 `q4-historical-unit.json`，结果是 `LoadState=not-found`、`historical_process_environ_available=false`，不能反向证明终止进程的 CA 环境。standalone 启动路径不读取 `production.env`，但没有保留的历史 `/proc` 环境就仍是未观测；当前 manager 的 CA 为空也不等于历史值为空。这里不把主机相同等同为进程环境相同。新四次请求的每份 `*-process.json` 均保存完整显式环境白名单（PATH、LANG、配置路径、helper、cli_request），没有 CA 或代理变量；helper 为 `f72e5c0f…b556`，probe 为 `b1e41d73…bd42`。这补足新样本启动环境证据，不能追溯改写旧样本环境。

### 2. 单样本实际 IPC 已补齐，网络字节与生产调度证明仍有限

主任务后续使用透明 Unix socket relay，在成功向真实 Rust helper socket 写入字节后计算摘要。`dispatch-live-q4/` 的一次请求已提供实际 IPC body、AT/workspace 哈希与请求 ID 关联，并核对 `/proc/<pid>/exe` 是固定 helper；与 local request/predicted 摘要一致。HTTP 200 和 completed 独立证明这次请求得到了上游完整响应，答案仍为 29。两条 helper 生命周期（含启动验证）均闭合，退出 0、已回收、未强杀，socket 已移除；最终四个采样 PID 都不存在。详情见[实际发送续查](../CODEX-QUALITY-DISPATCH-2026-09-24.md)。

这补足了这个新样本的实际发送边界，不向历史样本补造证据。IPC 位于 Rust 压缩/TLS 之前；owner scope 不在 IPC 里，明确保持未观测；生产 arm 的实际调度要求仍如下所述。旧 harness 的 predicted 字段也保持原名。

`gateway_quality_live_test.go` 在调用前以本地 `applyGatewayPolicy`/`prepareBody` 得到 `predicted_upstream_body_sha256`，随后由实际网关重新准备请求。这个字段是预测，不能改名为 sent 或 wire；当 arm 为 production 时，它还使用本地 settings，`credential_sha256` 也来自预备 snapshot，而不是远端实际选择账号的回执。已有记录没有被审计成实际生产 wire capture。

已有 Q01 loopback 断言可以证明覆盖的转换规则；真实 native 请求的最便宜决定边界是 Go→Rust IPC writer 或受限的 IPC relay，记录实际 body 的哈希、实际 Authorization 的不可逆摘要、owner scope、request ID 和原样转发结果。只向 `p.Client.Transport` 加 wrapper 无效：native client factory 直接走 `p.NativeHTTP`。强行切到 Go transport 会改变实验对象。当前不建议为了报告引入生产公共 hook；无法在不改路径的条件下取得该证据时，应继续明确写 predicted。

对 production arm，应从与 `X-Client-Request-Id` 精确关联的实际 attempt 记录验证账号与次数；本地 snapshot 标签不能代替命中证据。对单账号隔离 candidate，已知唯一账号、禁用刷新及实际凭据 scope 的证据较强，但仍不要跨层声称是 TLS wire 明文捕获。

### 3. 服务端模型元数据与内部路由之间还缺直接关联

固定 CLI 的 `codex-api/src/sse/responses.rs` 会读取 HTTP `openai-model` 和 SSE `response.headers`/顶层 `headers` 的模型信息；这与最终 `response.model` 是不同来源。网关 `gateway_observation.go` 已处理此优先级。旧 live harness callback 只数 event type，历史 direct 没有保留 SSE header 模型序列；官方 probe 的 JSONL 也明确 `model_returned=null`、`transport_observed=null`。因此目录相同、无 reroute 通知不能证明真实执行模型完全相同。

本专项已在 test-only `gateway_quality_metadata_test.go` 实现该最小补证，接入 live callback；复用 `gatewayObservation` 与 `observeGatewayHeaders`，分别记录 `http_headers`、`sse_headers`、`sse_response`。当前未来采集使用 **schema 2**：`observed_value_changes` 表示可见字段出现过不同值；`same_event_source_differences` 只表示同一 SSE 事件同时提供的 header model 与 body model 标签不同。两者都不宣称服务端自相矛盾、后台换档或切换集群。每条信号还保存白名单事件类型、生命周期阶段及首次事件序号，便于区分 HTTP 头、created、in_progress、terminal 和 metadata。

最多 32 条去重信号，超出明确标记截断；去重保留跨生命周期阶段的相同值，不把重复输出事件无限累计。不同 response identity 在 header/body 解析前拒绝合并。请求 ID 仅保存生产 parser 归一化后的 SHA-256，不能反解成原始上游支持工单 ID；auth、cookie、完整 catalog、加密 reasoning 与任意 metadata 不进入新增字段。`predicted_upstream_body_sha256` 命名保持，生产/native 路由没有改动。

四个离线测试覆盖 HTTP/SSE/正文模型来源差异、SSE response.headers 优先级、ID 哈希、不复制任意 metadata、foreign response 拒绝、无效类型、重复去重、有界截断及 `created:auto → in_progress:auto → completed:default`。最后一种只报告 `observed_value_changes.service_tier=true`，不报告同事件来源差异，更不称算力被降低。HTTP header 与后续 SSE model 不同也只记可见变化，只有同事件同时出现的两个模型来源才记 `same_event_source_differences.model=true`。关联 observation 与结构化错误测试已通过。

已执行 live 的 schema 1 产物和旧探针 `probe-semantics.test` 保留原样及原哈希。schema 1 的 `conflicts.service_tier=true` 当时只表示观察过多个不同字符串，**不能解释为真正冲突**；新运行观察到的 `auto → default` 正是需要保留事件阶段的反例。本次修正没有重跑 live、没有覆盖旧 hash，也没有为历史结果补造缺失的事件阶段。新字段只为之后的 live 请求提供更准确证据，仍不证明隐藏集群。

新增的四份实际结果已补上可见 metadata 这一层证据：四次均只有 `sse_response` 来源的 `gpt-5.6-sol`、`medium`，tier 先观察到 `auto` 再观察到 `default`；没有独立 HTTP/SSE header model 值。每次保存一个 HTTP request ID 的归一化哈希，`response_identity_conflict=false`、`signals_dropped=0`、`capture_complete=true`。这些是 collector 当次有限观测，不是“缺失 header 也证明相同”，也没有准确保存 tier 各值对应的事件阶段。schema 2 为未来补上阶段，不补造旧字段。metadata 缺口因此部分关闭，服务端执行/集群证据缺口仍未关闭。

若要验证“标记→弱集群”因果，需要上游按真实 request ID 提供：当时的账号/实验分组或风险状态、router 决策、实际模型构建及执行预算、目标 backend/cluster ID，以及这些字段的权威语义。当前保存的是 request ID 哈希，实际支持工单还需独立受限记录建立原始 ID 对应关系；不能从哈希恢复。仅给一个 opaque cookie/hash 仍不够。此处是具体外部证据缺口，不能拿它宣布整个网关目标完成；实际出站捕获缺口也保留，不为此添加额外生产 hook。

## 完成与阻塞判定

上轮有真实环境修复、live 验收及目录对照；本轮新增同账号原题配对、两个语义控制、明确进程环境及真实 metadata，并纠正采集命名，属于 progress，不是无进展重复。本审计不把未证明的质量恢复标作 complete，也不凭上游内部信息未知就认定满足连续三轮无可推进步骤的 blocked 条件。836 项 Linux 回归通过证明代码回归范围；其中 live、CLI、WSS TLS 等 21 项明确跳过，不能用总绿灯代替上述缺口。

本报告不提出新生产代码 bug：本次复核发现的是三个具体证据缺口，并补齐第三项的 test-only 采集能力，避免为无法证明的内部路由机制继续盲目修改 UA、cipher、cookie、plan 或答案。现有明确修复的有效性与质量目标未完成这两个结论同时成立。

原始证据目录为 `.local/quality-20260924/` 与 `.local/gateway-audit-20260924/`；上述路径按工作区根解析。离线重评原题的一条命令，不发送模型请求：

```powershell
python tools/quality_semantics_report.py .local/quality-20260924/semantics-live-q4 --output .local/quality-20260924/semantics-live-q4-report.json
```
