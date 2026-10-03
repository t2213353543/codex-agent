# Q05：账号、基础设施 Cookie 与 HTTP/WS 状态边界

检查对象为 2026-09-24 当前工作区、固定 CLI 0.155.0 源码和本地合成流量。本专项不使用真实账号，不请求 OpenAI，不修改生产配置。先用 `gateway_native_cookie_identity_test.go` 实测修复前边界，再修改 Go 侧 helper 分区；vendored Rust、helper IPC、指纹/TLS 实现和并发准入均未修改。

## 可以确认和不能确认的命题

修复前源码与 Linux 真 helper 实测共同确认：同 origin、代理及 timeout 的请求复用同一 native helper；helper 的官方 Cookie 容器为进程级共享。因此原来的 pool key 并不构成账号边界。`cli_request` 重建 HTTP client 并不清空该容器。修复后在保留这些 route 维度的同时加入稳定 owner 摘要。

这不是普通会话 Cookie 泄露：容器只允许官方基础设施 Cookie（包括 `__oailb`）；账户/session/auth Cookie 仍被拒绝。官方库明确允许基础设施 Cookie 跨 auth context；它没有向网关保证这个共享范围适合大量独立账号。`__oailb` 的路由含义也不能推出强/弱算力分档，更不能仅凭这个 Cookie 的存在认定答错题的原因。

TLS、连接和 Cookie 是三个不同边界：新 TLS 连接不能证明 Cookie 新建；同一 helper 也不能证明 WS 自动携带 HTTP Cookie。必须分别观察。

## 源码依据

- 修复前 `gateway_native_http.go` 的 `gatewayNativeKey` 只有 proxy ID/version/fingerprint、origin 与 timeout，没有 owner；修复后新增 `[32]byte accountScope`。
- `native-transport/src/main.rs` 的 `build_http_client` 调用官方 `with_chatgpt_cloudflare_cookie_store`；`cli_request` 在每个事务中重建 client，但该函数仍取得同一个 static store。
- `native-transport/vendor/codex-http-client/src/chatgpt_cloudflare_cookies.rs` 的 static `SHARED_CHATGPT_CLOUDFLARE_COOKIE_STORE`、`CookieStore` 实现与 host/HTTPS/名称筛选。
- 该文件与 `.local/codex-source-0.155.0/codex-rs/http-client/src/chatgpt_cloudflare_cookies.rs` 字节一致，SHA256 均为 `070912085118365df3a9944f8b595527110c4113b34b3a3e6e7e35d80c9a8011`。
- `native-transport/src/websocket.rs` 把传入 headers 交给 WebSocket connector，没有读取或写入上述 HTTP Cookie 容器；固定官方 websocket-client 源码也没有 Cookie 容器逻辑。

## 实验设计

本地临时 CA 签发合成 `chatgpt.com` 证书；只允许该 hostname 的 CONNECT 代理把 TCP 转发至 loopback TLS server。没有公网 DNS/上游连接；所有 bearer 均为以 `.synthetic` 结尾的合成值。

每次 HTTP 调用都新取一个 Go client，完整读完 body 后才发下一次请求。上游记录最终 Authorization/ChatGPT-Account-Id 相等性、Cookie 名/合成标记、TCP connection ID、HTTP major version、TLS `DidResume`。测试日志不输出 bearer 或 Cookie 值。HTTP timeout 特意设为 WS 固定的 90 秒，并检查两者共用一个 helper，防止“碰巧不同进程”掩盖 WS 结果。

修复前两种 lifecycle 均执行下列三个顶级测试，共六个 native 子测试：

