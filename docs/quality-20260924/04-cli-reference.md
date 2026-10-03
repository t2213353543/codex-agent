# Q04：官方 Codex CLI 直连质量对照

日期：2026-09-24。工具：`tools/quality_official_cli_probe.py`；live 请求由主任务在持有私有账号快照的服务器执行，本专项负责工具、源码核对及取回证据的离线复核。

## 交付与运行

该脚本只允许 POSIX 服务器运行，兼容 Python 3.9，使用调用方提供的绝对路径原生 CLI，执行前校验 `codex-cli 0.155.0`，并记录可执行文件 SHA-256。无需 Python 第三方依赖。

```bash
python3 tools/quality_official_cli_probe.py --credential-file /private/q1.json --cli /absolute/codex-0.155.0 --prompt-file /private/candy.txt --model gpt-5.6-sol --effort medium --output-dir /private/results/q1-cli-medium
```

凭据 JSON 格式是 `access_token`、`account_id`、可选 `exp` 与可选真实 `id_token`；不得包含 `refresh_token` 或整个 `auth.json` 的 `tokens` 容器。POSIX 输入文件权限须为 `0600` 或更严格。脚本不显示、修改或刷新原始凭据。

默认 `--websocket default` 使用内置 `openai` provider，保留官方 TLS、WebSocket、routing hint、压缩、HTTP fallback 与模型发现路径。`--websocket true` 是同义选择，**不代表禁止 fallback**。`--websocket false` 才是单独自定义 `official_http_reference` provider 的 HTTP 副对照；其名称、ChatGPT URL、认证方式与版本头对应官方 provider，但 provider ID 和能力开关不同，报告保留这项差异。

`--effort xhigh` 可测高推理档，`--timeout` 最大 240 秒。每次必须使用新输出目录，避免重复实验覆盖。

## 官方源码核对

本节的引用均是当前工作区 `.local/codex-source-0.155.0/codex-rs/` 下实际源码；没有凭其他版本配置文档推断行为。

| 边界 | 源码 | 已核实行为 |
| --- | --- | --- |
| AT 本地身份承载 | `login/src/auth/manager.rs:1739`，`login/src/token_data.rs:176` | 官方 `AuthDotJson::from_external_access_token` 自己将真实 AT 解析成 `IdTokenInfo` 并保存原 JWT；因此 AT 原样作为 `id_token` 的本地 claim 载体，不需要制造签名或身份 claim。提供真实 ID token 时优先用它。 |
| 身份信息用途 | `login/src/token_data.rs:27`、`:200` | `id_token` 经本地 JSON claim 解析提取 email、plan、user、account、FedRAMP 状态；脚本不改写这些字段。 |
| 主动刷新 | `login/src/auth/manager.rs:2947`、`:2974` | AT `exp` 优先，提前 5 分钟刷新。`last_refresh` 当天不能阻止临期 AT 刷新，因此脚本要求有效期大于 timeout + 5 分钟。 |
| 401 刷新 | `login/src/auth/manager.rs:2852`、`:2874`、`:1733` | `auth_mode=chatgpt` 即使空 RT 也可能尝试刷新；脚本明确设置 `CODEX_REFRESH_TOKEN_URL_OVERRIDE=http://127.0.0.1:9/refresh-disabled`，防止请求远端刷新服务。 |
| 内置 provider | `model-provider-info/src/lib.rs:443`、`:475` | 内置 OpenAI 支持 WebSocket、需要 OpenAI 认证，默认 header 带 CLI version。 |
| WebSocket 开关 | `core/src/client.rs:926`、`features/src/lib.rs:1741` | 传输由 provider `supports_websockets` 与会话 fallback 状态决定。旧 `responses_websockets` 和 `responses_websockets_v2` feature 均已 Removed，设 false 不构成强制 HTTP 证据。 |
| provider 覆盖 | `model-provider-info/src/lib.rs:611` | 内置 `openai` 配置通过 `or_insert` 保留，不能靠配置同名 provider 修改 WebSocket 能力；HTTP 副对照必须使用不同 provider ID。 |
| JSONL 元数据 | `exec/src/exec_events.rs:53`、`:65` | `turn.completed` 公开输入、缓存、输出、reasoning output token 数；该 JSONL 不公开实际返回 model 或实际 transport。报告这两项填 null。 |
| 启动警告与真正错误 | `exec/src/event_processor_with_jsonl_output.rs:401`、`:416`、`:437`、`:452`、`:487`；`exec/src/exec_events.rs:306` | ConfigWarning、Warning、DeprecationNotice 映射 `item.completed` + `item.type=error`，正文在 `item.message`；真正 Error 映射顶层 `error`。ModelRerouted 也用 error item，因此仅凭 item type 不能断言警告内容无关。 |
| 工具关闭边界 | `core/src/tools/spec_plan.rs:1082`、`:1142`、`:1255` | shell、plan 等能按 feature/config 禁用；`apply_patch` 等部分工具由模型目录控制，没有通用 CLI 配置保证请求 `tools=[]`。 |

