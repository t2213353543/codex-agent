# 网关高并发错误路径审计（2026-09-18）

审计范围：当前共享源码的 HTTP/WS 准入、账号选择、429/502/503、重试与连接池边界。仅阅读源码并执行明确限定的本地合成测试；未修改 Go、发生产推理请求、部署或访问其他项目。已阅读 `CODEX-SOURCE-COORDINATION.md`，连接池实现仍由源码会话负责，本报告不重复接管。

结论：连接池复用能减少握手开销，但不能解决账号级限流后的持续选中、失败刷新重复交换和准入身份使用可变名称的问题。以下“源码证实”指分支行为可以由当前源码确定；新增复现案例尚需独立 harness 执行，不代表已经测得生产发生率。

## 优先交接项

| 项目 | 证据与判断 | 合成复现与验收建议 |
| --- | --- | --- |
| **P1：账号改名可绕过并发上限**（源码证实的缺陷） | `gateway.go:173` 的 map 按名称计数，`:237` 按 `account.Name` 获取、`:241` 按旧名称释放；`gateway_sessions.go:174`、`gateway_websocket.go:311` 同样如此。管理改名保留 ID：`accounts_pg.go:520` / `:552`。 | 单账号 C=1，保持第一条 SSE 不结束；按 ID 改名并刷新账号快照；发第二条请求。当前第二条可进入同一上游身份，两个名称的计数各为 1。修复后 HTTP/WS/粘性会话均应按稳定身份共用槽位；改名不能重置占用，也不能让释放扣到新账号。 |
| **P1：上游 429 没有账号冷却状态**（源码证实的策略缺口） | `gateway.go:586` 仅透传上游错误并记录；`proxy.go:161` 保留 `Retry-After`，但选择器 `gateway.go:212` 不读取它。最高优先级账号只要令牌未过期且有槽位就继续被选中。HTTP/WS 都没有共享的 `cooldown_until`。 | 两合成账号 A 高优先级、B 低优先级；A 返回 429 + `Retry-After: 30`，B 返回 200。完成第一条后立即提交第二个**独立请求**。当前仍调用 A。测试应分别覆盖无会话与固定会话、秒数/HTTP-date、缺失头、到期恢复。固定会话不可静默切账号；冷却应反馈可重试状态。已有请求不得因 429 自动重放模型 turn。 |
| **P1：失败刷新只串行，不合并失败结果**（源码证实的策略缺口） | `accounts_pg.go:967` 先做本机锁，`:1003` 做 PG 锁；`:1033` 只在 access token 已变化时跳过交换。失败后 `:1038` 至 `:1054` 记录状态但不改变 token，等待者仍可再次交换。`gateway.go:237` 在 `ensureReady` 前占槽。 | 过期合成账号 C=8，本地 OAuth 始终返回 429 或 invalid_grant，发送 8 个同时请求；统计 OAuth 次数、槽位占用与请求耗时。再测试显式 401 恢复路径。期望同一凭据版本的失败短期合并/有界退避，成功轮换与重新导入可解除旧失败状态。PG 凭据保全与 CAS 不能因优化而被削弱。 |
| **P2：忙状态被混入 503，客户端重试提示不一致**（源码证实的接口行为） | Responses `gateway.go:521` 为名称含 busy 的错误返回 429 + 固定 `Retry-After: 1`；`forwardModels` 在 `:681` 对同一 busy 返回 503 且不带此头。若一个账号 busy、另一个刷新失败，选择器 `:253` 返回泛化 unavailable，Responses 也变 503。 | 占满单账号 C=1，分别请求 `/models` 和 `/responses`；再加入过期无 RT 的第二账号。记录错误来源、HTTP 状态与头。区分“容量忙”“暂时不可用”“配置无账号”；禁止靠错误字符串猜分类。固定 1 秒未参考长 turn 剩余时间，客户端若无抖动可同步回冲，放大量需压测量化。 |

## 连接池之外的容量边界