| 测试 | 边界和旧实现预期 |
|---|---|
| `TestGatewayNativeActualCookieIdentityCharacterization` | A 首次设置 route-a；A 再次保留；B 请求携带 route-a 后设置 route-b；返回 A 携带 route-b；A 的 AT 更新后仍带 route-b；同 workspace 不同 user 也携带 route-b 并设置 route-c；原 user 和匿名请求随后携带 route-c。所有授权头必须逐次正确。 |
| 同测试连接断言 | `cli_request` 的九个 HTTP 请求使用九条连接；`pooled` 使用一条连接。两个 lifecycle 的进程数量均为一，说明重建 client/连接不等于重建 Cookie store。 |
| `TestGatewayNativeActualCookieSeparateProcessesControl` | A 和 B 使用独立 Proxy/native pool，首次请求均无 Cookie；各自设置后返回只收到自己的标记。此对照单独检验进程边界。 |
| `TestGatewayNativeActualCookieHTTPWSBoundary` | HTTP 先设置 route-a；同 helper 上按 A→B→A 建立 WS，握手均无 Cookie。每次 WS 握手响应设置 `__oailb=ws-only` 后，HTTP 请求仍看到 route-a，证明 WS 没有改写 HTTP 容器。 |

修复前 Linux 使用真实 helper 的 `-race` 运行结果：三个顶级、六个 native 子测试全部通过，耗时 3.834 秒（由主任务保存基线输出）。这验证了上表的跨授权 Cookie 串用与进程隔离对照。旧 `Characterization` 是修复前行为记录，不是永久产品契约。

修复后将 `Characterization` 改为 `TestGatewayNativeActualCookieCredentialIsolation`：无可信 Account context 的 A/B、同 workspace 不同 user、新 AT 与匿名请求独立分区；同一个 bearer 可复用。新增 `TestGatewayNativeActualCookieTrustedOwnerContinuity`：通过真实 `doAccountRequest` dispatch 检查 A/B、同 management ID/workspace 不同 user 分离；调用真实 OAuth refresh 验证没有 Authorization 的刷新也复用该 owner；新 AT 保留 Cookie；Prepared WS 与 HTTP 共享同 owner helper；owner revision 变化取得空容器。测试检查 proxy ID/fingerprint 没有改变，并输出 2/4 owner 的 helper RSS/匿名内存/线程/FD 总量快照。

另新增两个普通 Go 测试检查 fallback header 大小写、重复值、新 AT 不信任 JWT claims、未知 owner 凭据变化、401→refresh→retry 的 scope、管理请求 scope、调用方 context 不被改写及取消继承。

Windows 关联回归 `Test(GatewayNative|GatewayOwner|GatewayRequestRoute|GatewayRecovery|Source401|AuthRefresh|ManagedUpstream|ManagedControl|ManagedPAT|ManagedOAuth)` 已通过，耗时 7.146 秒；native 子测试因 Linux-only 跳过，不能代替 native 行为验证。修复后 Linux 真 helper 的 `-race` 运行六个顶级测试、八个 native 子测试全部通过，耗时 5.644 秒。随后将 trusted-owner teammate 用例收紧为同 management ID/name、同 workspace、仅 user 改变；该最后测试强化由主任务的全量 final run 复验，production 补丁未再变更。

`go vet ./...` 通过。资源日志是测试时刻的 `/proc` 快照，不是负载长期峰值；RSS 相加包含多进程共享的文件页，不能直接当作新增独占内存，故同时记录 RssAnon。

修复后 Linux 同一测试环境资源实测：

| lifecycle | owner/helper 数 | RSS 合计 KiB | RssAnon 合计 KiB | FD 合计 |
|---|---:|---:|---:|---:|
| cli_request | 2 | 17,920 | 2,560 | 14 |
| cli_request | 4 | 36,480 | 5,760 | 28 |
| pooled | 2 | 18,560 | 2,560 | 16 |
| pooled | 4 | 37,120 | 5,120 | 32 |

`cli_request` 两/四 helper 线程数为 8/16。这确认隔离的进程资源开销会随活跃 owner/route 组合增长；本次不改变 account admission、不新增总并发上限。不能从两/四 owner 小样本直接外推生产千账号长期内存上限。

