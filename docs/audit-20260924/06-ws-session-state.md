# 06 — WebSocket 请求、会话与 turn 状态审计

基准为工作区锁定的 `.local/codex-source-0.155.0`，未操作生产环境。修改仅涉及 `gateway_websocket.go` 和新增 `gateway_ws_session_audit_test.go`；`gateway_ws_response_history.go` 经复核保持不变。HTTP/TLS、native WS framing 由其他专项处理。

## 已修复

1. **具名 lane 错误误杀整条连接。** 旧代码收到任何 `type:error` 都给全部 pending turn 写错误并取消连接。现在 `gatewayWSTurns.observe` 返回是否属于必须关闭连接的错误：非空 `stream_id` 只结束该 lane 的 FIFO 头，未知 lane 不污染其他 turn；无 scope 错误仍在转发后关闭两端再释放全部准入。见 `gateway_websocket.go:720`、上游响应 pump。
2. **迟到 error 仍能误断后续 turn。** 原有 history 只保护账本，pump 仍无条件因 error 断开。现在已完成 response ID 的重复/迟到 error 既不重复计账，也不关闭后续响应。原始响应字节仍逐字转发。
3. **WS 活跃 turn 没有 stream idle timeout。** 新增 connection-local watcher，使用每次 turn 的 `UpstreamStreamIdleTimeoutSeconds` 快照。仅为每个 lane 的 FIFO 头计时，同 lane 晋升重置计时，不把排队时间作为卡住时间；其他 lane 持续输出不能掩盖卡住的 lane。没有 pending turn 时不设全局 idle deadline。超时先取消并关闭两端，记录 `upstream/response_idle`，最后释放准入。见 `gateway_websocket.go:827`。
4. **WS 写请求使用固定 60 秒而未应用 CLI stream timeout。** `response.create` 上游写改用相同的配置 timeout。下游写和非 create 控制帧仍保留网关 60 秒写预算。见 `gateway_websocket.go:104,457,502,548`。
5. **非法 stream ID 可直接送上游。** 有值的 `stream_id` 必须为 1–256 个 ASCII 字母、数字、`_`、`-`、`.`。原实现允许 512 字节任意字符。缺失/null 保持默认 lane 兼容。见 `gateway_websocket.go:19` 和 create 参数验证。
6. **可恢复的请求校验错误结束整个 socket。** 本地 400/429 不会把没有发往上游的拒绝请求变成对其他活跃 turn 的取消；第一帧校验失败同样允许在首次请求 90 秒窗口内更正。授权、禁用、身份变更、连接故障仍结束 socket。
7. **后台关闭 WebSocket 后已有连接仍可无限发起新 turn。** 每次 create 重新验证 `WebsocketEnabled`，禁用后返回 403。
8. **body-only 首个 create 因设置变化误报已有握手身份不一致。** 只有已经建立上游 socket 才用 409 要求指纹变更后重连；尚未连接的 socket 使用最新配置建立首个握手。
9. **abort 不是幂等操作。** 现在清空 pending 和计数，第二次调用不重复记账或执行 release；保留明确的超时错误信息。
10. **首写失败路径的释放顺序。** 补齐 pump 尚未启动时的 defer，确保在可能存在含糊的首次写失败时，也先取消、关闭双方，再 abort/release。见 `gateway_websocket.go:310`。

## 与锁定 CLI 源码的逐项对照

