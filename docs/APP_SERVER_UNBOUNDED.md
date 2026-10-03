# App Server CLI 轻量通道：本地容量限制审查

审查日期：2026-09-27。本文记录当前源码与本地测试事实，不代表已部署，不替代真实 Rust helper 与统一冻结产物的验收。

后续真实原生验证已完成：最终 Rust helper `361b0df3…` 通过 50 项 Rust、26 项回环、8 项 HTTP/TLS 检查；与最终 Go artifact `af327b3a…` 的 13 项组合测试全部通过，0 skip。包括真实 33 MiB 普通/zstd HTTP 和 TLS WebSocket 文本/二进制逐字节往返、Realtime 无 deflate、Guardian、96 并发、401 恢复与取消。证据位于 `.local/appserver-parity/native-unbounded/final-interop-20260927T123600Z/result.json`。下文早期“待原生验收”的表述应结合该记录阅读，不能用模拟 IPC 测试替代实际组合证据。

## 生效范围

`Proxy.AppServerCLI != nil` 且本次请求选择 `official_codex_cli` 时，`gateway` 在读取请求体、WebSocket 分流及账号故障切换之前设置 `gatewayUnboundedPayload`。这是服务端 context 内的策略，客户端不能用 HTTP header 开启它。管理接口、PowerPoint 路径、未启用轻量适配器的兼容路径不因此改变。

新策略取消本地请求/响应容量拒绝。账号配置的并发数、准入、优先级、权重、认证、owner 校验与故障切换条件仍执行；不会把失效账号或超并发账号当成可用账号。

## 已核对的路径

| 边界 | 轻量 profile 当前行为 | 证据 |
| --- | --- | --- |
| HTTP 请求体 | 不使用旧 32 MiB `MaxBytesReader`；identity/zstd 进入 `readOfficialBody` | 33 MiB identity 和 zstd 入站通过；解压后内容 SHA-256 一致 |
| JSON 准备、策略与账号缓存键改写 | 不因原始或改写后 JSON 超过 32 MiB 拒绝；保留按实际选定账号重写缓存命名空间 | 大请求通过原处理链；客户端 cache key 不直接发送 |
| 请求压缩 | 不应用旧 prepared/wire 字节上限；native 模式需协商 helper capability | Go 路径检查与入站测试；真实 helper 的验证由统一产物验收另行记录 |
| HTTP SSE 读取与工具映射 | 固定 64 KiB 读取块，增量收集当前行/事件，无配置的 token/event 大小上限 | 33 MiB 工具事件通过映射并保留参数 SHA-256；UTF-8、BOM、CR/LF/CRLF 测试通过 |
| JSON 输出聚合 | 不应用旧 32 MiB 总量及 8192 项数量上限 | 33 MiB 单项、2 × 17 MiB 聚合、8193 个完成项通过；slim terminal 恢复 opaque 字段 |
| 流式用量统计 | 大事件不会使观察器提前报本地超限而丢失最终 usage | 33 MiB 工具事件后的 input/output/cached token 数值正确 |
| 取消与错误 | 关闭下游会关闭正在读取的上游；非法 UTF-8、非法 index、冲突 response ID 与上游失败仍是错误 | 取消释放及失败帧回归通过 |
| HTTP 账号切换 | context 策略和完整原始请求保留；下一账号重新隔离 cache key | 33 MiB 请求先获 401，再由另一账号成功；两次输入摘要相同、缓存命名空间不同、并发槽全部释放 |
| 模型目录 | `/models` 不应用旧 8 MiB 响应上限；轻量适配器内部 catalog decoder 也无该上限 | 9 MiB 目录经 gateway 完整返回；内部 decoder 源码审查 |
| 辅助 JSON 接口 | search/images/memories 共用无上限读写路径，仍走准入与换号 | `/images/edits` 33 MiB 请求与响应 SHA-256 一致；其他辅助路径共享同一函数 |
| WebSocket 消息 | 新策略给上下游连接设置无限消息读取模式；记录同一策略用于统计 | context/调用点审查；真实 native helper 大帧由统一产物验收另行记录 |
| WebSocket 本地标识符长度 | 不应用旧 model 512、session 4096、response ID 1024、stream ID 256 字节上限 | 真实双向 TCP 测试使用超过这些阈值的标识符，发送、归属与续接成功 |
| WebSocket 会话/待完成数量 | 不应用单连接 128 个绑定或 128 个 pending 上限 | 同一连接 258 个顺序会话、账号并发 1；130 个 pending 在账号配置 130 时通过 |
| WebSocket 账号并发 | 原账号上限仍生效 | 上述 130 个 pending 未完成时，第 131 个请求返回 429 且未到上游；完成后可继续使用连接 |
| WebSocket 响应策略历史 | 新 profile 保留当前连接全部完成响应的摘要和 cache policy，不再按 256 项 FIFO 丢弃；不保留完整回复 | 第 258 轮后回到第一轮长 `previous_response_id`，原 cache key 隔离结果保持不变 |
| 内容锚点与会话归属 | 新 profile 不因锚点超过 1 MiB、整体超过 32 MiB 或 model 超过 512 字节丢弃 affinity；显式 session 仍优先 | 大锚点测试及策略传递审查 |
| 本地兼容会话表 | 新 profile 不因 inference bindings 4096 或 account routes 16384 条目拒绝新会话；到期清理仍执行 | 填满旧阈值后可绑定新会话；Redis 路径原本未使用这些条目上限 |

