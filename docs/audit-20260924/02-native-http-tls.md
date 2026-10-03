# 02 — 原生 HTTP/TLS helper 审计与修复

审计日期：2026-09-24。只读对照：`.local/codex-source-0.155.0/codex-rs`。本专项修改范围：`native-transport/src/main.rs`、新增 `src/http_tests.rs`、新增 `tests/http_boundaries.py`。没有修改 vendored 源码、Cargo 依赖、WebSocket 实现或 Go 转发逻辑。

## 已修复

1. **非法 Content-Length 被当作合法长度。** 原实现直接调用 `usize::parse()`，会接受 `+0` / `+3`，与 HTTP 的十进制数字语法不同，也可能让下游编码器再次解释或改写已准备的 framing。`main.rs:385` 现在先校验非空 ASCII 数字，再解析、匹配真实 body 长度，并继续拒绝重复字段。合法 `003` 保持原值。
2. **总期限不包含 IPC 上传。** 原计时在读取完整 body 后才开始；发送已声明的大 body 却不完成上传时，120 ms 的总期限会退化成独立 30 秒 IPC 上传期限。`main.rs:504` 把读取 body 与完整上游交换放入同一 future，`main.rs:520` 在读取 body 前启动总期限；上传用掉的预算不会重置。读取 request head 仍有独立 30 秒上限，因为只有读到、鉴权并验证 head 后才知道请求预算。
3. **单次 accept 失败会终止整个 helper。** 原 `accepted?` 会将连接中止或暂时文件描述符压力升级为进程退出，从而取消其他 HTTP/WS 请求。`main.rs:553`、`main.rs:559` 现在将失败转成 100 ms backoff 后重试；backoff 在 `select!` 的 accept 分支内，父进程 EOF 和完成任务回收仍可抢先处理。
4. **单条 Unix IPC peer credential 查询失败可杀死共享 helper。** `main.rs:176` 现在只拒绝该连接；其他已鉴权请求继续工作。
5. **本地 TCP IPC 的小帧存在 Nagle/延迟 ACK 叠加。** `main.rs:170` 仅为 loopback IPC socket 设置 `TCP_NODELAY`。HTTP 上游 socket、TLS ClientHello、HTTP/2 配置、WS `TcpNodelay::Default` 均不由这个设置改变。选择 socket 选项而没有把每个 64 KiB DATA frame 再复制进连续大缓冲，避免额外 body copy。未运行独立延迟基准，不量化性能收益。

`main.rs:413` 的 `in_frame` 标记保留：总期限或取消截断 frame 时，不再把 ERROR 拼入尚未完成的 payload；接收端必须把缺失 END 的交换作为不完整响应处理。

## 一致性与差异矩阵

下表的“源码一致”表示选择相同库/算法/默认配置路径，不等于已验证全部线协议字段。上游路径以 `codex-rs/` 为根。

