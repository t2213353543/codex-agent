# 质量续查：完整官方参考与实际原生发送边界

2026-09-24。**完整官方 CLI 在同一 q4 账号上无告警完成原题，答案 29；另一次透明网关请求的实际 IPC 正文、凭据及模型参数核对一致，上游完整返回 29。原题恢复目标仍未达到，本轮没有找到新的生产降档缺陷。**

此前的 [同账号语义对照](CODEX-QUALITY-SEMANTICS-2026-09-24.md)仍保留原样。本轮只新增一次官方 CLI 调用和一次用于核对实际发送的网关调用，没有按答案重试。两次均复核 q4 原有 user/workspace、AT 摘要、assigned_at 与 OAuth 更新时间，和此前语义实验处于同一凭据窗口；不复制 RT、不刷新、不改 28765 推送地点。

## 1. 官方参考终于得到干净、可评分的原题答案

使用完整固定官方 0.155.0 平台包，CLI SHA256 为 `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`，配套 code-mode-host 存在且摘要匹配。隔离 home、严格配置、内置 `openai` provider；题面保持原文，没有注入答案或替换模型目录。调用 `gpt-5.6-sol / medium`。

实际结果：`completed`、`reference_clean=true`、退出码 0、65.573 秒，最终答案 **29**。保存的事件包含一次 turn.completed，没有启动 warning、error、可见工具 item 或 model reroute；报告和保留的最终文本事件逐字一致。模型目录正常载入，目标模型仍为 `code_mode_only`。

这与旧的“缺 host 的单文件 CLI”或“完整 CLI 403/429/容量失败”不同，是新的有效参考。它证明在该次运行中，**绕过本网关也会得到 29**。不能据此排除更早账号历史、出口或未观测的服务端状态，也不能反推某一种内部集群。

限制继续保留：JSONL 不提供真实 transport/response model，所以 `transport_observed` 与 `model_returned` 仍是 null；默认 WS 配置不等于已经抓到 WS 握手。关闭可配置工具和没有可见工具事件也不等于已经证明工具表为空。CLI 的 catalog 指令、工具与上下文同手写 Responses 探针不同，二者不是只改变 TLS 的 A/B。

证据：`.local/quality-20260924/official-q4-fresh/`，及 `official-q4-fresh-review.json` 的 11 项核对。参考的临时凭据、auth/home 已清理。

## 2. 从“预测正文”推进到实际 Go→Rust IPC

此前 harness 的 `predicted_upstream_body_sha256` 仅是本地重新准备的正文；本轮保留此命名，增加独立的本地 Unix socket 透明中继。中继接收 Go 原始 IPC，只改启动配置中的本地 socket 名；请求 head/body 与响应帧不重新序列化，HTTP、TLS、cookie、压缩及上游连接仍由同一个真实 Rust helper 处理。

只有 `backend.send` 成功接受的字节才进入观察摘要。这证明本地 socket 接受了哪些字节，**不单凭摘要证明 Rust 已解析或上游已接受**；后者由另行记录的 HTTP 200 与 completed 终态支持。本轮不是 TLS 抓包，正文摘要位于 Rust 压缩/TLS 之前。

一次原题透明网关请求的实际结果：

| 核验项 | 证据 |
|---|---|
| 请求次数 | 恰好 1 条实际 IPC 请求；网关关联 attempt 也是 1 |
| 实际正文 | 925 字节；完整转发，SHA256 为 `763f3380b4807815c1dab11c2baf7586e4466e3bcc4a4e668c658f0be69df7f5`，等于输入与 predicted 摘要 |
| 认证及身份 | 实际 Authorization 中 AT 摘要等于守卫快照；workspace 在服务器内核对相同；实际 client request ID 与响应记录关联 |
| 关键模型参数 | 实际 body model=`gpt-5.6-sol`、reasoning.effort=`medium`，没有额外指定 service_tier |
| 路由提示 | 实际 IPC 头包含 `x-codex-routing-hint: model=gpt-5.6-sol` |
| 真实进程 | `/proc/<pid>/exe` 摘要等于当前生产 Rust helper `f72e5c0f…b556`；没有 Go HTTP fallback |
| 环境与生命周期 | `cli_request`；实际 helper 的三个 CA 覆盖变量均不存在。共两个 helper（含无请求的启动校验），均退出 0、已回收、未强杀，内外 socket 都已删除 |
| 上游完成结果 | HTTP 200、completed、26.082 秒，最终答案 **29** |

