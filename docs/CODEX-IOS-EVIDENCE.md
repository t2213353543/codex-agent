# iOS Codex 客户端身份核验

核验日期：2026-09-17。范围：OpenAI 官方文档和本地官方源码快照 `.local/codex-source-0.154.0`；没有读取本机个人会话、账户或凭据，也没有获得 iPhone/iPad 实机网络抓包。

## 本次最终实现范围

用户已明确授权：如果不能确认完整 iOS 指纹，可使用 macOS/Linux。由于下述证据不足以还原 iOS 完整请求指纹，本次实现提供默认关闭的 **Codex 客户端指纹**开关，以及 `linux` / `macos` 两个配置，不提供不完整的 iOS 选项。

这两个配置是依据官方源码 UA 格式构造的参考模板，没有经过 macOS/Linux 真机抓包验证。系统版本 `Linux 6.8.0`、`Mac OS 15.0.0` 及架构是明确的模板值，不能声称是当前网关机器实测值。CLI 版本来自本项目的 `clientVersion`：

```text
linux: codex_cli_rs/{clientVersion} (Linux 6.8.0; x86_64) unknown
macos: codex_cli_rs/{clientVersion} (Mac OS 15.0.0; arm64) unknown
```

启用时统一 User-Agent、originator、version 和 models 的 client_version，并移除绑定原始客户端身份的 `x-oai-attestation`；会话与账号鉴权仍由原有链路处理。关闭后恢复原有客户端头透传与回退规则。请求仍由 Go HTTP/WebSocket 传输发出，**Go TLS/HTTP 行为不等同于真实 Codex Rust 客户端或 Apple iOS/macOS 网络栈**。

## iOS 调研结论

官方源码确实识别 iOS Remote 客户端名 **`codex_chatgpt_ios_remote`**。这只证明存在受支持的 Remote 身份，不足以证明完整 iOS 请求指纹。

官方当前文档确认，ChatGPT iOS 应用通过 Remote 控制已配对的 Mac/Windows 宿主，项目、工具、凭据和执行环境来自宿主。因此真实架构中的“手机端身份”和“向模型服务发请求的宿主传输栈”应分开理解。

## 已证实的事实

