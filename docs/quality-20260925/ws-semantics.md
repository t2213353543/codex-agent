# WS 多轮续接研究：精确协议字段与跨轮授权

2026-09-25。本次用实际客户端 WS → 网关 → loopback 上游 WS 做多轮协议实验。**正常 CLI 形态的 warmup、空增量续接、opaque reasoning、工具结果、瘦 terminal 和跨 lane 控制均保真；另复现并修复了字段大小写折叠导致的跨轮授权绕过和 admission 不释放。** 未 SSH、未调用真实模型，也没有修改提示题面。

发现的是网关协议/状态缺陷。官方 CLI 不会自行生成本次触发用的 `Type`、`STREAM_ID` 未知扩展，因此不把该异常输入缺陷作为普通 CLI 账号糖果题 29 的已证根因。正常请求控制实验与异常扩展实验分别保留。

## 正常 CLI 两轮以上的独立控制

固定 0.155.0 源码 `core/src/client.rs:1298` 调用 `get_incremental_items(..., allow_empty_delta=true)`；`:1796` 构造 `ResponseCreateWsRequest`，当逻辑 input 没有新 suffix 时允许 `previous_response_id + input=[]`，warmup 则设 `generate=false`。`:1758` 另构造 client_metadata/turn state。`codex-api/src/endpoint/responses_websocket.rs:286` 以 mutex 串行消费单个连接的官方 stream；网关 named lane 是兼容扩展，不能声称官方 CLI 发出多个 lane。

新增 `TestResearchWSSemanticsCanonicalWarmupEmptyDeltaAndLanes` 的真实序列：

1. main warmup 输入包含既有 encrypted reasoning，generate=false；完成后释放 slot。
2. main 以 previous_response_id 和空 input 续接；并发插入独立 side lane。
3. main 收到完整 function_call/message done 及同 ID 瘦 terminal；所有事件逐字保留，不经过 JSON 聚合修复。
4. 下一 main 回合只 append 工具输出和 opaque reasoning，包含大整数/null 未知字段；旧 completed 重发不能消耗新回合或 side lane。
5. side/main 各自完成，四个合法 turn 恰好四条记录，in-flight=0。

这个控制在修改前已通过，证明所覆盖的正常 WS 请求不是先前 JSON 瘦终态缺陷的另一实例。没有根据通过结果外推全部 WS 实现或后台质量。

## 新触发条件与 fail-before

Go `encoding/json` 解码到 struct 会把大小写不同的 JSON key 也匹配到已标记字段；协议字段及 serde 对应字段是精确大小写。网关既保留原始消息发给上游，又用折叠后的字段决定 `response.create` 是否走每轮授权、模型校验和 admission，导致两侧解释不同。

`TestResearchWSSemanticsTypeShadowCannotSkipNextTurnAuthorization`：完成合法 warmup 后，实际禁用当前账号，再发送第二轮合法 lowercase `type=response.create`、previous_response_id、input=[]，附加未知 `Type=response.cancel` 或 `TYPE=ping`。旧 prepare 将其认作非 create，直接转发，实际上游按 lowercase type 执行第二轮。canonical 对照返回 403，异常分支却 completed；新代码对这些扩展均执行原本需要的授权并拒绝。未知 Type 为对象等无关类型也不能干扰已知字段读取。

`TestResearchWSSemanticsLaneShadowCannotStrandEmptyContinuation`：warmup 使用 lowercase `stream_id=main` 并附加未知 `STREAM_ID=shadow`。旧网关把 turn 登记到 shadow，上游按 main 完成；网关保留一个 admission slot，账号 concurrency=1 时下一合法空增量被本地 429。新代码在 main 登记并正常释放，第二轮送达。

红日志 `.local/quality-20260925/ws-semantics/fail-before.jsonl`：**1 个顶级正常控制通过、2 个顶级异常边界失败**，包含子项 8 条终态事件。两组失败均发生在真实双 WS 路径，不是只测试 helper 返回值。

