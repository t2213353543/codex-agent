# 当前非计费目标完成审查

最新用户范围变更：已明确允许仅被动日志采集，主动观测由用户自行解决。本轮真实实现图像Token、已有客户端关联ID、授权/响应阶段和独立准入拒绝，旧“等待被动采集范围确认”不再是当前阻断。见 [被动观测验收](MANAGEMENT-PASSIVE-OBSERVATION-VERIFICATION-2026-09-18.md)。未采集尺寸/张数或网络细阶段不推造，仍未授权的运行行为改变不混入。

最终增量更新：severity 已完成并通过 108真实管理race、101组账号/日志浏览器、154前端单元和完整Go/build验收，见 [分级验收](MANAGEMENT-ERROR-SEVERITY-VERIFICATION-2026-09-18.md)。下方“正在实现”只描述审查起点，不再是未完成项；采集与运行行为边界仍未消除。

审查时间：2026-09-18。本轮只读检查当前源码、本机 Sub2API 实际接入链和已保存验收证据；未修改产品、读取真实账号、访问服务器或调整代理。严重性分级正在由本轮 owner 实现，不重复列作未开始的缺口。

## 当前可以确认的范围

- 最新已验候选为 `account-status-20260918`，源码 `3b8d4b0e...`、前端 manifest `4a29cbad22816256db72146b5cf2674b14114afbda8708ac9aeb91b6c29adf11`；独立重算 54 个静态文件全部相符。机器报告明确 `deployed:false`，账号 55 组、日志 42 组浏览器验收通过，不包含本轮在途 severity。
- 真实空库预览证据为 `.local/management-parity/account-status-preview/api-verification.json`、`browser-verification.json`：相同源码/前端、真实 Cookie、排序/筛选/CSV、退役计费路由 404 通过；未导入账号、未发上游请求。这里只核对保存的证据，不以此声称刚刚再次探测服务。
- 账号创建、OAuth/移动 RT/session/PAT、定向/批量重授权、CRUD/筛选/全结果排序选择、资源/租期备份、CRS、用量/测试/计划、隐私/重置、非金额日统计均已找到 UI -> API -> 后端连接，详 `ACCOUNTS-NONBILLING-CURRENT-COVERAGE-2026-09-18.md`。
- 使用记录已接列表/快照/全量统计/趋势、稳定资源关联、CSV/JSONL、错误人工处理、IP 显式查询、清理预览及现有被动观测。证据为 `Logs.vue`、`api/logManagement.js`、`admin_usage_options.go:183`、`admin.go:1479`；请求/发送/返回模型及尝试边界见 `LogObservationDetails.vue`。
- 在上述非计费且不改变网关行为的现有范围内，本次未找到 severity 之外的新独立管理硬缺口或可复现产品故障。不能据此写成与参考所有网关/认证能力完全等同。

## 尚未完成且有实际来源的差异

