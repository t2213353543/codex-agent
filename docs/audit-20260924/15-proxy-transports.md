# 专项 15：SOCKS5 与 native HTTP CONNECT 桥接

审查日期：2026-09-24。实现范围为 `socks5.go`、`gateway_native_socks.go`；新增测试 `socks5_audit15_test.go`、`gateway_native_socks_audit15_test.go`。未修改 proxy store、native Rust/vendor、CLI 基准或生产系统。`gateway_client_pool.go` 的 SOCKS 变体清除继承 `DialTLS` / `DialTLSContext` 由专项 13 实施。

## 结论及实际修复

修复 9 组有代码或回环测试证据的问题：

1. SOCKS 目标端口原先通过 `uint16(port)` 静默截断，空主机、零端口、超长域名、带 zone 的 IPv6 未提前拒绝。现在在创建代理 TCP 连接之前校验；合法 IPv4/IPv6 使用地址类型 1/4，域名仍以地址类型 3 交给代理解析，不做本地目标 DNS。
2. RFC 1929 认证回复原先只检查 STATUS；现在同时要求 VER=1。RFC 1928 CONNECT 回复现在要求 VER=5、RSV=0，并拒绝空 BND.DOMAIN、未知 ATYP 和截断回复。
3. greeting/auth/connect 的单次 `Write` 原先未处理短写。现在逐段写完，零进展立即返回 `io.ErrShortWrite`，不无限循环。
4. 密码存在但用户名为空原先静默变成匿名代理；超长认证字段也可能在代理选择 no-auth 时绕过长度校验。现在提前拒绝。空密码仍允许，保持 CLI pinned SOCKS 客户端兼容行为。
5. 连接底层错误原先可直接展示代理地址；握手固定文本又丢失底层 timeout 身份。现在用固定公开错误文本包装原因，保留 `errors.Is` / `errors.As`，不在错误字符串中展示 endpoint 或凭据。
6. `httputil.ReverseProxy` 的 Rewrite 模式默认清理查询串，会删除 semicolon 或错误转义的参数。固定 origin 桥接现恢复原始 RawQuery，回环服务器验证完整 request URI。
7. `Request.Clone` 在入站 body EOF 前克隆 Trailer，导致 chunked 请求 trailer 值丢失。现以 EOF reader 同步已声明的端到端 trailer，并剔除代理认证、hop-by-hop、Connection 指定字段和转发身份字段。请求及响应 entity/trailer、重复 header 均经过回环验证。
8. Go `http.Transport` 使用 `context.WithoutCancel` 为连接复用分离 dial 生命周期，导致客户端取消后仍等待 SOCKS 握手。桥接本来禁用连接复用，现通过私有 context key 保存原始请求取消信号，并绑定实际 SOCKS dial。HTTP 取消回环测试原先失败，修复后代理端及时观察 EOF。
9. CONNECT 隧道任一方向结束便完整关闭两端，截断半关闭后的响应。现正常 EOF 仅 `CloseWrite`，保留反向流，出错关闭两端；bridge context 的取消回调显式关闭 hijacked client 与 upstream。双方先 half-close 的回环测试均通过，8 个并发 Close 也能结束正在等待上游的半关闭隧道。

## 边界核查矩阵

| 边界 | 处理与证据 |
| --- | --- |
| SOCKS greeting | 检查版本、no-auth、user/pass、0xff、未提供认证、未知方法及截断回复；恶意 peer 在 loopback TCP 上测试 |
| RFC 1929 | 检查 auth VER/STATUS、字段字节长度、密码无用户名；短写逐段完成；保留 CLI 空密码兼容 |
| CONNECT port | 拒绝负数、0、65536、带加号或不可解析端口；最大 65535 wire encoding 验证 |
| CONNECT reply | 检查 RSV、ATYP、域名非空及截断；三种 BND.ADDR 精确消费，不吞首段 application bytes |
| IPv4/IPv6/domain | IPv4=ATYP 1、IPv6=ATYP 4、domain=ATYP 3；未解析的 `.invalid` 域名在 SOCKS peer 捕获 |
| IPv6 zone | SOCKS 协议没有 zone 字段，拒绝，不删 zone 后错误投递到另一接口，也不误发 DNS |
| 目标语法 | 拒绝空域名、超长域名、控制字符、路径、userinfo、query、fragment、非法括号/冒号 |
| 取消与期限 | 原有三阶段主动取消测试通过；新增三阶段期限测试保留 timeout 身份并关闭 socket；成功连接清除握手 deadline，后续 dial context 到期不关闭已建立连接 |
| CONNECT authority | 固定 scheme+host+port；规范化域名大小写、合法 IPv6 地址和十进制端口；拒绝 query/fragment 空分隔符、尾冒号、未加括号 IPv6、userinfo、zone |
| 原始 Host 的范围 | handler 直接测试 URL/Host 不一致会拒绝。Go HTTP parser 对 CONNECT 和 absolute-form 请求会将 `r.Host` 设为 request target authority，无法在此层辨识被 parser 丢弃的原始 Host header；实际拨号始终只允许固定 `b.authority`，不存在由该原始 header 改拨其他地址的路径 |
| HTTP 与 HTTPS | HTTPS CONNECT 仅传输原始字节，TLS 在 Rust 客户端到 origin 之间完成；明文 HTTP 经 Go serializer，保留 entity/query/end-to-end 重复 headers，承认 framing 变化；明文 WS 用固定 port 80 CONNECT，沿用既有测试 |
| 桥接权限与错误 | 随机本机 Basic 认证、单一认证字段、constant-time 比较；错误认证 407、目标违规 403、代理失败 502，无凭据或原始错误返回 |
| header/chunking | Proxy-Authorization 不到 origin；Connection token 字段移除；重复 headers、chunked entity、请求/响应 trailers 保留；trailer 不能绕过代理/hop header 过滤 |
| CONNECT 首包 | CONNECT headers 和首段 TLS bytes 同一个 TCP Write，验证 hijack buffered reader 不丢首包 |
| 半关闭与资源 | client/upstream 双方向 half-close、8 个并发 Close、原有 96 并发连接两轮回填、活跃握手 shutdown 均通过 |
| 显式代理失败 | 保留原有认证失败无 CONNECT/无直接回退测试；SOCKS 唯一 dial 路径没有 direct fallback；transport TLS dial hook 由专项 13 清理 |

