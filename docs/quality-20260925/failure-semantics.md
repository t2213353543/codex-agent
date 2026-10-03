# HTTP 200 后的失败归属与冷却污染修复

后续更新：本文记录的“HTTP首failed/incomplete即结束”保留差异已在2026-09-26的后续版本修正；当前HTTP延后错误并等待合法完成或EOF，WS即时错误规则保持。本文仍作为当时修复证据保留，当前行为见[HTTP终态顺序修复](../CODEX-HTTP-TERMINAL-SEQUENCE-FIX-2026-09-26.md)。

日期：2026-09-25。子任务只执行本地源码研究及 loopback 测试；没有 SSH、没有真实模型请求，未编辑质量实验 harness。

## 新发现：被观测层排除的外来失败仍改变账号调度

对照当前生产链路，HTTP 响应已返回 200 后，`observeEvent` 同时调用 metadata/usage 观察器与 `observeGatewayCooldownEvent`。前者拒绝不同 `response_id` 的字段进入当前记录；后者原来只读取限流 code/status，完全没有核对这些失败事件属于哪一个逻辑响应。

新增真实 TCP 上游和真实 TCP 下游 loopback 建立以下链路：先发送并 flush `response.created`，ID 为 A；再发送带 `rate_limit_exceeded` 与 `Retry-After:60` 的失败，身份为 B。第一次 SSE 请求仍是 200、原始字节完全相同；JSON 适配仍是 502。随后同账号、同模型的一条独立请求却被网关本地返回 429，实际到上游的请求数仍为 1。这证明错误的副作用，不依赖日志推测或模型答案。

修复前的 6 个失败场景是 JSON/SSE 两种模式各覆盖：

- `response.failed.response.id=B`，而前面已建立 A。
- 同一事件顶层 `response_id=A` 与嵌套 `response.id=B` 冲突。
- 顶层 `error.response_id=B`，而前面已建立 A。

同 ID 的失败、没有 ID 的普通 `rate_limit_exceeded` 两组控制保持应有冷却。失败前日志为 `.local/research-20260925/failure-semantics-red.log`。这不是旧 WS stale-response 测试的复述：本次是单条 HTTP 流的明确响应归属与后续独立请求的调度状态。

## 修复

每条已接受的 HTTP 响应创建独立 `gatewayHTTPCooldownIdentity`。它从真实事件读取顶层与嵌套 response ID，拒绝显式冲突，保留第一次有效身份；后续不同 ID 不允许更新当前账号冷却。身份只保存原始字符串的 SHA256，不保存正文或可变长 ID，也不使用被截断/脱敏的 metric 字符串比较，因此两个具有 1024 字节以上共同前缀的不同 ID 仍然隔离。

没有 ID 的 legacy quota error 仍视为该 HTTP 请求的无名错误，保留原有冷却行为；失败是首个事件、此前没有 created 的兼容源也仍有效。错误消息不参与限流分类，依旧只采用结构化 code/status 和 Retry-After。

该过滤只改变冷却副作用。streaming 客户端仍收到原始 SSE 字节，已经发送的 HTTP 200 不会被改成第二份 JSON 错误；JSON 客户端仍采用现有的失败事件 502 适配。foreign 失败的 usage 不进入当前响应记录，不将异常失败当成功，也不重发被接受的推理请求。

## JSON 字段大小写：共享限流解析的补充修复

Q13 对 WS 的并行研究指出 Go `encoding/json` struct 解码接受大小写别名，但 Rust serde 的协议成员区分大小写。因此这次身份过滤必须避免新引入相同问题。原 `gatewayEventRateLimit` 也会让未知 `Type`、`STREAM_ID`、`RESPONSE_ID`、`response.ID` 覆盖标准小写字段，改变限流分类或归属。

已有 helper `gatewayWSDecodeExact` 由 Q13 在 `gateway_ws_event.go` 提供，本任务复用于 HTTP 身份检查及共享限流事件的 `type`、`stream_id`、`response_id`、`response.id`。这 7 个大小写干扰用例先执行失败，再完成修复：`.local/research-20260925/failure-case-shadow-red.log`。

