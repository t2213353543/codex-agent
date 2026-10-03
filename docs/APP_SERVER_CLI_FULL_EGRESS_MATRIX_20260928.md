# App Server CLI / codex exec 出站差异矩阵（2026-09-28）

## 验收标准

必须同时证明：方法、路径、查询、鉴权/账号 headers、请求 body/数字精度、流式/WS 终态、重试边界和副作用边界；“路由存在”或“普通 Responses 成功”不算完成。客户端拥有工具、历史、turn、compaction；网关只做已捕获的 wire 适配。账号优先级/权重/并发、owner CAS、加密历史归属和 fail-closed helper 协商保持不变。

## 实际观测路径

来源：`.local/appserver-parity/client-profile/20260927T161644.919873Z`，App 与 exec 各自保存 `requests.json`；机器化报告为 `.local/latest-operations/appserver-cli-egress-diff.json`。

| 归一化路径 | App | exec | 当前状态 |
|---|---:|---:|---|
| `/v1/user-auth-credential/whoami` | 10 | 3 | 已实现委托身份绑定 |
| `/backend-api/codex/models` | 2 | 2 | alias → `/v1/models` |
| `/backend-api/codex/responses` | 4 | 4 | AppServerCLI writer/native transport |
| `/backend-api/codex/images/generations` | 1 | 1 | owner-bound auxiliary bridge |
| `/backend-api/codex/alpha/history/v2/list_windows` | 1 | 1 | 原字节 History bridge |
| `/backend-api/codex/alpha/notes/v2/thread_hint` | 1 | 1 | 原字节 Notes bridge |
| `/backend-api/ps/mcp`、connector directory | App only in this fixture | 0/1 | owner-bound MCP/connector bridge；MCP 现在移除已观测的 CLI-only `thread_source`；能力清单标为 `apps_directory=app_server_executor_only`，不伪造 exec 的自动发现 |

以上是已运行 fixture 的事实，不代表条件路径已被自动触发。

机器化对照还记录了每条路径的 App/exec 计数、方法集合、header key 集合、body SHA 和脱敏结构。当前 profile 的唯一方法/header 集合差异是：App 路径额外请求 `/backend-api/connectors/directory/list`；MCP 的结构差异定位为 CLI-only `thread_source`，已在 MCP bridge 中移除；Responses/History 的 body SHA 差异仍包含各自客户端会话/请求 ID，不能仅凭 SHA 直接判定语义差异。

## 固定源码出站类别

| 类别 | 当前处理 | 证据/边界 |
|---|---|---|
| Responses、compact、models、Guardian 基础 HTTP/WS | 已有 | Guardian 仍缺真实条件 capture；普通客户端 body 透明转发 |
| Realtime calls/sideband/live | 已有 | `/backend-api/codex/realtime/*` alias 已修复；WebRTC/媒体权益仍未证明 |
| alpha/search、images、memories | 已有 | 透明 auxiliary bridge；不执行服务端工具循环 |
| History/Notes | 已有 | 10 条固定路径、密文头、owner/CAS 已测 |
| account/settings/usage/tasks/plugins/connectors | owner-bound allowlist | 固定/动态 method、segment 和账号归属已测；副作用只由客户端显式调用 |
| turn cost / thread estimates | 已有 | ledger 保持 unknown ≠ zero；需真实条件 profile 才能证明后端权益 |
| Agent identity JWKS | 未代理 | 官方请求不使用当前账号请求契约；错误套 Bearer 会改变语义 |
| OAuth/device、remote-control、telemetry/Sentry/OTLP | 未代理 | 生命周期/动态目的地址，不是普通 provider Responses 出站 |
| 动态 MCP、插件 download/upload URL | 客户端拥有 | 目的地址来自响应或用户配置，不能由网关静态代理任意 URL |

## 当前 CLI 适配入口

设置 `CODEX_APP_SERVER_CLI=1` 后，服务启动时要求已验证的 native HTTP/WS helper 能力和 identity profile；缺失则在监听前失败。该模式不启动官方可执行文件，不托管 agent，不执行工具。默认生产部署仍不启用该模式。

`/api/settings` 的 inference metadata 现在暴露 `app_server_cli_capabilities`：明确标出 client-owned tools/history/compaction、已桥接的 Responses/History/Search/Images/Realtime/Guardian-transparent，以及仍未验证的生命周期、动态目的地址和真实 provider 语义。该字段是能力声明，不会授予权限或自动发起请求。

## 未完成项

- Guardian 条件请求的官方 App/exec 双 profile capture。
- OAuth/device、agent identity、remote-control、telemetry 等生命周期请求的隔离 capture。
- 动态 MCP/plugin URL 的客户端级出站对照。
- 真实上游能力/回答语义等价；synthetic loopback 不能替代。
