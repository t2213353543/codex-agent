# 官方 CLI 协议集成测试

`real_cli_test.go` 直接启动未经修改的官方 Codex CLI 二进制，经过本项目的真实 `Proxy`，连接仅监听本地随机端口的受控上游。它验证真实客户端行为；受控上游按预定顺序返回协议事件，不提供真实模型推理，因此此测试不能代替真实模型联调。

测试默认跳过。独立安装 0.154.0（不会更新全局 Codex）：

```powershell
npm install --prefix .local/codex-cli-0.154.0 @openai/codex@0.154.0
$env:CODEX_REAL_CLI_BIN = (Resolve-Path .local/codex-cli-0.154.0/node_modules/@openai/codex-win32-x64/vendor/x86_64-pc-windows-msvc/bin/codex.exe).Path
go test -run '^TestRealCodexCLI$' -v -count=1 .
```

其他系统将 `CODEX_REAL_CLI_BIN` 指向对应官方可执行文件即可。测试打印实际 `codex --version`，不从 `PATH` 自动选择客户端。

覆盖以下往返：

- `codex exec --json` 接收函数调用、实际运行无害 `echo`，返回退出码为 0 的命令结果；上游收到对应 `function_call_output`。
- `codex exec resume --json <thread_id>` 恢复已保存会话；后续上游请求包含原用户消息、助手回复、工具结果和新用户消息，并保留 `thread-id`。
- 开启 `supports_websockets`，检查客户端先尝试 WebSocket，再通过网关的 HTTP 降级继续工具和会话往返。
- 通过受控 usage 值触发真实客户端的自动压缩，检查 `/responses/compact` JSON 响应、压缩后的继续请求和跨进程恢复。此场景关闭 `remote_compaction_v2`，专门验证独立 compact 路由。

每个场景使用临时目录保存 CLI 配置、会话和工作文件。`CODEX_HOME` 只设置在子进程的环境中；不会修改运行测试的进程环境、用户已有配置或认证。上游和网关认证均为测试夹具。子进程不继承当前会话的 OpenAI/Codex 认证、代理配置或 app-server 环境。为让无害 `echo` 在各平台稳定运行，临时配置禁用审批和沙箱；受控上游只生成这一条固定命令。

夹具参考官方 0.154.0 源码的 `codex-rs/core/tests/common/responses.rs` 和 `codex-rs/core/tests/suite/compact_remote.rs` 中的 SSE、函数调用和 compact 响应构造。普通单元测试仍无需安装 CLI。

## 可选真实模型联调

`codex_cli_integration_test.go` 复用 `tools/codex_cli_smoke.py`，链路为：官方 CLI → 本次源码构建的本地 `Proxy` → 显式选择的已有上游网关 → 真实模型。该测试会发送真实模型请求，默认跳过，需要显式配置：

```powershell
$env:CODEX_CLI_BIN = (Resolve-Path .local/codex-cli-0.154.0/node_modules/@openai/codex-win32-x64/vendor/x86_64-pc-windows-msvc/bin/codex.exe).Path
$env:CODEX_CLI_UPSTREAM_URL = 'https://your-gateway.example/v1'
$env:CODEX_CLI_UPSTREAM_KEY = (Get-Content -LiteralPath 'C:\private\gateway-key.txt' -Raw).Trim()
$env:CODEX_CLI_MODEL = 'your-model-id'
$env:CODEX_CLI_EVIDENCE_FILE = '.local/codex-cli-live-report.json'
go test -run '^TestCodexCLIIntegration$' -v -count=1 -timeout 15m .
```

凭据保存在指定的环境变量中，不作为 CLI 参数传递或写入日志。`CODEX_CLI_EVIDENCE_FILE` 为可选结果文件，仅保留允许的断言、计数和用量，不记录正文、凭据或会话 ID。测试创建隔离的临时账号存储，不更新生产账号，不刷新 OAuth，不部署网关。真实模型必须完成随机标记记忆、同会话恢复和读取临时文件的实际工具调用，并产生真实用量，才判定通过。认证失效、超时或任一断言失败均作为失败报告。
