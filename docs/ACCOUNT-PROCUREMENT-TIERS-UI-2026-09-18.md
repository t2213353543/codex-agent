# 采购 Token 阶梯界面

增量基于现有独立采购成本，不改变网关计价或调度。旧 `pricing-next-20260918` 及冻结包保持不动，本轮验收候选为 `commercial-20260918`。

## 行为

- 采购规则编辑增加“自定义 Token 阶梯”，支持新增、删除与后续版本复制。每条规则最多32段；历史版本的阶梯深拷贝为草稿，编辑草稿不改变旧版本。
- 区间为 `(min,max]`：下界不含，上界包含；允许相邻和空隙，无上限只能位于排序后的末段。上下界为0至9007199254740991的安全整数，且上界大于下界。保存前按下界排序。
- 四个绝对单价全部必填，包括缓存写入，允许真实零；不会将空字段当免费或沿用基础价。原基础费率的缓存写入留空规则不变。
- 阶梯匹配基数为本站输入总 Token + 输出 Token。输入已包含缓存读写，不再次相加。命中后整次请求使用该段四桶单价，再应用规则采购倍率，不累进计费。
- 总量为0或落在空隙时使用原基础价；用量缺失、桶数据不一致或超出安全整数范围时保持未知。已确定命中区间但未记录独立写入桶时，区间仍可展示，采购金额保持未知。
- 规则列表可展开每个历史版本的区间与四桶价格。采购报价展示后端返回的实际匹配基数、总量、边界、命中区间与单价，不在浏览器重新猜测用量或采购费用。
- 导入提示更正为“原系统的计费倍率、自动暂停等扩展配置未自动导入，请在本站核对配置”，不再将已经存在的管理配置误说成不适用。
- 账号统计另加独立配置售价金额及已定价/未知请求数，与基础参考价、采购成本三项并列。KEY明确指定的管理计价组决定销售规则，不把账号多组成员关系当计价归属，不声称发生真实扣款。

## 组件与验证

`AccountCostIntervalsEditor.vue` 与 `accountCostIntervals.js` 负责草稿/边界；`AccountCostIntervalQuote.vue` 显示 `quote.token_tier`。`AccountCostDialog.vue` 和 `AccountCostQuote.vue` 接入；没有修改 Logs 页面、商业报价入口或网关源码。

11 项 helper 测试通过（原采购5项、阶梯5项、独立配置售价统计1项）。新增浏览器5组覆盖历史隔离、编辑增删/拒重叠和缺价、四主题1440/390、真实quote上下边界/零/gap/无上限，以及未知用量与缺写入桶。原有账号41组加新增5组，共46组在新候选实际通过，`errors=[]`；统计组新增售价金额与未知覆盖断言。

验收候选：`commercial-20260918/dist`，账号 `accounts-managed-BGYtKyiw.js`；构建清单 SHA-256 为 `fa845eab012f20603703d31488415b26b9adf9eb594c5d5318cd95c1324ddcb9`。采购基础32张、阶梯编辑/报价24张、原账号回归17张、运行状态20张，共93张截图写入新候选报告目录。阶梯表单逐段取景，1440/390四主题均检查无溢出，深色手机和浅色桌面关键区域已目视。机器证据位于同目录 `account-procurement-verification.json`，包含实际加载资源与测试源码的哈希，未覆盖旧冻结记录。

```powershell
$env:MANAGEMENT_UI_DIST = '.local/management-parity/commercial-20260918/dist'
$env:MANAGEMENT_COST_REPORT_DIR = '.local/management-parity/commercial-20260918/account-cost-browser'
node frontend/tests/management-account-cost-browser.mjs
$env:MANAGEMENT_ACCOUNT_REPORT_DIR = '.local/management-parity/commercial-20260918/account-regression-browser'
node frontend/tests/management-accounts-runtime-browser.mjs
$env:MANAGEMENT_RUNTIME_REPORT_DIR = '.local/management-parity/commercial-20260918/account-runtime-browser'
node frontend/tests/management-account-runtime-browser.mjs
$env:MANAGEMENT_CANDIDATE = 'commercial-20260918'
node frontend/scripts/record-account-procurement-verification.mjs
```

后端真实 file/PG 公式、快照和导出专项见 `docs/MANAGEMENT-PROCUREMENT-TIERS.md`，不能以浏览器合成响应代替。
