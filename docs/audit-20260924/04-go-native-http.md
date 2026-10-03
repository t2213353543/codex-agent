# 04 — Go native HTTP adapter 专项审计

日期：2026-09-24。范围：`gateway_native_http.go`、`gateway_native_startup.go`；新增回归：`gateway_native_http_audit_test.go`。全部测试使用合成 IPC/helper，不访问生产服务。

## 结果

已修复 11 类明确问题，并完成进程池、启动/取消/关闭、代理发送检查、IPC、长度、超时、并发和内存边界审查。`gateway_native_startup.go` 的 disposable helper 验证路径经检查和既有测试验证，不需要修改。文件行号以本专项完成时为准。

| 问题 | 修复位置 | 修复后行为 / 回归 |
| --- | --- | --- |
| `strconv.ParseInt` 接受 HTTP Content-Length 不允许的 `+3`、`-0` | `gateway_native_http.go:939` | 仅接受非空 ASCII 数字，拒绝重复值和溢出；保留合法前导零。响应和请求使用相同规则，且请求字段名按大小写不敏感检查。 |
| 响应头只过滤 CR/LF/NUL，遗漏其他控制字符和 DEL | `gateway_native_http.go:930` | 与 Rust `HeaderValue` 对齐：允许 HTAB 和 opaque 0x80–0xff，拒绝其他 CTL/DEL；HTTP 与 05 专项 WS 共用校验。 |
| Go `[2]string` JSON 解码会丢弃多出的 tuple 字段、为缺失字段补空串 | `gateway_native_http.go:826` | 使用 `[][]string`，每个 header tuple 必须恰好两个字符串，避免把畸形 metadata 当合法空值。 |
| context 截止时间未交给 helper | `gateway_native_http.go:798` | 在 body preparation、helper startup、IPC dial、最终代理检查后发送剩余 `total_timeout_ms`；毫秒向上取整，父 context 始终负责硬截止。 |
| `Writer.Write` 短写且无 error 时被当成功 | `gateway_native_http.go:650`、`:663` | 配置/request 的 4-byte 长度头、JSON 和 body 均检查写入长度，返回 `io.ErrShortWrite`，不重放部分请求。WS 共享 JSON 写入修复。 |
| IPC ReadFull 将所有原因都丢弃为 UnexpectedEOF | `gateway_native_http.go:964`、`:980` | 截断仍返回/包含 `io.ErrUnexpectedEOF`，网络 reset/timeout 等原始原因通过 `errors.Join` 保留，可由 `errors.Is/As` 获取。 |
| body read 取消回调与正常返回各 Close 一次 | `gateway_native_http.go:679` | `sync.Once` 协调 Close，取消会解阻塞 Read，错误优先保留调用方 context，body 只关闭一次。 |
| URL 为 nil 时在 body defer 前返回，未关闭请求体 | `gateway_native_http.go:679` | body 清理先建立，再检查 URL；非法请求也释放 body。 |
| 无界 header 编码、排序及读 body 后才发现非法 metadata | `gateway_native_http.go:700`、`:885` | 编码前限制 128 个字段、字段值长度及编码预算，提前拒绝非法 token/CTL；拒绝无法表示的 Opaque URL/TransferEncoding 和 helper 不支持的 CONNECT/TRACE。 |
| ready helper 的 idle 时钟包含启动时间，reaper 未考虑剩余 startup waiters | `gateway_native_http.go:215`、`:395`、`:645` | reaper 不回收有启动等待者或正在关闭的条目；ready 时及正常领取前刷新闲置时刻。 |
| consumed DATA 零长度 slice 仍引用最后一个 64 KiB backing buffer | `gateway_native_http.go:1066` | 完整消费帧后将 pending 置 nil，已读完但调用方仍持有的 Response 不继续保留该帧。 |

此外修正 204/304 的 `http.Response.ContentLength` 为 0；HEAD 保留 representation Content-Length，但 IPC DATA 必须为空。helper error 与取消竞态时通过 `mapError` 保留调用方 cancellation。

## 一致性与有意差异