## 最小生产修复范围

- 新 `gateway_ws_event.go::gatewayWSDecodeExact` 只取指定的精确 JSON member。用于 WS create 类型、model/stream_id/previous_response_id/generate 校验、错误 stream_id、turn 入队/归属和观测关联。不更改原 payload、不剥离扩展、不将同名不同大小写视作相同协议字段。
- `gateway_websocket.go::observe` 对顶层 response_id 与 nested response.id 同时存在但不等的事件拒绝归属；原事件仍转发，不能由矛盾事件提前释放某一 lane。新增真实两 lane 例验证随后各自正确终态仍能完成。
- WS 专用 observation view 只屏蔽会影响共享 Go struct 观测的 envelope 大小写别名，不触碰 tool/history 深层字段。它仅供日志读取；未知字段和原始请求/响应仍逐字经过 socket。canonical 路径无需重新编码，但仍有一次 map 解码成本，当前优先正确性，未增加泛优化。
- `response_events.go` 仅抽取 `terminalResponseEvent` 公共分类 helper。HTTP 的原解析不变；WS 使用精确 envelope/status 再调用相同终态逻辑，避免复制一整套 terminal switch。上游 top-level error 的 param 与未知字段保留原 payload，已有错误诊断不会被 observation view 替换。
- shared cooldown 精确 type/stream_id/response identity 由同时工作的 Q12 在其独占 `gateway_cooldown.go` 整合；本专项未并发修改该文件。其 HTTP 外来 response 归属修复另有专项证据。

没有顺手引入全部项目 JSON 反序列化规则，也没有更改 exact 重复 key 的既有解析策略。类型明确为空/缺失仍按原有参数契约处理，不通过添加默认 input 或重放历史来“修好”请求。

## 验证与冻结

新增 6 个顶级测试包括上述正常控制、两个 fail-before 场景、上游字段别名不污染 warmup/success 记录、矛盾 response IDs 不能完成 lane、原始错误 payload 保留。相关旧 WS/session/response identity、Q01、JSON terminal、collector 和 Q12 cooldown 回归一起运行：**64 个顶级通过、0 失败、0 跳过**；含子项 156 条，package 12.311 秒。日志为 `.local/quality-20260925/ws-semantics/green-associated.jsonl`。

```powershell
go test ./... -run '^(TestResearchWSSemantics|TestGatewayWebSocket|TestGatewayWSSession|TestGatewayWSResponse|TestQualityQ01WebSocket|TestQualityQ01RoutingHintWebSocket|TestSourceWS|TestResponse|TestCollectedResponse|TestResearchFailure|TestAuditOutput)' -count=1 -timeout=90s
```

交给主任务集成时的 SHA-256：

| 文件 | SHA-256 |
| --- | --- |
| gateway_websocket.go | `52cef82c6f67d4fea95304017124b9a30cb31e833c3a66ac8c7fc751d53eea12` |
| gateway_ws_event.go | `90999add47c65041d70f2b246ba1b08d3a6738070ff0edce1ef4327b0635391b` |
| response_events.go | `d53af79ee3c76257a9005955753e246f51c8309a39cdaaee0c89c67dd45d27ad` |
| gateway_research_ws_semantics_test.go | `ae8a80992799e0f07c6bbc7df262b2ff508781357c523fa243489dd5e5e5dee6` |

这是局部验证结果，不代替主任务的全量 race、Linux/native、真实 CLI 与部署验收；未在本报告中宣称已上线或原账号质量恢复。

主任务随后已完成最终冻结验证并上线 `ac25b0f0…d3d82`：Linux race/PG/Redis/native **864 项通过**，固定 CLI 独立 **7 项通过**，真实 HTTP models 200、WS 101、关闭后 in-flight=0。此追加更新发布状态，不将局部历史验证数字改写为全量结果；原题恢复依然未证明。详见[最终上线记录](../CODEX-GATEWAY-STREAM-RESEARCH-2026-09-25.md)。
