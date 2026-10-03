# 专项 14：配置、启动、关闭与响应状态

日期：2026-09-24。只读基线：`.local/codex-source-0.155.0/codex-rs`。修改 `main.go`、`gateway_native_factory.go`、`gateway_response_writer.go` 及 `store.go` 的 settings 部分；新增两个 runtime audit 测试文件。未访问生产环境。

## 修复

1. **启动失败泄漏 listener。** `serveManaged` 原先在 ingress collector 初始化失败时直接返回，已绑定 socket 没有释放。现在进入该函数即承担 listener 关闭职责。真实 loopback listener 回归确认返回后 `Accept` 为 `net.ErrClosed`。
2. **持久化坏配置绕过验证。** 管理 API / `Set` 校验不等于磁盘或 PG 文档一定有效；旧 `Get` / `Latest` 直接发布坏值，超大秒数可在后续 `time.Duration` 换算时溢出。新增 `validatedSettings`，先迁移缺失字段，再校验显式字段，失败不发布缓存。启动在监听前加载验证；存储值错误可导致启动失败或运行请求 503，不再静默执行损坏策略。缺字段的旧文档仍兼容。
3. **settings 版本溢出及负版本绕过。** `Set` 拒绝负请求版本，拒绝持久负版本和 `MaxInt64` 再递增；失败保留文档及缓存。0 仍是现有内部调用兼容值。最大合法版本可以读取，但不能继续增量修改。
4. **readiness 未反映坏配置。** `/health/ready` 在已有 PostgreSQL / Redis / metrics 检查后，使用原有 2 秒 readiness context 读取并验证当前 settings 文档；不使用可能陈旧的 runtime cache，不引入 SettingsStore 的独立 20 秒查询预算。坏配置返回 `settings_unavailable`，修复后可重新 ready；liveness 继续独立。
5. **native 配置 nil / 重复调用。** nil Proxy / getenv 现在返回明确错误。已配置 pool 或残留 native WS 标志时，即使新环境值为空也拒绝重复初始化，避免看似成功却保留旧传输模式。此配置入口仍是启动单线程入口，不宣称可在运行中并发热切换。
6. **101 和无效状态记录错误。** ResponseWriter 将 101 作为最终 HTTP 状态，其他 1xx 保持临时状态；先让底层验证 status，再记录，防止底层 panic 的非法 1000 被记为实际响应。支持可选 nil record；首次最终状态不被后续调用改写。Flush 的隐式 200、错误返回和 ResponseController unwrap 保持。
7. **避免 PG 设置读取的无用文件 stat。** 原 `if` 的 initializer 在 PG 分支也执行 `os.Stat("")`；现在仅文件兼容模式检查缺失文件。

专项 11 的 TTL 测试与报告已按 root 授权同步：非法持久化 TTL 在 SettingsStore 被拒绝且不创建绑定；其 `sessionAccount` 换算前范围检查继续作为防御。

## 检查边界与一致性

