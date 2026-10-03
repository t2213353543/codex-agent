# 03 · Native WebSocket / TLS 审计（2026-09-24）

范围：`native-transport/src/websocket.rs` 及新增 `native-transport/src/websocket_tests.rs`。本专项未修改 `main.rs`、Go、Cargo 或 vendor。基准为只读 `.local/codex-source-0.155.0/codex-rs`，以及 Cargo 固定的 OpenAI Tungstenite fork commit `4fffad30fe373adbdcffab9545e9e9bf4f2fc19f`。结论区分源码一致性、helper 回归与实际 TLS 抓包，不以其中一项代替另一项。

## 已修复

| 边界 | 原问题 | 修复与位置 |
| --- | --- | --- |
| CLI WS TLS 配置生命周期 | helper `State.websocket` 长期复用 `Arc<ClientConfig>`，因此可跨新连接复用 rustls session cache；CLI 每次 `connect_websocket` 新建 connector | `websocket.rs:212`：`CliRequest` 每次握手创建 connector，`Pooled` 显式保留共享；同一 WS 上的多轮请求仍复用原 socket |
| application 限额与控制帧 | `max_frame_size = max_message_bytes` 会让小消息额度误拒绝合法 125-byte Ping 和正常 Close | `websocket.rs:201`：保留 CLI fork 默认 frame 限制，只对完整 application message 设置额度 |
| 压缩后的 wire 大小 | 很小或难压缩的内容，其压缩后的 frame 可能大于解压后的 message，旧配置错误拒绝 | 同上；由固定 fork 在解压期间及最终 message 层检查 `max_message_size` |
| Close IPC 元数据 | `SEND_CLOSE` 同时被 data-message 的 `limit + 9` 限制；例如 `limit=1` 连正常 close JSON 都不接受 | `websocket.rs:74`：SEND 使用 message envelope 限制，SEND_CLOSE 使用独立 `MAX_HEAD`，仍执行 wire code / 123 UTF-8 bytes 检查 |
| 半个 IPC 命令头 | 读入 kind 后无限等候 u32 length | `websocket.rs:64`：命令一旦开始，剩余 header + payload 共用一个 `write_timeout_ms` 期限；报 `ws_ipc_read_timeout` |
| 半个 IPC 命令内容 | 收到合法长度后无限等候 payload，连接与 busy 状态永久占用 | 同上；等待下一条命令的空闲时间不套用该期限 |
| 不回答主动 Close 的上游 | `SEND_CLOSE` 写入后，旧 pump 无任何关闭等待期限 | `websocket.rs:145`：close 成功发出后最多再等一个 write timeout，报 `ws_close_timeout`；Ping 不重置关闭期限 |
| 非 http(s) 输入 URL | 旧函数将所有非 https scheme 默认为 ws，依赖外层始终提前验证 | transaction 显式只允许 http→ws / https→wss，其他 scheme 返回 `ws_request_invalid` |

新增错误代码只有 `ws_ipc_read_timeout`、`ws_close_timeout`。Go 的 `nativeWSRemoteError` / `gatewayNativeError` 必须将两者纳入超时映射；已交给主任务的 Go 专项统一处理。没有修改 IPC 字节布局、序号、ACK 含义或重放策略。

## 源码一致性与明确差异

