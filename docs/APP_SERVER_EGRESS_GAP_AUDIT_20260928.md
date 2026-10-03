# App Server / Codex CLI 出站差异审计（2026-09-28）

> 只读审计报告；本次没有修改生产配置、账号或部署。依据当前工作树源码、`docs/APP_SERVER_EGRESS_INVENTORY.md`、`docs/APP_SERVER_REMAINING_EGRESS.md`、固定 `codex-rs 0.155.0` 清单，以及 `.local/appserver-parity/client-profile/20260927T125942.320919Z/verified-profile.json`。

本轮后续实现说明：`CODEX_APP_SERVER_CLI=1` 的可选 client-owned adapter 已恢复到启动路径；它只要求已校验的 native HTTP/WS helper 与 identity profile，不启动官方可执行文件或 hosted agent。默认生产模式仍不切换。

## 验收口径

目标不是“路由字符串存在”，而是：官方客户端实际会发出的条件请求，经过相同方法、路径、查询、headers/body、流式语义、账号归属和副作用边界；未触发或动态目的地址必须明确标为未验证，不能伪称已覆盖。

## 当前已覆盖（源码已存在）

| 出站面 | 当前桥接证据 |
| --- | --- |
| `/responses`、`/responses/compact`、`/models`，HTTP/SSE/WS | `gateway.go:633-674`；`appServerCLI.RoundTrip` 与 native transport |
| `/guardian`、`/guardian-classifier` 的基础 HTTP/WS 路由 | `gateway.go:633-650`；但没有 App Server writer/真实 guardian capture，见未完成项 |
| `/alpha/search`、`/images/generations`、`/images/edits`、`/memories/trace_summarize` | `gateway_appserver_auxiliary.go:15-20`；`appServerModelAlias` 可将 `/backend-api/codex/...` 映射到 `/v1/...` |
| History/Notes 的 10 个 `/alpha/{history,notes}/v2/*` 路由 | `gateway_appserver_history.go:25-35`；账号 owner 绑定和加密参数头保留 |
| `/backend-api/ps/mcp`、账号设置/配置/额度/任务、插件目录、连接器目录 | `gateway_appserver_identity_bridge.go`、`gateway_appserver_backend.go`；固定/动态路径均有 owner-bound allowlist |
| 费用查询 `/v1/analytics/codex/turn-costs`、`/backend-api/wham/usage/thread-estimates/query` | `gateway.go:582-584` 及 turn-cost ledger |
| Realtime 实现本身（`/v1/realtime/calls`、`/v1/realtime`、`/v1/live`） | `gateway_appserver_realtime.go:22-29`；有 WebRTC/WS owner 绑定测试 |

## 必须继续补齐或实测的差异

### P0：Realtime backend base path 当前无法从客户端 profile 到达（已修复于当前工作树）

固定客户端捕获记录了：`/backend-api/codex/realtime/calls?intent=...&architecture=...`（见 `docs/APP_SERVER_REALTIME_CAPTURE_20260927.md:10-13`）。

**已实现：**`appServerModelAlias` 现在严格把 `backend-api/codex/realtime/*` 映射到 `/v1/realtime/*`，复用 `appServerRealtimePath` 的 call-id 校验；新增 allowlist 测试，覆盖 alias 和路径逃逸。仍需独立 WebRTC/multipart capture 证明完整客户端条件行为。

### P0：Guardian 仅有通用路由，未证明 App Server 出站形状

`gateway.go:633-650` 接受 `/guardian` 和 `/guardian-classifier`；但 `gateway.go:960-962` 只有 `path == "/responses"` 才进入 `AppServerCLI.RoundTrip`。Guardian 请求走旧透明转发，尚未有官方 0.155.0 请求 writer、独立 stream/JSON body、错误/usage capture 的对照证据。清单也将两条标为“未触发”。

固定源码显示 Guardian 与普通请求共用 `ApiResponsesClient`，差异是 endpoint、`parent_response_id` metadata、Guardian 清除 `service_tier`，且不写普通 Responses routing hint。当前透明 HTTP/WS 测试已保留这些 body/header 边界；仍缺真实官方 Guardian 条件触发 capture，因此不把它升级为完整 App Server 语义证明。

### P1：插件发布/上传仍有明确 allowlist 缺口（固定路径已补，动态上传仍不代理）

当前动态 allowlist（`gateway_appserver_identity_bridge.go`）覆盖插件详情、install/uninstall、shares、workspace publish/delete、upload-url、credits nudge 和任务/usage 查询；这些固定路径已通过矩阵测试。动态 `upload_url` 仍不被服务端任意代理。仍需验证的外部动作包括：

* `PUT <动态 upload_url>`（目的地址来自上游响应，不能静态伪造；应在客户端下载/上传器侧验证）；
* 上述 publish/delete/upload-url 的真实客户端 method/body/响应语义。

