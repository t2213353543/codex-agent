# 专项 05：Go 原生 WebSocket 适配层

日期：2026-09-24。范围：`gateway_native_ws.go`、新增 `gateway_native_ws_audit_test.go`。CLI 对照为只读 `.local/codex-source-0.155.0`，Rust helper 由专项 03 修改，HTTP 公用校验由专项 04 修改。本专项没有修改生产配置或部署。

## 结论

Go 适配层发现并修复了真实的错误分类丢失、IPC 元数据容错过宽、取消竞争与关闭码不一致问题。新增 11 个顶层测试，与现有 IPC/预算测试共 29 个顶层、92 个测试通过事件，0 失败、0 跳过；定向连续运行 10 轮通过。常见消息路径现在不创建背压定时器，预算与队列微基准为 37.92 ns/op、0 B/op、0 allocs/op。

这些结果证明 Go 合成 IPC 边界和状态处理；TLS ClientHello、真实压缩/分片和 Linux race 验证由主任务统一运行，不能用此处 Go 合成测试代替。

## 已修复问题及证据

| 边界 | 修复前 | 修复后 / 文件证据 |
|---|---|---|
| helper 超时 | `ws_ipc_read_timeout`、`ws_close_timeout` 被归类为非法 IPC | `gateway_native_ws.go:277` 识别全部 5 种超时，保留 `net.Error.Timeout()`；与 `native-transport/src/websocket.rs:111`、`:163` 对接 |
| accepted / phase | `accepted=true` 可以带 `phase=handshake`；反向也允许 | `gateway_native_ws.go:287` 强制阶段匹配，不把建立前失败当作已接受消息错误 |
| IPC 读错误 | 长度头或 payload 的所有错误都改写为 `io.ErrUnexpectedEOF`，超时原因消失 | `gateway_native_ws.go:229` 仅将 EOF 转为截断，保留 deadline / socket error 身份；握手及读泵共同使用 |
| 在途写错误 | read pump 先收到 helper 超时并关闭 IPC 时，阻塞写返回本地 `io.ErrClosedPipe`，覆盖真实原因 | `gateway_native_ws.go:544` 在写失败后返回首次 failure；关闭后新的 Write 不增加 sequence、不重发 |
| 握手 header tuple | `[2]string` JSON 解码静默补齐短数组、截断长数组 | `gateway_native_ws.go:164` 使用 `[][]string`，逐项严格要求 2 个元素 |
| 握手 header bytes | 仅拒绝 CR/LF/NUL，其他 C0/DEL 可进入响应对象 | `gateway_native_ws.go:184` 复用专项 04 的 `gatewayNativeHeaderValue`；允许合法 TAB 和 obs-text，不损失 base64 解码后的原始字节 |
| 握手 body 语义 | `body_scope=none` 可同时携带 body / truncated 信息 | `gateway_native_ws.go:178` 拒绝矛盾元数据；保留拒绝响应的 opaque tail、重复 headers、原始状态，ContentLength 仍为未知 -1 |
| Unicode | `encoding/json` 把无效 UTF-8、未配对 UTF-16 escape 替换为 U+FFFD，随后 utf8.Valid 检查无法发现 | `gateway_native_ws.go:239` 先校验 UTF-8 与 surrogate 配对再解码；合法 U+FFFD、emoji surrogate pair、转义的字面 `\\ud800` 不受影响 |
| Close reason | 缺失 / null reason 被当作合法空字符串 | `gateway_native_ws.go:381` 要求 reason 字段有效，按 UTF-8 字节数限制 123 bytes |
| Close code | Go 接受 1014，固定 CLI fork 判为 Bad | `gateway_native_ws.go:500` 与固定 fork 保持一致，1014 拒绝；1000/1001/1002/1003/1007..1013、3000..4999 保持支持 |
| read limit | 合法 IPC 长度超过应用 read limit，或 payload 途中下调 limit，被报告为协议损坏 | `gateway_native_ws.go:209`、`:353` 分离硬 IPC 长度错误与 `errNativeWSLimit`；仍在分配 payload 前检查边界 |
| outbound limit | 调用方超长消息被报告为协议损坏 | `gateway_native_ws.go:544` 返回 `errNativeWSLimit`，不改变 sequence、ACK 或连接状态 |
| 已取消 Read | canceled context 与已排队消息同时就绪时，select 可能交付消息 | `gateway_native_ws.go:511` 在等待 reader token 前后先检查 ctx；明确已取消请求不消费队列 |
| 配置与请求验证 | query BaseURL 会把 `/responses` 拼入 query；非法 header 直到启动 helper 后才失败 | `gateway_native_ws.go:69` 拒绝 query/ForceQuery、空 hostname，并在 acquire 前复用 `gatewayNativeRequestHeaders` |
| IPC Dial 取消 | DialContext 因取消失败时被统一改写 unavailable | `gateway_native_ws.go:120` 保留 caller context error |
| 热路径开销 | 每条正常消息即使不阻塞也创建两个 60 秒 timer | `gateway_native_ws.go:413`、`:447` 仅在 channel 或 byte budget 实际阻塞时创建 timer，保持原有背压上限 |

公用 header 编码保留排序和重复值顺序，限制 128 fields 与元数据预算。该实现来自专项 04，WS 不再保留另一套容易漂移的编码逻辑。

