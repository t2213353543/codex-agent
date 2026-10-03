# CLI 0.155.0 源码对齐

2026-09-18 本轮重新访问官方 GitHub release API 与 npm latest，两者均返回正式版 `0.155.0`。GitHub 发布时间为 `2026-09-17T23:14:43Z`。[官方 release](https://github.com/openai/codex/releases/tag/rust-v0.155.0)

本地源码 `.local/codex-source-0.155.0/` 来自该固定 tag，归档 SHA256 `5dd67c690996955464b82fa6413ff0023b8105a8acf88c391948c1a4c1abde16`，GitHub 解包前缀 `openai-codex-799f378`。Linux 官方 CLI 通过 npm 指定版本包下载并校验 SHA512 integrity，二进制 SHA256 `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`。旧 0.154 报告保持原版本含义，不覆盖。

| 范围 | 0.154 -> 0.155 观察 | 网关处理 |
| --- | --- | --- |
| HTTP TLS / custom CA / WS connector | 对应源码逐字未变；reqwest/native-tls/rustls/OpenSSL/hyper/h2 及两 WS fork revision 未变。 | 继续复用固定官方 crate，重新编译并做新 CLI 对照；不能只看版本号宣称等价。 |
| ChatGPT 基础设施 cookie | `http-client/src/chatgpt_cloudflare_cookies.rs` 新增允许 `__oailb`，仍限制基础设施 cookie、域、路径、Secure 与过期。 | HTTP helper 使用官方受限 jar；合成 chatgpt.com TLS + 固定本地 CONNECT 测试验证 __oailb/CF、过期、路径/外域和拒绝账户 cookie。 |
| 工具结果 metadata | `core/src/client.rs` 在 HTTP/WS 请求发送前按 resolved HTTPS 官方 host allowlist 过滤原始工具 metadata。 | 保留客户端真正发来的数据，不补回 CLI 主动删除的数据，不把这项客户端过滤再误用于更改工具结果正文。 |
| 身份所有者 | `login/src/auth/change_state.rs` 的 same_owner 同时比较用户、workspace 和 auth mode；未知 owner 不能证明相同。`core/src/client.rs` 在 owner generation 改变时重置 WS/turn state。 | generation 是客户端本地状态，不伪造 wire 字段。审查发现网关旧绑定只含 workspace，需将已知 UserID 纳入不可逆绑定身份并验证跨用户拒绝。缺失身份的证据边界仍单列。 |
| AWS credential export / compact 重构 | provider 新增 AWS credential_export 配置，client compact 代码拆分与工具格式构造有调整。 | 不把 ChatGPT OAuth 网关伪装成 AWS SDK；保持客户端生成的请求、原生 compact 输入与已有超时契约，单独重跑调用路径。 |

本轮原生 helper 加入官方 `codex-websocket-client`，并采用两个固定官方 fork。仅 WS connector 增加一个显式选定代理入口，其 patch 和原始/修改后 hash 都保存在 `native-transport/UPSTREAM.json`、`WEBSOCKET-ADAPTER.patch`。不得把这个文件说成原样源码；其余 30 个 Rust 源文件与 0.155 来源一致。解析依赖 265 包，registry/git 每项与官方锁核对，本地 4 个包按显式清单检查。

新 CLI 的真实可执行文件已在合成上游验收 HTTP 工具回传和 WS warmup / shell / incremental / process resume。TLS、SOCKS 和 cookie 的样本另测，不把模拟服务器的合成 usage 当真实缓存命中，也不把 en_US/TZ 环境变量称为美国真实设备。最终网关和 helper 的配对证据以本轮集成报告为准。
