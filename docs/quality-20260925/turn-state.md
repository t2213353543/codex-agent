# 多轮 turn-state 与回退状态保真检查

日期：2026-09-25。基准为固定 CLI 0.155.0 源码及 `parse-fidelity-continuation/build-manifest.json` 所记录的 gateway `30634195e375a51fe3372bac14fdf5f86759fa148ef444afbd3bd534dadf95be` 代码。只新增测试，没有生产代码修改、SSH 或模型调用。

## 本次结果

未在本次普通多轮状态路径发现新的漏传/串轮反证。真实 TCP HTTP 和 WebSocket 回环显示：工具结果续接、同 owner 的 401 恢复、同 session 的新用户回合，以及 WS 426 拒绝后的 HTTP 回退，按客户端提供的状态保持边界。这里不把“未发现”提升为所有状态/所有服务端情况完全一致。

新增 `gateway_research_turn_state_test.go` 提供三个组合场景，区别于先前单个 header 复制或正文大小测试：

- HTTP 首请求获得 state A 并收到合成 function call；工具结果续接携带 A。上游先返回带另一 state 的 401，网关刷新后重试仍携带原 A、相同 turn metadata/session/thread/conversation/cache/routing hint 和完全相同 prepared body，仅账号 bearer 更新。成功响应自己的新 state 正常回传，401 上的值没有注入重试。
- 同一 session/cache 下的新用户回合不提供旧 turn state，实际上游也没有该旧值。HTTP 200 后的 `response.failed` 原样返回，不触发 OAuth 或自动重放；下一新回合继续无旧状态。
- WS 两个独立用户回合各建立新连接，每个连接发首请求和单个工具结果增量。实际帧保留 `previous_response_id`、tool call ID、cache key 和完整 `client_metadata`。首 `response.metadata` 提供 state，fixture 客户端模拟 CLI OnceLock 在同回合保留首值；后续 metadata 的不同值不被 gateway 偷换进出站请求。新连接首帧不携带旧回合 state。
- WS 拒绝返回 426 与拒绝专属 state，随后客户端发完整 HTTP input。网关没有把拒绝 state 或 `previous_response_id` 塞入 HTTP 请求，只有一次 WS 握手和一次 HTTP 请求；HTTP 接受后的 state 单独回传。

所有请求/响应都使用合成身份与内容。工具结果由 fixture 客户端构造，测试不声称在本次执行了官方 CLI 工具程序。已有官方 CLI 端到端工具续接与 fallback 用例是源码交叉参考，本任务没有把它们未运行的结果计入本次通过。

## CLI 源码规则

2026-09-27补充：下表涉及底层API的能力，固定0.155.0 core实际WS建连时传入的turn-state参数是 `None`，不会用101响应头填充该回合OnceLock；流内 `response.metadata` 才是此路径的状态来源。本轮已进一步逐调用核对，见[新的请求环境复核](../CODEX-REQUEST-ENVIRONMENT-REVIEW-2026-09-27.md)。不改变下述历史合成测试的执行结果或范围。

读取 `.local/codex-source-0.155.0/codex-rs/core/src/client.rs`：

| 源码位置 | 规则 | 本次对应检查 |
| --- | --- | --- |
| 约 257–283 行 `ModelClientSession` | turn-state 属于一个用户回合，OnceLock 首次取得后不改；新回合创建新 session，不跨回合重放 | 同回合工具续接使用 A；新回合无 A |
| 约 1404 行 `websocket_connection` | auth owner generation 改变时清空 turn_state 和 socket 状态 | 本次只测同 owner 凭据恢复；不将 owner 更换测试冒称已覆盖 |
| 约 1217 行 `build_responses_options` | HTTP 附带 session/thread、compatibility headers 和当前 turn_state | HTTP 四类 opaque/header 捕获与 401 重试比较 |
| 约 1764 行 | WS turn_state 放进 `client_metadata["x-codex-turn-state"]` | WS 帧原文及 metadata 首值续接 |
| 约 1783–1806 行 | WS 增量请求明确携带 previous_response_id 与新增 input；warmup 可 generate=false | 单个工具结果增量不被扩成全历史；warmup 已有专门测试，本次不重复计数 |
| 约 1547、1701 行 | routing hint 根据实际 model/tier 生成；WS hint 是建连时的 header | 显式 matching hint 与 future 扩展原样转发；不在持久 socket 中凭后续帧重写握手 |

`codex-api/src/sse/responses.rs` 约 65 行将已接受 HTTP 响应的 `x-codex-turn-state` 设置到 OnceLock。`core/src/responses_metadata.rs` 约 337 行把完整 turn metadata 放入 client_metadata；约 344 行 compatibility header 有意省略不受限的工具 inventory。本任务不要求两者内容完全相同，也不让 gateway 用 header 覆盖正文 metadata。

`codex-api/src/requests/headers.rs` 的正式 session/thread header 是 `session-id` 与 `thread-id`。测试还保留 gateway 已有 `conversation_id` 兼容字段，但不把它说成官方当前必须字段。

## Gateway 边界核对

`copyCodexRequestHeaders` 保留当前请求的 x-codex 扩展；`doAccountRequest` 在唯一允许的显式 401 恢复中 clone 原请求、重建 body 并更新账号 bearer/account header，没有从被拒绝响应复制 turn-state。网关不维护跨 HTTP 请求的 turn-state 缓存，所以不应主动记忆上次响应值。

`dialGatewayWebSocketPrepared` 复制建连请求 headers；上游帧中的 response metadata 原始透传。WS 后续 create 的 `client_metadata` 随该帧正文处理，握手 header 不被当成可变的每帧元数据。明确 session 的预连接可以原样把 101 response header 返回下游；没有 header、等待正文才建连的兼容路径不能补写已经发送的 handshake，本次不扩大或伪造这个契约。

回退是客户端发起的新 HTTP 请求，gateway 不在收到 426 后自行转发推理。测试分别计数 handshake 和 POST，避免把一次客户端选择当成网关自动重放。

## 验证与复跑

Windows 本地针对性回归：**17 个顶层测试通过、1 项明确跳过、0 失败，1.012 秒**；含子项 49 pass、3 skip。日志 `.local/research-20260925/turn-state-green.jsonl`。新增 HTTP 和 WS 两项均提供 Linux 真实 Rust helper 分支，WS 使用已有本地测试 CA；Windows 对这些分支明确跳过，不计作 TLS/native 成功。

```powershell
go test . -run '^TestResearchTurn' -count=1 -timeout=90s -v
```

测试文件 SHA256：`19522a71d3692691babea4fc60d58ea27e843fba7b9a216297a403d4b87c41f5`。

没有产生需要修复的生产差异，因此没有以改写 turn-state、清空用户 metadata、重放失败请求或变更模型来追求某个题目答案。本任务不证明 q4 实际请求的模型质量恢复，也不推断服务端隐藏路由。