旧路径的默认有限模式仍保留，原限制回归继续通过。没有把全局 `maxBody` 简单改成更大的数字，也没有用 `Scanner` 的最大整数充当“无限”配置。

## 压缩结果的保留规则

`responsesCompactionResult` 是旧 JSON compact 接口的适配层。旧路径通过 `64000 * 4 = 256000` 字节预算挑选可保留的近期消息；新 profile 不再用此本地近似预算截掉调用者的 user/developer/system 消息。

它仍按原适配契约保留这些消息并附加一个真实上游 opaque compaction item；不会重新执行或拼回 assistant 工具调用，不保留旧 compaction trigger。300 KiB 用户消息与 developer 指令已通过保留测试。

这不是官方 sidecar 的完整上下文保留策略复刻。官方选择还依赖本地 harness 元数据、图像预算等状态；此处只证明本地近似容量阈值不再丢弃该适配契约要保留的调用者消息。

## Context 与调度审查

- `gatewayWithAccountFailover` 从已有请求 context 派生，`r.Clone(ctx)` 重放时保留 payload policy；原 zstd 请求的解码体被重放，旧 Content-Encoding 清除，避免二次解压。
- route、cooldown、cache affinity、native 授权与 session lease 均以现有 context 派生。WebSocket turn record 继承 `unboundedPayload`。
- 轻量连接池的 key 包含 payload policy，有限与无限模式不会复用同一状态不一致的连接。
- 压缩结果聚合、非流式响应、模型目录和辅助接口分别检查，未把单一 SSE 测试当成所有响应路径的证明。
- `pickSessionAccount`、`pickFailoverAccount` 和 `acquireGatewayAccount` 未因本轮容量改动被绕开。账号 priority/weight 和失效、忙碌账号跳过回归已通过。

## 保留的协议与实际边界

无限容量不取消 JSON 结构、UTF-8、字段类型、非空/字符合法性、身份一致性、授权和真实整数容量校验。`stream_id` 的 256 字节阈值在所核对的官方 Responses 源码中没有依据；仅轻量 profile 去掉该长度阈值，当前中转扩展已有的非空 ASCII 字符集合校验保留。

HTTP 错误响应在新 profile 的 WebSocket 握手失败路径不再被 2 MiB 截断。指标/诊断仍只保留有限摘要或错误前缀，这些诊断字段不会截断转发给客户端的主体。

以下独立边界尚未被本文的新 payload 测试消除或证明为无限，不能据此宣称“服务中的所有上限已删除”：

- Native IPC head/headers 的独立验证预算及字段数量验证；Rust helper capability、分块传输与真实编译产物由其专门验收负责。
- OAuth grant 成功响应的 2 MiB 解析预算和失败码检查的 64 KiB 诊断预算；这不是模型响应主体路径。
- 管理接口导入、身份文件、离线诊断工具等独立输入预算。
- 当前配置的连接/空闲/写入超时与会话租约；这些不是本轮移除的模型 payload 容量阈值。
- 操作系统可用内存、整数表示能力及真实上游自身的协议/模型限制。

HTTP client pool 的 128 条目淘汰用于空闲连接复用管理；其 retire 不拒绝已在执行的请求。本文未把缓存复用管理误当成账号并发或模型 payload 配额。

## 已执行的验证

以下组合在 Windows 本地执行通过，最后一次输出 `ok codex-agent/internal/server 7.130s`：

```powershell
go test ./internal/server -run '^(TestAppServerUnbounded|TestGatewayWSResponseHistory|TestGatewayWSSessionAuditValidation|TestGatewayRequestFailover|TestGatewayRoutingAuditPriority|TestGatewaySoftAffinityWeighted)' -count=1
```

响应读取/聚合与旧行为回归组合通过，输出 `ok codex-agent/internal/server 4.024s`：

```powershell
go test ./internal/server -run '^(TestAuditSSE|TestAuditOutput|TestAuditNonStreaming|TestAuditObservation(SSE|LocalParser)|TestAppServerCLI(MappedStream|AuditSSE)|TestGatewayPerformanceAudit(MaximumFragmentedSSELine|ManyItemsAndSparseIndices|SSEEveryTwoCuts)|TestLightweightUnbounded)' -count=1
```

本地 `-race` 尝试因 Windows `CGO_ENABLED=0` 且没有可用 C 编译器在测试开始前退出。不能记为 race 通过；应以统一冻结源码后的 Linux race 产物及其 source hash 为准。本文编写时没有据此宣称 Rust 编译、真实 helper 大帧或生产部署通过。
