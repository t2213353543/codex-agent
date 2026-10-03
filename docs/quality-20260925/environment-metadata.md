# 官方 0.155 环境、安装和会话标识来源

本轮只读核查官方固定源码与网关现有透传路径，未改生产、未执行模型请求。结果用于真实运行环境指纹修复：应让 OS/架构/terminal 声明来自对应进程环境，同时保留客户端已有会话标识的生命周期，不能把所有 ID 都做成永久设备 ID。

| 字段 | 真实来源及生命周期 | 固定源码定位 |
|---|---|---|
| User-Agent OS/version/arch | `os_info::get()` 的实际进程环境；terminal 由环境变量检测；不是写死参考 OS | `.local/codex-source-0.155.0/codex-rs/login/src/auth/default_client.rs:164` |
| terminal | 优先 TERM_PROGRAM/TERM_PROGRAM_VERSION，再 terminal-specific 变量，再 TERM；一次进程内 OnceLock。没有变量时 unknown 合法 | `.local/codex-source-0.155.0/codex-rs/terminal-detection/src/lib.rs:192,234,253` |
| installation_id | CODEX_HOME/installation_id；文件锁保护，已有合法 UUID 复用，没有或非法时生成 UUIDv4 并持久化。与 MAC/CPU/账号无派生关系 | `.local/codex-source-0.155.0/codex-rs/core/src/installation_id.rs:17-62` |
| thread_id / session_id | 新线程生成；resume 恢复 thread ID 和 rollout SessionMeta 的 session ID；根会话 session ID 等于根 thread ID，子 agent 可继承根 session | `.local/codex-source-0.155.0/codex-rs/core/src/session/session.rs:787-822` |
| window_id | `thread_id:window_number`，随会话上下文窗口推进，而非设备或桌面窗口句柄 | `.local/codex-source-0.155.0/codex-rs/core/src/session/mod.rs:4323-4336` |
| context_window_id | 新窗口 UUIDv7；首次窗口 first/current 相同，推进保留 previous 并生成新的 current | `.local/codex-source-0.155.0/codex-rs/core/src/state/auto_compact_window.rs:12-20,78-85` |
| root_turn_id | task 启动默认当前 turn sub_id；继承/恢复在启动前写入，OnceLock 保持首值，后续邮件不能换根 | `.local/codex-source-0.155.0/codex-rs/core/src/tasks/mod.rs:282-293,493-498`；`core/src/turn_metadata.rs:329-344` |

源码中的 `device_auth_id` 属于设备码登录流程的服务器授权会话，见 `.local/codex-source-0.155.0/codex-rs/login/src/device_code_auth.rs:23,103-113`。本次搜索普通 Responses 的 core/login/codex-api 未找到把硬件 device_id 自动加入请求的实现。`x-oai-attestation` 则是独立 host integration：`core/src/attestation.rs:20-27` 定义可选 provider；`core/src/client.rs:732-744` 没有 provider 时返回 None，不能用 installation UUID 代替或合成它。

## 同一请求的多个表示

`.local/codex-source-0.155.0/codex-rs/core/src/responses_metadata.rs:210-215` 明确 `client_metadata["x-codex-turn-metadata"]` 是完整快照；flat client_metadata 和直接 HTTP/WS headers 是兼容投影。`:305-338` 同步 installation/session/thread/turn/window/root_turn 等字段；`:345-360` 生成直接 header 时有意清除 `tool_namespaces_info`，避免把无界工具清单放入 header。因此字段关系可以核对，整份 header JSON 与 body JSON 不应一律要求相同。

官方 installation ID 的已证明位置是 body 中 `client_metadata["x-codex-installation-id"]` 及 turn metadata 内 `installation_id`，不是必须另外补同名 HTTP header；官方测试 `core/tests/suite/client.rs:166-196` 正是核对这两个位置。HTTP 与 WS 使用同一 metadata 来源，见 `core/src/client.rs:700-729,886`。

## 网关与实验环境的具体边界

当前 `proxy.go:143-165` 放行 x-codex-* 和原 session/thread 头；prepare/policy 使用 RawMessage 保留 client_metadata，未生成或覆盖上述 IDs。`codex_fingerprint.go:45-70` 的 profile 只改 UA/originator/version，移除跨身份的 opaque attestation，不改会话元数据。静态 Linux 6.8.0/macOS 15.0.0 profile 是参考值；它与实际 host UA 的差异属于本轮应修的环境声明范围，不能靠重造 installation/window/root_turn 去“对齐设备”。UA 修复由独立实现任务负责。

现有实验工具 `tools/quality_official_cli_probe.py:671-684` 每个 cell 创建全新 CODEX_HOME，只写 auth.json/config.toml，并在结束时销毁；因此每个 cell 的 installation ID 必重新生成。这是隔离实验环境的确定行为，不能解释为同一物理设备换硬件。工具 `:167-176` 同时清除了 TERM/TERM_PROGRAM 等变量，所以 unknown terminal 与该实验环境一致。未来若专门检验稳定安装身份，需要预登记并复用安装标识；不能悄悄修改旧 cell，亦不能因此声称既往账号差分已经排除了安装标识随机效应。

本次没有发现当前网关把正常 CLI 的上述 ID 改坏的新增证据。可行动修复对象是由网关主动声明的真实 OS/架构/terminal 字段；透传会话字段与唯一 metadata 来源应保持。本结论不把参考 header profile、真实机器环境、安装持久状态、会话状态或 attestation 混成同一层，也不声称影响答案质量的因果关系。