| 主题 | 官方基准 | helper 结论 |
| --- | --- | --- |
| TLS backend | `websocket-client/src/lib.rs:61-82`：`new` 使用 `ExplicitCodexTls`，调用 `build_rustls_client_config_with_custom_ca` | 相同，未降级到 `TungsteniteDefault` 或 native TLS |
| crypto provider | `utils/rustls-provider/src/lib.rs`：初始化 aws-lc-rs，并要求 ECDSA P-521 验签支持 | vendor 复用此实现；本专项没有更改 provider 顺序、cipher suites 或 signature schemes |
| CA 与校验 | `http-client/src/custom_ca.rs:239-286`：native roots + custom bundle，`CODEX_CA_CERTIFICATE` 优先于 `SSL_CERT_FILE`，保留正常服务器证书与名称校验 | vendor `custom_ca.rs` 与官方基准 SHA-256 相同：`C796E150BE2D0F93F4AC6620EAC4324CFC5168F3EAE1D80146A4690ACD6FC776` |
| 目标/HTTPS proxy TLS | `websocket-client/src/dialer.rs:101-133`：HTTPS proxy 先做 proxy TLS，再 CONNECT，再目标 TLS；复用显式配置 | vendor `dialer.rs` 与官方基准 SHA-256 相同：`85F90895A52963C41776108A351AAC3FD52B5705DF8E14B8FE5877CC4BEE162B` |
| DNS / 连接竞争 / Nagle | 同文件 happy-eyeballs 250ms；connector 默认 `TcpNodelay::Default` | 相同，未为了优化擅自启用与 CLI 不同的 socket 配置 |
| 路由策略 | 官方 `WebSocketConnector::connect` 使用 factory/env/system 策略解析 | 明确差异：gateway 调用新增的 `connect_with_explicit_route`，只使用不可变指定 proxy，不允许 NO_PROXY/env 绕过。对多账号网关属既有隔离策略 |
| 握手构造 | `codex-api/src/endpoint/responses_websocket.rs:513-521`：`into_client_request` + headers + 新 connector | 相同生成随机 key。helper additionally 禁止调用方覆盖 Key/Version/Accept/Extensions，Host/Connection/Upgrade 在外层禁止；end-to-end 重复 headers 保留 |
| permessage-deflate | `responses_websocket.rs:559-565`：`DeflateConfig::default()`，其余 `WebSocketConfig::default()` | 相同，真实 loopback 测试检查 offer 为 `permessage-deflate; client_max_window_bits` |
| frame / message 上限 | 固定 fork `src/protocol/mod.rs:108-109`：message 64 MiB、frame 16 MiB | **显式差异**：gateway 的 message 上限仍为 32 MiB，可调低；frame 恢复官方 16 MiB。旧 helper 允许的 >16 MiB 单帧会被拒绝；32 MiB 聚合分片/可压缩 message 不因此被整体改为16 MiB。不能宣称可直接接收32 MiB单帧 |
| Ping/Pong | `responses_websocket.rs:76-112` pump 自动回 Pong，并隐藏 Ping/Pong | 相同；对上层输出仅 Text/Binary/Close，控制帧不会成为 MESSAGE |
| 应用 idle timeout | `responses_websocket.rs:707`：每轮请求等待 application message；Ping/Pong 已在 pump 中过滤 | helper 不解析 turn completion，因此**不对整个 WS 生命周期追加 idle timeout**。该行为由 Go response-turn 层负责；不能将 `State.idle_timeout` 机械套入 socket.next() |
| 发送超时 | `responses_websocket.rs:897`：request send 使用 provider idle timeout | helper 使用调用方 `write_timeout_ms`（最大60秒）及 IPC write deadline，属于网关传输保护差异 |
| 缓冲与背压 | CLI pump command 队列32、incoming 无界 channel | helper 每方向队列1、有界 IPC 输出、一个 outstanding write ACK；内存受限但慢消费者可延迟 Ping/ACK，直到恢复或写超时。保留既有差异 |
| Close / EOF | CLI 将 Close 与应用流提前结束交给 response 层 | helper 将 peer Close 写成 CLOSE+END，突然 EOF 为 `ws_unexpected_eof`；主动 Close 现在有期限，不把超时当成功 |
| 1014 Close code | 固定 fork `src/protocol/frame/coding.rs:230-256` 未单列1014，落入 `Bad`；`is_allowed`拒绝 | Rust相同。Go `nativeWSAllowedCloseCode`目前允许1014，是额外发现的跨层差异，已交Go专项核对；本专项未擅自放宽fork行为 |
| 非101握手 | 官方保留 `WsError::Http` 的 status/headers/body tail | helper保留重复headers、最多16 KiB tail、`body_complete:null`，不谎报完整HTTP body；无自动重连/重放 |
| ACK | CLI `oneshot` 表示 `inner.send` 完成 | helper WRITE_ACK同样只表示 sink.send完成，不能推断服务器已接受模型请求；ACK发布前释放busy避免跨核竞态 |

