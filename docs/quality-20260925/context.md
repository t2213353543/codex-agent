# 多轮研究：终态瘦对象覆盖已完成上下文

2026-09-25。发现并修复一处真实的网关上下文处理缺陷：**非流式 JSON 聚合在同 ID 的 terminal item 缺少字段时，会删除此前 `response.output_item.done` 已给出的 opaque reasoning、加密工具参数和消息 phase；legacy compact 适配还可能因此错误返回 502。**

这是从官方 0.155.0 的历史消费方式提出的新实验，不是又一次无状态糖果题对照。已通过本地真实 HTTP 两轮与 compact→followup 路径复现并修复；没有 SSH、调用真实模型或修改题面。生产集成和部署由主任务负责。本问题可能损伤多轮上下文，但不能据此声称已经解释账号糖果题 29，更没有证明内部账号标记或算力路由。

## 决定性源码边界

固定 `.local/codex-source-0.155.0/codex-rs/` 中：

| 官方边界 | 行为与实验依据 |
| --- | --- |
| `codex-api/src/sse/responses.rs:357` | `response.output_item.done` 独立解码为 `ResponseEvent::OutputItemDone`。 |
| `codex-api/src/sse/responses.rs:483` | `response.completed` 只构造包含 response ID、usage、end_turn 的 Completed 事件；不拿 terminal output 覆盖已经消费的历史 item。 |
| `core/src/session/turn.rs:2536`、`:2626` | 回合处理已经完成的 OutputItemDone，并交给历史/工具处理。 |
| `core/src/compact_remote_v2.rs:437` | 远程压缩从 OutputItemDone 中选取 compaction；Completed 只确认终态及使用量，要求恰好一个 compaction。 |
| `protocol/src/models.rs:1006`、`:1046`、`:1062` | Message.phase、Reasoning.encrypted_content、FunctionCall.encrypted_function_args/namespace 是后续请求上下文的一部分。缺失、空数组及 null 不能互换。 |

当前网关 raw SSE 和 WS 会转发原事件；非流式 JSON 需把这些事件合成一个 response。旧 `response_output.go::complete` 已支持 terminal 缺 output 或只含部分已知 ID，但对 **同 ID 对象内缺失字段**没有补齐：全量 IDs 直接返回 terminal，部分 IDs 合并时仍用瘦 terminal item 整体覆盖 done item。因此流式与非流式调用者取得不同历史。

## 新实验及旧代码失败

新增 `gateway_research_context_test.go`，使用真实 `ServeHTTP → loopback HTTP upstream`。上游事件形态参考上述官方协议；其中 opaque 内容全部为合成哨兵，不伪装成合法服务端密文。

第一轮发出三个完整 done item：reasoning（encrypted_content、summary、大整数未知字段）、function_call（encrypted_function_args、namespace）、assistant message（phase=commentary）。terminal 中出现同 ID 的瘦对象；分别测试所有 IDs 齐全、terminal 只含 message、原生 SSE 对照。客户端从第一轮结果生成第二轮 input，实际上游核对历史。SSE 控制从 done events 取历史，绝不调用待测 JSON collector。

旧代码的真实结果：

- SSE 对照通过，原事件字节完全一致，第二轮历史完整。
- JSON 全量 IDs 分支丢失 encrypted_content、summary、encrypted_function_args、namespace、phase 及未知字段。
- JSON 部分 IDs 分支丢失 message.phase 及同对象未知字段。
- `/responses/compact` 收到有效 done compaction，但同 ID 瘦 terminal 没有 encrypted_content，适配器错误返回 HTTP 502，无法产生可回传的压缩历史。

red 日志保存在 `.local/quality-20260925/context/fail-before.jsonl`：两个顶级测试失败，原生 SSE 子项通过。修改前生产文件副本为 `response_output.before.go`，位于同目录。

## 局部修复与不可越过的边界

唯一生产改动为 `response_output.go`：确认 terminal 所有 item ID 都能关联到本次收集的 done item 后，只补相同 item **缺失的顶层字段**。不深合并数组或对象，不解密、不推导字段，也不修改 raw SSE/WS。

- terminal 明确提供的 encrypted_content=null、summary=[]、arguments=""、encrypted_function_args=[]、phase=null、future={} 等值继续保留。可选 namespace=null 也保持清空。
- 全量 terminal 数组保留原顺序；terminal 缺少整个已知 item 时继续按收集到的 output_index 恢复。未知或匿名 terminal ID 仍按原兼容边界保持 terminal，不猜测与 done 的关系。
- 同 ID 却改变 type、call_id、工具 name/namespace、message role 等身份时拒绝拼接，不把旧工具参数继承给另一工具或用户消息。若 terminal 改过 plaintext arguments 却缺少与之匹配的 encrypted_function_args，也不能把旧密文拼回去。
- 缺失字段中的 RawMessage 原样保留，包括 nested null、9007199254740993 和 opaque 未知对象内重复成员；不会经 float64 或嵌套 map 重编码丢失信息。
- 修复后的单 item 和最终完整 response 继续受 32 MiB 限制；拒绝超限不会污染已收集历史。完全相同的 item 直接复用原字节，避免通常路径额外解码。

新测试还覆盖明确空值、身份冲突、未知 terminal ID 两种顺序、全量数组逆序、大上下文合并上限，以及 compact 成功后把 opaque summary 实际送到下一轮。严格拒绝身份冲突可能将此前表面 200 的不一致源响应变成 502；这是避免输出混合历史的预期行为，不是自动重试或账号迁移。

## 验证结果与范围

本地关联验证：**36 个顶级测试通过、0 失败、0 跳过**，包含子项共 98 条通过事件，package 耗时 1.637 秒。覆盖新增 7 个顶级多轮研究测试，以及已有 collector、output identity、compact、Q01 HTTP/WS 字段透传回归。green 原始日志为 `.local/quality-20260925/context/green-associated.jsonl`。

```powershell
go test ./... -run '^(TestResearchContext|TestCollectedResponse|TestAuditOutput|TestAuditNonStreaming|TestGatewayCompactV2|TestQualityQ01)' -count=1 -timeout=90s
```

冻结摘要：

- `response_output.go`：`91790eb14f4929f4015b5dad9ae8d82f74a27df95440d152f7b883ea096f72a0`
- `gateway_research_context_test.go`：`f2995decf89f02788395d8785c8509b3640e500cff4d91aab9e8c467e262c213`

这些测试证明触发条件下网关曾丢上下文，以及局部修复能够保留第二轮实际收到的字段。它们不证明生产曾遇到相同的瘦 terminal，也不证明已有单轮糖果题 29 是本缺陷造成。原生 CLI 的 raw stream 此前就通过本实验对照，因此不应把本修复描述为改变 CLI 的模型能力或账号状态。

主任务最终已完成 Linux race/PG/Redis/native 全套 854 项通过、官方 CLI 独立 7 项通过，并部署 `2ed703ba…173c6`；实际 HTTP models 200、WS 101。完整范围和不可混用的历史测试数字见[集成上线记录](../CODEX-GATEWAY-RESEARCH-2026-09-25.md)。
