# Codex CLI 请求身份设置

> 历史功能说明。当前 PowerPoint 推理入口固定使用 BPS 请求配置，界面已移除旧 CLI 指纹开关；见[当前网关设置](GATEWAY-SETTINGS.md)。下文保留原设计记录。

系统设置新增“Codex CLI 请求身份”，可打开“统一 Codex CLI 请求身份”并选择 Linux 或 macOS。默认关闭；预选配置默认为 Linux。保存立即生效，PostgreSQL 多进程部署沿用最多两秒的设置缓存期限。

本次用户已确认目标为 ChatGPT iOS 内的 Codex，并允许在无法核实完整 iOS 指纹时参考 macOS/Linux。已找到官方 `codex_chatgpt_ios_remote` 名称，但它是远程控制入口；[官方说明](https://learn.chatgpt.com/docs/remote)指出实际工作由所连接的电脑执行。完整取证见 [CODEX-IOS-EVIDENCE.md](CODEX-IOS-EVIDENCE.md)。因此本次提供明确标为源码参考配置的 Linux/macOS CLI 身份，没有新增无法验证的“完整 iOS”选项。

## 生效范围

- HTTP Responses：SSE 和转为 JSON 的请求。
- `/responses/compact`、网关已有的 guardian 路由。
- `/models` 的请求头及 `client_version` 查询参数。
- WebSocket 上游握手。已建立的连接在开关或已启用的配置变更后，下一次 `response.create` 返回 409，并要求客户端重连；已开始的响应继续完成。
- 自动与手动 OAuth 刷新；保持原账号的 OAuth `client_id` 和令牌，不改变认证身份。

统一字段为 `User-Agent`、`originator`、`version`。启用时，即使调用方是 Windows CLI 或其他原生 Codex 客户端，也使用所选配置。原请求的 `x-oai-attestation` 在身份被改写后不再转发，因为不能继续代表改写后的请求身份。保留会话 ID、线程 ID、turn state、有效 beta 协议能力及追踪字段，提示词和缓存策略继续由其各自设置控制。

## 参考配置与边界

所有配置的 CLI 版本统一引用后端 `clientVersion`，当前为 `0.154.0`。

| 配置 | User-Agent |
| --- | --- |
| Linux | `codex_cli_rs/0.154.0 (Linux 6.8.0; x86_64) unknown` |
| macOS | `codex_cli_rs/0.154.0 (Mac OS 15.0.0; arm64) unknown` |

以上为依据官方 `login/src/auth/default_client.rs` 和终端检测源码构造的固定参考配置。操作系统版本用于定义配置，**不是实际网关宿主信息或实机抓包结果**。`unknown` 为官方终端检测无可用终端信息时的标识。`originator` 为 `codex_cli_rs`，`version` 与 CLI 版本一致。

该功能统一应用层请求身份。网关仍使用 Go HTTP/TLS 网络栈；没有声称复现 iOS URLSession、Rust 客户端的 TLS ClientHello、HTTP/2 SETTINGS 或头顺序，也没有承诺任何服务端访问权限、风控或限流结果。真实模型推理验证与这些本地身份/协议回归分开记录。

## 管理 API

`GET /api/settings` 新增：

```json
{
  "codex_fingerprint_enabled": false,
  "codex_fingerprint_profile": "linux",
  "codex_fingerprint_profiles": [
    { "id": "linux", "label": "Linux CLI（源码参考配置）", "user_agent": "...", "originator": "codex_cli_rs", "version": "0.154.0" },
    { "id": "macos", "label": "macOS CLI（源码参考配置）", "user_agent": "...", "originator": "codex_cli_rs", "version": "0.154.0" }
  ]
}
```

`POST /api/settings` 支持部分更新及原有 `settings_version` 乐观锁。例如将 GET 得到的版本号填入：

```json
{
  "settings_version": 1,
  "codex_fingerprint_enabled": true,
  "codex_fingerprint_profile": "linux"
}
```

旧数据缺少字段时采用默认值；关闭开关会保留预选配置。未知配置、错误类型、显式空配置返回 400，版本冲突返回 409，均不覆盖已保存设置。已保存的指纹配置列表由后端提供给前端，页面不硬编码另一份 CLI 版本。

## 使用授权账号进行真实验收

复用 `TestCodexCLIIntegration`：在原有 `CODEX_CLI_ACCOUNT_FILE`、`CODEX_CLI_MODEL` 和可选 `CODEX_CLI_BIN` 环境变量之外，设置 `CODEX_CLI_FINGERPRINT_PROFILE=linux` 或 `macos`。测试使用临时账号/设置存储，经实际管理 API 预览与导入，随后移除临时副本中的刷新令牌，防止测试轮换原账号凭据。

`CODEX_CLI_EVIDENCE_FILE` 输出三轮真实 CLI 会话和实际用量；`CODEX_CLI_FINGERPRINT_EVIDENCE_FILE` 输出独立的应用层身份审计。该审计检查实际出站请求的 User-Agent、originator、version，不记录 token、账号、请求/响应原文或客户端会话 ID。未启用 profile 时原有 baseline 行为保持不变。

```powershell
go test -run '^TestCodexCLIIntegration$' -count=1 -v -timeout 15m
```

两种配置应按顺序运行，避免同账号并发产生难以归因的结果。当前 CLI smoke 明确使用 HTTP/SSE；该报告不等价于真实 WebSocket、OAuth 刷新或 iOS 真机验收。模型必须来自账号当前可用目录；导出文件的模型映射不能证明账号具有对应模型权限。

如果本机 shell 沙箱存在独立故障，可运行明确分开的 `TestCodexFingerprintCLIConversation`，只验证官方 CLI 的初始会话与续接记忆。报告 `acceptance_scope=conversation_only`，要求两轮成功、至少两次真实推理、每次都有实际 usage，以及出站身份审计通过。原 `TestCodexCLIIntegration` 始终包含 shell 读取断言，报告 `acceptance_scope=conversation_and_shell`，其通过标准没有降低。

2026-09-17 已使用用户授权账号完成 Linux/macOS 两组完整真实 CLI 验收：首轮、续接与只读 shell 工具均通过；共 8 次上游 HTTP 200，实际写出身份头全部匹配。官方 Windows CLI 为 `0.154.0`，模型为 `gpt-5.6-luna`。测试仅修改临时存储、禁用刷新，没有部署。详见 [真实验收说明](CODEX-FINGERPRINT-LIVE-2026-09-17.md)与[脱敏 JSON 证据](CODEX-FINGERPRINT-LIVE-2026-09-17.json)。