| 边界 | CLI 0.155.0 证据 | helper 证据 | 结论/处理 |
| --- | --- | --- | --- |
| 默认 TLS backend | `http-client/src/client_builder.rs:276`、`:301`：默认 `TransportDefault`，只有显式 Rustls 才切换 | `main.rs:309`：原生 reqwest builder，不强制 rustls | 源码一致；Linux OpenSSL、Windows Schannel 不属于同一种指纹 |
| 自定义 CA 选择 | `http-client/src/custom_ca.rs:296`、`:307`、`:330`：强制 rustls、追加系统根；`:398`：CODEX 优先、空值视为未设置 | `main.rs:338`：直接复用官方 custom-CA builder | 源码一致；新增 CA 优先级、空 override 回归 |
| CA 无效时行为 | `login/src/auth/default_client.rs:305`：legacy 构造存在 custom-CA fallback | `main.rs:338`：构造失败明确返回 `tls_or_client_config` | 有意差异，保留可观测失败，不静默换信任配置 |
| SNI 与 hostname 校验 | 官方 reqwest/rustls 栈，未禁用验证 | 同栈，无 invalid-certs/invalid-hostnames bypass | 新增 DNS localhost SNI、可信 CA 下 IP hostname 不匹配失败测试 |
| ALPN 与 HTTP/2 | `http-client/Cargo.toml:14`：reqwest 默认 + json/rustls native roots/stream；完整 workspace 决定 feature union | `Cargo.toml:26`、`:29`、`main.rs:309` | 未添加 native-tls-alpn；custom CA/rustls 新增真实 h2 preface、SETTINGS、DATA、HEADERS 交换测试；完整 SETTINGS 值比对属于指纹专项 |
| HTTP 客户端生命周期 | `core/src/client.rs:1040`、`:1503` 每次构建 API transport；`login/src/auth/default_client.rs:252` 创建客户端 | `main.rs:441`：默认 `cli_request` 每次新 client，`pooled` 显式复用 | 默认路径一致；pooled 是明确的性能模式，与 CLI 默认 TLS session/连接池生命周期不同 |
| 基础设施 cookie | `http-client/src/chatgpt_cloudflare_cookies.rs:106`，host/path 过滤及允许的 CF / `__oailb` cookies | `main.rs:336`：直接复用同一进程共享 jar | 源码一致；不同 route helper 的进程级 jar 各自隔离，与单 CLI 全进程不完全相同 |
| 应用默认头 | `login/src/auth/default_client.rs:296` 使用 default headers，含 UA/originator | `main.rs:309` 不主动加 UA/originator/version，由 Go 的已准备应用头提供 | 层次划分有意差异；完整头一致性应合并 Go 专项结论 |
| 重定向 | `http-client/src/client_builder.rs:305` 默认允许 | `main.rs:314` 明确 `Policy::none()` | 有意差异；将 3xx 原样返回 Go，不在 helper 重放 POST |
| 网络重试 | CLI transport 使用 reqwest 默认网络策略，业务层也有 retry | `main.rs:315` 明确 `.retry(never())` | 有意差异；由 Go 单点决定重放。不能声称 CLI 的全部 retry 状态已复制 |
| 代理 | `login/src/auth/default_client.rs:252` 可走系统/PAC route policy，默认客户端也可读环境 | `main.rs:313` `.no_proxy()`，仅指定固定显式 proxy；HTTP 无 SOCKS feature | 有意差异；代理失败不直连；SOCKS 由 Go 的受控 CONNECT 桥承担 |
| 压缩与原始 body | `http-client/src/transport.rs:55` 调用 `prepare_body_for_send` | `main.rs:440` 使用独立 `request_compression` 模块；none 原样，zstd 复用官方 encoder | 既有压缩实现未改；不在 HTTP transport 猜测 OAuth/provider 意图 |
| Content-Length 与重复头 | CLI 使用 HeaderMap / reqwest，根据 prepared body 发送 | `main.rs:375` 使用 append 保留重复字段及非 UTF-8 bytes；`:385` 严格数字、唯一且真实长度一致 | 网关 IPC 增加拒绝非法 framing 的校验；此次修复带符号长度 |
| target/path/query | CLI 由 provider 和 URL builder 准备目标 | `main.rs:355` 要求 origin-form ASCII，`:361` 比对解析后的 target，拒绝 dot normalization | 网关明确边界；合法编码/重复 query 原样保留，无法完整保留的路径拒绝 |
| 响应状态和错误 body | `http-client/src/transport.rs:139` 非 2xx 先读取 text 再返回 TransportError | `main.rs:453` 任何 status 先 HEAD，随后 DATA；101 后 `upgrade_unsupported` | 有意差异，避免 helper 聚合错误正文或吞掉状态；重试/错误判定由 Go 负责 |
| timeout | `http-client/src/transport.rs:70` 可设置请求 timeout；SSE idle 在上层 | `main.rs:451` headers deadline；`:477` idle 仅包 upstream read；`:520` total 包 upload+交换 | 适配层明确三个期限；idle 不把下游背压算成上游 idle；0 headers/idle 仍表示关闭额外期限 |
| IPC framing 与取消 | CLI 没有此 IPC 层 | `main.rs:413` 有界帧，`:508` 上传完成后任意 byte/EOF 取消；`:525` 半帧不追加 ERROR | 额外网关协议；单独连接隔离请求，父 stdin EOF 取消全部 |
| pool 与背压 | reqwest/hyper 自身 pool / socket 缓冲；CLI 生命周期决定实际复用 | `main.rs:441` 显式生命周期；`:474` 每 chunk 等待下游 write 后才读下一 chunk | 没有额外无界响应队列；64 KiB 只是 frame 上限，不能据此宣称整体内存上限 |
| 准入和进程故障隔离 | CLI 无多账户 helper | ready `max_active: 0`，Go 按账号准入；`main.rs:553` accept backoff | 保持不叠加固定 32 并发槽；此次修复单连接故障触发全部取消的问题 |

