# 账号采购成本管理

采购成本独立于现有参考售价估算。人工来源和供应方是管理记录，不自动探测 ChatGPT OAuth 进价，不把未配置采购成本默认为售价或零；不改变账号调度、网关计量写入或上游请求。

参考 Sub2API `account_stats_pricing.go` 的自定义账号成本与 `usage_log_repo_stats.go:283` 的成本/标准价/用户价分别统计。本站没有参考项目的真实计费组身份，因此仅允许稳定账号 ID 或明确管理分组匹配；多组冲突由后端返回未知，不任选首组或将多组倍率相乘。采购规则不附加参考售价的分时规则。

## 界面与入口

- 账号列表工具栏“采购成本”管理全部规则；账号详情同名入口先筛选当前账号及其明确所属组规则，可切换全部。
- 规则表默认显示每条规则的最新版本，可按账号/组、目标 ID、模型、供应方或来源筛选，并打开全部版本历史。
- 每条规则填写模型精确名或末尾 `*` 前缀、服务层、稳定目标 ID、供应方、必填人工来源、四个用量桶单价、采购倍率及生效时间。单独 `*` 表示全部模型。
- 单价单位 USD / 百万 Token，保持十进制字符串；明确零单价和零倍率保留。缓存写入单价可留空沿用普通输入单价，缺少独立写入用量等无法估算的情况显示未知。
- 首版可历史生效且必须启用，启用复选框固定选中；后续版保留账号/组/模型/服务层身份，须晚于旧版且不早于当前时间。停用也新增版本，明确确认后执行，不停用账号。历史版本不覆盖或删除。
- 409 冲突保留草稿。网络或服务器错误导致结果未确认时阻止直接重发，要求刷新并核对已经保存的版本。
- 预览填写真实留存请求 ID，按已保存规则获取后端计算结果；草稿不参与，不向上游发送请求。显示来源、版本、对象、倍率前总额、倍率和倍率后的四桶金额；缺失或已清理的请求不保留旧成功结果。
- 账号统计中原参考估算金额保留，另列 `account_cost_usd` 与 `costed_requests` / `uncosted_requests`，统计口径仍为留存明细。

## 实现与验证入口

组件 `AccountCostDialog.vue` 支持 `v-model` 与可选 `account`；`AccountCostQuote.vue` 接收完整 `record.account_cost`。独立 API 为 `api/accountCosts.js`，纯校验及版本 helper 为 `utils/accountCostState.js`。两个采购入口在 `Accounts.vue`；Logs 接入由对应负责人维护。

```powershell
node --test frontend/tests/account-cost-state.test.js
$env:MANAGEMENT_UI_DIST = '<本轮独立候选 dist>'
node frontend/tests/management-account-cost-browser.mjs
```

5 项 helper 测试已通过：十进制与零保留、来源/字段上限、稳定目标与模型模式、版本身份和未来时间、历史及来源判定。独立候选 `.local/management-parity/pricing-next-20260918/dist` 上采购 8 组、既有账号 26 组及运行状态 7 组，共 41 组实际浏览器通过，`errors=[]`。采购测试另外覆盖分组稳定 ID、全模型规则、CAS 冲突草稿保留、停用确认、未知提交不重放、真实记录预览及取消旧请求、缺失记录清除旧估价。

新报告位于同候选目录的 `account-cost-browser/report.json`、`account-regression-browser/report.json`、`account-runtime-browser/report.json`。采购 32 张四主题1440/390截图包含表单上下半、历史及估价；原账号回归17张和运行状态20张均写入新目录。采购报告包含实际加载资源 SHA-256，输出目录可用 `MANAGEMENT_COST_REPORT_DIR` 指定。此前账号冻结报告未改写。后端 PostgreSQL 和公式聚合验收由后端负责人单独提供，不将浏览器模拟响应作为真实后端验证。

共享详情 API 增加快照参数后完整重跑41组；其后修正首个采购版本必须启用的表单约束。最终候选账号 chunk 为 `accounts-managed-D_F84EDE.js`，最终报告以本轮再次完整回归为准，未沿用旧资源测试结果；机器证据核对包含入口 HTML 在内的全部实际加载资源。