## 测试隔离与判定

工具把题面文件原样送入 stdin，不传期望答案、不添加判分规则。只添加固定 developer 指令“仅解答问题，不调用任何工具，直接回答”，保持 CLI 的模型系统指令。其上下文与裸 Responses 请求不同，所以用于独立官方客户端行为对照，不能声称逐字节相同请求。

每次创建独立临时 `CODEX_HOME` 和空工作目录，使用 `--ephemeral`、只读 sandbox、never approval、关闭 shell/web/apps/MCP apps/多 agent/图像/插件/host skills 等功能。环境只继承 PATH、语言和时区；不继承代理、API key、自定义证书、provider override 或当前任务 routing 变量。`/etc/codex` 系统配置是否存在会作为报告字段留下；主任务需结合它判断服务器环境是否纯净。

禁止工具是配置与 developer 指令共同约束；工具事件或未知 item 一旦出现即终止子进程组，并使样本标为 `invalid_reference`。这是“检测到工具污染则废弃”，不是协议层全部工具绝对不可调用的承诺。已完成的本地动作不能被事后终止撤销，因此本工具只允许数学题与只读沙盒环境。

报告的 `status=completed` 表示一轮 CLI 请求完成、无工具污染或流错误，**并非糖果题答案正确**。错误答案仍是有价值的有效实验结果，不被工具隐藏或重试到正确为止。新版 schema 2 允许仅有启动阶段 warning item 的完整回合标为 `completed_with_startup_warnings`，`reference_clean=false`；顶层 `error`、`turn.failed`、回合内 error item、model rerouted 仍使样本为 `invalid_reference`。这三种状态均保留已生成的最终回答，另列 `generated_answer_available` 与 `turn_completed`。429、401、断流、超时、非 JSON 输出、输出量超限、工具事件均不能直接计作模型降质样本。

保留 `report.json` 和 `events.safe.jsonl`：最终回答、白名单 usage、事件计数、请求 model/effort、CLI/config/prompt hash、认证承载方式、配置隔离标志。schema 2 为错误来源保留 startup/turn/after_turn 阶段、分类与最多 240 字的单行脱敏摘要；清除 token、email、路径、URL，并在多行或 request body/system context/JSON body 前截断。不会保存 raw stdout/stderr、reasoning 文本、thread ID、工具参数、请求正文或原始认证。临时 auth、缓存、日志在结果发布之前删除；已知 token、account 与 email 即使被回答意外回显也会脱敏。

## 验证

2026-09-24 本地离线测试 **23 / 23 通过**；另外已按 Python 3.9 grammar 解析两个脚本，并逐项检查配置键/feature 键都存在于官方 0.155 JSON schema：

```powershell
python -m unittest discover -s tools -p test_quality_official_cli_probe.py -v
```

