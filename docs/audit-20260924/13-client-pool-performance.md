# 13 — 客户端池、超时与资源性能审计

日期：2026-09-24。范围：`gateway_client_pool.go`、`gateway_custom_settings.go`、`gateway_timeout.go`；新建 `gateway_sse_idle.go`、`gateway_pool_idle_audit_test.go`。未修改 native 生命周期配置、生产环境或 `proxy.go`。

## 修复结果

1. **SSE 半帧/注释能够无限延长 idle 的一致性缺陷。** 旧实现对每次 `Body.Read` 重新给予完整 idle；上游持续滴入半帧、注释或只有 `id/event/retry` 的事件就永远不超时。现在保留跨 Read 的剩余预算，只在完整含 `data` 字段的事件结束时重置。`data:` / `data` 空值也会 yield event，不能误归为注释心跳。字节原样返回，不缓冲整个事件。
2. **shutdown 漏关基础直连池。** 默认直连客户端没有进入 `clientPool`，原来的 `closeGatewayClients` 只关了变体池。现在同时调用基础客户端的 `CloseIdleConnections`，不取消活动 HTTP 响应或升级的 WS。
3. **SOCKS 变体继承直接 TLS 拨号钩子可能绕路。** Go HTTPS 在 `Proxy == nil` 时优先使用 `DialTLSContext/DialTLS`，因此只覆盖 `DialContext` 仍可能绕过 SOCKS。选择 SOCKS 时清除两种直接 TLS hook，保留克隆后的 TLS trust 配置；新回归通过真实本机 SOCKS→TLS 请求验证 HTTP/compact/WS 三种变体，基础 transport 不被修改。
4. **每次流式 Read 创建 timer/channel/closure。** 同一 body 复用一个 `AfterFunc` timer 和完成 channel。停止失败时仍等待 callback 完成，避免回调跨 Read 遗留；Close 的 `sync.Once` 所有权和取消语义保留。

## 与实际 CLI 源码比较

依据 `.local/codex-source-0.155.0`：

- `codex-rs/core/src/client.rs:1040` 的 `build_api_transport` 调用 `create_client_for_route`；`codex-rs/login/src/auth/default_client.rs:252` 在默认代理策略下创建客户端。网关 native 默认 `cli_request` 保持不变；不能为了复用改成默认 `pooled` 并继续声称生命周期一致。
- `codex-rs/codex-api/src/sse/responses.rs:576` 先 `stream.eventsource()`，随后 `timeout(idle_timeout, stream.next())`。这要求等完整 SSE event，而不是每个传输 chunk。
- `.local/sse-audit-20260924/eventsource-stream-0.2.3/src/event_stream.rs` 的 `EventBuilder::add/dispatch`：只有带 data 字段的空行完成会返回 `Some(Event)`；空值 data 会先追加 LF，因此同样 yield。注释、空行、只有 event/id/retry 都不 yield。
- 同目录 `parser.rs` 使用 CRLF/CR/LF；streaming CRLF 在尾部 CR 时等待 lookahead。新状态机支持分段 CRLF、尾 CR 等待、UTF-8 分段和首个 BOM。它只保留最多 4 字节 UTF-8 前缀和固定长度字段状态，空间 O(1)。非法 UTF-8 不会通过后续 data 假造 progress。
- `codex-rs/model-provider-info/src/lib.rs:29,430`：默认 stream idle 为 300000 ms，允许显式毫秒配置。网关管理设置刻意限制 10..3600 秒、零值缺省恢复到 300 秒（`store.go:654,840`），并非完全相同的配置接口。辅助函数传入 0 仍表示不等待，未擅自改为关闭超时。

成功的 Responses 请求即使缺失或错标响应 Content-Type，只要请求明确 `Accept: text/event-stream`，仍使用事件预算，避免用错误响应头绕过限制。普通非 SSE JSON body 保留字节等待超时。

## 已审查边界

