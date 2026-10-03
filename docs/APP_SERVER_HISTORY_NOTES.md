# History / Notes 官方协议验证

2026-09-27 已完成固定端点的官方离线运行时捕获，以及捕获请求经过轻量 Go 中转的真实 HTTP 转发测试。本文不声称真实账号已具备 History / Notes 后端权限，也没有对真实账号写入笔记。

## 官方实现与实际请求

核对官方 `codex-rs/ext/history-notes/src/{tools,backend,extension}.rs`，运行固定 SHA-256 的官方 Codex 0.155.0 App Server：

`660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`

捕获目录为 `.local/appserver-parity/history/20260927T125631.869676Z`，服务端对应 `/opt/codex-agent-integration/appserver-parity/history/20260927T125631.869676Z`。退出码 0、stderr 0 字节，官方 `turn/completed` 状态为 `completed`，10 个固定路径全部观察到。网络使用新的 Linux network namespace，只有 loopback 接口和路由；账号令牌、工具参数、后端回复均为合成夹具，未加载线上凭据。

捕获使用官方自带 gpt-5.5 模型元数据，使原生 History / Notes 工具直接暴露；所有模型回复由 loopback 夹具产生，没有调用真实模型。这是请求写入器与工具执行协议验证，不是回答质量评测。

| 路径（均为 POST） | 工具必要参数 | 加密参数头 |
| --- | --- | --- |
| `/alpha/history/v2/list_windows` | 无 | 不携带 |
| `/alpha/history/v2/list_items` | 无 | 不携带 |
| `/alpha/history/v2/read_item` | `item_id`, `window_id` | 不携带 |
| `/alpha/history/v2/search_contents` | `query` | `true` |
| `/alpha/notes/v2/list_files_by_prefix` | 无 | 不携带 |
| `/alpha/notes/v2/read_file` | `path` | 不携带 |
| `/alpha/notes/v2/search_contents` | `query` | `true` |
| `/alpha/notes/v2/append_to_file` | `text`, `path` | `true` |
| `/alpha/notes/v2/write_file` | `text`, `path` | `true` |
| `/alpha/notes/v2/thread_hint` | 非模型可见工具；上下文贡献器调用 | 不携带 |

加密参数头为 `x-openai-encrypted-tool-arguments`。所有请求携带 `x-openai-tool-output-truncation-policy`：本次捕获的 `thread_hint` 为 `{"mode":"bytes","limit":4000}`，其他工具为 `{"mode":"tokens","limit":10000}`。工具的截断策略来自原调用，不在中转硬编码成这两个数值。

所有正文均有官方客户端注入的 `context.session_id` 与 `context.current_agent_name`。搜索 `query`、写入 `text` 的 schema 带 `encrypted:true`。完整 9 个模型工具的实际 schema、10 个原始正文及正文 SHA-256 保存在 `internal/server/testdata/appserver_history_official_oracle.json`。`thread_hint` 没有作为第 10 个模型工具暴露。

官方后端 writer 的源码 SHA-256 为 `46b5cfc089e65c36a3a30cf4760697a0e30373a4b6a29b19379e03af7bb64629`；实际捕获脚本 SHA-256 为 `e8dca0bd06cb6c06f80d6f719a598ccaad743c7cf5b77bb143e47a94ef04a835`。后续给脚本增加的导出命令不冒充原捕获版本，oracle 保留原版本归属。

## 中转行为

`gateway_appserver_history.go` 精确匹配这 10 条路径及其 `/v1` 前缀版本；拒绝其它操作、尾部路径、query 与编码路径变体。正文作为有效 JSON 原字节发送，不改写 `context`、窗口/条目 ID、虚拟笔记路径、密文参数或未知字段；两个控制头按原值透传，不凭空补写。

账号从 `context.session_id` 的现有绑定确定；若同时提供显式 session header，两者必须一致。`current_agent_name` 必须由客户端提供，不推测为 `/root`。首次绑定复用现有优先级、权重与账号准入；已有绑定不因粘性会话设置关闭、优先级变化、账号忙碌或失效而改派其它账号。

History / Notes 使用同账号恢复：只有明确的 HTTP 401 可尝试同 owner 的 token 刷新后重试一次。网络失败、写入后读断、已接受的响应或其它错误均不触发跨账号重放。后台 session lease、HTTP 发出前、native helper 发出前、恢复重试前和成功内容交付前检查 owner；并发改变绑定时不泄露另一归属下的结果。

入站客户端认证、Cookie、Proxy-Authorization、actor credential 不转给上游；上游使用服务端已选定账号的凭据。输出 JSON（包含 `encrypted_output` 和图像附件）原文返回。没有标准 usage 就不构造 token/model 成功用量。

这是客户端已经构造好请求时的固定协议转发。工具暴露、服务端权限、令牌是否能访问此功能，仍由官方客户端和后端决定；中转不持有或制造笔记内容、历史项或虚拟文件状态。

## 已执行测试

```powershell
go test ./internal/server -run '^(TestAppServerHistory|TestAppServerAuxiliary|TestGatewayContentSessionNilRequest)' -count=1
```

结果：`ok codex-agent/internal/server 0.257s`。包括原字节与控制头、20 个路径别名调用、错误路径/方法/冲突上下文、关闭粘性后的强归属、账号禁用、不确定写入不重放、同 owner 401 刷新、并发 429、并发 owner 改变，以及 10 条真实官方捕获请求穿过两侧 TCP HTTP 中转。

新捕获的重跑命令：

```powershell
python -B tools/appserver_parity_history_loopback.py --remote
```

运行只使用隔离 loopback 和合成凭据；成功后在新目录保存请求、RPC 事件、summary 和 writer oracle。协议测试不等于生产部署或真实后端权限测试。
