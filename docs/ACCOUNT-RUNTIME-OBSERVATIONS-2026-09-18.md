# 账号运行状态只读增量

此增量实现账号矩阵 L09 / H08 / G01 中能直接观察的现有事实。保留既有网关转发、调度、准入和冷却行为；不增加解除冷却、清错误即恢复、每账号覆盖等会改变语义的动作。

## 来源与边界

参考 Sub2API 的 `frontend/src/components/account/AccountStatusIndicator.vue`（连接与模型限流、临时不可调度截止）、`frontend/src/views/admin/AccountsView.vue:1803`（最近使用列）。本项目的实际来源如下：

| 显示 | 实际来源 | 范围 |
|---|---|---|
| 并发占用 / 已占满 | `inFlight(gatewayAdmissionKey(account))` | 当前服务进程，非多节点全局值 |
| 限流冷却截止 | `gateway_cooldown.go` 的锁内 Map / Redis sorted set | 连接、模型目录、用户指定模型；未指定的模型不能从散列反查 |
| 容量保护 | `cooldownOverflowUntil` / Redis `!capacity` | 网关原有保守保护，不能描述成某账号上游必然限流 |
| OAuth 刷新等待 | `gateway_refresh_cooldown.go` 已有失败类别与截止 | 限制刷新重试，不证明当前访问令牌不可用 |
| 指纹、WS、兼容等 | `managementSettings()` 的有效全局设置和现有账号参数 | 全局开关，无每账号覆盖；开启不证明上游能力 |
| PG 最近业务请求 | 持久 `usage_events` 的稳定 `account_ref` 与 `max(created_at)` | 全部已成功持久化事件，包括失败/取消；独立管理测试不进入业务事件 |
| 文件最近业务请求 | 留存日志中 `AccountID` 与 `Time` | 仅留存详情；不以名称匹配不明旧记录 |

网关现有冷却“读取”会清理过期项，OAuth 读取还更新 LRU。因此新管理代码仅在原锁内复制值；Redis 只发 `TIME`、`ZSCORE`、`GET`，不调用原修改性读取。共享读取失败显示未知，已过截止显示“请重新读取”，不自动宣称恢复。空记录不是“从未使用”。

`usage_totals.last_at` 会合并旧迁移按天桶时间，不能作为精确最近使用；新增查询不使用它。日志详情清理不删除 `usage_events`。查询未按账号名称兜底，避免删除/重建/重命名后混入同名账号。

## API

现有管理员认证和 `Cache-Control: no-store` 仍适用；不返回令牌、刷新令牌、Client ID、上游账号 ID、内部散列键、私钥或内置提示词正文。

`GET /api/accounts/{稳定ID}/runtime?model={可选模型ID}`：

- `id` / `version`：读取的账号版本；`checked_at`：Unix **毫秒**。
- `admission`：`scope: process`、`in_flight`、`limit`、`saturated`。
- `cooldowns`：`coverage: connection_models_and_requested_model`、可选 `model`、`shared_state: not_configured|ok|unavailable`、`entries`。
- 每条冷却含 `kind`、`scope`、`source: process|shared_redis`、Unix **毫秒** `until`，OAuth HTTP 错误含 `http_code`。同一范围本机和共享观测可分别列出，不虚构唯一“健康值”。
- `effective_policy`：`source: global_settings`、`settings_version`、`per_account_override: false`、实际全局开关及账号启用/并发/刷新令牌存在/到期参数。启用指纹时显示现有源码参考 Profile；只描述应用层请求头，不声称 iOS、TLS 或 HTTP 栈仿真。
- `last_used`：`at` 为 Unix **秒**或 `null`；`scope: durable_usage_events|retained_request_details`、`source: gateway_request_records`。

`POST /api/accounts/last-used`：请求 `{ "ids": ["稳定ID"] }`，最多100个唯一非空ID。只读返回 `items: [{id, at}]`、`scope`、`source`、Unix秒 `checked_at`。10秒查询上限；失败返回错误，不回部分结果或伪零。列表默认不启用此列，开启后只为当前页面分批读取。

## 验证

新增后端测试：`TestManagedAccountRuntimeReadOnly`、`TestManagedAccountLastUsedStableID`、`TestManagedAccountRuntimeSharedReadOnly`；PG 等价项为 `TestPGManagedAccountRuntimeReadOnly`、`TestPGManagedAccountLastUsedStableID`。验证不清过期项、不改变LRU/准入/账号版本、不调用写 Redis 命令、共享服务器时钟偏移、失联未知、同名不同ID隔离、持久事件在详情清理后仍可查询，以及未泄露凭据。

新浏览器 runner 使用单独候选和报告，不覆盖既有冻结包或26组报告：

```powershell
$env:MANAGEMENT_UI_DIST = '.local/management-parity/followup-20260918/dist'
node frontend/tests/management-account-runtime-browser.mjs
```

报告路径 `.local/accounts-runtime/browser/report.json`，包含实际加载资源 SHA-256；1440/320截图同目录。新增7组实际浏览器已于2026-09-18 05:41通过（账号chunk `accounts-managed-DeD8Vq7Q.js`），`errors=[]`，20张运行状态/策略基础1440/320及四主题1440/390截图已完成巡检，无横向溢出。原26组在同候选也通过，报告独立位于 `.local/accounts-parity/followup-20260918/browser/report.json`；旧runner支持 `MANAGEMENT_ACCOUNT_REPORT_DIR` 指定输出目录。PG两项已在root冻结源码的Linux73项race中通过，报告 `.local/management-parity/linux-race-20260917T214152-2d29dc/report.json`，0skip/fail，源码哈希未变。

最终 Logs Batch / Geo 对比度候选构建清单 SHA-256 `8d9faa86fca4f023c939cc6a38fda26741bc35b44b717897822ae9a29e1eb5db`：28个实际加载的账号与共享运行时资源逐哈希一致，账号33组回归可复用。独立机器证据和报告/截图已固化到 `.local/management-parity/followup-20260918/account-verification.json` 及 `account-evidence/`，未覆盖旧冻结包。
