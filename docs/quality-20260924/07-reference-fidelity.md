# Q07：官方参考 CLI 的安装、警告、模型目录与失败边界

日期：2026-09-24。本专项改动 `tools/quality_official_cli_probe.py`、新增 `tools/prepare_quality_cli_bundle.py` 及对应离线测试；没有读取真实账号、发送模型请求或操作远端。服务器安装和 live 请求由主任务执行。

结论：此前缺失 `code-mode-host` 是把官方 CLI 只复制为单个文件造成的参考环境问题，不能解释为账号标记或低算力集群。固定版本的模型目录还能要求 `code_mode_only`，所以 `features.code_mode=false` 不能消除 host 依赖。已采用完整、固定 SHA-512 的官方平台包，保留模型目录、内置 OpenAI provider、认证身份、reasoning effort 与原题面。没有填入期望答案，也没有按回答内容重试、筛选或停止。

## 1. code_mode 与 host 是不同边界

以下源码路径均相对于 `.local/codex-source-0.155.0/codex-rs/`，已读取实际文件。

| 边界 | 固定源码证据 | 结论与处理 |
| --- | --- | --- |
| `code_mode` 与 host feature | `features/src/lib.rs:1015`、`:1027` | `code_mode` 默认 false，`code_mode_host` 独立且 stable 默认 true。只关闭前者不会关闭 host provider。 |
| 模型目录优先级 | `core/src/tools/mod.rs:68` | `requested_tool_mode` 先读取 `model_info.tool_mode`，只有 None 才检查 feature。bundled `models-manager/models.json` 已包含 `code_mode_only` 的模型；不能靠 feature=false 覆盖它。 |
| host 可用性 | `core/src/thread_manager.rs:488`、`code-mode/src/remote_session.rs:63`、`:93` | 启用 host 选择 ProcessOwned provider；可用性检查真实 host 文件。关闭 host 返回 unavailable，不能解决目录要求 code mode 时的警告。 |
| 警告触发 | `core/src/session/turn_context.rs:1145`、`core/src/tools/code_mode/mod.rs:100` | 模型请求 code mode 而 host unavailable 才生成相应 Warning。不是糖果题内容触发，也不能用 warning 文本反推服务端算力。 |
| 官方定位规则 | `install-context/src/lib.rs:176`、`:185`、`:245`、`:271` | 先找 package `codex-resources/codex-code-mode-host`，再找 package `bin/codex-code-mode-host`。package 依赖 `codex-package.json` 与原目录布局。单独复制 `codex` 丢失此依赖。 |

probe 新增 `validate_cli_bundle`：请求前检查完整 `bin/codex` 布局、0.155.0 manifest、entrypoint、真实可执行 host、资源优先顺序与目录边界；记录 host 和 manifest SHA-256。不会在 PATH 中拾取其他版本 host，也不会制造一个假的 host 文件消除警告。

## 2. 固定官方包安装工具

artifact 来自已有 `.local/native-transport-build/official-cli-0.155.0-source.json`，与主任务提供的 pin 一致：

- URL：`https://registry.npmjs.org/@openai/codex/-/codex-0.155.0-linux-x64.tgz`
- SHA-512 Base64：`CiXkVTy4ERdFSXXP8n98/iOyVUCOYFPQ8cuwgeOEEbLhe1LnlQbgH/H8ZeuxO9WXKIZLJg0x626UgR9axY2zSA==`
- 固定 target：`x86_64-unknown-linux-musl`；CLI SHA-256：`660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`。

新安装器仅访问固定 npm artifact URL，拒绝重定向；完整下载后验证固定 SHA-512，再检查 tar 索引。拒绝绝对路径、`..`、反斜线、重复成员、符号/硬链接、设备/FIFO 和体积超限。只复制目标 package 的普通文件和目录，保留可执行位、manifest、host 与其他配套资源；只发布到一个不存在的新目录。不会覆盖已有安装，不触碰认证文件，不调用模型接口。

服务器运行：

```bash
python3 tools/prepare_quality_cli_bundle.py --output-dir /private/quality/official-bundle
```

然后把 probe 的 `--cli` 指向 `/private/quality/official-bundle/bin/codex`。可用 `--archive /private/codex-0.155.0-linux-x64.tgz` 重用原包，但仍强制相同 SHA-512。

主任务回报已在服务器成功验证 pin 并解包 **44 个文件**；CLI SHA 与上述一致，host 与 package manifest 摘要已由安装器记录。本专项没有把此回报当作新的模型请求结果。

## 3. 只抑制配置提示，不掩盖模型或传输错误