## CLI 一致性与明确差异

| 项目 | CLI 0.155.0 / 固定 fork | 当前网关 | 结论 |
|---|---|---|---|
| TLS connector 生命周期 | `responses_websocket.rs:519` 每个新连接创建 connector | Rust `websocket.rs:212` 的 `cli_request` 分支相同；Go 只发送 IPC 命令 | 代码路径一致，真实 TLS 验证归主任务 |
| WS framing / masking / deflate | CLI `responses_websocket.rs:559` 使用 fork 默认配置加默认 permessage-deflate | Rust helper 使用同一依赖；Go 没有 RFC6455 framing 或压缩状态 | 传输责任边界一致 |
| 单 frame 上限 | 固定 fork 默认 16 MiB | 专项 03 保留默认 16 MiB | 一致；32 MiB message 可由多个 frame 承载 |
| 单 message 上限 | 固定 fork 默认 64 MiB | helper 和 Go IPC 采用 32 MiB gateway hard limit | 明确保留的网关资源限制，不能声称完全同值 |
| 上游消息队列 | CLI `responses_websocket.rs:66` 使用 unbounded channel | Go 16 条队列 + 32 MiB queued payload 预算，等待最多 60 秒 | 明确的有界资源差异，避免 gateway 多会话无界增长 |
| 发送确认 | CLI `WsStream::request` 等待 sink.send 结果 | Go 按 sequence 等待 helper WRITE_ACK | 语义相同：ACK 只证明传输提交，不证明模型处理成功 |
| 应用 JSON | CLI 上层解析 Responses events | Go native adapter 将 text/binary payload 字节原样交给 gateway 上层 | 协议适配层不重写消息、不重连、不 replay |
| Peer Close | CLI pump 把 close 交给消费者，结束 pump | Go 保留关闭前已排队完整消息，之后返回 CloseError | 数据顺序一致；Go `CloseNow` 明确放弃未消费数据 |
| 本地释放 | CLI `responses_websocket.rs:151` abort pump task | Go CloseNow 关闭 IPC、停止 pump、清队列、release once | 同一资源生命周期意图，具体 IPC 实现不同 |
| 拒绝握手 body | CLI HTTP 错误可能只有握手读缓冲区 tail | Go 最多保留 16 KiB opaque tail，`body_complete=null`，长度未知 | 不伪造完整响应 body；截断 tail 是网关 IPC 限制 |
| ReadLimit(-1) | coder/websocket 的 -1 是无限 | native WS -1 恢复 gateway 32 MiB hard cap | 网关边界差异，生产上层统一设置 maxBody |

固定依赖的关闭码核验使用 [tungstenite coding.rs 固定 revision](https://github.com/openai-oss-forks/tungstenite-rs/blob/4fffad30fe373adbdcffab9545e9e9bf4f2fc19f/src/protocol/frame/coding.rs)：`is_allowed` 排除 Bad/Reserved/Status/Abnormal/Tls，`From<u16>` 没有 1014 映射，2000..2999 也属于 Reserved。不能用新版本 IANA 注册状态推断此 fork 的行为。

默认上限核验使用 [tungstenite protocol/mod.rs 固定 revision](https://github.com/openai-oss-forks/tungstenite-rs/blob/4fffad30fe373adbdcffab9545e9e9bf4f2fc19f/src/protocol/mod.rs)：`WebSocketConfig::default` 中 message 为 64 MiB、frame 为 16 MiB。固定 rev 与 `native-transport/Cargo.toml` 一致。

## 测试及复跑

新增 `gateway_native_ws_audit_test.go` 的 11 个顶层测试覆盖：握手字段与 tail、typed timeout 与 phase、IPC 类型/长度、原始读错误身份、Close Unicode 与固定 fork code、已取消排队 Read、读取中下调 limit、写前验证、helper 超时中断在途 Write、pool 前配置拒绝、并发 reader/writer cancel。

现有测试继续覆盖：unsolicited 消息在 ACK 前到达、错误/缺失 ACK、不重放、并发写 sequence、close 前消息保留、32 MiB byte budget、20 MiB 大消息顺序背压、空消息、Explicit Close 与并发读、取消时释放一次。

```powershell
go test . -run '^TestGatewayNativeWS(Audit|IPC|Budget)' -count=10 -timeout=120s
```

结果：通过，约 2.6 秒测试执行时间。单轮 JSON 结果保存在 `.local/a05-native-ws-tests.jsonl`，29 个顶层测试、92 个测试通过事件（包含 subtests），0 失败、0 跳过。包级 pass 另计。

```powershell
go test . -run '^$' -bench '^BenchmarkGatewayNativeWSAuditMessageBudget$' -benchmem -benchtime=1s
```

结果：Windows amd64 / Ryzen 7 9700X，37.92 ns/op、1292.09 MB/s、0 B/op、0 allocs/op。此微基准只测 payload 预算及队列交接，不代表端到端网络吞吐或模型响应速度。

本机 `CGO_ENABLED=0`，本专项未把本地非 race 结果标作 race 验证。主任务应使用其 Linux harness 对同一正则执行 `go test -race`，并用真实 Rust helper 继续检查 TLS、压缩、分片及错误传递。
