# Codex CLI 请求身份真实验收（2026-09-17）

使用用户明确提供并授权实测的账号，Linux 与 macOS 两种应用层身份配置均通过完整三轮验收。测试时间为北京时间 20:48:13–20:50:22，官方 Windows CLI `0.154.0`，模型 `gpt-5.6-luna`。

完整脱敏证据：[CODEX-FINGERPRINT-LIVE-2026-09-17.json](CODEX-FINGERPRINT-LIVE-2026-09-17.json)。功能与开关说明：[CODEX-FINGERPRINT-SETTINGS.md](CODEX-FINGERPRINT-SETTINGS.md)。

## 实际结果

| 验证项 | Linux 配置 | macOS 配置 |
| --- | --- | --- |
| 首轮回复精确匹配 | 通过 | 通过 |
| 同一会话续接记忆 | 通过 | 通过 |
| shell 读取一次性文件并返回随机标记 | 通过，1 次工具完成 | 通过，1 次工具完成 |
| 测试标记文件保持不变 | 通过 | 通过 |
| 上游推理请求 | 4 次 HTTP 200 | 4 次 HTTP 200 |
| 网络实际写出身份头匹配 | 4 / 4 | 4 / 4 |
| 网络错误 / 上游失败 / 缺少 usage | 0 / 0 / 0 | 0 / 0 / 0 |
| 输入 tokens | 31,106 | 30,974 |
| 输出 tokens | 292 | 219 |
| 缓存 tokens（输入的一部分） | 19,712 | 5,888 |
| 耗时 | 63.89 秒 | 64.92 秒 |

总计 8 次真实上游推理请求，输入 62,080 tokens、输出 511 tokens，其中缓存输入 25,600 tokens。缓存数量仅为本轮观测值，不用于比较两种配置的性能。

## 验证方法与范围

实际链路为官方 Windows CLI → 当前源码编译的临时本地网关 → 账号对应的官方模型服务。依次打开 Linux/macOS 配置；每组调用完整 `TestCodexCLIIntegration`，验收范围为 `conversation_and_shell`。未采用仅对话入口替代工具验收。

网关审计使用 `httptrace.WroteHeaderField` 与 `WroteHeaders` 核对实际发出的 `User-Agent`、`originator`、`version`，同时确认未发送与改写身份不一致的 `x-oai-attestation`。全部 8 个请求都通过请求输入与实际写出两层检查。

两组使用同一个固定 Go 测试二进制与官方 CLI，SHA-256 保存在 JSON 证据中。执行前后 smoke 脚本 SHA-256 相同，`smoke_script_unchanged=true`；每轮必需断言均存在且为 `true`。

账号经管理 API 导入隔离临时存储，仅临时副本移除刷新令牌，避免测试轮换原账号凭据。测试没有修改原账号文件或生产配置，没有部署。工具在 `read-only`、`never` 与 Windows `unelevated` 沙箱下读取一次性测试文件。报告仅保存允许的检查结果、公开参考身份和汇总计数，不保存令牌、账号、会话 ID、请求/响应原文。

本次真实验收覆盖 HTTP/SSE 及 Linux/macOS 应用层身份。它不构成 iOS 真机、TLS 指纹、真实 WebSocket 或 OAuth 刷新验收；这些范围不能从本报告推导。当前仍使用 Go 网络栈，iOS 取证与回退依据见 [CODEX-IOS-EVIDENCE.md](CODEX-IOS-EVIDENCE.md)。