- **没有排队器**：仓库未找到 `scheduler*.go`；当前调度在 `pickAccount` / `pickSessionAccount`。准入是进程内原子计数、满即拒绝，并无有界等待队列。队列是可选策略，不能把它的缺失本身当作并发锁失效。已有 `TestGatewayAtomicAdmissionAndAvailableSelection` 验证同名 C=1 原子性。
- **粘性会话热点是设计边界**：`gateway_sessions.go:17` 说明加密历史绑定账号，`:174` 忙时不切换；一个 root session 及 descendants 共享账号。其他账号空闲不能证明调度 bug。验收应同时报告“单会话并行度”和“多会话总吞吐”，不能通过关粘性掩盖 429。
- **池条目上限不是连接总上限**：`gateway_client_pool.go:17` 的 128 限制 client 缓存条目，`:139` 限制空闲时间；当前 `newHTTPClient`（`proxy.go:27`）克隆 Go 默认 Transport，未设置 `MaxConnsPerHost` / `MaxIdleConnsPerHost`。本机 Go 1.26 源码 `net/http/transport.go:61` 默认每 host 空闲连接为 2，总活动连接字段为 0。请连接池 owner 单独验证 HTTP/1.1 与 HTTP/2 的活动连接数、FD、突发结束后的复用率；此处只给验收边界，不重复其在途修复。
- **多实例限制未共享**：session affinity 在 Redis，admission map 在每个 `Proxy` 内；如果未来运行 N 个进程，同账号最多可达 N×配置并发。这是部署拓扑相关风险，本轮未断言生产有多个进程。
- **入站内存限流在账号准入之前**：`gateway.go:469` 读取最多 32 MiB 请求，经过 JSON 规范化、重新编码后 `:516` 才选择账号。账号 C=1 不限制并发请求体解析的内存。建议只在独立 harness 用 1/4/16 MiB 合成输入测 RSS/P95 与拒绝时机；本轮未执行大体积压测，也未测得 OOM。
- **慢下游的非 SSE 分支待复现**：SSE `proxy.go:383` 已有 60 秒写期限；`copyErrorBody`（`:417`）、JSON/compact 的 `w.Write` 未设置同类写期限，服务 `main.go:115` 无全局 `WriteTimeout`。独立 socket 客户端不读大 JSON 响应时，可能长期占用 admission；需以受控本地连接验证，不能据此宣称生产发生了槽位泄漏。

## 错误归因与必须保留的行为

| 表象 | 当前来源 | 不能据此直接得出的结论 |
| --- | --- | --- |
| HTTP 429 | 本地 admission 满；或上游原始 429 | 不能全归为上游配额，也不能只凭错误率扩大账号并发。 |
| HTTP 503 | settings/Redis session/账号不可用/代理不可用；`/models` busy；或上游原始 503 | 不能全归为服务器 CPU、连接池或 PG 负载。 |
| HTTP 502 | 连接错误统一为 `upstream connection failed`；无效 compact/JSON/SSE 聚合；或原始上游 502 | 当前简化文案不足以分辨 DNS、TLS、连接期限、请求取消。应脱敏保存阶段/错误类别，不记录令牌、完整代理 URL 或请求正文。 |
| HTTP 200 + `response.failed` | SSE 已开始后上游失败；JSON 聚合该终态返回 502 | 必须同时统计传输状态与模型终态；连接池命中、HTTP 200 都不等于模型完成。 |

`gateway_upstream.go:11` 仅对显式 401 做同账号恢复并至多重放一次；网络失败、429、已接受的 SSE 不自动重放，这点应保留。`proxy.go:402` 已在完整终态送达后停止读取并释放请求，不能将“上游 TCP 保持打开”继续算作未完成 turn。会话续租失败取消正在运行的请求（`gateway_session_lease.go:41`）是保护账号归属的一致性边界，Redis 故障验收须单列。

## 本轮执行与版本边界

执行命令（仅本地合成 fixture，不会启用 opt-in live 测试）：

```powershell
go test -count=1 -run '^(TestGatewayAtomicAdmissionAndAvailableSelection|TestGatewaySessionStaysOnAccount|TestProxyUpstreamErrors|TestCLIStreamingIdleClosesAndCancelsUpstream|TestGatewaySessionLeaseCancelsOnBindingFailure)$' .
```

结果：`ok codex-agent 0.107s`，退出码 0。覆盖基础原子准入、固定会话、429/Retry-After 透传、空闲取消、续租失败取消；**不覆盖**上表新增改名、冷却、失败刷新风暴复现，不是全量网关或生产性能验收。

行号基于 2026-09-18 03:34（Asia/Shanghai）附近共享工作树；源码 owner 正在并行接线设置/连接池，最终交付需重新核对。审计时重要文件 SHA-256：

```text
gateway.go              b2d9871dd648e6355eab4d2ca638402a9d5f3e46ff0d372b83b7611d4cc868e9
gateway_client_pool.go  83101050810b116a9104ca44bc99058560263faaae035bdc2471d8ec74dbf1c7
proxy.go                f515fb51ba11837f5408d90f5baf6dbb4c75f938f4a59fd1c2cba9b4baab0d57
gateway_sessions.go     c1052676c353c40ff73d06d4146e2d2b68d86255f423a9caa1424184a7171eb2
gateway_websocket.go    04e76a6507ebaec1823aae766d5ca217b764d5604d10d146b3751a566e9c6c9b
accounts_pg.go          385f6b4ac1cb0d28d268e0c93fcb807d0d704c21bede95901ef2efed760a0f54
```