| 边界 | 结论与验证 |
| --- | --- |
| HTTP/1 重用 | 现有 12 轮请求/1 TCP 回归通过；两批并发 burst 保留连接且不扩大 account admission |
| HTTP/2 重用 | 6 轮终止 SSE 共享 1 CONNECT/TLS 隧道回归通过，关闭单个 stream 不关闭整个连接 |
| clone/自定义 RoundTripper | 直连注入的 transport 原样保留；不对其私有超时策略做推断。compact 复制 Client，基础对象不变 |
| TLS/代理绑定 | SOCKS 清除直拨 TLS hook；保留自定义 roots；HTTP proxy 仍走 CONNECT。Go TLS 自然不等同 Rust/rustls 指纹 |
| 代理秘密与 ABA | key 用包括版本、enabled、凭据的 SHA-256；Get 在池锁内解析最新状态，RoundTrip 再验指纹；旋转/disable/delete/ABA 既有回归通过 |
| 已选择 request route | admission 内保持最初代理快照，版本变化拒绝而不是跨 refresh/inference 换线路；既有 rotation 测试覆盖 |
| 并发池创建 | 单锁下命中/构建/淘汰，32 并发 miss 复用同一 client；池 metadata 最大 128 |
| TTL/LRU/retired | 10 分钟惰性 metadata 淘汰，transport idle 最多 90 秒；淘汰不终止活动 body，body 结束再清 retired idle |
| header/compact/stream 隔离 | timeout 是不可变 key 维度；普通 header 90 秒、compact 20 分钟总超时、SSE 清除 Client 总超时；独立变体回归通过 |
| standalone timeout helper | 原始 transport 相同 timeout 返回原 client；需要改变时 clone；pooled clone 保留 proxy guard，cleanup 只处理私有变体 |
| Read/Close/取消 | ctx cancel 打断 Read；Close 多次只有一次底层 Close；timer callback 在返回前 join；完整事件新逻辑不修改这些所有权 |
| 下游背压 | 只有实际等待上游的 Read 时间计入；下游写阻塞时没有运行的 idle timer。半帧预算跨 Read 保留 |
| event 解析 | half-frame/comment/metadata 不续时；完整空 data/multiline/CRLF 正确续时；BOM/UTF-8 任意切片测试通过 |
| shutdown | 基础直连与变体 idle 都关闭；活动响应继续完成。shutdown admission 由外部 lifecycle 负责，closeGatewayClients 本身不禁止再次获取 client |
| native lifecycle | 未修改 `cli_request` 默认；native helper IPC/pool/WS 由其他专项审查，不能把 Go 回环连接结果当 native TLS 指纹证据 |

## 性能与限制

2026-09-24，本机 Windows/amd64，AMD Ryzen 7 9700X，Go 1.26：

| benchmark | 本次结果 |
| --- | --- |
| `BenchmarkGatewayAuditSSEIdleRead`，10 字节完整 SSE，300 ms | 60.39 ns/op；0 B/op；0 allocs/op（循环稳定态） |
| 已有 `BenchmarkGatewayProxyConnectionReuse/clone_per_request`，200 ms | 342265 ns/op；1 connection/op；24849 B/op；180 allocs/op |
| 已有 `BenchmarkGatewayProxyConnectionReuse/pooled`，200 ms | 50005 ns/op；0.0002006 connection/op；7144 B/op；89 allocs/op |

这些是本机合成短响应实验，不代表生产 RTT、吞吐或 Rust TLS 成本。流式 wrapper 每个 body 仍有首次 timer/channel 分配；事件进度扫描 O(n)，未缓存 payload。池命中目前仍在锁内扫描最多 128 个 metadata，且代理读取/指纹检查带来开销；没有未经测量地改为无锁缓存。

128 是 transport 变体数量上限，不是 socket 或活动请求上限。每个 Go transport 的默认全局 idle 上限 100，因此理论 idle 容量是变体数乘以单 transport 上限，另有基础直连池；活动连接由 admission 和请求实际生命周期控制。retired 旧 handle 可以完成已获得的有效请求，其 idle 在响应结束后清理；代理变更始终继续 fail closed。

与 CLI 仍有明确差异：Go 连接池生命周期/HTTP2 栈/TLS ClientHello 不等于 reqwest/rustls；网关有自己的 header/compact 管理上限；本实现排除每次下游写等待，CLI 原生只在 event 分发 `tx.send` 阶段自然排除消费者背压。特别是半帧被原样向慢下游转发时，网关按累计上游等待时间计时，并非严格的整段墙钟时间。这是透明流式网关的保留语义，未声称完全时序相同。

## 执行结果与复跑

- `go test . -run 'TestGatewayAudit|TestCLIStreaming|TestGatewayClientPool|TestGatewayTimeoutPool|TestGatewayHTTP1' -count=1 -timeout=90s`：PASS（修改最终版，1.144 s）。
- `go test . -run 'Test.*(SSE|Stream|Compact|Pool|Timeout|Cancel|Close)' -count=1 -timeout=150s`：PASS（15.487 s；随后增加了仅 Accept/Content-Type 的识别回归，并跑上面聚焦集）。
- `go vet ./...`：PASS。
- 测试覆盖真实 Go HTTP/1、HTTP/2、SOCKS/TLS 回环；SSE drip/budget 使用 `testing/synctest` 虚拟时钟，不依赖亚秒级机器调度。
- 本机 `CGO_ENABLED=0`，没有把普通测试说成 race 测试。主任务 Linux race 需包含本专项 `TestGatewayAudit|TestCLIStreaming|TestGatewayClientPool|TestGatewayTimeoutPool`。

一条复跑命令：

```powershell
go test . -run 'TestGatewayAudit|TestCLIStreaming|TestGatewayClientPool|TestGatewayTimeoutPool|TestGatewayHTTP1' -count=1 -timeout=90s
```