| 事实 | 证据 | 实现含义 |
| --- | --- | --- |
| ChatGPT iOS 支持 Remote，连接的 Mac/Windows 宿主执行工作 | [Remote connections](https://learn.chatgpt.com/docs/remote-connections)、[Codex Remote](https://learn.chatgpt.com/docs/remote)，已打开正文 | 不能把手机 UI 误认为模型请求一定由 iOS 网络栈直接发送 |
| CLI 官方安装文档列 macOS、Linux 和 Windows | [Codex CLI](https://learn.chatgpt.com/docs/codex/cli)，已打开正文；快照 `codex-cli/bin/codex.js:16` 的平台包映射没有 iOS | `aarch64-apple-darwin` 是 macOS 目标，不能拿来冒充 iOS CLI |
| 官方识别 iOS Remote 客户端名 | `codex-rs/app-server/src/request_processors/thread_resume_redaction.rs:10`；`codex-rs/app-server/tests/suite/v2/thread_resume.rs:2200` | 使用准确值 `codex_chatgpt_ios_remote`，不要自行创造 `codex_ios` 或 `ios_codex_cli` |
| Remote 协议存在 iOS 设备元数据 | `codex-rs/app-server-transport/src/transport/remote_control/tests/clients_tests.rs:76`，测试值 `platform: "ios"`、`device_model: "iPhone"` | 证明 Remote 接入端的设备分类；测试样例不是实机 UA 或 TLS 证据 |
| `initialize.clientInfo.name` 参与默认 `originator` | `codex-rs/app-server/src/request_processors/initialize_processor.rs:89`、`:112` | 同时用于连接的客户端名；进程身份首次初始化后不可任意覆盖 |
| 初始化会设置 UA 后缀 | 同文件 `:94`、`:138` | 格式为 `({clientInfo.name}; {clientInfo.version})`，版本来自调用方，而非 iOS 系统版本 |
| 默认 UA 保留宿主系统和真实 CLI 版本 | `codex-rs/login/src/auth/default_client.rs:164` | 不能把宿主 OS 字段随意替换成 iPhone/iOS |
| 线程 `originator` 可覆盖默认请求头 | `codex-rs/login/src/auth/default_client.rs:133`；`codex-rs/core/src/client.rs:1243`、`:1315` | HTTP Responses 和 WebSocket 请求均存在该机制；必须防止不同身份的旧线程或连接复用 |
| 网络栈来自 Rust 宿主 | `codex-rs/http-client/Cargo.toml:14`、`:16`；`codex-rs/Cargo.toml:421` | 源码使用 reqwest、native-tls/rustls 等；没有证据表明它是 iOS URLSession 的字节级复刻 |

以上源码路径均相对于 `.local/codex-source-0.154.0`，属于该快照的静态分析结果，不代表已通过官方生产服务或 iOS 实机验证。

## `originator` 和 User-Agent 的准确关系

默认 UA 的代码格式如下：

```text
{process_originator}/{CARGO_PKG_VERSION} ({host_os_type} {host_os_version}; {host_arch}) {terminal_user_agent} ({clientInfo.name}; {clientInfo.version})
```

这个快照的 `CARGO_PKG_VERSION` 是 `0.154.0`（`codex-rs/Cargo.toml:152`）。它是 CLI/宿主构建版本，不是 ChatGPT iOS 应用版本。若能从真实运行时的 `initialize` 响应读取 `userAgent`，应使用运行时返回值，不手工猜测宿主版本、终端或移动应用版本。

`CODEX_INTERNAL_ORIGINATOR_OVERRIDE` 优先于初始化传入的名称，`set_default_originator` 只允许首次成功设置。初始化其他真实客户端连接还可能更新进程级 UA 后缀。这意味着共享一个已初始化的 Desktop 宿主后再接入 iOS，不能推导出所有请求的 `originator` 必然变为 iOS。

线程实际身份的优先级为：受支持的 `metrics_service_name` → 已保存线程身份 → 继承身份 → 环境覆盖 → 进程默认身份（`codex-rs/core/src/thread_manager.rs:300`、`:316`）。其中 service name 映射只包含 `codex_work_desktop`、`codex_work_web`、`codex_work_mobile`、`codex_work_cca` 和 `chatgpt_cca`。因此启用 iOS 身份时，应隔离运行时和会话缓存，或在同等边界强制使用一致身份，不能简单复用旧 Desktop 线程。

## 当前无法证实的内容

- ChatGPT iOS App 当前准确的 `clientInfo.version`、原生 HTTP User-Agent 和所有额外请求头。
- iOS 客户端直接连接服务时的 TLS ClientHello、JA3/JA4、HTTP/2 SETTINGS、头顺序、HTTP/3 行为以及具体网络库。
- 某个任意 iOS 应用版本所有网络请求是否统一使用 `codex_chatgpt_ios_remote`；源码只证明该 Remote 客户端名受到支持。
- 仿写这些身份字段是否产生任何特定的服务端识别、访问权或限流效果。
- iOS 原生 CLI 的官方发行包；本次核验未发现，不应把“未发现”表述为任何未来或私有实现绝不存在。

## iOS 后续实现的可审查边界（本次未采用）

若未来实现“iOS Codex Remote 身份”，其边界是使用官方 iOS Remote 客户端标识，并将模型请求实际交给真实 Codex 宿主处理。切换后需要隔离身份相关会话和长连接；实际宿主 UA、会话标识及运行时版本应来自真实运行链。

如当前实现仍由 Go 直接向上游发送请求，该设置只能声称“iOS Remote 标识兼容”，不能声称传输层已完全变成 iOS，或已经使用真实 Codex Rust 网络栈。只有请求确实经由所配置的真实 Codex 运行时发出后，才可声明使用真实 Codex 宿主。

要继续达到“完全使用 iOS”的字节级验证目标，需要明确到某个真实设备、iOS 版本和官方应用构建的脱敏网络样本，逐项比对手机到 relay、宿主到模型服务这两段链路。未经这样的证据，不应在产品中承诺“完整 iOS 指纹”。
