# Codex 原生 HTTP 与 WebSocket 传输

独立 helper，复用官方 `rust-v0.155.0` 的 `codex-http-client`、`codex-utils-rustls-provider` 与 `codex-websocket-client`。30 个 vendored Rust 文件与该 release 源码逐字节相同；WS `lib.rs` 另有窄 explicit-route adapter，原始/修改后 SHA256 及补丁见 `UPSTREAM.json`、`WEBSOCKET-ADAPTER.patch`。许可证/上游 notice 原样保留。生产启用由 Go 独立开关控制，不执行 CLI 或工具。

默认 HTTP 使用 reqwest 的平台 native TLS；`CODEX_CA_CERTIFICATE` / `SSL_CERT_FILE` 按官方 helper 切换到 rustls。Linux native 是 OpenSSL，Windows native 是 Schannel，不能因使用相同 crate 就声称跨平台指纹一致。WS 始终使用官方 ExplicitCodexTls 与 TcpNodelay::Default。默认不添加 UA、originator、version、Accept-Encoding 或账户 cookie；现有网关决定应用头。HTTP 启用官方仅允许 Cloudflare 与 `__oailb` 的基础设施 cookie jar，不持久化账户/session/auth cookie，不扩大官方 host/path 规则。

## 构建

需要 Rust/Cargo 1.95.0、C/C++ 编译器、CMake、Perl、make、pkg-config。Linux musl 还需对应 musl linker。两个 musl target 显式启用官方 CLI 原由 codex-core 引入的 `openssl-sys/vendored`；不引入 codex-core、V8、TUI 或 sandbox。

```sh
export CARGO_BUILD_JOBS=2
export AWS_LC_SYS_NO_JITTER_ENTROPY=1
cargo build --locked
python3 tools/check_lock.py
cargo tree --locked -e features > resolved-features.txt
python3 tests/loopback.py --binary target/debug/codex-native-transport --report test-results.json
```

当前 Cargo.lock 已从官方完整 workspace seed 解析裁剪为 265 个包，并完成真实 `cargo build --locked`。重新提取源码时，先在隔离环境执行 `cargo fetch` 裁剪 seed，再运行 `tools/check_lock.py`，在编译依赖构建脚本之前拒绝漂移。校验要求 registry/git 包的 version、source、checksum 已存在于官方锁，并检查关键传输版本、TLS feature union、两 WS fork revision、原样源码及单独记录的 adapter hash。`tools/vendor_source.py` 会重置锁到官方 seed，正常构建不要运行它。

直接传输 pins：reqwest 0.12.28、native-tls 0.2.14、rustls 0.23.36、hyper 1.8.1、h2 0.4.16、openssl-sys 0.9.111、openssl-src 300.6.1+3.6.3。完整允许包清单 `LOCKED-PACKAGES.json` 来自官方锁，最终实际集合以本目录 Cargo.lock 为准。

HTTP crate 的 features 为 reqwest 默认集合 + cookies/json/rustls-tls-native-roots/stream；rustls 为 aws_lc_rs/std/prefer-post-quantum，provider 使用官方初始化。同平台真实对照曾发现：不补 prefer-post-quantum 时 helper 的组顺序为 `[29,23,24,4588]`、ClientHello 235 字节，官方为 `[4588,29,23,24]`、1455 字节；这与 rustls 0.23.36 的 provider 条件编译源码一致，因此显式保留该特性，而不修改 provider 实现。失败证据保留在 `.local/native-transport-build/compare-20260918T023759-4accc8/`。helper 的 tokio 额外启用网络、io-util、多线程 runtime；futures 启用 std/async-await。没有添加 reqwest socks、gzip、native-tls-alpn。WS 引入官方 tokio-tungstenite revision `0e5b2d73aa18dd9f0a50ee9ff199d5aef7594186` 和 tungstenite-rs revision `4fffad30fe373adbdcffab9545e9e9bf4f2fc19f`，保留 proxy/native-roots/deflate features。最终构建保存 `cargo tree -e features`；不能仅因依赖相同宣称全部 wire 指纹已经验证。

## IPC v1