覆盖真实 AT 原样承载、可选真实 ID token、账号交叉匹配、临期/exp 冲突、RT 拒绝、敏感环境拒绝继承、默认与 HTTP provider 分离、禁止答案注入、JSONL 白名单、未知工具与工具事件废弃、错误/超时/超量不能通过、回答脱敏、mock subprocess 的 stdin 边界、发布前临时目录清理、无 `hashlib.file_digest` 时的流式 SHA-256。未将这些离线测试冒充官方服务 live 结果。

新增回归覆盖相同 error item 在 startup 与 turn 的区别、启动顶层 error 仍无效、ModelRerouted 仍无效、错误摘要的路径/email/token/body 脱敏、答案数字/usage 数值 `429` 不构成限流、HTTP 状态码匹配数字边界、旧安全事件离线复核与报告一致性验证。

主任务首次服务器执行发现 Python 3.9 不存在 `hashlib.file_digest`，当时请求已运行但生成报告失败；这两次运行属于**工具输出失败**，没有可判定的答案证据，不能计作模型错误或成功。临时目录按设计已清理，未保留 raw 响应。现已改成 1 MiB 分块 SHA-256，并把 CLI 摘要计算提前到任何模型请求之前，避免该类本地 preflight 错误发生在请求之后。

## 取回的官方 CLI 实际对照证据

已只读复核 `.local/quality-20260924/official2-q0/` 和 `official2-q1/` 中原始 `report.json`、`events.safe.jsonl`。两个样本都显示 `thread.started → 两个 item.completed/error → turn.started → 最终 agent_message → turn.completed`；CLI returncode 为 0，未超时、未观察到工具、没有回合内 error 事件。

| 账号匿名代号 | 最终生成答案 | input tokens | cached input tokens | reasoning output tokens | CLI 耗时 |
| --- | --- | --- | --- | --- | --- |
| Q0 | 21 | 6859 | 0 | 3070 | 83.564 秒 |
| Q1 | 29 | 6859 | 5888 | 429 | 33.027 秒 |

两者请求 `gpt-5.6-sol / medium`，CLI、题面与配置 SHA-256 完全一致，provider 为官方内置 `openai`，系统配置检测为无，未使用网关。该证据说明 Q1 的错误答案**在独立官方客户端直连中也出现**；它不能证明账号被标记、具体路由集群或 TLS 指纹的因果关系。两账号缓存命中不同，不能把耗时或 reasoning token 差异直接换算成算力差异。

原工具把所有 `item.type=error` 当作失败，故原报告保留 `invalid_reference`；现已修复启动警告分类。旧安全文件未保留两条 startup message 文本，不能反推它们是具体哪一种警告，也不能排除 model rerouted。离线重评因此使用更保守的 `completed_with_unresolved_startup_events`，标记 `reference_clean=false`，保留答案与 usage，**不把它们称作无警告、无干扰的纯净参考**。

Q1 旧 `diagnostic_indicators.rate_limit=true` 已作废：旧实现搜索整段 stdout 中的 `429`，而本样本 `reasoning_output_tokens` 正好是 `429`，足以触发这一指标。实际错误文本和 stderr 已不保留，因此不能用旧指标证明官方 CLI 遭遇 HTTP 429；也不能据此否定主任务其他 HTTP 请求明确返回 429 的独立证据。新版仅扫描真实 error.message 与 stderr，分别输出 startup/turn/after_turn/stderr_unphased，不再扫描答案或 usage。

原文件未覆盖。已新增两个独立结果：

- `.local/quality-20260924/official2-q0/summary_from_prior_safe_evidence.json`
- `.local/quality-20260924/official2-q1/summary_from_prior_safe_evidence.json`

结果包括原 report/events SHA-256、真实事件阶段、原状态、文本不可恢复限制、旧 429 指标失效原因；`new_live_request=false`。复核命令（为防覆盖，只能在未生成 summary 的复制目录上重跑）：

```powershell
python tools/quality_official_cli_reclassify.py .local/quality-20260924/official2-q0 .local/quality-20260924/official2-q1
```