## 与 CLI 0.155.0 的一致点和有意差异

CLI 源码只读基准：`.local/codex-source-0.155.0/`。

| 基准位置 | 一致性判断 |
| --- | --- |
| `codex-rs/codex-api/src/endpoint/responses_websocket.rs:519` | CLI 以有效 `HttpClientFactory` 创建 WebSocketConnector；此专项不修改请求协议和 native connector |
| `codex-rs/websocket-client/src/dialer.rs:100` | CLI 先连接显式 proxy，再 `connect_via_proxy`，最后 `client_async_tls_with_config`。bridge 同样先 SOCKS CONNECT，再让 Rust 对 opaque tunnel 做目标 TLS，显式 route 出错不直连 |
| `codex-rs/websocket-client/src/dialer.rs:107` | CLI 自身支持 HTTPS proxy 的 TLS-to-proxy；本专项 SOCKS bridge 是本机 HTTP→SOCKS 协议适配，并不声称 SOCKS hop 有 TLS，也不更改 HTTPS proxy 功能 |
| `native-transport/ws-plan/pinned-source/tokio-tungstenite-proxy.rs` | pinned 依赖按 no-auth + 可选 user/pass greeting、`write_all`、RFC1929 VER/STATUS、IP/domain 编码及网络字节序端口。Go 实现现在对齐上述正常 wire 行为 |
| 同上，CONNECT reply parser | pinned 实现未验证 RSV；本次 Go 实现更严格，拒绝 RSV 非零和空 bound domain。这是畸形代理回复处理强化，不是正常协议差异 |
| 同上，认证 | pinned 允许空用户名/密码字节串；Go 保留空密码，但有密码无用户名直接报错，避免现有模型以空用户名表示未认证时静默降级 |
| HTTP 代理客户端 | native HTTP 使用 `native-transport/src/main.rs:313` 的 `no_proxy()` 及显式 `Proxy::all`；WS 使用 `native-transport/src/websocket.rs:253` 的显式路由。网关账户路由隔离优先，不继承 CLI 的环境 NO_PROXY/系统自动代理选择行为 |
| 明文 HTTP serialization | bridge 使用 Go HTTP/1.1 serializer，禁用 compression/reuse；正常 body、原始 query、end-to-end headers、declared trailers 保留，但 header 大小写/顺序、分块大小、hop-by-hop 字段不逐字节模拟 CLI。HTTPS/WSS tunnel 不进行 HTTP 反序列化，目标 TLS 指纹不由该 bridge 生成或重写 |
| 超时/关闭 | Go SOCKS 有 30 秒握手上界及更早 context deadline；bridge 还叠加配置 header timeout。与 CLI connector timeout 不逐值相同，属于网关资源边界；已建立连接不受 dial context 到期影响 |

协议核对来源：[RFC 1928](https://www.rfc-editor.org/rfc/rfc1928)、[RFC 1929](https://www.rfc-editor.org/rfc/rfc1929)。字段规则也与本地 pinned 依赖逐段对照。

## 验证结果与复跑

最终本专项完整命令：

```powershell
go test . -run 'TestAudit15|TestSOCKS5|TestGatewayNativeSOCKSBridge' -count=3 -timeout=90s
```

结果：`ok codex-agent 1.918s`，最终代码新增 14 个顶层测试，包含多项 table-driven 边界、真实 TCP/HTTP 回环和 short-write 故障注入。修复中另有一次 5 轮通过，最终新增期限释放及 HTTP route escape 后再执行上述 3 轮通过。

此专项未把 Windows 普通测试当作 Linux race 或真实 Rust TLS 指纹实测。root 统一执行 Linux race 和 `CODEX_NATIVE_HTTP_TEST_BIN` 指定真实 helper 的集成验证，其中现有 `TestGatewayNativeActualSOCKSTLSAndRemoteDNS` 及 `TestGatewayNativeActualSOCKSFailureDoesNotRefreshOrFallback` 覆盖 native SOCKS TLS/远端 DNS/代理失败。没有对 CLI 基准、Rust/vendor 做写入，也未部署生产。