Linux 复验命令（`CODEX_NATIVE_HTTP_TEST_BIN` 必须指向已核验 helper）：

```bash
CODEX_NATIVE_HTTP_TEST_BIN=/absolute/path/codex-native-transport go test -race . -run '^TestGatewayNative(AccountScope|ActualCookie)' -count=1 -v
```

## 隔离方案比较

| 方案 | 能隔离什么 | 工作量及资源取舍 |
|---|---|---|
| 在 Go pool key 增加稳定 owner 摘要，每 owner helper | 官方全局 Cookie 容器天然分开；`pooled` HTTP 连接/TLS session、WS connector/TLS cache 同时按 owner 分开；无需改官方 Cookie 代码、allowlist 或 helper IPC。 | 实现路径最短、边界最清楚。活跃 helper 数由 route 数变为实际活跃 owner×route 组合数，增加 RSS、进程/socket/线程数与冷启动成本。现有 idle 上限 16 只限制空闲进程，不限制 active/startup；不能误报为“全池最多 16 进程”。 |
| 单 helper 内按 owner 维护 Cookie jar/client | `cli_request` 可保留每 owner jar 并每请求建 client；减少进程基线成本。 | 需新增可信 owner IPC 字段、更新能力握手、实现/暴露 scoped allowlist store、并发 owner map、idle/LRU/引用生命周期。只分 jar 不分 pooled client 仍共享 TCP/TLS；WS connector 也需额外按 owner 分区。改动与验证面更大。 |
| 每次请求清空 Cookie / 每次启新 helper | 避免跨请求串用。 | 同时丢失同账号正常 affinity 和官方进程内 Cookie 连续性，额外冷启动，不能作为默认优化。 |

本次采用 owner hash pool 分区作为修复；大规模多账号运行测出 RSS/冷启动瓶颈后，才考虑将边界下沉到 helper 内。没有只按 `ChatGPT-Account-Id` 分区，因为 workspace 可有多 user；也没有把所有生产请求仅按 AT 分区，否则同 owner 刷新每次丢 affinity。

`gateway_native_identity.go` 复用现有 `gatewayAccountRef(account)` 所表达的所有权连续性，加域分隔并 SHA256，避免把管理名称/ID放进进程 key。已知 user+workspace 的 AT refresh 保持 owner；未知 owner 使用现有 credentials-qualified ref 保守分离；re-import/owner revision 失去旧 Cookie/TLS 状态。生产路径从已选择且校验过的 Account 派生 context scope，不依靠用户传来的账号头作为信任来源。没有 Account 的工具/测试 transport 使用 exact Authorization+ChatGPT-Account-Id 摘要 fallback，避免授权 HTTP 和匿名探测共享默认 key；不从未验证 JWT 中猜测稳定 user。无 Authorization、无可信 owner 的请求保留零 scope。

接入点为 `doAccountRequest` 每次 dispatch（Responses、models、compact、401 retry）、`refreshAccountWithSettings`、`dialGatewayWebSocketPrepared`、`managedUpstreamHeaders`（管理模型、用量、测试流、compact、privacy/reset 及 PAT）。不同 origin/timeout/代理仍保留原有池分区。OAuth refresh 从被刷新的 Account 派生 scope，不能把缺少 Authorization 的 token 请求误判为匿名；尚无账号身份的新登录/OAuth exchange 不虚构 owner。此变更不把已有 Cookie 在 helper 间迁移。

## 质量结论边界

本实验直接检验账号间是否共享请求路由状态和连接/TLS状态；即便全部复现，也不验证该共享导致模型质量下降。需要保留原始糖果题、模型/effort/tier/instructions、出口和请求版本，比较同账号旧共享/新隔离/原生直连的交错样本。此前异常账号在原生直连和官方 CLI 也答错，故不能把修复 Cookie 边界写成已恢复该账号质量。