helper 不接受命令行秘密或请求文件。启动后从 stdin 读取 4 字节大端 JSON 长度，再读取配置 JSON（最多 64 KiB）。父进程保持 stdin 开启；其 EOF 或随后任意 1 字节会停止监听并取消所有活动请求。stdout 仅输出一次不带凭据的 ready JSON 行，包含 `protocol`、动态 `listen` 地址、版本与上限；stderr 只有固定错误代码。

配置示例结构（值均为示例，实际 ipc_token 使用随机 32 字节十六进制）：

```json
{"protocol":1,"listen":"127.0.0.1:0","upstream":"https://example.invalid","ipc_token":"<64 hex characters>","proxy":null,"headers_timeout_ms":90000,"idle_timeout_ms":300000}
```

`listen` 必须是 loopback IP。`upstream` 是固定的 HTTP(S) origin，不允许 userinfo、path、query 或 fragment。proxy 可为显式 HTTP/HTTPS 代理（含需要的代理认证）；null 始终直连，不读取系统或环境代理。一个 helper 固定一个出口配置，配置变化应创建新实例、排空旧实例，不并发修改进程环境。代理不可用不会降级直连；WS 官方 adapter 还支持 SOCKS5/SOCKS5h，HTTP 直接指定 SOCKS 会拒绝该 operation，Go 可提供固定目标的受控 CONNECT 桥。

Linux 服务接入使用 `unix_socket` 代替 `listen`，两者必须择一。socket 必须位于当前 UID 所有、模式恰好 `0700` 的真实绝对目录内，拒绝路径祖先 symlink；socket 创建后设为 `0600`，连接还核验 peer UID。已有文件、socket 或 symlink 均拒绝，不删除旧路径。正常父进程 EOF 停止后，仅删除设备号和 inode 与本进程创建值相同的 socket。非 Unix 平台不支持 `unix_socket`，返回固定错误；TCP 仍保留为显式兼容/测试入口，不能被 Go Linux 接入静默选作降级。

ready 额外包含 `transport: "unix"` 和 `unix_socket`，或 `transport: "tcp"` 和 `listen`；UDS 路径只经父进程 stdout 控制管道返回，不写日志。每条 IPC 连接承载一个请求。先发 4 字节大端 JSON 长度与请求头（最多 64 KiB），再发恰好 `body_length` 字节原始 body。保留写方向开启，直到 END；提前 half-close 被视为取消。

配置 `http_client_lifecycle` 默认为 `cli_request`，ready 必须回报相同字段。该模式依照 CLI 0.155.0 `ModelClient::build_api_transport`，每个 HTTP 调用创建新的官方客户端，基础设施 cookie jar 仍按官方进程级共享；不会跨调用复用连接或 TLS session cache。每次新建 WS 连接也创建新的官方 connector/TLS 配置；已建立 socket 上的多轮请求继续复用该连接。显式 `pooled` 才允许跨 HTTP 请求及跨 WS 连接复用对应客户端/TLS 配置，属于性能模式，不冒称 CLI 原始生命周期。此分离来自实际反证：旧 pooled HTTP helper 在 custom-CA 四次强制重连中恢复了三次 TLS session，而 CLI 四次均为完整握手。

```json
{"protocol":1,"ipc_token":"<same secret>","method":"POST","path":"/responses","headers":[["content-type","YXBwbGljYXRpb24vanNvbg=="]],"body_length":123,"total_timeout_ms":1200000}
```

headers 是有序 pair 数组，值为标准 Base64 原始字节，允许重复。body 由调用者准备，默认 none 模式不解析 JSON、不注入 tools、不聚合 SSE、不压缩或解压。`Content-Encoding: zstd` 的已准备 bytes 原样发出。允许不提供 Content-Length，由 reqwest 从真实 body 长度构造；若提供则必须唯一且匹配。Host 及 hop-by-hop 请求头必须由调用者剔除，否则拒绝；不转发 CONNECT/TRACE。