| 边界 | CLI 0.155.0 证据 | 网关结果与差异 |
| --- | --- | --- |
| create 类型与字段 | `codex-api/src/common.rs:313` 的 `ResponseCreateWsRequest`，枚举只声明 `response.create` | 保留 create 字段，不自行补 HTTP stream 字段；非 create 扩展在已有上游连接时透明转发且不占 inference slot，属于兼容扩展 |
| 同连接响应串行 | `responses_websocket.rs:293` 持 stream mutex 直到本响应结束 | CLI 没有 `stream_id` 字段；网关既有具名多路复用属于协议扩展，保持 lane 内 FIFO、lane 间独立计账，不声称这是该 CLI 自身实现的 multiplex |
| 命名 lane 格式 | 锁定 CLI 不发 `stream_id` | 新增校验来自公开 [WebSocket mode](https://developers.openai.com/api/docs/guides/websocket-mode) 的具名 lane 约束；主代理已读取该官方来源 |
| 预热 | `core/src/client.rs:1769` 注释将 generate=false 视为 connection setup；`:1804` 发送 `Some(false)` | 不改 generate，不丢 `client_metadata`，预热 terminal 正常释放网关准入。网关保留请求级记录和资源预算，不伪称模型进行了推理 |
| previous_response_id / 空增量 | `core/src/client.rs:1783,1802` 保留之前 ID 并选择增量 items | opaque ID、空 input 与元数据保持原样；不把前一个已完成 ID 当成新请求的 response ID |
| 发送和读取 idle | `responses_websocket.rs:707,898` 在活跃 response stream 的读/写上套 idle timeout | 新增活跃 lane watcher 和 create 写 timeout；空闲连接不受该 timeout 影响 |
| ping/pong | `responses_websocket.rs:92` 的内部 pump 直接 pong，不把 ping 送给 response reader | coder/native transport 控制帧由底层处理；业务 watcher 只收到消息事件，ping 不充当模型输出 |
| 消费者取消 | `responses_websocket.rs:292,319` 的 `tx_event.closed()` 取消 stream | 下游 reader 发现离开即取消 session，两端关闭后回收未终止 turn；既有取消回归保持通过 |
| 顶层错误 | `responses_websocket.rs:610,618` 解析 wrapped error；流错误后取走并丢弃连接 | 无 scope error 仍关闭；具名错误隔离是 CLI 之外的 lane 扩展；不在已接受 socket 重播 create |
| completed / failed / incomplete | `responses_websocket.rs:817` 附近交给共享事件处理；completed 结束本 stream | 网关沿用共享 `terminalResponse`；保留 failed/incomplete 诊断与 token 统计；本专项不改 HTTP/SSE 共享解析语义 |
| response ID 去重 | CLI 串行消费单个 stream，无独立多路复用计账历史 | 网关保留 connection-local SHA-256 FIFO history，最多 256 个完成 ID，每 ID 最多 1024 字节；history 是观测保护，不修改转发内容 |
| 准入与会话绑定 | CLI 没有多账户池或网关 key 授权 | 连接期间固定账户；每 create 重查 key/account/cooldown、每请求占槽，terminal 释放，idle socket 不占槽；session lease 每 20 秒续期属于网关扩展 |
| 动态设置 | CLI 自身没有网关管理后台 | 新 create 使用最新 prompt/policy/idle 设置；已有握手的 fingerprint 改变要求重连；禁用 WS 阻止后续 turn |

## 明确的一致性限制

- 没有宣称网关整体与 CLI 完全相同。多账户路由、并发准入、具名 lane、有限历史、请求日志、可恢复本地校验均为网关功能。
- 命名 lane idle 以可归属的有效消息推进；CLI 单 lane reader 会在收到无法解析的文本后继续读。无效文本不能无限刷新网关的活跃业务期限，这是当前可观察差异。
- watcher 包含网关转发造成的等待时间；CLI response consumer 的本地事件队列调度与网关的下游 socket 背压不是完全相同的模型。下游写另有 60 秒预算。
- 无 ID 的重复 terminal 与真实下一次无 ID terminal 无法可靠区分，因此保留既有无 ID FIFO 兼容，不造出响应身份。超过 256 个完成响应后的迟到 ID 不在保留窗口内。
- `observeCooldown` 位于其他所有权文件。已将「未知具名 lane 的 429 fallback 到 connection scope」和 response ID 不匹配时可能扩大冷却 scope 的问题交主代理/冷却专项复查。
- 该专项没有验证真实生产端或宣称发送了真实推理请求；真实 CLI、native TLS、PostgreSQL/Redis、Linux race 由主代理统一集成验证。

## 验证

新 `gateway_ws_session_audit_test.go` 用真实上下游 socket 验证：

- 3 个已准入 turn 的跨 lane 并行与同 lane FIFO；具名 error 仅结束头部。
- 已完成 ID 的 scoped/unscoped late error、未知 lane error、重复 completed 不污染下一条响应。
- 无 scope error 确实关闭双方，所有 turn 记录为 upstream response_event 中止且释放。
- noncreate opaque 控制帧不改变字节、不占槽。
- 首帧非 create、model 类型、空/空格/非 ASCII/257 字节 stream ID、previous ID 类型、generate 类型均返回 400，之后能在同 socket 发起合法请求。
- 256 字节合法命名 lane、generate=false、client_metadata、previous_response_id、空 input 原样保存；中途局部校验错误不取消已接受的 continuation。
- WS 后台禁用与握手指纹变化在下一 turn 生效；未建上游的 lazy socket 使用最新指纹配置。
- 真实 socket 在配置 10 秒 stream idle 后关闭并保留超时来源。
- 状态 watcher 对 idle connection 不设 deadline，对队列晋升重置期限，busy lane 不延长 stalled lane，abort 二次调用不重复 release/记账。

已执行并通过：

```powershell
go test ./... -run 'TestGateway(WebSocket|WSResponse|WSSessionAudit|WarmupObservationWS|CooldownWebSocket|ObservationHooksWebSocket|Shutdown)' -count=1
```

结果：`ok codex-agent 11.138s`；native-transport/tests 没有 Go 测试文件。更早的纯新增测试通过为 10.448s。

本机 `go test -race` 实际返回 `go: -race requires cgo; enable cgo by setting CGO_ENABLED=1`，PATH 未发现 gcc/clang/zig。主代理已安排隔离 Linux Go 1.26.8 + gcc 环境统一跑 race，因此本报告不把未执行的 race 写成通过。
