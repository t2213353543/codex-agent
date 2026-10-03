# 原始 Sub2API 租期导入

2026-09-18，portability 管理增量。普通账号导入新增默认关闭的 `include_external_leases`，将有明确来源的外层账号租期转换成独立待确认配置，不修改网关，不将账号租期混入 OAuth 令牌到期。

## 参考来源与识别范围

本机 Sub2API `backend/internal/handler/admin/account_data.go` 的 DataAccount/ExportData/ImportData 使用账号外层 Unix秒 `expires_at` 和 `auto_pause_on_expired`；`backend/internal/service/account_service.go` 的 CreateAccountRequest/UpdateAccountRequest 存在 `*time.Time` JSON 形态，因此适配也接受明确带时区的 RFC3339 字符串。`admin_account.go:453` 对缺失自动暂停字段使用 true，本站会标记该默认来源而不假称字段由文件显式声明。

只识别账号对象本身的 `platform:"openai"`、`type:"oauth"` 与对象型 `credentials` 同时存在的封套。API name/credentials 外包装可向下找到这样的完整封套；不根据文件名、数据 envelope 的 type 或缺失平台字段推测账号类型。没有外层 expires_at/auto_pause_on_expired 就没有租期配置。真正 credentials 内部的 expires_at 始终属于凭据，不作为管理租期。

支持外层 expires_at 为：

- 0–253402300799 的 JSON 整数秒；0为不限期。
- 有时区的 RFC3339 时间字符串，可带小数秒，按秒精度保存。
- null 或缺失时为不限期；没有任何租期相关字段则不生成配置。

字符串数字、无时区日期、负数、毫秒级越界数字、非整数、布尔值或错误对象均不接受。auto_pause_on_expired 仅接受布尔值或 null/缺失；显式 false 保留，null/缺失用参考默认 true。`restore_on_extend` 固定 false，因为原格式没有授权自动恢复已有停用账号。

## API

`POST /api/accounts/import/preview`、`POST /api/accounts` 保持原来的 raw、name+credentials、accounts 三种互斥请求形式。可增加：

```json
{
  "include_external_leases": true,
  "accounts": [
    {
      "name": "synthetic-example",
      "platform": "openai",
      "type": "oauth",
      "expires_at": "2033-05-18T03:33:20Z",
      "auto_pause_on_expired": false,
      "credentials": {"access_token": "synthetic-example-only", "expires_at": 2100000000}
    }
  ]
}
```

前端保留原始每行对象及文件/行来源，不先拆成纯 credentials 再发给服务器。例中外层时间与内层 Token 到期独立，样例令牌不可实际使用。

不开启选项时，响应和行为不变，继续返回 `account_expiry_not_imported` / `sub2api_settings_not_imported` 等警告。开启后无效租期逐行返回 `status:failed,code:invalid_external_lease`，该行账号不会导入，其他有效行正常处理；通用账号格式本身错误仍沿原有格式/凭据错误。

有效项响应增加：

```json
{
  "lease_restore_source": "sub2api_outer_account",
  "external_leases": [{
    "account_index": 1,
    "expires_at": 2000000000,
    "auto_disable": false,
    "restore_on_extend": false,
    "source": "sub2api_outer_account",
    "expiry_format": "rfc3339",
    "auto_disable_source": "explicit"
  }],
  "lease_restore": {"dry_run": true, "items": []}
}
```

上面的 items 只用于示意 envelope，实际会对每个有效租期返回同 account_index 的既有租期回执项。expiry_format 为 unix_seconds/rfc3339/unlimited；auto_disable_source 为 explicit/sub2api_default。account_index 是**本次请求**内一基行号，不是所有文件或多批提交的全局序号。

预览只给配置来源、expired/would_disable、pending_import/skipped，没有可执行 receipt_id，不写账号或租期。实际账号导入后复用 `prepareManagedBundleLeases` 生成30分钟、操作者绑定的稳定ID/账号版本/租期版本回执；账号阶段不会自动应用租期。重复凭据可保留原账号并生成对应回执；同名不同凭据未明确替换时拒绝，多个源租期条目指向同一ID则 ambiguous_import_target，不能猜哪条配置优先。

租期后续仍使用 `POST /api/accounts/import-leases` 的独立 dry-run、confirm_restore、confirm_expired 和 confirm_replace，以及 `GET /api/accounts/import-leases/receipts/{receipt}` 核对。多次逐行导入产生多个回执，不合并或互换其固定索引。详情见 `MANAGEMENT-LEASE-BACKUP-API.md`。

成功识别仅移除已处理的 expiry/auto_pause 警告，并加 `external_lease_pending_confirmation`。如原对象还有 rate_multiplier 或 extra，则保留 sub2api_settings_not_imported；模型映射、组/代理映射等警告不删除。凭据、原始私密JSON不进入 external_leases 或租期回执。

## 实现与验证

后端改动限定 `admin.go` 的 importRequest/handleImport 和新增 `admin_account_external_leases.go`、对应测试；无需修改通用 accounts_import.go 的默认解析/警告行为，没有数据库迁移。旧 self-bundle 租期和 root 新成本管理备份段不被此适配器改写。

本地专项4个顶层/25个含子测试通过；一次性本机 PostgreSQL 16.15 专项4个顶层/9个含子测试通过，覆盖本功能及 root 的 BundleCosts/BundleCostPartialAndValidation/BundleCostTargetConflicts。测试含三种请求形式、RFC3339/Unix/null/0、默认auto_pause来源、拒绝错误外层字段、Token到期隔离、未开启兼容、相关/无关警告、逐行失败、重复与同名冲突、已过期的独立停用确认。

证据位于 `.local/management-parity/portability-20260918/{local,local-docker-pg}-external-lease-{summary.json,tests.jsonl}`。PG用现成本机镜像、随机loopback端口/内存密码/tmpfs，测试后容器已删除；未使用真实账号、生产数据库或上游推理。新前端 Accounts 导入交互与最终候选静态清单由页面负责人另行合并验证，旧commercial-preview/completion-preview保持不变。
