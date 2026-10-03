# 账号全结果排序

非计费管理接口增加 `sort_by=id` 和 `sort_by=status`。默认仍为 `created_at desc`，分页、筛选、全结果 ID/version 选择、元数据导出与凭据备份均使用相同排序。排序不会修改账号、请求记录或网关调度。

## 契约

`GET /api/accounts/manage` 的查询参数，以及 `POST /api/accounts/selection`、`POST /api/accounts/export`、`POST /api/accounts/export/credentials` 的 `filters` 接受新值，`sort_order` 仍只接受 `asc` 或 `desc`。

- `id`：按本系统稳定账号 ID 的 C/字节序排序，不使用显示名或上游 workspace ID。
- `status`：按本站 `accountManagementStatus` 的最终状态键排序。升序为 `disabled`、`error`、`expired`、`ok`、`unverified`，降序相反；状态相同按稳定 ID 同向排序。
- 缺少真实请求且凭据未验证时为 `unverified`，没有伪造 healthy 或新增 null/unknown 状态。账号停用、令牌到期、凭据错误、最后请求状态、凭据验证的优先级沿用原管理规则。
- 状态来源由共用 `managedAccountLastStatuses` 按真实稳定账号 ID 获取。改名不会丢失状态，同名但不同 ID 的旧账号请求不会污染当前账号。窗口用量口径不在此排序变更中调整。

## 实现

PG 使用同一只读 repeatable-read 事务计算过滤总数与分页数据，状态过滤及排序关联 `account_request_state` 的唯一账号行，状态 CASE 由 `managedAccountStatusSQL` 构造，没有每行客户端查询。状态与 ID 均显式 `COLLATE "C"`，和 Go 字节序相同。

文件模式一次读取账号集和完整留存日志的稳定 ID 最新状态，先全量过滤和排序，再截取分页。全结果选择及两类导出复用该路径，不在当前页面内局部排序。

## 来源与验证

参考本机 Sub2API `backend/internal/handler/admin/account_handler.go` 的 `List` 从 `sort_by` / `sort_order` 传给 `adminService.ListAccounts`，继而由 `backend/internal/repository/account_repo.go` 的 `accountListOrder` 接受 `id`、`status` 并按 ID 同向消除并列。本系统状态集合不同，因此保留本站真实状态键，未照搬参考系统的状态名称或优先级。

`TestManagedAccountIdentityStatusSort` / `TestPGManagedAccountIdentityStatusSort` 包含 id/status、正逆序、全量/标签/状态过滤共 12 组，覆盖跨页排序、摘要状态一致、完整稳定 ID/version 选择、元数据与凭据导出。另覆盖五类状态优先级、改名前请求、同时间按记录 ID 选最新请求、同名不同账号隔离，以及不支持的排序与注入参数拒绝。

真实 PostgreSQL 16.15 定向验证已通过 ID/状态排序、最近使用排序、既有完整账号合同三项顶层测试，共 22 项含子测试，无跳过；临时 tmpfs 容器已清理，证据位于 `.local/management-parity/account-status-20260918/account-status-pg-final-summary.json`。首轮共享源码因其他 owner 正在编辑的 WebSocket 测试缺少导入而未编译，其构建失败报告单独保留，没有冒充产品或 SQL 失败。

最终全库、race 与构建证据以此轮冻结归档验收为准；共享源码中其他 owner 的网关变更不由本管理排序任务修改或认证。