| 对照项 | 证据 | 判断 |
| --- | --- | --- |
| 每个 Responses 调用建立 CLI API transport | `.local/codex-source-0.155.0/codex-rs/core/src/client.rs:1040`；`native-transport/src/main.rs:444` | Go 默认 `cli_request`，readiness 必须回报相同生命周期；`pooled` 是显式性能模式，不能宣传为默认 CLI 完全一致。 |
| 请求 total timeout | CLI `http-client/src/transport.rs:69` 将 `Request.timeout` 交给 builder；helper `main.rs:505`、`:522` | Go 将剩余 context budget 传给 helper，helper budget 包括 IPC body upload；不存在上传后重置。Go 本地准备、启动和 IPC header 也受父 context 截止约束。 |
| 超过 24 小时的 context | helper `main.rs:371` 最大接受 86,400,000 ms | Go 省略超范围 helper 字段，避免错误缩短请求或触发 timeout_invalid；父 context 继续完整执行原截止时间。这是 IPC 协议上限差异。 |
| 原始头值和重复字段 | CLI/Rust `HeaderMap`；helper `main.rs:374`；Go `gateway_native_http.go:885` | Base64 传递字节、保留重复普通字段；不会把 opaque header bytes 当 UTF-8 重编码。Content-Length 重复被严格拒绝。 |
| Content-Length | helper `main.rs:388` 新增 ASCII digit 校验 | 请求/响应均严格按数字语法；HEAD/204/304 的 wire body 预期为 0，普通响应少字节为 UnexpectedEOF，多字节为 protocol error。 |
| HTTP 非 2xx | CLI `http-client/src/transport.rs:117`、`:143` 聚合/封装错误体 | 网关仍返回原始状态和流式 body，由上层路由/重试处理，避免吞掉上游错误体。这是网关角色的有意差异。 |
| TLS/HTTP 指纹 | Go `RoundTrip` 只发送 IPC；helper `build_http_client` 位于 `main.rs:309` | 本专项确认 native 模式没有落回 Go TLS。TLS backend、CA 和 wire fingerprint 由 Rust 专项实测，不把合成 IPC 测试等同 TLS 一致性证明。 |
| 并发 | Go `acquire` 账户 admission 模式、readiness `max_active == 0` | 无 transport-local 活跃数限制；16 是闲置 helper 缓存预算，不是请求并发上限。96 并发/20 活跃路由既有回归通过。 |
| 代理变更 | `gateway_native_http.go` acquire 后与 IPC 写入前两次 `checkProxy` | 保持选择的代理 snapshot；失效则拒绝，不悄悄换线路、不重放已发送请求。已有 body/startup/最后 dispatch 轮转回归通过。 |
| 流关闭/崩溃 | `gateway_native_http.go:430`、`:1073` | helper crash 主动收集，不等待新请求；missing END 不变成成功 EOF；Close/cancel release 使用 once；独立路由不受其他 helper crash 影响。 |

## 验证

新增 9 个 test functions（包含 40 个表驱动子案例）：响应 Content-Length/HEAD/no-body、header tuple/CTL、剩余 deadline、IPC 截断/原始错误/短写、metadata 提前拒绝、帧缓存释放、取消 Close 一次、ready waiter 防 reaper、启动取消与 8 个 concurrent Close。

最终 targeted 命令：

```powershell
go test ./... -run '^TestGatewayNative(HTTP|Startup|Resource|Audit|WSIPC|WSAudit)' -count=1 -v
```

结果：`PASS`，`ok codex-agent 5.904s`。完整日志：`.local/gateway-audit-20260924/04-go-native-regression.log`。较早的纯新增边界日志：`.local/gateway-audit-20260924/04-go-native-audit.log`。

检查中首次扩大回归发现 Windows helper Kill 返回 WSAECONNRESET，而既有测试要求 UnexpectedEOF。已经用保留原因的 `errors.Join(io.ErrUnexpectedEOF, cause)` 修复，并重新通过上述 helper crash/资源回归。

当前 Windows `CGO_ENABLED=0`，PATH 不含 gcc/clang；本专项没有声称执行 race。生产 Unix socket identity/权限用例在 Windows 按既有条件跳过。真实 Linux helper、race 与 CLI 对照由 root 在冻结集成版本上统一执行。

## 交接

05 Go native WS 专项已复用 `gatewayNativeRequestHeaders` 和 `gatewayNativeHeaderValue`，保持其函数签名不变。无需 factory/WS/SOCKS 文件的本专项额外改动；root 统一验证所有 agent 合并后的 Linux native transport 路径。