这不是普通模型请求，必须保持用户明确操作才转发，不能启动时自动补发。实现时分别约束 method、路径 segment、Content-Type 和 owner；动态 upload URL 不应变成网关任意 URL 代理。

### P1：账户/云任务副作用面仍未逐路实测

源码已 allowlist `tasks`、`thread_usage/query`、reset-credit consume 和 credits nudge（`gateway_appserver_backend.go`），但尚无当前 profile 的真实 synthetic capture 证明 method/body/返回字段与官方客户端一致。尤其：

* `POST /backend-api/wham/tasks` 创建云任务；
* `GET /backend-api/wham/tasks/{id}` 与 sibling turns；
* `POST /backend-api/wham/usage/thread_usage/query`；
* `POST /backend-api/wham/rate-limit-reset-credits/consume`；
* `POST /backend-api/wham/accounts/send_add_credits_nudge_email` 仅在客户端明确调用时转发。

前四项应做 synthetic fixture 后再称“已适配”；最后一项只有用户明确触发时才应加入 owner-bound POST，不能由网关自动调用。

### P1：Agent identity、OAuth/device 登录及 remote-control 未适配为普通网关请求

清单中的 `/backend-api/wham/agent-identities/jwks`、`POST /api/accounts/v1/agent/{runtime_id}/task/register`、workload-identity token exchange、OAuth/device-code、remote-control enroll/pair/refresh 等仍不是当前 `gatewayAppServerBackend` 的路径。它们属于认证/设备生命周期，不是普通模型转发；要么实现明确 capability adapter，要么保持客户端/官方登录进程直接负责并在报告中标为 out-of-scope，不能用 Responses 200 代替。

### P1：动态插件下载和任意 MCP/工具目的地址不可能由静态路径清单覆盖

官方插件响应可返回 download URL，MCP/connector 工具可包含任意用户配置目的地址。当前 `/backend-api/ps/mcp` 只桥接官方 MCP 服务入口；它没有也不应把任意目标 URL 变成服务端出站白名单。验收应记录“客户端执行权/目的地址动态”，不能把目录接口成功等同于工具执行成功。

### P2：Realtime 独立 `/v1/realtime` / `/v1/live` WebSocket 的认证边界

`gateway_appserver_realtime.go:160` 对 sideband/call-id 有 owner 绑定，但文档已说明不带 WebRTC call 的独立 realtime/live WS 不属于 ChatGPT 账号链路，合成 API-key 不足以证明成功。需要真实 provider capability 或明确返回“先创建 call”，不能自动选择另一个账号或伪造权益。

### P2：Telemetry、Sentry、OTLP 与 GitHub/插件下载不应强行“补齐”

源码清单中的 `ab.chatgpt.com/otlp/v1/metrics`、Sentry DSN、GitHub marketplace、动态 codeload、MCP OAuth 等由官方客户端/用户工具分别控制，可能完全不经过 provider `base_url`。当前网关未捕获它们是预期边界；如要“所有出站”验收，必须用隔离网络的客户端级 capture 给每个目的地址单独编号，而不是在 Go 网关里复制一套无条件请求。

## 当前 profile 证据的限制

`.local/appserver-parity/client-profile/20260927T125942.320919Z/verified-profile.json` 的 App/exec 两路仅实测：whoami、models、responses、images/generations、history、notes/thread_hint、MCP、connectors/list、wham/settings/user；全为 synthetic fixture，外部 CONNECT=0。它证明客户端认证/工具交接，不证明 guardian、realtime、plugins 写操作、云任务、额度消费、telemetry 或真实 provider 权益。

## 推荐执行顺序

1. 已修 realtime backend alias；下一步用 gateway-level route + WebRTC fixture 做完整 method/query/Location/sideband capture。
2. 采集 guardian/classifier 最小双 profile capture，确定透明还是专用 writer。
3. 对插件 publish/upload/delete 已有严格 method/segment allowlist，下一步补 synthetic side-effect tests。
4. 对 credits nudge、agent identity 等条件接口继续维护 capability 清单；只对用户显式动作转发。
5. 对条件/动态 URL（OAuth、GitHub、MCP、telemetry、Sentry）维持 out-of-scope 标记，分别做客户端隔离 capture，不把“当前未触发”改写成“已适配”。

## 结论

当前实现已覆盖主要 Responses/History/Notes/Auxiliary/Backend/MCP/Cost 面，并修复了 profile 使用的 `/backend-api/codex/realtime/calls` alias；**尚不能证明“抓出 App Server 所有出站请求并把 Codex CLI 更新为完全等价 App Server CLI”**。剩余硬缺口是 Guardian 条件 capture、动态/生命周期出站、真实 provider 能力与质量等价，以及插件动态上传目的地址。生产部署前应以本报告的 P0/P1 验收证据为硬门槛。
