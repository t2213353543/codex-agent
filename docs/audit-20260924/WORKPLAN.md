# Codex 网关源码一致性与健壮性审计（2026-09-24）

基准：本地 `rust-v0.155.0` 源码、实际锁定依赖与运行时配置。参考旧版 `0.154.0` 的既有捕获不能自动证明新版行为。任务开始时完整 Go 测试、`go vet` 通过。原始源码快照及 SHA256 位于 `.local/gateway-audit-20260924/`。

最多并发 4 个 agent（包含主 agent），以下专项分批执行。每项要求检查多个相关边界，明确修复、测试和有意差异，避免将未验证事项写成通过。各报告由对应 agent 写入本目录；主 agent 负责交叉审查、整体验证和最终差异矩阵。

| 编号 | 专项 | 范围 |
|---|---|---|
| [01](01-cli-fingerprint.md) | CLI 身份与指纹 | UA、originator、version、attestation、配置覆盖、refresh |
| [02](02-native-http-tls.md) | Rust HTTP/TLS | CA、backend、SNI、ALPN、HTTP2、IPC、超时、取消 |
| [03](03-native-websocket-tls.md) | Rust WS/TLS | rustls、握手、deflate、控制帧、ACK、背压、关闭 |
| [04](04-go-native-http.md) | Go 原生 HTTP 适配 | helper 生命周期、IPC、取消、长度、错误归类 |
| [05](05-go-native-ws.md) | Go 原生 WS 适配 | 序号、ACK、读预算、握手、关闭、超时映射 |
| [06](06-ws-session-state.md) | WS 请求与会话状态 | 多轮、stream_id、准入、终止、断线、恢复 |
| [07](07-http-headers-routes.md) | HTTP 请求与响应头 | 大小写、白名单、身份回退、编码、路由 |
| [08](08-sse-response-assembly.md) | SSE 与响应重建 | event framing、终止、工具调用、输出上限、错误 |
| [09](09-policy-compression.md) | 请求策略与压缩 | body、compact、提示词、透明性、zstd |
| [10](10-auth-refresh-replay.md) | 认证刷新与重放 | OAuth、401、取消、轮换、失败冷却、并发 |
| [11](11-session-ownership.md) | 会话归属与授权 | 黏性、缓存、过期、身份变更、撤权 |
| [12](12-routing-cooldown.md) | 选路与冷却 | 429、Retry-After、代理固定、并发容量 |
| [13](13-client-pool-performance.md) | 客户端池与性能 | HTTP1/2、连接复用、资源回收、超时隔离 |
| [14](14-runtime-config-shutdown.md) | 配置与关闭 | 原生开关、启动校验、运行时设置、优雅停止 |
| [15](15-proxy-transports.md) | 代理传输 | HTTP CONNECT、SOCKS、IPv6、远端 DNS、取消 |
| [16](16-observation-consistency.md) | 观测一致性 | 用量、错误、流中断、terminal、日志敏感信息 |
| [17](17-tls-provenance-capture.md) | 源码和 TLS 证据 | vendored hash、lock、features、构建及捕获边界 |
| [18](18-cli-contract-review.md) | 实际 CLI 场景 | HTTP/WS、续接、工具、compact、回退 |
| [19](19-performance-fuzz.md) | 性能与异常输入 | benchmark、fuzz、并发回归、上限与资源消耗 |
| [20](20-final-review.md) | 独立最终复审 | 修改审查、差异矩阵、残留缺陷、可重跑验证 |

部署状态与源码验证状态单独记录。以实际执行结果为准，不以历史报告代替本次构建和测试。

20项均已完成；最终结果以[总审计报告](../CODEX-GATEWAY-AUDIT-2026-09-24.md)及其中的最终源码快照/测试JSON为准。报告中的早期进度或待验证记录由主任务最终实测回填补充。