固定 fork frame 上限已通过直接获取原始源码确认：[OpenAI Tungstenite protocol/mod.rs](https://github.com/openai-oss-forks/tungstenite-rs/blob/4fffad30fe373adbdcffab9545e9e9bf4f2fc19f/src/protocol/mod.rs#L104)。同文件 `:849-870` 先解压、再执行 message 限额，故 frame limit 不应直接等于 application limit。

## 回归清单与执行状态

新增20个测试，原有4个真实socket测试保留：

1. frame 配置维持CLI默认，message额度独立。
2. 1-byte压缩消息、125-byte Ping/Pong、123-byte close reason、重复请求headers、path/query。
3. 三轮 SEND/ACK/MESSAGE 在同一个 socket 上完成，无重连。
4. 1-byte application额度下正常SEND_CLOSE。
5. 无效close codes、UTF-8字节长度超限、缺code但有reason、未知字段。
6. 4001 application close code及无code空Close。
7. 非UTF8 Text、非法message kind、起始sequence 0/2。
8. ACK后重复sequence。
9. 未ACK时第二条命令，即使第一条已dequeue仍拒绝。
10. 部分IPC header/payload截止。
11. 两轮之间空闲不误触发命令截止。
12. 超长SEND/CLOSE、过短SEND、非空CANCEL在读取payload前拒绝。
13. 握手前CANCEL允许、SEND拒绝。
14. ACK发布前busy释放且不打断packet framing。
15. IPC输出背压超时保持in_frame，防止追加ERROR损坏半帧。
16. 上游send阻塞超时且不伪造ACK。
17. 主动close无回应超时。
18. IPC输出堵塞时CANCEL仍能中断整个transaction。
19. 无效101的Sec-WebSocket-Accept不能发布accepted握手。
20. 调用方覆盖4种transport-owned握手头全部拒绝。

已完成源码复核、vendor哈希比较与固定fork默认值验证。主任务首轮隔离Linux Rust 1.95 release编译成功，35项helper测试33通过、2失败；失败均为本专项新增服务端夹具使用 `accept_async` 未启用deflate，固定fork返回 `Protocol(InvalidHeader("sec-websocket-extensions"))`。现已统一为 `accept_async_with_config(..., Some(websocket_config(MAX_BODY)))`，准备重跑；尚未将修正后结果记作通过。主任务最终执行后应补实际结果和日志路径。复跑命令（项目根目录）：

```sh
cargo test --locked --manifest-path native-transport/Cargo.toml websocket::
```

仍需区分的验证：实际 TLS ClientHello JA4/extension/order 与新连接session resumption抓包、真实HTTPS/SOCKS代理、企业CA/名称不匹配失败场景、生产真实上游均不由这些loopback测试证明。本专项没有生产SSH、真实账号请求或发布。

## 主任务实测回填

2026-09-24 隔离 Linux x86_64 musl / Rust 1.95.0 release 构建通过；35 项 Rust 单元/真实 socket 测试全部通过（含 HTTP 8、WS 24、压缩 3），26 项既有 loopback 与 8 项新增 HTTP/TLS/HTTP2 边界测试全部通过，零跳过。原先两项 WS fixture 压缩配置错误已经修正并重跑。证据：`.local/gateway-audit-20260924/native-rust-tests.log`、`native-loopback-result.json`、`native-http-boundaries.log`、`native-source-manifest.json`；这是本次源码快照的实测，未据此宣称全部平台或全部 TLS 字段相同。