| 范围 | 源码证据 | 依赖与最小边界 |
| --- | --- | --- |
| 图像 Token、张数、实际尺寸来源 | 参考 `components/admin/usage/UsageTable.vue:182-191` 展示图像输入/输出 Token；`service/openai_gateway_usage.go:389-396` 写入 Token/数量/输入输出尺寸/来源。本地 `stats.go:22` 的 `RequestRecord` 无这些字段。 | 管理层不能从总 Token 或模型名推算。需网关 owner 对当前已支持的 Responses/WS 真实返回增加 nullable 被动采集，再接管理展示/导出；不需要计费或新增图片路由。旧记录保持未知。 |
| 独立请求关联 ID | 参考 `middleware/client_request_id.go:16-43` 为请求生成/保留 context ID 并返回 `X-Client-Request-ID`；`OpsErrorDetailModal.vue:200,260`、`handler/admin/ops_handler.go:360-404` 使用它关联错误。本地 `gateway_observation.go:99-104` 的 `RequestID` 取上游响应头，`EventID` 是本地记录标识。 | 这是 `client_request_id`，不是 OAuth `client_id`。仅展示本地现有两个 ID 已完成；新关联来源需 gateway/context 采集。若坚持不改响应，不能完整照搬参考自动添加响应头的行为；应明确“仅记录已有来源”或另行允许新的返回关联头。 |
| 准入前错误与更细阶段耗时 | 参考 `routes/gateway.go:377` 在 API-key auth 前接 Ops logger；`ops_error_logger.go:1072,1639-1642` 记录错误及 auth/routing/upstream/response 阶段。本地 `gateway.go:454-490` 的鉴权/方法/路径拒绝早于 `RequestRecord` 和 `finishRecord` 建立；`stats.go` 仅有当前实际采集阶段。 | 若“全部错误”包含无效 Key、错误方法/路径及鉴权耗时，需要独立、有界、脱敏的运维事件采集，不能伪造业务用量记录或回填为 0。不能把已有准备/账号选择/代理选择耗时硬映射成参考阶段，也不能累加重叠阶段。 |
| 恢复、可调度状态与每账号协议配置 | 参考 `routes/admin.go:386,398-402`、`scheduled_test_runner_service.go:133-155`、`AccountsView.vue:2439`，以及 `CreateAccountModal.vue:2993-3266`。 | 清冷却、自动恢复、独立 schedulable、模型/compact 映射、每账号 WS/指纹/CLI-only、Spark shadow 会改变选择或转发行为。当前只读状态不是这些操作的替代，需单独扩大网关行为范围。 |
| 完整 Agent Identity 与可选 TOTP step-up | 参考 `CreateAccountModal.vue:3484` / `openai_agent_identity.go`；`routes/admin.go:408` / `middleware/step_up.go:41`。 | 单独 Agent 私钥认证依赖任务签名/凭据路由；本站只支持现有 Token 加非秘密身份摘要。TOTP 是参考默认关闭的可选多用户会话门控，本站是现有单管理员 key/session 模型。不是应偷偷塞入本次管理 UI 的开关。 |

其他平台专有功能、API-key 专有路径和全部金额功能不属于本次 Codex OAuth 非计费范围。参考图像展示同时存在价格字段，不应因此把价格实现重新引入。

## 生产状态与阻断

1. 旧的“SSH 不通，所以无法发布”已经过时。`GATEWAY-DEPLOYED-2026-09-18.md` 记录 12:00:54 功能发布及 12:28 现场复验成功：production 仍是稳定 `956` 源码 / `a66` binary / 原 47 文件 UI。新 54 文件管理候选没有部署凭证，不能把本地验收写成上线。
2. 新管理 Go 快照继承了其他任务的网关/OwnerRevision/native 后续代码。`CODEX-OWNER-REVISION-2026-09-18.md`“升级边界”明确旧 writer 会丢弃新增内部 revision；发布前必须统一 writer、排空旧写入并明确回退的会话失效策略。管理测试通过不能替代这一联合发布准备，也不代表应自动启用 native。
3. `GATEWAY-ENVIRONMENT-COORDINATION.md` 顶部明确用户已取消后续真实账号/上游测试。此项应写为未验证的范围，不再索要账号或把取消当作用户待补材料。真实 30/50/70/100 容量与自然 AT 轮换仍无完成证据，但属于独立环境目标，不应成为重复扩充管理 UI 的理由。
4. 本次未向服务器确认最新实时状态；关于线上状态的结论限定于上述最后保存证据。旧网络失败和旧门禁失败报告保留历史意义，不覆盖较新的成功发布记录。

## 最小范围决策

完成并验证本轮 severity 后，先收束管理候选。若仍要求补齐上述日志来源差异，只需明确一项边界：是否允许网关 owner 增加**纯被动、有界、脱敏、nullable 的运维采集**（图像用量、已有客户端关联来源、真实阶段、准入前独立错误事件），保持路由、重试、请求/响应字节及业务 Token 计数不变。不存在的历史来源不补造；参考自动返回关联 ID 不在这项被动范围内。

运行时恢复/每账号协议配置、完整 Agent 签名、认证体系改造应独立决定，不与被动采集捆绑。若已有会话授权已经覆盖某项被动采集，owner 应按已有授权执行，不再重复请求许可。生产发布则先形成可审阅的准确 binary/UI/writer/回退组合，再根据已有部署授权继续；不把“再同意上线”当作技术准备的替代。