可选 `request_compression: "zstd"` 仅限 HTTP operation，默认 `none` 保持上述不解析/不转码行为；ready.capabilities 包含 `request_zstd_v1`。zstd 模式接受 Go 已准备的最多 32 MiB JSON，用 RawValue 保留内部键序、重复键、数字和转义，再复用官方 EncodedJsonBody 与 Request.prepare_body_for_send 的 zstd level 3。外围空白导致 bytes 不一致时明确拒绝，不静默重排。已有 Content-Encoding 与压缩意图冲突时拒绝；Content-Length 先校验原始 decoded 长度，压缩后移除，由 reqwest 按实际 wire bytes 生成，压后上限 33 MiB。WS 拒绝 HTTP 压缩意图，不混淆其 permessage-deflate。

CLI 0.155 压缩条件是稳定默认开启的 enable_request_compression、uses_codex_backend auth 和 is_openai provider 三者同时成立；API key/自定义 provider 不因此被强制压缩。网关通过已观察到的入站 zstd 意图选择对应 Responses 路径，OAuth、compact 和没有该意图的请求不应猜测启用。重复压缩相同 prepared bytes 输出确定性相同，但 helper 没有声称复用了 CLI 单个 PreparedRequestBody 的重试 CPU 工作。

`path` 必须是已准备的 ASCII origin-form target，可包含原样 query；拒绝空格、控制字符、反斜线、fragment、绝对 URL，以及会被 URL 库规范化的 dot segment。已编码的 path/query 和重复 query 参数保持原值。显式 proxy 也必须是 origin，不允许路径、query 或 fragment。

helper 返回帧格式为 `type:u8 | length:u32be | payload`：

| type | payload |
| --- | --- |
| 1 HEAD | JSON `status`、HTTP `version`、全部响应 `headers` pair（值 Base64） |
| 2 DATA | 最多 64 KiB 原始响应 bytes |
| 3 END | 长度 0，正常结束 |
| 4 ERROR | 固定 ASCII 错误代码，不含 URL、body、凭据或库错误串 |

所有 HTTP status 都先返回 HEAD，包括 401/429/5xx；不转换为 helper 错误、不自动重试、不跟随重定向。网络及协议不支持错误才返回 ERROR。101 的 status 保留，但随后明确返回 `upgrade_unsupported`，不伪造正常 END。响应重复值保留，但 HeaderMap/HTTP 编码会规范化名称和顺序，不保证整个 HTTP wire 字节逐字节一致。HTTP operation 不提供 trailers 或 101 raw upgrade；WS 必须使用下述独立 message operation。

完成上传后，客户端可在反方向发送任意 1 字节（约定 `0x03`）取消，或关闭连接。helper 同时等待取消和整个交换，即使正在往慢客户端写 DATA 也可取消；它只丢弃此请求的 future/body。取消、IPC 断线可能截断一个帧，调用者必须丢弃该连接，不能继续解析或重放 POST。总期限包含下游传输，若恰好截断输出也按不完整响应处理；此时只关闭连接，不把 ERROR 帧拼入未完成的 payload。只有正常收到 END 才表示完整成功。

每请求单独连接避免共享 stdout 队头阻塞。响应逐 chunk 读取后等待 IPC 写出，慢消费者通过 IPC 背压阻止进一步应用读取；上游 idle 计时只包住下一次 upstream read，不包住 DATA write。仍存在 reqwest/hyper/内核缓冲，不把 64 KiB 帧上限冒充总内存上限。并发由 Go gateway 的账号 concurrency 准入控制；helper 的 ready 必须显式回报 `max_active: 0`，表示没有额外的 route 活跃请求上限。Go 启动时拒绝旧版 32 槽 helper，两个二进制必须一起更新。每个请求 body 仍最多 32 MiB；上传先缓存在 helper 内存，运行内存随实际并发和请求体大小增长。

helper 接受每个已获账号准入的 IPC 连接，HTTP 与 WebSocket 均不再叠加 32 槽 semaphore；连接关闭会取消对应任务，父 stdin EOF 会停止监听并回收全部任务。Go 按需创建活跃 route，最多缓存 16 个空闲 helper，额外空闲实例释放后立即回收，其余仍有空闲超时。SOCKS 桥也不另设连接并发上限。`tests/refill_probe.py` 与主回归固定覆盖 96 条同时保持活动的流，以及 10 次无等待 cancel/refill；Go 集成另验证 96 HTTP 同时到达合成上游、第 97 条由账号设置拒绝，以及 96 WebSocket 的握手、消息和释放。