## 新增验证

`src/http_tests.rs` 共 8 个 Rust 测试，覆盖：十进制 Content-Length 与重复值、重复二进制头及敏感标记、hop-by-hop/header injection、prepared target 规范化、body/header/token/timeout 上限、IPC 长度和未知字段、上传阶段总期限、背压中截断帧标记。

`tests/http_boundaries.py` 共 8 个真实 loopback 测试，覆盖：非法长度不触达上游、未完成上传总期限、上传时间不返还、短 body EOF 不提交、已声明 body 后额外字节取消、DNS SNI 与 hostname mismatch、CA 优先级/空 override、rustls ALPN + 真实 HTTP/2 帧交换。测试只生成临时 CA/服务端叶证书，使用 synthetic 请求，不读取账户、不调用生产接口。

在 `native-transport/` 执行：

```sh
cargo test --locked --bin codex-native-transport
cargo build --locked
python3 tests/loopback.py --binary target/debug/codex-native-transport --report test-results.json
python3 tests/http_boundaries.py --binary target/debug/codex-native-transport
python3 tools/check_lock.py
```

本专项已实际完成：`python -m py_compile native-transport/tests/http_boundaries.py`，通过。当前 Windows shell 没有可用 Cargo；本机 Docker Desktop daemon 未运行。root 已找到项目隔离 Linux Docker 构建环境，将合并 HTTP/WS 修改统一执行上述 Rust 与 socket 测试。此处在收到 root 实测前不把新增运行回归标成通过。

## 仍需区分的边界

- 尚未对 Windows/macOS 编译和系统信任栈做此次实测。
- 新增 HTTP/2 测试证明能够协商并完成实际 HTTP/2 交换；不证明 SETTINGS 顺序、WINDOW_UPDATE、HPACK、TLS 扩展顺序或恢复行为与官方二进制逐字段一致。该结论需要同平台官方 CLI 捕获对照。
- accept 背压/peer credential 修复有明确调用路径证据；本专项未通过故障注入制造操作系统 `EMFILE` 或 peer credential syscall 失败。
- helper 仍先缓冲最多 32 MiB 请求体，每个已准入请求都有独立 body 内存；此次没有把 frame 上限当作进程内存上限。
- compression 和创建 TLS client 为同步工作；Tokio timeout 不能在同步 CPU 工作中强制抢占。原生库 client 构造生命周期仍遵循 CLI，异步建客户端/CPU offload 应另行以并发测量验证，避免改变默认 TLS session 生命周期。
- accept 连续失败时会每 100 ms 重试并保留既有请求，未增加带凭据的库错误日志；长期资源不足的外部监控属于进程运行层。

## 主任务实测回填

2026-09-24 隔离 Linux x86_64 musl / Rust 1.95.0 release 构建通过；35 项 Rust 单元/真实 socket 测试全部通过（含 HTTP 8、WS 24、压缩 3），26 项既有 loopback 与 8 项新增 HTTP/TLS/HTTP2 边界测试全部通过，零跳过。原先两项 WS fixture 压缩配置错误已经修正并重跑。证据：`.local/gateway-audit-20260924/native-rust-tests.log`、`native-loopback-result.json`、`native-http-boundaries.log`、`native-source-manifest.json`；这是本次源码快照的实测，未据此宣称全部平台或全部 TLS 字段相同。