根任务与 Q13 复核进一步发现：如果只精确读取 ID，而 nested error 仍经原 struct 从整个 payload 解码，未知 `Response` 会污染 error，再被错误归到标准 `response.id=A`。最终实现同时从精确的小写 `response` 取精确的小写 `error` 容器，再沿用原 error.code/status 类型规则；不允许跨容器拼接。新增 4 个失败前用例覆盖未知 `Response` 添加/隐藏限流、未知 nested `Error` 添加限流，以及顶层/嵌套合法字段的互相矛盾。日志 `.local/research-20260925/failure-wrapper-shadow-red.log`。

HTTP header 名称的大小写规则没有改变，`Retry-After` 仍按 HTTP 规则不区分大小写。其余既有 error/code/status 兼容解析未扩展修改。大小写干扰数据是构造的回归 fixture，不表示官方 CLI 正常产生这种输入。

## CLI 0.155.0 对照与保留差异

读取本地固定源码 `.local/codex-source-0.155.0/codex-rs/codex-api/src/sse/responses.rs`：`response.failed` 在约 417 行按结构化 code 映射 context window、quota、capacity、retryable 等错误；`response.incomplete` 约 472 行映射 Stream 错误；`process_sse_with_treatment` 约 568 行保存事件错误，在 EOF 时报告，遇到 `Completed` 则结束。

网关在已经 HTTP 200 的 SSE 上不发起 401 recovery 或服务端重试，失败原文仍交给 CLI 处理；本次没有添加“429 一律可重试”之类推断。structural cooldown 是网关下一请求的调度约束，必须正确归属，不能由其他响应的错误触发。

旧 20 项审计已经明确记录的适配差异继续保留，本次不把它们宣称为新修复或完全一致：CLI 的 failed/incomplete 后可以继续读到 Completed，而网关既有策略以首 terminal 为结果；JSON collector 的无空行 EOF 兼容比 CLI 的事件派发规则宽松；incomplete 可返回 partial JSON 但计量记 error；顶层 `error` 是网关保留的诊断扩展。相关位置与解释见 `docs/audit-20260924/08-sse-response-assembly.md`。这次新修复不改变这些终态/EOF策略。

## 测试与结果

`gateway_research_failure_test.go` 新增三个顶层测试：真实 HTTP loopback 的 6 类错误 × JSON/SSE、原始 identity 的长 ID/空白/Unicode/匿名兼容、共享协议字段精确读取及四个跨容器冲突用例。真实 loopback 还提供 Linux 真实 Rust helper 模式，复用相同上下游与第二请求计数，不把 Go transport 的通过算作 native 通过。

最终 Windows 针对性回归包含新测试及既有 SSE/collector/output/冷却/observation 用例：**71 个顶层测试通过、2 个明确跳过、0 失败，1.766 秒**；含子项 244 pass、3 skip。日志 `.local/research-20260925/failure-semantics-green-final.jsonl`。跳过项为 Redis 两项及真实 native 分支（后者是子项）；Linux race/Redis/真实 helper 回归由根任务统一执行，结果须以对应实际日志为准。

复跑命令：

```powershell
go test . -run '^TestResearchFailure' -count=1 -timeout=90s -v
```

Linux 设置 `CODEX_NATIVE_HTTP_TEST_BIN` 为固定真实 Rust helper 后，同一命令启用 native loopback 分支；不访问模型端点。

修改范围仅 `gateway.go` 的 HTTP observeEvent 连接点、`gateway_cooldown.go` 的 HTTP 归属过滤/共享协议字段解析、上述新增测试，以及本说明。未改质量测试评分、提示词、真实账号或线上运行时。本次不能证明此前 q4 错答来自这一缺陷，也不能证明服务端存在特定算力集群路由。

最终冻结 SHA256：

- `gateway.go`：`26544a61ea7fc548ea3115e07e66a5d4b005b27a0a153c1e31d14c8ffa02b667`
- `gateway_cooldown.go`：`cc3bf88b64cf2a8b192969b5be17a49a6af964876e780283dd62008f092c636a`
- `gateway_research_failure_test.go`：`6a0e4033a1508889182a546254ad0e60a693953d1c2b0d2767d64920bb0306ba`

主任务最终回填：Linux race/PG/Redis/native 全套 **864 顶级通过**，此测试的真实 Rust 分支执行成功；固定 CLI 独立 **7 项通过**。后端 `ac25b0f0…d3d82` 已上线，HTTP models 200、WS 101、关闭后 in-flight=0。完整证据与质量结论边界见[最终上线记录](../CODEX-GATEWAY-STREAM-RESEARCH-2026-09-25.md)。
