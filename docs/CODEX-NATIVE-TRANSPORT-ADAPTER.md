# 官方 Rust 传输适配器可行性记录

2026-09-18，依据本地正式版 `rust-v0.154.0` 源码。结论是：**可以独立复用官方底层 Rust HTTP 传输，无需编译整个 CLI；它适合做可选的常驻子进程。当前尚无可用适配器二进制，也没有完成编译或网络验收。** HTTP 可先实现，WebSocket 需要额外的代理注入接口及官方分支依赖。此研究没有修改生产 Go 路径、部署或全局工具链。

本机 `rustc`、`cargo`、`rustup` 不在 PATH，用户目录 `.cargo/bin` 等已检查位置未发现工具链，`cmake`、`clang` 也不在 PATH。WSL 只列出 `docker-desktop`，Docker API 连接失败；未启动 Docker 或下载构建环境。因此本轮不能把源码分析写成“编译通过”。静态检查脚本和带 SHA256 的依赖证据保存在 `.local/native-transport-prototype/inspect.py`、`inspection.json`。脚本已实际运行，证实 HTTP 仅需要两个本地生产 crate，WS 合计三个；这不表示第三方依赖只有两个或三个。

应用层身份与实际 TLS 必须分别描述。当前 `linux` / `macos` profile 配置的是请求头。它不会改变 Go 的 TLS、操作系统或出口 IP。官方 `native-tls` 在 Windows 使用 Schannel、macOS 使用 Secure Transport、其他平台使用 OpenSSL；Linux 上编译相同 Rust crate 也不会得到 Windows Schannel。[native-tls 0.2.14 文档](https://docs.rs/crate/native-tls/0.2.14)

| 官方路径 | 源码行为 | 适配器需要保持的条件 |
| --- | --- | --- |
| HTTP 默认信任 | `http-client/src/client_builder.rs:276` 沿用 reqwest 默认 TLS；`tls_backend_fallback.rs:1` 明确 native TLS 默认 | 同平台、相同库版本与编译 feature；不能统一强制 rustls 后称为默认 CLI |
| HTTP 自定义 CA | `custom_ca.rs:295` 有 CA 时明确 `use_rustls_tls()`；`CODEX_CA_CERTIFICATE` 优先于 `SSL_CERT_FILE` | 复用此函数及原证书校验；不能把成功的自定义 CA 样本当默认 TLS 样本 |
| 特定 TLS 协商失败 | `tls_backend_fallback.rs:104` 只识别连接阶段协议版本错误，排除证书错误和超时 | 该策略在 route-aware 客户端层；只引底层 HTTP builder 不会自动获得全部策略 |
| Responses WebSocket | `codex-api/src/endpoint/responses_websocket.rs:519` 使用 `WebSocketConnector::new`，后者显式构造 rustls 配置 | 与 HTTP 的 native 默认不同；保留 aws-lc provider、根证书及 WS 配置 |
| 官方 Linux release | release workflow 构建 musl；`core/Cargo.toml:128` 对两种 Linux musl 目标启用 vendored OpenSSL | 独立 helper 必须补这项 feature；否则可能误用系统动态 OpenSSL |

已有官方二进制 loopback 观察记录在 [传输观察](CODEX-NATIVE-TRANSPORT.md)：Windows 默认信任只捕获到首个 ClientHello；临时自定义 CA 才完成 TLS 1.3 / HTTP/2。该证据也明确保留 Go 与官方 TLS 的差异，不支持“完全相同”的结论。

建议新 helper 直接依赖官方 `codex-http-client`，由它继续依赖 `codex-utils-rustls-provider`。官方 crate 没有生产依赖 `codex-core`、`codex-login`、TUI、V8 或工具执行组件。加入 `codex-websocket-client` 才扩展 WS 支持。可在独立 workspace 中使用固定源码 path 依赖，或仅复制这几个 crate 并保留 Apache-2.0 许可证与来源；不修改下载的完整源码。

| 依赖 | 官方 Cargo.lock 中的版本 / 来源 |
| --- | --- |
| Rust / edition | 工具链 `1.95.0` / `2024` |
| reqwest / hyper / hyper-util / h2 | `0.12.28` / `1.8.1` / `0.1.20` / `0.4.16` |
| native-tls / hyper-tls | `0.2.14` / `0.6.0` |
| openssl / openssl-sys / openssl-src | `0.10.75` / `0.9.111` / `300.6.1+3.6.3` |
| rustls / hyper-rustls | `0.23.36` / `0.27.7` |
| rustls-native-certs / rustls-pki-types | `0.8.3` / `1.14.0` |
| aws-lc-rs / aws-lc-sys / ring | `1.16.2` / `0.39.0` / `0.17.14` |
| tokio / tokio-rustls / zstd | `1.52.3` / `0.26.4` / `0.13.3` |
| tokio-tungstenite | `0.28.0`，OpenAI fork rev `0e5b2d73aa18dd9f0a50ee9ff199d5aef7594186` |
| tungstenite | `0.27.0`，OpenAI fork rev `4fffad30fe373adbdcffab9545e9e9bf4f2fc19f` |

锁文件另外包含 reqwest `0.13.4`；它不是这里的 `codex-http-client` 依赖，不能误替换。HTTP crate 的 reqwest features 来自 workspace 的 `cookies`，加 `json`、`rustls-tls-native-roots`、`stream`，默认 features 仍然开启。workspace 的 `otel`、`responses-api-proxy` 还声明了 `blocking`、`rustls-tls`；独立构建必须检查完整 CLI 与 helper 实际 `cargo tree -e features`，不能仅凭版本号宣称同一 feature 集合。官方 rustls 明确使用 `aws_lc_rs` 与 `std`；provider helper 安装 aws-lc，不能任由 ring 自动选择。

reqwest 的默认 features 包含 `default-tls` 与 `http2`，而 `native-tls-alpn`、`gzip`、`socks` 是独立 feature。本地 CLI manifests 没有显式启用这三项；完整传递依赖是否统一启用，需要 Cargo 实际解析确认。不要为了“支持 HTTP/2”擅自添加 native ALPN，也不要自动增加 gzip。WS 的两个 `[patch.crates-io]` 必须在 helper 根 workspace 保留；依赖自己的 patch 不会自动成为新根的 patch。[reqwest 0.12.28 features](https://docs.rs/crate/reqwest/0.12.28/features)

最小 HTTP 实现可用公开的 `build_reqwest_client_with_custom_ca(reqwest::Client::builder()...)`，加显式 direct / proxy 配置，再经 `HttpClient::new_without_request_logging` 发出原始字节。现有 Go 网关继续决定账号、会话、权限、代理配置版本、请求头、payload、刷新 CAS 与重试。helper 只执行给定请求，不载入 `auth.json`，不启动 CLI，不重建 tools，不改模型输入。这样复用的是官方低层传输及 CA 实现，而不是声称完整复制 CLI 所有高层行为。

直接套 `ReqwestTransport::execute/stream` 不是最稳妥的网关边界：其非 2xx 分支会先读完 body 并转换为 transport error；网关仍需保留真实 status、重复响应头及原始错误 body。应使用 `HttpClient` 的响应流，所有 status 都先交回 Go。若使用 `HttpClient` wrapper，必须禁用请求日志且不安装额外 tracing propagation，以免凭据出现在日志或新增追踪头。

| 请求类型 | 原始数据与语义边界 |
| --- | --- |
| HTTP / models | 保留 method、URL、重复 header 值、status 与 bytes。请求头仍由 Go 现有来源决定；不重写 UA/version。重定向默认与官方库一致但账号凭据跨 origin 必须沿用网关约束 |
| SSE | 按 bytes 向 Go 交付；helper 不解析、聚合或修补事件。响应头完成不等于首 token。禁止为了重连重放已发请求；持续输出无总超时，空闲计时只包住网络读取 |
| compact | 原样传 JSON 请求，整体 deadline 由 Go 传入；目前默认 20 分钟。helper 不把 unary 响应当 SSE，也不引入固定 10 分钟 Client.Timeout |
| Zstd | 若 Go 已提供 `Content-Encoding: zstd`，传输其原字节，不能再次压缩。若后续选择由 Rust 压缩，必须显式单独约定；官方 `request.rs` 使用 zstd level 3。压缩后的字节应跨重试复用 |
| OAuth | 同一已固定 proxy / TLS 配置执行原请求；401 恢复、失败合并、RT 轮换持久化由 Go 管。helper 不因 401 或连接错误自行刷新或重试。Go 已脱离用户取消的有效刷新不能被推理请求 Cancel 误杀 |
| WS | 通过官方 fork 发送原始消息内容，保留文本/二进制、ping/pong、close code/reason、握手错误 status/body。Responses 显式启用 `permessage_deflate`，不能替换成普通未打补丁 tungstenite 后称为相同 |

每账号显式代理是 WS 的一个真实接入缺口。`WebSocketConnector::connect` 会自行解析平台/环境代理；接受 `OutboundProxyRoute` 的 `connect_with_route` 是私有方法。HTTP 可在公开 reqwest builder 直接指定代理，WS 则宜在隔离 vendor 中增加一个窄的公开 `connect_resolved_route` 包装，把 Go 选定的 direct / proxy 传给现有 dialer，不修改握手业务。每请求修改进程 `HTTP_PROXY` 会产生并发串路，不可采用。按代理启子进程虽可避免接口补丁，但增加进程/连接池成本，还需额外处理凭据环境暴露，通常不是最小运行方案。

官方 WS dialer 已处理 HTTP CONNECT、HTTPS-to-proxy TLS，以及 fork 的代理实现。SOCKS5 必须独立验证：若当前 HTTP feature 集无法满足网关既有 SOCKS5 需求，可显式开启 reqwest `socks` 作为可记录的兼容扩展，或拒绝此 native 模式配置；不能悄悄直连。不得假设 HTTP 与 WS 的 SOCKS DNS/认证行为天然一致。Go 选定的代理配置版本变化后，helper 应按 route generation 作废旧池；已在传输的流继续完成，新的请求失败关闭或使用明确的新版本，不能复用旧密码的连接。

建议生产 IPC 采用受权限保护的 Unix domain socket；Windows 使用仅服务身份可访问的 named pipe。协议分控制通道与数据流，使用有长度上限的二进制 frame；request ID 只在当前 helper 连接内唯一。请求元数据、body、response headers、data、end、error 各有独立 frame，header value 保留原 bytes。不要把 Authorization、RT 或代理密码放进 argv、环境、URL 日志或临时文件。

取消与背压需要在真实实现中满足这些条件：

1. Go context 取消发送独立 `Cancel(request_id)`；Rust 以 `select!` 取消该 send/read future 并丢弃相应 body / WS。不得通过杀死共用 helper 来取消单请求。helper 或 IPC 退出时，Go 关闭受影响请求并明确返回传输失败；不能自动重放可能已执行的 POST。
2. 每流设置固定字节额度，例如最多 256 KiB 在途数据和 64 KiB 单帧；全局也有硬上限。Go 只在下游消耗后返还 credit。Rust 等额度时不继续读取上游，等待下游不计作上游 idle。HTTP/2/TCP 内部缓冲仍存在，需要实测其内存上界，不能把 IPC 限额等同总 RSS。
3. 控制通道不能被某个慢消费者的数据写入阻塞。共享一个 stdout 的朴素 NDJSON 会造成队头阻塞，特别是大 compact/body；最小可验证设计是控制 socket 加独立每请求数据连接，或真正带公平调度和 credit 的多路复用。
4. EOF、Cancel、deadline 与正常结束必须只有一次完成通知；Go `Body.Close` 要触发取消并释放额度。OAuth 交换使用自己的请求 ID 与生命周期，允许 Go 先接收和持久化成功轮换结果，再结束该 IPC 请求。
5. 账号/代理池键包含不可逆摘要或不透明 handle，不缓存默认 Authorization，不共享账号 cookie jar。关闭诊断与 panic 中的敏感字段；不得持久化 TLS session keys。令牌仍短暂存在于两进程内存；常规 allocator、swap、core dump 意味着无法仅靠“不写文件”保证物理零落盘，应沿用服务权限并关闭 core dump。

Linux 部署目前是 `tools/deploy.py` 交叉编译的 `CGO_ENABLED=0` Go 程序，由 systemd 运行，不是已确认带 Rust 的容器。新增 helper 是额外发布产物，要带版本、源码/锁文件/产物 SHA256 与健康握手；Go 无需引入 cgo。官方 Linux release 对齐时应选相同 musl target，锁 vendored OpenSSL，并在构建环境提供 Rust 1.95.0、C/C++、CMake、Perl/make 和 musl linker。aws-lc 与 zstd 均有原生构建部分；不能假设“Rust crate”可纯 Rust 无 C 工具编译。官方 musl workflow 还设置 `AWS_LC_SYS_NO_JITTER_ENTROPY=1`。无需搬运与 CLI 沙箱、V8、libcap 等无关的整个 release 构建流程。

可执行的下一步是先在隔离 Rust 构建环境产出 HTTP-only helper，再运行固定合成 loopback 验收：原字节 body/重复 headers、401/429/5xx 不丢失、连续 SSE 不缓冲、慢消费者背压、有活动续命、真正 idle 截止、Cancel/Close 不泄漏、两轮复用 TCP、代理凭据轮换不串路。随后加官方 fork WS 的压缩、ping/close、握手错误和双向取消。先完成这些检查，再接入可选 Go transport；native 模式失败应可见，不能静默切回 Go 后报告“native 成功”。

最后需要在同平台、同 CA、同目标/SNI、同 proxy 条件下对比官方 CLI 与 helper 的多次 ClientHello、ALPN、协商 TLS/HTTP 版本、HTTP/2 SETTINGS、WS extensions 和连接复用。忽略随机数并区分首次握手与恢复握手。即使依赖锁完全一致，也应以这些观测确认实际行为，不能仅靠 User-Agent、JA3 或 uTLS 模板宣称真实设备或整个链路逐字节一致。