| 边界 | 检查结果 / CLI 差异 |
| --- | --- |
| helper 文件和 SHA256 | 构造要求绝对路径、正常文件及完整 32 字节摘要；启动再次校验文件。已有 binary-change / seal 测试由 native HTTP 专项覆盖。摘要是完整性边界，不是 TLS 一致性证明。 |
| startup capabilities | 既有测试覆盖 compression、WS、unknown capability、bad protocol、old/wrong lifecycle、missing/legacy concurrency、超时、非零退出和强制终止；失败不发布 pool。 |
| startup disposable child | probe 不占 serving pool；必须正常退出、回收进程并清理私有目录；原生 Unix socket replacement 身份检查保留。 |
| HTTP / WS 模式 | 原生 HTTP 与 WS 开关明确分离；HTTP-only 模式的 WS 仍走 Go。`CODEX_NATIVE_WEBSOCKET=1` 必须同时有 helper 和 digest。禁用 HTTP JSON/SSE 不会隐式禁用 WS。 |
| nil / 重复 configure | 新 audit 拒绝 nil reader、nil owner、已有 pool、无 pool 却启用 WS 的非法残留状态。并发 reconfigure 不在支持范围。 |
| 环境 CA 的真正读取 | `gatewayNativeEnvironment` 只继承 PATH 和 CA 相关环境；Rust `main.rs:294-296` 真正构造 HTTP client 与 WS connector，`main.rs:536` 完成后才在 `:541` 输出 ready。不是看到 PEM 文件存在就判 TLS 可用。 |
| CA 优先级 | CLI `http-client/src/custom_ca.rs:167`、`:387-407`：非空 CODEX 优先，空 CODEX 回退 SSL；helper 复用该实现。新真实 helper 测试覆盖现存坏 PEM、坏 primary + 好 fallback 必须失败、好 primary + 坏 fallback 成功、空 primary 使用有效 fallback。 |
| CA 的平台与握手限制 | 启动成功只证明该运行时加载并构造 TLS 配置，不能证明目标服务器握手。HTTP 与 WS 的真实证书、SNI、ALPN、指纹对照见专项 02 / 03 / 16 / 17；Go 默认模式和 Rust native 模式不可当作同一 TLS 指纹。 |
| lifecycle | `cli_request` 默认与 CLI 请求建客户端路径匹配；`pooled` 是明确的性能扩展，TLS session / connection reuse 不应宣称等同 CLI 默认。 |
| custom timeout 实际加载 | 新 self-exec helper 回显已加载启动配置：HTTP 默认、显式 17 秒、显式 0、compact 默认、compact 自定义均一致；原始共享 client / transport 不变。 |
| long stream | `doStreamingRequestWithIdleTimeout` 清除继承的 client 总 timeout，只对上游等待施加 idle 限制。CLI `model-provider-info/src/lib.rs:29` 默认 idle 300000 ms，与网关 300 秒默认相同。 |
| WS connect timeout | CLI `model-provider-info/src/lib.rs:34` / `core/src/client.rs:1081` 默认 15 秒；网关有可配置 headers/idle 策略及多租户适配层。不能用默认 SSE idle 相同推导所有 timeout 完全一致；timeout/pool 专项 13 负责具体执行边界。 |
| settings 范围和 cache | 新 audit 覆盖负/零/溢出 header、compact、idle、TTL、无效 fingerprint / prompt mode、坏缓存不发布与修复重读。文件缓存依赖当前实例 Set 更新；PG 运行请求最多约 2 秒传播延迟，readiness 当前文档直读。 |
| settings 修复与版本 | 新 PG opt-in 测试覆盖 fresh cache 下 Latest 拒绝坏值、过期 Get 拒绝、保留旧 cache 对象、合法 Set 修复和版本递增。另有 MaxInt64/负版本磁盘不变断言。 |
| listener 启动失败 | 本次已修；net.Listen 自身失败时原有 p.closeGatewayClients 仍执行。 |
| workers 与最终持久化 | context 取消后等待 metrics ticker、三个管理 worker；停止接收后 `active.Wait`，然后关闭 ingress 并最终保存 metrics/key。CLI 单客户端无对应多租户管理生命周期。 |
| WaitGroup / hijacked WS | handler 入口先 Add，随后才可能 hijack；WS request context 在 shutdown 明确取消，解决 http.Server.Shutdown 不等待 hijacked socket 的边界；已有 idle / active WS 真实 socket 回归通过。 |
| HTTP drain / refresh | Shutdown 为普通 HTTP 保留 55 秒 drain，超时后 Close 取消请求；不在信号到达时提前取消正常 HTTP。既有 HTTP drain 回归通过；轮换 refresh 持久化依赖进程级 PG + SIGTERM 测试，交 root Linux gate，未用 Windows skip 冒充验证。 |
| ResponseWriter 1xx / 101 / Flush | 新 audit 覆盖 103→100→201、101、重复最终 status、101 后 status、implicit Write 200、FlushError 原样返回、非法 status 不记账。既有真实 stream flush / write deadline 回归通过。 |

## 验证

本机 Windows 已执行，最后一轮主包 **3.549 秒通过**：

```powershell
go test ./... -run 'Test(GatewayRuntimeAudit|PGGatewayRuntimeAudit|AuditSessionPersistedTTL|GatewayNativeStartup|GatewayNativeConfiguration|GatewayNativeFactory|GatewayResponseWriter|GatewayShutdown|GatewayFeatureSettings|SettingsCustomization|GatewaySettings)' -count=1 -timeout=120s
```

新增 `gateway_runtime_config_audit_test.go` 8 个测试函数，`gateway_runtime_config_integration_audit_test.go` 3 个测试函数。后者的真实 native CA 测试需要 Linux + `CODEX_NATIVE_HTTP_TEST_BIN`；PG tests 需要 `TEST_DATABASE_URL`，readiness 还需要 `TEST_REDIS_URL`。本机没有这些服务环境，相关测试明确 skip，已交 root 统一运行。最终 race、全量 PG、真实 native 构建与进程 shutdown 验证以 root 总报告为准。

没有声称 Windows/macOS 原生 helper 已支持、生产已更新、readiness 证明目标可连、或所有 CLI 请求字节已完全一致。