SSE 没有默认总期限；`headers_timeout_ms` 限制连接及收到响应头，`idle_timeout_ms` 限制上游读取等待，两者显式为 `0` 时关闭 helper 对应额外期限，由 Go 请求上下文或运行时策略负责取消。Go 另按完整 SSE 事件计空闲预算，半帧或注释滴流不续期。`total_timeout_ms`（例如 compact 的 1200000）从鉴权后的请求头处理完开始，覆盖 IPC 正文上传和上游交换；Go 发送 context 的剩余预算，上传后不重置。OAuth 必须由 Go 保留独立生命周期、刷新锁、CAS 与 RT 持久化，helper 不管理账号、不读取 auth.json，也不自行 401 恢复。

## 合成验证与边界

2026-09-18 已在本地隔离 Linux musl 容器实际完成构建及 16 项 loopback 回归，0 failures / 0 errors，10.758 秒；来源 hash 与构建证据在 `.local/native-local-build/20260918T024358Z-f23140f1/`。二进制 SHA256 为 `95dc45ab20b063a2b5367f12efbd0f7184b572cf783929c42f03ee3b14b1868d`。较早一轮的 TLS 测试证书误用 CA 根作为服务端叶证书，实际 15 项中有 1 项失败；改为正常 CA 链和 CA:false 叶证书并加入响应头超时项后，16 项全部通过，旧失败证据保留。

同平台官方 Linux CLI 0.154.0 对照也已实际通过，报告 `.local/native-transport-build/compare-20260918T024519-b551e0/`：默认 native 路径只观测首个 ClientHello，已解析字段相符；custom CA 路径同时观测到相符的 ClientHello、TLS 与 HTTP 协商结果。比较忽略随机扩展顺序，不采集密钥；目标是 loopback IP，未覆盖生产 SNI、TLS 会话恢复、HTTP/2 SETTINGS、WS 或全设备指纹。首次对照发现并修复的 prefer-post-quantum 差异有独立失败记录，不能将最终通过描述为从未存在差异。

`tests/loopback.py` 只使用 Python 标准库、loopback 假上游与合成凭据。TLS 场景还需 openssl CLI 临时生成证书，不写系统 trust store。自定义 CA 场景明确测试 rustls；Linux 默认 native 场景只设置 `SSL_CERT_DIR` 的临时 hash 根目录，避免 `SSL_CERT_FILE` 触发 rustls。未信任证书必须失败。

覆盖原始非 UTF-8 body/双向头、重复请求/响应头、三轮连接复用、gzip 不解压、所有错误 status/断连/重定向不重试、101 明确失败、SSE 首块及时到达、活动流超过 idle 时长、真正 idle/总期限、响应头前及流中取消/EOF、阻塞写取消/总期限、父进程退出同时取消多个请求、慢消费者不阻其他请求、鉴权/路径/上限拒绝、错误配置有限诊断、显式代理及其失败不降级直连、环境代理忽略和真实 TLS 验证。测试报告只输出断言与计数，不输出请求内容或凭据。

### 官方源码复核

`http-client/src/client_builder.rs` 默认使用 `TransportDefault`，只有显式 `Rustls` 选项才调用 `use_rustls_tls`；`http-client/src/custom_ca.rs` 的 `build_reqwest_client_with_env` 在选中 CA bundle 时强制 rustls，并保留调用方其余 builder 设置。`CODEX_CA_CERTIFICATE` 优先于 `SSL_CERT_FILE`，空值等同未设置，CA 追加到系统根而非替换。这里直接复用上述 CA 实现，未放宽证书校验。

并非所有 CLI 行为都由此 helper 复制：`login/src/auth/default_client.rs` 的默认客户端会添加 UA/originator，legacy 构造还可能在无效自定义 CA 时退回系统根。本 helper 不接管应用头，无效 CA 明确启动失败；也不启用 route-aware pool 的可选 TLS backend fallback，避免在 Go 重试策略之外再做重放。这些差异不应被解释为已经完整复现 CLI 的所有状态与线路行为。

