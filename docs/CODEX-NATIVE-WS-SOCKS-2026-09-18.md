# 原生 WS 与 SOCKS5

本轮已将原生 WebSocket 和 SOCKS5 路由接入代码，并把参考正式版更新为 **Codex CLI 0.155.0**。全部实验使用隔离合成上游，不使用生产账号、不部署。源码变化依据见 `CODEX-CLI-0155-SOURCE-DELTA.md`，准确配对与证据哈希见同名 JSON。

## 实现

- `CODEX_NATIVE_HTTP_HELPER` + `CODEX_NATIVE_HTTP_SHA256` 继续显式启用原生 HTTP；额外 `CODEX_NATIVE_WEBSOCKET=1` 才启用原生 WS。默认均不启用，WS 不支持时不会静默回退 Go。
- Rust 使用官方 WS connector、固定两个 fork revision、ExplicitCodexTls、默认 deflate 与 TcpNodelay；唯一来源补丁是接受 Go 已选定代理的窄入口，禁止环境代理或 NO_PROXY 改变路由。
- WS 通过消息 IPC 传递完整解码后的 text/binary，握手、掩码、分帧、压缩、Ping/Pong 留在 Rust。Go 保留账号准入、会话、凭据恢复、逐轮授权、用量和 429 冷却；不伪造 101 body。
- SEND/ACK 有序且写入模糊失败不重放；Go 读 pump 从握手完成即启动。16 条消息队列与 32 MiB 已排队 payload 上限并存，另有当前帧/编码拷贝和库/内核缓冲，不能把 32 MiB 称为总 RSS 上限。背压超时或取消明确结束连接，不创建无界队列。
- 只有目标握手的明确 HTTP 401 可同账号恢复一次。代理连接失败不是目标 401，不能触发 OAuth；429 和 Retry-After 保留。非 101 body 仅为官方 fork 已捕获的有界 tail，完整性未知，不补请求获取完整 body。
- SOCKS5 使用已有拨号器和每 helper 的受认证固定目标 CONNECT 桥。HTTPS/WSS 隧道内 TLS 原样由 Rust建立，域名交 SOCKS 远端解析；明文 HTTP 使用标准反向代理重新序列化，保留实体及重复头语义，但不声称 wire 字节一致。失败不直连、不读取系统代理。
- 0.155 的受限基础设施 cookie jar 已接入 HTTP，允许官方 CF 名称及 `__oailb`，不存储任意 session/auth cookie。Cookie 只在各 helper 进程内，不落盘。

## 真实客户端验证

官方 `codex-cli 0.155.0` 的真实二进制运行了 HTTP 和 WS。模型返回由合成上游生成，工具执行、客户端状态和 resume 均是真实 CLI 行为，而非模拟 CLI 或真实模型推理。

| 验证 | 结果 |
| --- | --- |
| HTTP CLI -> Go -> Rust UDS -> TLS | 工具调用和回传通过，17 项断言通过，Go 上游 fallback trap 为 0。 |
| WS CLI -> Go -> Rust UDS -> WS | 2 条连接、2 次 warmup、3 个生成响应、3 次增量续接；真实 echo shell、进程 resume 与上下文校验通过，Go fallback 为 0。此项明文 WS；TLS 和 SOCKS 另有专测。 |
| 原生 WSS 工具多轮 | deflate、大消息、Ping/Pong、逐轮 usage 和准入释放通过。 |
| WSS 401 / 429 | 握手 401 一次恢复和 RT 持久化通过；429 + Retry-After 原样保留，不重放已接受轮次。 |
| 原生 HTTP / WSS 经 SOCKS | TLS 1.3 / H2、direct/SOCKS ClientHello 已解析字段一致、远端 DNS、3 次请求复用 1 隧道；WSS SOCKS 收发通过。错误密码仅尝试 SOCKS，不直接访问目标、不触发 OAuth、不回退 Go。 |
| 基础设施 cookie | 合成 chatgpt.com TLS 经固定本地 CONNECT；带 Secure 标记的 __oailb/CF 在 TLS 重发、Path/外域/过期规则及拒绝账户 cookie 通过。本轮没有 HTTP 降级的 Secure 负例，也没有访问真实 ChatGPT 域名。 |
| Rust 自身 | 24 项 HTTP/UDS 回归 + 4 项真实 WS socket 测试通过；固定 lock 校验 265 个包，30 个原样 Rust 源文件及 1 个记录补丁的 adapter。 |

最终 helper 位于 `.local/native-local-build/20260918T035634Z-80cf0e2a/`，SHA256 `fb535a5564e2125ad924ab79c9c487cf8307e1884fe960a13a0584f85f8aea29`。最终 Go 验证源码为 `.local/source-20260918/gateway-overlay-20260918t040658-95b49d/source`，SHA256 `39347d9d6ad717f1df988c3be13b53dcefc846ae09ed5e07092f54d36a266357`，基于已验管理 5a818 加明确网关文件，不夹带共享树之后的管理代码。

该同一 Go 快照全库测试与 vet 通过，Windows/Linux 构建通过；Linux 真实 PG/Redis `-race` 204 顶层、442 通过事件、零失败/跳过/遗漏，88.944 秒。真实 PG/Redis 上的合成用户身份更换与 CAS 隔离、停机刷新持久化和冷却必测均实际执行，临时容器、网络及凭据已清理。Linux 网关产物 SHA256 `cbe3bf9c09cdb245c35be2138e0336b13bbc7f46cb7a5cf75062fceb21bc0594`。最终配对的 12 个原生联调顶层测试及 TLS 对照都重新运行通过，不借用前一候选的通过结果。

## 回归与修复

接口迁移曾把 Go 的 typed nil 包装成非 nil 上游接口，导致旧 Go WS 握手拒绝路径 panic。旧回归和 race 找到后，已显式返回 literal nil，并增加专项测试。大 WS 响应的首轮测试失败源于测试客户端默认 32 KiB 读限，已给测试客户端设置网关同等上限；SOCKS 错误密码测试最初误期待 HTTP 的 502，后核对既有 WS transport 契约为 503，仅修测试预期。所有失败记录保留，不把首轮说成全绿。

另修正式停机测试夹具：初始化 PG 管理密钥并使用 defaultSettings，使测试实际进入刷新/断开/SIGTERM，而非停在 401/403。仅测试初始化变化，独立真实 PG/Redis race 已通过并交付部署会话。

0.155 身份复核还发现旧绑定遗漏用户维度：同 workspace 换用户可能继续使用旧线程。现已知 UserID 的哈希加入会话 owner reference，同用户刷新保持稳定，换用户或删除已知身份时旧 HTTP/WS 绑定拒绝继续。旧格式的已知用户绑定不自动迁移，需要新线程；不会迁移加密历史。未知 UserID 仍保留既有行为，尚未实现 CLI 完整的保守 unknown-owner generation，不能宣称该边界完全对齐。

## 尚未完成

当前没有生产部署、没有本版本的真实模型任务或生产错误率/缓存率前后对照。TLS 样本仅为同平台已解析 ClientHello 和 custom-CA 协商；生产 SNI、恢复握手、HTTP/2 SETTINGS、全设备与美国出口环境仍需独立证据。此前 84.22% 缓存占比属于旧候选实网任务，不移植为本轮收益。

复跑本轮真实客户端合成链路：

```powershell
python tools/native_transport_compare.py --native-run 20260918T035634Z-80cf0e2a --source-dir .local/source-20260918/gateway-overlay-20260918t040658-95b49d/source --suite gateway
```