`go_account_scope.observed=false` 保留：Go 的 owner scope 没有传入 IPC，不能从 helper PID 或 token 摘要伪装成观测到这个字段。该样本来自隔离单账号网关，不是生产多账号调度的全面回执；历史 production arm 仍不能用预备 snapshot 代替实际选中账号的证明。

捕获日志不含原始凭据或请求正文，只记录摘要及白名单控制字段。实验自身另有 0600 临时凭据文件和保留原题面的私有 probe 配置；临时凭据已删除，不能泛称整个实验从未将凭据写入临时文件。

证据：`.local/quality-20260924/dispatch-live-q4/`、`dispatch-live-q4-review.json`。离线验证共 18 项检查通过。探针 SHA256 为 `502d9e84c998e8bace5a9e38e7837764c546054ba4d75191601b7c530ec84b89`；本次捕获工具 SHA256 为 `2bd345da020e8749bec42bb6759a5583a9fa4d6abdd3a98477b4774dc699de68`，源码快照保存在 `capture-live-v1/`，不被之后的工具修正覆盖。

## 3. 请求一致性与诊断工具修正

[Q13 源码复核](quality-20260924/13-cli-request-fidelity.md)确认：手写 direct 不带 routing hint，而 transparent 会补齐；CLI 的 exec 身份、thread header、catalog 指令、工具、include、cache、WS/fallback/压缩状态也与手写 HTTP 不同。没有找到新的网关删除这些已传入字段、降低 effort 或替换 model 的路径。已有 loopback HTTP/WS 字段保留测试通过；不能把这些参考差异直接列作生产 bug。

实际 schema 2 观测也补足了前轮的阶段信息：本次 created/in_progress 为 `service_tier=auto`，completed 为 `default`；model 与 effort 各阶段保持相同，没有同一事件的不同模型来源。这里记录可见值变化，不称服务端矛盾或后台降档；不为旧 schema 1 样本补造阶段。

修复了一处评分器漏判：官方答案采用末尾 `\boxed{29\text{个}}`，旧解析器会记为无法识别。先复现失败，再增加保守的终末方框整数解析；多个、条件、冲突及非最终方框不作为确定答案。原题答案键仍为 21，官方结果因此记录为 **29 / wrong_answer**，原始文本没有改动。评分器 50 项、语义工具 45 项回归通过。

独立复核还发现捕获工具的日志写盘异常可能中断中继。这是诊断工具的异常路径，本次捕获日志完整、生命周期闭合，没有触发该问题。新版已隔离观测错误：转发继续，捕获永久失效，尽力写 `.invalid` 标记并在自然结束后返回失败；缺正常退出记录或出现失效标记时不能采用捕获结果。Linux **28/28 测试通过**，含写盘失败、中途回调异常、不提前 EOF、后续请求继续转发的故障注入；新包装器的 Go/Rust 四项路由回归也通过。新版没有重跑模型，旧捕获工具、实际样本及其摘要保持不变。具体版本边界见 [Q12](quality-20260924/12-native-dispatch.md)。

## 4. 验证、清理及剩余目标

本次实际捕获前，Linux Python 3.9 **23 项全部通过、零跳过**，含真实 Rust 本地 HTTP、并发、短写、截断、EOF/SIGTERM 回收。再使用同一包装器执行 Go `TestGatewayNativeActualRoutesAndOAuth`，SSE、JSON、compact、models 四个子项全部通过。它们是合成 loopback，不计模型调用。

最终只读检查：生产 ready、账号数 1、in-flight 0；CLI 和 IPC 两个临时凭据快照均已删除，四个采样 PID 均不存在，旧 receiver 无凭据残留。网关二进制与 helper 未替换，CA 覆盖变量仍不存在。此轮生产源码没有新改动，前述 836 项完整 Linux 回归仍对应未改变的生产文件，不能把新增诊断测试计入旧总数。

本轮关闭了两个具体证据缺口：异常账号有干净完整 CLI 答案，单次真实网关请求有实际 IPC 正文和认证证据。**原题仍是 29，恢复没有得到证明；内部风险标记、路由决策、实际执行模型及算力预算仍没有权威证据。** 不通过改答案、改题目、换正常账号、按结果重试或猜测指纹来宣布完成。

一条命令离线重核实际发送记录，不发网络请求：

```powershell
python .local/quality-20260924/verify_dispatch.py
```