## WebSocket Message IPC

ready.capabilities 包含 `websocket_message_v1`。请求 envelope 新增 `operation: "websocket_message_v1"`，必须 `method: "GET"`、`body_length: 0`；默认 `max_message_bytes: 33554432`、`write_timeout_ms: 60000`，只能调低。握手使用配置的 headers timeout；目标 http(s) origin 对应 ws(s)，原 path/query 不变。Rust 创建随机握手 key、协商默认 permessage-deflate、执行帧/mask/fragment/compression 和 ping/pong，不接收调用方 Sec-WebSocket-Key/Version/Accept/Extensions。

WS frame 上限保持固定版本 CLI 的默认 16 MiB，独立于网关 32 MiB 的解码 message 上限；超过16 MiB的消息需要合法分片或压缩。控制帧和 Close metadata 不受调用方的小 message limit 限制。IPC 命令开始后的半帧读取和主动 Close 等待均有期限，分别返回 `ws_ipc_read_timeout`、`ws_close_timeout`；两轮之间空闲连接不使用这个命令期限，活动响应的 idle 由 Go turn 层处理。

所有 WS 帧沿用 `kind:u8 | length:u32be | payload`，基础 codes 见 `ws-plan/protocol.json`：

- 65 HANDSHAKE：JSON status、Base64 重复 headers、body_scope=handshake_tail、body_complete=null、body_base64、tail_truncated。非 101 尾部最多 16 KiB，并非完整 HTTP entity；之后 69 END。代理/网络失败为 68 ERROR，不伪装 target HTTP 401。
- 66 MESSAGE：u64BE 单调序号、u8 kind（1 text/2 binary）、完整解码 message bytes。message 上限为配置值加 9 字节 envelope，metadata 上限 64 KiB。
- 67 CLOSE：JSON 可空 code、reason；69 END 表示协议正常结束。68 ERROR 是有限 code 与 phase/accepted，不包含库错误原文或目标秘密。
- 82 SEND：与 MESSAGE 同结构，发送序号从 1 严格递增，70 WRITE_ACK 返回同 u64BE，仅表示 Rust sink.send 完成，不表示模型接受请求。调用方等待 ACK 后再发下一 SEND。
- 83 SEND_CLOSE：JSON 可空 code、reason；84 CANCEL：空 payload。IPC EOF 同样取消，无自动重连或重放。

每方向 channel 容量 1，另有正在读取/处理/输出的消息和临时转换副本。满额消息组合可超过单个 32 MiB，总内存随账号准入后的连接数量增长。输入取消任务独立于 WebSocket pump 与 IPC 输出，即使背压也可取消。输出消息不可被 ACK 插进半帧，慢消费者会阻塞控制帧直到恢复读取或 60 秒写期限失败；这是有界背压，不使用无界消息缓存规避它。Go 应从握手成功起持续读取 IPC 并分别处理 ACK 与应用消息。

Rust 同 fork loopback 测试验证真实 socket 语义。2026-09-24 另以固定版本官方 CLI 0.155.0 和本轮 helper 做了独立 HTTP/WSS 对照：HTTP ClientHello、custom-CA HTTP/2 SETTINGS/窗口与恢复序列一致；WSS 在默认根加载/自定义 CA 下完成12条连接、24条消息，并验证未知CA及名称错误拒绝。比较归一化后的握手字段，不比较随机密钥、原始头顺序或跨平台TCP指纹。详细构建摘要、证据范围和剩余差异见[本轮审计](../docs/audit-20260924/17-tls-provenance-capture.md)。

Linux UDS 已提供服务身份权限和同 UID 验证；TCP token IPC 仍是显式兼容入口。本轮 Rust release 35项测试、26项HTTP/UDS loopback、8项新增HTTP/TLS边界测试通过。实际CLI、Go adapter与race验证单独记录；Windows/macOS原生构建、真实账户推理和生产部署不在上述loopback结论范围。
