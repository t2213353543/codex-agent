# 账号租期备份与恢复

2026-09-18，R3 管理能力增量。租期独立于 OAuth token expires_at。全部新接口沿现有管理员认证和 no-store，不改变网关，不导入计划/自动消费券策略，不复制旧实例的运行所有权。

格式范围仅为本系统 `codex-agent-accounts` v1 的可选 management.leases。原始 Sub2API 文件经普通导入时仍返回 `account_expiry_not_imported` / `sub2api_settings_not_imported`，不会自动映射外层账号租期和到期暂停；本轮不宣称跨格式租期恢复已经完成。

## 可选导出

`POST /api/accounts/export/credentials` 原请求可加 `include_leases:true`。metadata 导出使用此字段返回 400。省略或 false 时文件保持原 v1 格式，不额外读取或输出租期；选中时 v1 bundle 增加可选段：

```json
{
  "management": {
    "version": 1,
    "leases": [
      {"account_index": 1, "expires_at": 2100000000, "auto_disable": true, "restore_on_extend": true}
    ]
  }
}
```

account_index 为 accounts 数组的一基位置，与账号名无关。只包含真实存在的租期配置；没有租期的账号没有条目，空集合仍返回 leases:[]。只输出截止时间、到期停用和延期恢复策略；不输出 disabled_by_lease、disabled_version、disabled_at 或旧版本号。租期及对应账号版本在同一 PG repeatable-read / 文件文档快照中读取，并核对之前选定账号版本。

## 账号与租期分阶段恢复

`POST /api/accounts/import-bundle` 向后兼容旧文件。management 存在时校验 version=1、索引唯一且在范围内、截止时间为0或有效Unix秒；旧运行所有权等未知字段被拒绝。账号阶段继续原新增/同名显式替换/重复凭据处理，**不会自动应用租期**。

响应新增 `lease_restore:{dry_run,items,receipt_id?,expires_at?,error?}`。items 包含 account_index、配置、name、status、expired、would_disable；实际导入完成后再根据稳定 ID 核对实际凭据，并捕获 id/account_version/lease_version 和 replaces_existing。

- dry_run:true 只返回 pending_import 或 skipped，不产生可执行回执、不写租期。
- 实际账号阶段成功后返回30分钟、绑定当前管理员的持久 receipt_id。配置及目标ID不可由后续请求替换。
- 账号导入失败返回 skipped/account_import_failed；多个备份条目对应同一个目标ID返回 skipped/ambiguous_import_target，不根据名称或条目先后猜租期。
- 回执持久化失败保留真实账号结果，返回 lease_receipt_unavailable，不能声称账号回滚或租期已应用。

`GET /api/accounts/import-leases/receipts/{receipt}` 只读核对已保存回执。其他操作者无法读取，过期回执返回409。运行超过90秒仍无确定结果时显示 unknown，不自动重放。后台清理 `lease-restore` 的过期文档。

## 独立租期确认

`POST /api/accounts/import-leases`：

```json
{
  "receipt_id": "lease_restore_example",
  "items": [{"account_index": 1, "id": "new_stable_id", "account_version": 3, "lease_version": 0}],
  "dry_run": false,
  "confirm_restore": true,
  "confirm_expired": false,
  "confirm_replace": false
}
```

每批1–100项，前端固定整个待恢复目标并分批；可停止后续批次，不撤销已完成项。提交目标必须逐字段匹配回执，任何换ID、换版本或重复索引拒绝。dry_run 仅读取当前账号和租期，双版本不符显示该项 failed/version_conflict。执行必须 confirm_restore；过期且 auto_disable 的配置还需 confirm_expired，覆盖已有租期还需 confirm_replace。过期状态在执行事务内再次核对，不能靠预览时未过期绕过确认。

每项先持久 claim，然后在原 `updateManagedLease` 的账号+租期+审计原子事务中做双 CAS。配置覆盖时清空旧停用所有权，**即使 restore_on_extend=true，也不因此启用当前已停用账号**。如果租期确已过期且当前账号仍启用，本次原子停用会建立新的本地所有权及版本，后续正常租期延期操作可按原规则处理。

结果状态 pending_import/pending/skipped/running/applied/failed/unknown；只 applied 表示确定完成，返回 result_account_version/result_lease_version。版本冲突明确failed；写入或结果持久化不确定则unknown。已claim/完成/失败/unknown不会再次执行，重复请求仅返回已有事实；未知项必须读取回执并人工核对账号租期。单项失败不掩盖其他完成项，账号阶段不因租期失败自动撤销。

## 页面与验证

旧蓝壳的 AccountExportDialog 增加“包含账号独立租期配置”；AccountBundleImportDialog 保留原资源/账号恢复步骤，并增加 AccountLeaseRestorePanel。明确展示新ID、双版本、过期和覆盖影响，独立预览与三个确认，不展示账号令牌。修改文件或关闭对话框清除前端租期回执，租期执行时阻止关闭、修改资源目标。

Go 实现为 `admin_account_bundle_leases.go`，原 `admin_accounts_management.go` 最小导入/导出接入，`admin.go` 两个管理路由。helper/API/component 独立文件位于 `frontend/src/{utils,api,components}/`。当前商用固定预览未更新。

验证输出 `.local/management-parity/completion-20260918/`：

- 本地3组通过：两项租期完整/部分恢复与既有关联资源回归。
- 本机一次性 PostgreSQL 16.15 两项真实API专项通过，含并发claim、错ID/操作者/TTL、双CAS、已过期/覆盖确认、不恢复旧所有权、部分失败及审计写失败回滚，容器已清理。
- Node helper3组通过，保护文件索引/纯配置、固定ID双版本和回执合并。
- 旧壳浏览器3组通过，覆盖独立导出确认、账号/租期两阶段、部分冲突、断线未知只读核对、1440/320布局，errors=[]。已在最终统一 `.local/management-parity/completion-20260918/dist` 重跑，54文件逐hash验证，manifest `ebd3d90fbeae6debe7ba78ea3f80f20575982646b2d5daa4c192ddd6a7b19ebb` 前后不变；临时lease-completion产物及较早01349清单不作为最终交付。

日志：`local-lease-backup-summary.json`、`local-docker-pg-lease-backup-summary.json`、`lease-browser/report.json`。全部用合成账号、假上游/本地存储，没有生产访问或真实推理。
