# 管理接口审查覆盖 — 2026-09-27

源码注册共 86 个管理 HTTP 路由。另检查 `/health` 与静态入口；推理网关由并行任务维护，未将其请求优化计入本报告。

“审查保留”表示检查现有实现并执行相关测试，不能解释为该接口被改动或已有独立性能数字。全量业务路径组合无法由有限测试穷尽；测量只覆盖性能报告明确列出的场景。

| 方法 | 路由 | 本轮处理 | 核对重点 |
| --- | --- | --- | --- |
| POST | `/api/session` | 审查保留 | 审查鉴权、同源校验、Cookie属性、Redis失败关闭和注销；已有隔离PG/Redis回归。 |
| GET | `/api/session` | 审查保留 | 审查鉴权、同源校验、Cookie属性、Redis失败关闭和注销；已有隔离PG/Redis回归。 |
| DELETE | `/api/session` | 审查保留 | 审查鉴权、同源校验、Cookie属性、Redis失败关闭和注销；已有隔离PG/Redis回归。 |
| POST | `/api/settings/rotate-key` | 审查保留 | 审查版本校验、有效范围、密钥轮换事务和幂等重放；不缓存会改变权限生效时机的数据。 |
| GET | `/api/accounts/{name}` | 已优化 | 仅聚合 stable-ID usage；只查本页资源名；manage 复用 ETag JSON；保持当前凭据状态与分页语义 |
| POST | `/api/accounts/import/preview` | 审查保留 | 输入限制与preview校验；导入store/crypto共享实现不修改 |
| POST | `/api/accounts/batch` | 审查保留 | 逐项版本冲突/部分成功保留；修改按原事务提交；refresh 不并发放大上游操作；删除后清理 schedules |
| GET | `/api/accounts/manage` | 已优化 | 仅聚合 stable-ID usage；只查本页资源名；manage 复用 ETag JSON；保持当前凭据状态与分页语义 |
| POST | `/api/accounts/selection` | 审查保留 | 严格筛选/排序校验、精确 total、最多 10000 个版本化选中项；PG repeatable-read 一致快照 |
| POST | `/api/accounts/actions` | 审查保留 | 逐项版本冲突/部分成功保留；修改按原事务提交；refresh 不并发放大上游操作；删除后清理 schedules |
| POST | `/api/accounts/{name}/reauthorize` | 审查保留 | stable-ID / version 校验、identity change 前置条件、credential generation 替换不变 |
| POST | `/api/accounts/reauthorize-batch` | 审查保留 | stable-ID / version 校验、identity change 前置条件、credential generation 替换不变 |
| GET | `/api/accounts/{name}/metadata` | 审查保留 | 只返回 identity/privacy/upstream 元数据；privacy 依据凭据代际和 300 秒快照有效期 |
| GET | `/api/accounts/{name}/runtime` | 审查保留 | 只读 admission/cooldown；Redis 批量 pipeline；不触发 credential refresh/清理冷却 |
| POST | `/api/accounts/last-used` | 审查保留 | stable-ID 最近实际 event 时间；拒绝重复ID；请求最多100账号；不使用 legacy bucket 时间 |
| GET | `/api/accounts/{name}/upstream` | 审查保留 | management upstream 边界不变；batch最多10；缓存须匹配代际、TTL和reset边界；读取与推理分别处理 |
| POST | `/api/accounts/{name}/upstream` | 审查保留 | management upstream 边界不变；batch最多10；缓存须匹配代际、TTL和reset边界；读取与推理分别处理 |
| POST | `/api/accounts/{name}/test-stream` | 审查保留 | 独立 management SSE、60秒超时、ctx断开停止、phase/delta/result写入；不改 gateway stream |
| POST | `/api/accounts/upstream/batch` | 审查保留 | management upstream 边界不变；batch最多10；缓存须匹配代际、TTL和reset边界；读取与推理分别处理 |
| POST | `/api/accounts/{name}/privacy` | 审查保留 | version/confirm 前置条件、重置操作幂等与unknown结果约束；保持审计 |
| POST | `/api/accounts/{name}/reset-credit` | 审查保留 | version/confirm 前置条件、重置操作幂等与unknown结果约束；保持审计 |
| GET | `/api/accounts/{name}/auto-reset` | 审查保留 | 配置/账号双版本；执行claim/lease；reset结果unknown不自动重复消费；先观察窗口恢复 |
| PUT | `/api/accounts/{name}/auto-reset` | 审查保留 | 配置/账号双版本；执行claim/lease；reset结果unknown不自动重复消费；先观察窗口恢复 |
| GET | `/api/accounts/{name}/schedules` | 审查保留 | cron/timezone校验；单账号最多20计划；request_id重放；version与执行租约；结果有界 |
| GET | `/api/accounts/{name}/lease` | 审查保留 | lease/account双版本；同事务更新；延长时仅恢复由同版本租赁禁用的账号 |
| PUT | `/api/accounts/{name}/lease` | 审查保留 | lease/account双版本；同事务更新；延长时仅恢复由同版本租赁禁用的账号 |
| POST | `/api/accounts/{name}/schedules` | 审查保留 | cron/timezone校验；单账号最多20计划；request_id重放；version与执行租约；结果有界 |
| PUT | `/api/account-schedules/{id}` | 审查保留 | cron/timezone校验；单账号最多20计划；request_id重放；version与执行租约；结果有界 |
| DELETE | `/api/account-schedules/{id}` | 审查保留 | cron/timezone校验；单账号最多20计划；request_id重放；version与执行租约；结果有界 |
| GET | `/api/account-schedules/{id}/results` | 审查保留 | cron/timezone校验；单账号最多20计划；request_id重放；version与执行租约；结果有界 |
| POST | `/api/accounts/export` | 已优化 | 仅聚合 stable-ID usage；只查本页资源名；manage 复用 ETag JSON；保持当前凭据状态与分页语义 |
| POST | `/api/accounts/export/credentials` | 已优化 | 资源 metadata 无代理解密、无共享 index 更新；凭据=true 维持完整导出；版本一致性与租赁快照不变 |
| POST | `/api/accounts/import-bundle` | 审查保留 | native bundle validation、账号/资源mapping、租赁receipt恢复流程保留 |
| POST | `/api/accounts/import-resources` | 审查保留 | dry-run不落库；显式映射/同名冲突保留；资源创建审计/事务语义不变 |
| POST | `/api/accounts/import-leases` | 审查保留 | 租赁配置与运行所有权分离；receipt归属、版本和幂等；不复活原runtime ownership |
| GET | `/api/accounts/import-leases/receipts/{receipt}` | 审查保留 | 租赁配置与运行所有权分离；receipt归属、版本和幂等；不复活原runtime ownership |
| POST | `/api/accounts/sync/crs/preview` | 审查保留 | preview归属/expiry，sync request hash重放；单账号原子提交与冲突处理；不改远端传输 |
| POST | `/api/accounts/sync/crs` | 审查保留 | preview归属/expiry，sync request hash重放；单账号原子提交与冲突处理；不改远端传输 |
| POST | `/api/account-oauth/start` | 审查保留 | owner隔离、PKCE/state、会话expiry、目标版本、幂等apply/cancel状态转换保留 |
| POST | `/api/account-oauth/exchange` | 审查保留 | owner隔离、PKCE/state、会话expiry、目标版本、幂等apply/cancel状态转换保留 |
| POST | `/api/account-oauth/import` | 审查保留 | owner隔离、PKCE/state、会话expiry、目标版本、幂等apply/cancel状态转换保留 |
| POST | `/api/account-oauth/apply` | 审查保留 | owner隔离、PKCE/state、会话expiry、目标版本、幂等apply/cancel状态转换保留 |
| GET | `/api/account-oauth/{id}` | 审查保留 | owner隔离、PKCE/state、会话expiry、目标版本、幂等apply/cancel状态转换保留 |
| DELETE | `/api/account-oauth/{id}` | 审查保留 | owner隔离、PKCE/state、会话expiry、目标版本、幂等apply/cancel状态转换保留 |
| GET | `/api/overview` | 已优化 | 账号总量/启用数直接COUNT；其他统计语义及已有10秒默认概览缓存保留。 |
| GET | `/api/diagnostics` | 审查保留 | 已有并发依赖检查、共享2秒截止时间、脱敏结果；无新增缓存。 |
| GET | `/api/operations` | 审查保留 | 并行任务正在维护；本轮审查及回归，未修改此模块或将其收益归因本次。 |
| GET | `/api/audit-logs` | 审查保留 | 已有(created_at,id)游标与limit+1查询；保留5秒期限、筛选绑定和错误响应。 |
| GET | `/api/accounts` | 已优化 | 仅聚合 stable-ID usage；只查本页资源名；manage 复用 ETag JSON；保持当前凭据状态与分页语义 |
| POST | `/api/accounts` | 审查保留 | 输入限制与preview校验；导入store/crypto共享实现不修改 |
| POST | `/api/accounts/refresh` | 审查保留 | 逐项版本冲突/部分成功保留；修改按原事务提交；refresh 不并发放大上游操作；删除后清理 schedules |
| PUT | `/api/accounts/{name}` | 审查保留 | 逐项版本冲突/部分成功保留；修改按原事务提交；refresh 不并发放大上游操作；删除后清理 schedules |
| DELETE | `/api/accounts/{name}` | 审查保留 | 逐项版本冲突/部分成功保留；修改按原事务提交；refresh 不并发放大上游操作；删除后清理 schedules |
| GET | `/api/accounts/{name}/requests` | 共享读取已优化 | 复用日志分页查询，稳定账号ID筛选不变。 |
| GET | `/api/groups` | 已优化 | 关系表直接聚合账号数量；一次构建 Key 分组计数，避免按分组反复扫描所有实体。 |
| POST | `/api/groups` | 审查保留 | 审查参数校验、版本冲突、关联删除与审计原子提交。 |
| PUT | `/api/groups/{id}` | 审查保留 | 审查参数校验、版本冲突、关联删除与审计原子提交。 |
| DELETE | `/api/groups/{id}` | 审查保留 | 审查参数校验、版本冲突、关联删除与审计原子提交。 |
| GET | `/api/keys` | 已优化 | 用量改为一次批量读取；数据库故障返回错误，避免成功返回全零用量。 |
| POST | `/api/keys` | 审查保留 | 审查分组存在性、版本冲突、密钥脱敏与持久化更新后的缓存刷新。 |
| PUT | `/api/keys/{id}` | 审查保留 | 审查分组存在性、版本冲突、密钥脱敏与持久化更新后的缓存刷新。 |
| DELETE | `/api/keys/{id}` | 审查保留 | 审查分组存在性、版本冲突、密钥脱敏与持久化更新后的缓存刷新。 |
| GET | `/api/proxies` | 已优化 | 关系表聚合绑定数；元数据读取不改运行代理缓存或清空密码。 |
| POST | `/api/proxies` | 审查保留 | 审查地址/端口校验、密码保留/脱敏、关联删除；网络探测使用已有假上游测试。 |
| POST | `/api/proxies/test` | 审查保留 | 审查地址/端口校验、密码保留/脱敏、关联删除；网络探测使用已有假上游测试。 |
| PUT | `/api/proxies/{id}` | 审查保留 | 审查地址/端口校验、密码保留/脱敏、关联删除；网络探测使用已有假上游测试。 |
| DELETE | `/api/proxies/{id}` | 审查保留 | 审查地址/端口校验、密码保留/脱敏、关联删除；网络探测使用已有假上游测试。 |
| GET | `/api/logs` | 已优化 | PG页码跳转单SQL，cursor仍键集分页；时间/ID并列顺序保留。 |
| GET | `/api/ingress-events` | 审查保留 | 已有有界窗口、筛选绑定游标和失败显式返回；保留被动观测语义。 |
| GET | `/api/logs/stats` | 已优化 | 文件聚合平均值存储复用；PG完整统计维度与准确性保留。 |
| GET | `/api/logs/export` | 一致性修复 | 复用导出schema常量；保留快照、外排、隐私脱敏和先完成后响应。 |
| GET | `/api/logs/{id}` | 已优化 | 规范数字ID用bigint主键；event_id用文本索引；冲突时仍返回最大行ID。 |
| GET | `/api/settings` | 审查保留 | 审查版本校验、有效范围、密钥轮换事务和幂等重放；不缓存会改变权限生效时机的数据。 |
| POST | `/api/settings` | 审查保留 | 审查版本校验、有效范围、密钥轮换事务和幂等重放；不缓存会改变权限生效时机的数据。 |
| POST | `/api/logs/cleanup/preview` | 审查保留 | 审查cleanup/snapshot/options/IP geo等状态与取消边界；保留事务/幂等/期限。 |
| POST | `/api/logs/cleanup/jobs` | 审查保留 | 审查cleanup/snapshot/options/IP geo等状态与取消边界；保留事务/幂等/期限。 |
| GET | `/api/logs/cleanup/jobs` | 审查保留 | 审查cleanup/snapshot/options/IP geo等状态与取消边界；保留事务/幂等/期限。 |
| POST | `/api/logs/cleanup/jobs/{id}/cancel` | 审查保留 | 审查cleanup/snapshot/options/IP geo等状态与取消边界；保留事务/幂等/期限。 |
| POST | `/api/logs/ip-geo` | 审查保留 | 审查cleanup/snapshot/options/IP geo等状态与取消边界；保留事务/幂等/期限。 |
| GET | `/api/logs/errors` | 已优化 | 共用优化后的PG列表查询；错误过滤/严重程度不变。 |
| GET | `/api/logs/{id}/review` | 共享读取已优化 | 日志详情索引查询复用；review状态读取保留。 |
| PUT | `/api/logs/{id}/review` | 共享读取已优化 | 详情索引查询复用；review版本与审计事务保留。 |
| GET | `/api/logs/coverage` | 一致性修复 | export_schema与实际CSV v3统一；exact count不改估算。 |
| POST | `/api/logs/snapshot` | 审查保留 | 审查cleanup/snapshot/options/IP geo等状态与取消边界；保留事务/幂等/期限。 |
| GET | `/api/logs/options` | 审查保留 | 审查cleanup/snapshot/options/IP geo等状态与取消边界；保留事务/幂等/期限。 |
| POST | `/api/accounts/usage-batch` | 已优化 | 只聚合需要返回的account维度；保留nullable观测、identity、时区和溢出语义。 |

功能审查与原始细节：

- [账号47路由明细](../.local/system-perf-20260927/accounts/endpoint-coverage.md)
- [日志/统计审查](../.local/system-perf-20260927/logs/coverage.md)
- [前端10页面审查与基准](../.local/system-perf-20260927/frontend/frontend-report.md)
- [机器可读接口清单](../.local/system-perf-20260927/api-inventory.json)