加入顶层 `suppress_unstable_features_warning=true`，保留 `skip_host_skill_discovery=true` 来隔离宿主技能。`features/src/lib.rs:1789` 明确此选项仅使 under-development feature warning 构造函数提前返回，不改变生效 feature、provider、请求 reasoning 或模型身份。官方配置参考亦提供对应配置入口：[OpenAI Configuration Reference](https://learn.chatgpt.com/docs/config-file/config-reference)。实现语义仍以固定源码为准。

没有静默删除 JSONL 中的 warning/error。未知启动警告继续标为 `completed_with_startup_warnings`，`reference_clean=false`；顶层 error、回合内 error 和 model reroute 仍是 `invalid_reference`。

## 4. 五次重连不是“429 连重试五次”

已读旧 `.local/quality-20260924/official3-q1/events.safe.jsonl`：`Reconnecting... 2/5` 至 `5/5` 的真实原因是 **WS 403 Forbidden**，之后 fallback 到 HTTPS 才得到最终 **429**。因此不能把这几种状态合并成同一类限流。

- `model-provider-info/src/lib.rs:30` 默认 stream retries=5；`:374` 的 API request policy `retry_429=false`。
- `protocol/src/error.rs:372`：UnexpectedStatus、stream RateLimitExceeded 可重试；HTTP RetryLimit/UsageLimitReached 不可重试。
- `core/src/session/turn.rs:1614`：UsageLimitReached 直接退出。
- `core/src/responses_retry.rs:101`：stream retries 耗尽后可切换传输；`:122` 在 release 中隐藏第一次 WS retry 通知。
- `model-provider-info/src/lib.rs:643`：配置 merge 对内置 `openai` 使用 `or_insert`。向 `model_providers.openai` 写 retries=0 并不会覆盖内置 provider，不能声称生效。

默认参考继续使用内置 `openai`。probe 现在看到已使样本无效的第一个可见顶层 error / 回合错误 / reroute 就终止进程组，避免继续完成 5 次重连。这是外层实验停止策略，报告单列 `retry_policy`；**不宣称精确零内部重试**，第一次 release WS retry 可能未公开。

HTTP 副对照使用独立 `official_http_reference`，显式 `request_max_retries=0`、`stream_max_retries=0`，配置可实际生效；仍单列 provider ID 与 transport 差异。403、reconnecting、transport fallback 与429已独立分类。任何答案字符串，包括错误答案和数字 429，都不会触发 fail-fast。

## 5. 模型发现、缓存与重路由透明度

每次 probe 都使用新的临时 CODEX_HOME，没有注入或跨账号复用 catalog。`models-manager/src/manager.rs:435` 按 cache 策略决定发现；`:483` 再验证 endpoint identity；`:511` 的 Codex backend 认证路径允许发现；`:525` 对具有可见模型的 ChatGPT 目录使用 authoritative remote catalog。

`model-provider/src/models_endpoint.rs:90` 从当前 provider/auth mode 建构 endpoint 与认证；`model-provider/src/models_identity.rs:14` 的缓存 identity 包括 endpoint、认证模式、account/user/email/plan/FedRAMP 等摘要。缓存不是可以随意从“正常账号”复制给“异常账号”的中性文件；本次不做这种替换。

schema 3 在临时目录删除前只读取 CLI 自己写出的 cache，记录：存在性、parse 有效性、client version 是否匹配、identity 是否存在、请求 model 是否存在/重复及其 tool mode。不保留身份摘要、模型系统指令或原始 catalog。缓存缺失只记未知，不能据此声称发现必然失败或请求 model 已改变。

`exec/src/event_processor_with_jsonl_output.rs:487` 把 ModelRerouted 通知转成 `model rerouted: from -> to (reason)` error item。probe 现在保留安全的结构化 from/to/reason；stderr 中出现 reroute 也不能获得 clean 状态。`model_returned` 和 `transport_observed` 仍保持 null：JSONL 不公开真实 response model/握手协议，不能从“未看到 reroute”推导服务端集群或实际完整模型身份。

## 6. 工具隔离的真实边界

保留禁用可配置工具、只读沙盒、仅解题 developer 指令，以及观察到工具/未知 item 即终止且废弃的策略。`config/src/config_toml.rs:624` 的 ToolsToml 没有任意工具通用总开关；`core/src/tools/spec_plan.rs:1255` 的 apply_patch、`:1210` 附近的 clock 可由模型 metadata 参与决定；模型 code_mode_only 还可使 exec/wait 注册。因此不存在本次已经证明的 `tools=[]`，schema 3 明确 `tool_surface_zero_verified=false`。

另外 `exec/src/event_processor_with_jsonl_output.rs:311` 的兜底 `_ => None` 表示并非每一种内部 ThreadItem 都映射到 JSONL；“未观察到工具事件”不是全量工具调用审计。此限制不会通过更改题目或 catalog 来掩盖。参考只用于纯数学 stdin，原始 reasoning 不保存；不把模型答案是否等于指定值当作保留或拒绝条件。

## 7. 本地预检、凭据与可复验结果

凭据读取改为同一 fd 的 no-follow + nonblock + fstat + bounded read：拒绝非普通文件、POSIX 非当前 owner/非私有权限、重复 JSON 字段、RT/auth 容器和未知字段。临期 AT、account 不匹配仍在请求前拒绝；不更改 AT/ID token claim。

输出目录需当前 owner 的私有目录，拒绝 symlink、已有输出及悬空 symlink；服务器存在 `/etc/codex/config.toml`、`requirements.toml` 或 `managed_config.toml` 时预检失败，避免隔离 home 仍混入系统配置。日志不回显本地异常中的凭据或路径。

本机测试：probe **32 项，29 通过 / 3 POSIX 限定跳过**；installer **6 / 6 通过**。4 个 Python 文件按 Python 3.9 grammar 解析通过，2 个工具 py_compile 通过。本机 Docker daemon 不在线且无普通 Linux WSL，未谎称执行过 POSIX 集成测试。新增 POSIX 项覆盖 FIFO/symlink/public credential、private output 和真实子进程收到可见403重试后在第二次动作之前停止，可由主任务在服务器无凭据运行。

```bash
python3 -m unittest discover -s tools -p 'test_*quality*cli*.py' -v
```

这些测试与安装验证只证明参考工具边界，不能证明账号质量已经恢复，更不能证明“低算力集群”假设。完整目标仍需主任务使用相同原题、独立官方原生参考与网关路径进行有控制的 live 对照。
