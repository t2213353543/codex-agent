# 真实 Codex CLI 网关验收

`tools/codex_cli_smoke.py` 调用已安装的真实 `codex exec --json`，从 `thread.started` 读取会话 ID，再调用 `codex exec resume <ID>`。第二轮提示不包含第一轮的随机标记，必须仅凭会话历史准确返回该标记。脚本不会模拟 CLI 或直接发送替代 CLI 的 HTTP 请求。

实现依据为 2026-09-17 查阅的官方 [Non-interactive mode](https://learn.chatgpt.com/docs/non-interactive-mode)、[Configuration Reference](https://learn.chatgpt.com/docs/config-file/config-reference) 和 [Developer commands](https://learn.chatgpt.com/docs/developer-commands?surface=cli)。本机默认安装为 `codex-cli 0.153.4`，本轮完整接入尝试使用独立安装的官方 `codex-cli 0.154.0`；每次运行的 JSON 记录实际版本。安装版本不同可先运行 `codex exec --help`、`codex exec resume --help` 核对选项。

## 运行

需要 Python 3.10+、已安装的 Codex CLI，以及网关提供的 Responses API 地址、模型 ID 和 API key。API 地址应是 `/responses` 之前的基地址，例如 `http://127.0.0.1:8080/v1`；脚本不自动补 `/v1`，也不接受 URL 内的凭据、查询参数或 fragment。模型必须显式指定，避免测试到意外的默认模型。

在 PowerShell 中，可让已配置的环境变量提供密钥：

```powershell
python tools/codex_cli_smoke.py --base-url http://127.0.0.1:8080/v1 --model YOUR_GATEWAY_MODEL --api-key-env CODEX_GATEWAY_API_KEY --with-tools --output codex-cli-smoke-result.json
```

也可以预设 `CODEX_GATEWAY_URL`、`CODEX_GATEWAY_MODEL`、`CODEX_GATEWAY_API_KEY`，然后运行：

```powershell
python tools/codex_cli_smoke.py --with-tools
```

`--api-key` 支持显式传值，但环境变量更适合避免密钥出现在 shell 历史和启动脚本的进程参数中。脚本不从现有 Codex 登录状态或用户配置中提取凭据。`--codex` 可以指定可执行文件或 npm launcher；提示通过 stdin 传入，API key 只进入 CLI 的子进程环境，不写入临时配置或 CLI 参数。

默认检查两轮文本和历史续接；`--with-tools` 增加第三轮：在临时工作目录生成另一随机标记文件，要求 CLI 实际执行 shell 读取。成功必须同时满足 `command_execution` 完成、退出码为 0、工具输出包含标记、最终回复完全匹配、文件未被修改。仅模型声称“已执行工具”不能通过。

## 隔离与结果

每次运行创建独立临时 `CODEX_HOME` 和工作目录，两轮或三轮共享该临时 home 以保留会话。退出后清理临时配置、日志、会话和标记；不修改真实用户的 Codex 配置、登录状态或工作区。没有使用 `--ephemeral`，因为它会阻止所需的会话持久化。

临时配置使用自定义 Responses provider、`env_key`、HTTP SSE、只读 sandbox、`approval_policy = "never"`，禁用 web search 和 shell profile。子进程只继承启动所需的系统变量及代理/证书变量，并将 loopback 地址加入 `NO_PROXY`；不继承父会话的 Codex 路由变量、MCP 配置或其他 API 凭据。shell 工具的环境采用 `core` 并启用默认密钥排除。系统级组织策略和操作系统 sandbox 仍然适用；sandbox 不能运行会报告失败，不自动放宽权限。

原生 Windows 的隔离 home 还会明确设置 `windows.sandbox = "unelevated"`，启用受限令牌沙箱，同时保持 `read-only` 与 `never`。该配置不创建提权沙箱用户；实现限制以官方 [Configuration Reference](https://learn.chatgpt.com/docs/config-file/config-reference) 和 [Permissions](https://learn.chatgpt.com/docs/permissions) 为准。此前未显式配置 Windows 沙箱时，真实对话可完成，但 shell 工具在执行前被客户端阻止。

启用工具检查时，Windows 会为新建的临时测试根目录授予读取/穿越权限，为其 `workspace` 子目录授予可继承的读取权限，以便受限令牌能读取合成标记。权限只作用于本次临时目录，不授予写权限，不向同级 CLI home 继承，不修改用户工作区或凭据文件。此修复已用官方 `codex sandbox -P :read-only` 单独验证。

每轮默认超时 120 秒，可用 `--timeout` 调整；超时会结束该 CLI 进程树。provider 自动重试关闭，SSE 空闲超时至多 30 秒，以便失败及时暴露。包含工具检查时最多三轮，每轮有独立超时。

stdout 输出 JSON，成功退出码为 `0`，参数/凭据缺失、启动、协议、断言或超时失败为 `1`，中断为 `130`。`--help` 输出普通帮助。`--output` 可保存同一份 JSON 摘要；其父目录需已经存在。

JSON 包含实际 CLI 版本、事件数量、每项断言、耗时、进程退出码和失败诊断；原始 prompts、模型回复、工具输出、会话文件均不导出，会话 ID 只记录 SHA-256。已知 API key、Bearer token 和常见 `sk-` 密钥会脱敏，stderr 只保留末尾 4000 字符。摘要中的网关地址和模型 ID 是运行者主动提供的非密钥元数据。

通过标准是每轮进程成功、出现合法 `thread.started`、`turn.started` 和 `turn.completed`、没有 `error`/`turn.failed`、最终回复准确；续接轮还必须保持原 thread ID。前两轮禁止 shell 调用，防止通过读取会话文件伪装记忆。该检查验证真实 CLI 的文本、会话持久化和可选 shell 往返，不代表图片、长会话压缩或所有工具协议均已验收。

## 本地验证

以下检查不需要真实网关或凭据：

```powershell
python tools/codex_cli_smoke.py --help
python -m unittest discover -s tools -p test_codex_cli_smoke.py
```

无凭据运行必须以非零退出并给出 `missing_api_key`，不能退回真实用户的登录信息。真实网关验收必须另外执行上面的完整命令；单元测试通过不等同于网关通过。

## 验证当前本地网关代码

`codex_cli_integration_test.go` 可在临时数据目录中启动当前 `Proxy`，把现有网关作为上游，再运行上面的真实 CLI 验收。这样无需部署改动或连接生产数据库，即可检验本地代码的转发及计量行为。

在进程环境中提供 `CODEX_CLI_UPSTREAM_URL`（已有网关的 `/v1` 基址）、`CODEX_CLI_UPSTREAM_KEY` 和 `CODEX_CLI_MODEL`。可选 `CODEX_CLI_BIN` 指向独立安装的官方可执行文件，`CODEX_CLI_EVIDENCE_FILE` 指向已经存在目录下的 JSON 报告。密钥由调用者放入环境，不通过命令行传递。

```powershell
go test -run '^TestCodexCLIIntegration$' -count=1 -v -timeout 15m
```

缺少所需环境变量时测试明确跳过。启用后要求三轮 CLI 检查全部通过，至少三个模型请求完成，且每个请求有真实 input/output usage。报告只保存固定字段的布尔断言、状态、统计和 CLI 版本。

也可设置 `CODEX_CLI_ACCOUNT_FILE` 为明确授权的单账号导出文件路径，替代 `CODEX_CLI_UPSTREAM_URL` / `CODEX_CLI_UPSTREAM_KEY`，配合相同的 `CODEX_CLI_MODEL` / `CODEX_CLI_BIN` 运行。该模式通过本地管理 API 执行预览与导入，验证预览不写入、凭据字段保持一致，再让 CLI 经当前网关直连官方 Codex 上游。它只使用临时账号存储，不导入生产库；导入验证完成后移除测试副本的刷新令牌，避免刷新轮换。因此访问令牌必须至少还有 20 分钟有效期。报告的 `imported_account` 标识该模式，文件路径及账号身份不会写入报告。

2026-09-17 最终使用用户新提供的单账号导出文件，官方 0.154.0 与 `gpt-5.6-luna` 的首轮、续接及真实 shell 三项均通过，4 次推理 HTTP 200，证据见 [CODEX-CLI-IMPORTED-ACCOUNT-2026-09-17.json](CODEX-CLI-IMPORTED-ACCOUNT-2026-09-17.json)。较早使用旧网关失效 OAuth 的 HTTP 401 失败记录保留在 [CODEX-CLI-LIVE-2026-09-17.json](CODEX-CLI-LIVE-2026-09-17.json)，不代表新账号结果。生产账号和服务未做修改。

源码侧的 [官方 CLI 协议测试](CODEX-REAL-CLI-TEST.md) 使用真实客户端与模拟上游，覆盖工具、恢复、压缩和 WebSocket 降级；它与此处的真实模型验收分别记录结果。
