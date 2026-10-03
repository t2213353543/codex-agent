# Codex 账号管理完整对齐矩阵

> 当前范围修订（2026-09-18）：用户明确不需要任何计费。下方涉及费用、倍率、采购与售价的实施和待办全部为历史记录，不再属于对齐目标；当前账号界面已移除这些功能。保留范围与接口兼容见 [无计费范围](MANAGEMENT-NO-BILLING-SCOPE.md)，账号专项见 [账号无计费验收](ACCOUNTS-NO-BILLING-2026-09-18.md)。

审计日期：2026-09-18。参考是本地 `../sub2api/current-source`，其中包含该项目自己的扩展；本文不把这些扩展声称为所有上游版本均有的标准功能。目标是现有 OpenAI / Codex 账号可用的全部管理能力，不扩展其他供应商，不改现有网关协议转换、指纹、调度、重试、会话绑定与计量写入行为。新管理动作可以独立实现，现有账号网关字段可以正常编辑。

下方原始逐项表保留开发前基线，当前进度以本节实施记录为准。本文不是“已全部完成”的发布说明。审计及浏览器验证只用本地源码、合成凭据与假上游，没有调用生产或发送真实推理。

## 2026-09-18 实施与验收记录

最新 account-status 增量已补 ID/状态全结果排序；修复文件模式按名称/ring读取导致状态串用，以及超过1000账号元数据导出的内部批量限制。账号55/日志42浏览器、150前端单元、104真实管理race通过，见 [状态与观测验收](MANAGEMENT-STATUS-WARMUP-2026-09-18.md)。下方历史“ID/状态排序尚缺”已由本轮取代，网关运行边界保持不变。

非计费 operational-fixes 当前增量：最近业务请求全结果排序、异常日期链接拦截、IANA 时区规范化、定时计划稳定创建请求与历史加载竞态、CRS 单账号原子提交、删除账号清理计划及到期租期无变化跳过已验收。账号53组、Logs38组、146前端单元、94真实管理race通过，见 [最新增量报告](MANAGEMENT-OPERATIONAL-FIXES-2026-09-18.md)。参考 ID/状态排序仍未接，网关行为与真实采集边界保留；不将后续旧段计费功能重新计入目标。

Observations 最新阶段已完成原始 Sub2API 账号倍率显式迁移：JSON数字词法精确保留、null/缺失不伪1、0真0、独立回执和策略确认，不改weight或自动开启全局策略。58账号+外部租期5/倍率6+自家租期3及日志57组通过，125真实Linux管理race通过，见 [本轮验收](MANAGEMENT-OBSERVATIONS-VERIFICATION-2026-09-18.md)。以下旧阶段原始倍率“未适配”仅为历史记录，当前剩余仍按最新审查及网关行为边界，不据迁移通过宣称整个账号功能全齐。

portability 最新增量：原始 Sub2API 外层租期显式适配、自家账号绑定成本完整历史/当前显式倍率备份恢复已经实现，见 [本轮验收](MANAGEMENT-PORTABILITY-VERIFICATION-2026-09-18.md)。账号58、外部导入5、租期3、日志51组及115真实PG/Redis Linux管理race通过。原始 Sub2API rate_multiplier 自动映射仍未完成；本系统成本备份不是该跨格式功能的替代。新网关被动字段已由其owner进入共享记录，完整管理查询和展示仍需下一轮接入真实来源，不再笼统说“没有字段”。

最新 completion 增量独立验收见 [完整报告](MANAGEMENT-COMPLETION-VERIFICATION-2026-09-18.md)：账号52组、独立租期3组、日志51组，105项真实PG/Redis Linux管理race通过。R1/R2有序采购和显式跟随已接，R4完整每日摘要/费用趋势已接；R3当前仅自家bundle租期往返，原Sub2API外层租期仍未适配，不能把此局部格式支持算成跨格式全部完成。旧阶段事实继续保留，不用最新界面存在证明所有上游行为或历史缺失数据已恢复。

最新 commercial 增量独立完成 [采购阶梯 UI](ACCOUNT-PROCUREMENT-TIERS-UI-2026-09-18.md) 与 [渠道售价管理联合验收](MANAGEMENT-COMMERCIAL-VERIFICATION-2026-09-18.md)：账号 46 组、日志 44 组浏览器及 95 组真实 PG/Redis Linux 管理竞态通过。账号统计已并列三类费用，未改旧壳。完整复审仍发现 A12/E05 的租期随备份迁移、U07/U08 的日摘要/峰值日/费用日趋势，以及采购来源/有序规则两项差距，详 [非网关剩余审查](MANAGEMENT-REMAINING-AUDIT-2026-09-18.md)；不得用下方旧阶段“统计完成”表述覆盖这些新核验出的细项。

当前真实旧界面验证产物为 `.local/management-parity/dist`，2026-09-18 05:00 本地时间完成 `frontend/tests/management-accounts-runtime-browser.mjs`：基础 7 组、授权与上游 7 组、生命周期 10 组、关联资源迁移 2 组，共 26 组通过，浏览器 `errors=[]`。报告 `.local/accounts-parity/browser/report.json`，四主题各 1440/390 共 8 张截图；320 宽的选中列表、授权/检查抽屉、定时计划另有行为断言。对应账号 chunk 为 `accounts-managed-AJ1IN3_r.js`。旧壳保持蓝色、连续账号列表；本次没有恢复用户此前否定的整站新版设计。

单元验证含账号管理 8 项、SSE 4 项、ETag 3 项、批量重授权 3 项、递归敏感错误脱敏 2 项，以及既有 import/accountFields 21 项，共 41 项通过。完整源码编译通过；最终行为回归使用旧壳局部产物，不使用全量新版 `dev-dist`。浏览器附加 9 张授权控制、计划、统计、CRS、批量重授权和关联恢复弹窗截图，含320宽，已目视核对。

| 需求 ID | 当前实现与准确完成程度 | 源码 / 验收证据 |
|---|---|---|
| L01、L03–L06 | 已接独立精确列表、全局排序、固定 ID/version 的全部筛选选择、跨页选中；超过限额拒绝；失败保留列表 | `Accounts.vue`、`utils/accountManagement.js`；`accounts-management-browser.mjs` 第1/3/7组；后端 `/accounts/manage`、`/selection` |
| L02 | 名称/备注/标签/ID、状态、组/代理及缺省关系、启用/RT/到期范围、真实隐私 disabled/unknown 筛选均已接；未知不伪造为已启用 | `Accounts.vue`、`utils/accountManagement.js`；生命周期最后1组校验filter→selection完全一致 |
| L07、L10 | 保留列偏好、蓝色旧壳、桌面连续表与紧凑手机列表；长名称、320/390宽、弹窗与键盘操作已回归 | `Accounts.vue`；8张截图和浏览器第7组、授权第7组、计划320断言 |
| L08 | 5/10/15/30秒偏好、隐藏页面/选中/表单/弹窗/输入焦点暂停；内存 ETag、同过滤304保留结果、登录变更清缓存 | `utils/accountListReader.js`；3单测；生命周期第1/9组验证304及5秒自动刷新/已选暂停 |
| L09 | 原状态/并发/配置/期限、今日统计保留；身份声明显示来源。新增可选“最近业务请求”列与详情：PG 按稳定 ID 读全部持久 usage_events 的精确 max(created_at)，文件仅扫描留存且带 ID 的详情；未匹配/读取失败分开，不宣称从未使用。调度评分/自动代理回退仍属边界 | `admin_account_runtime.go`、`Accounts.vue`、`AccountRuntimePanel.vue`；本地 runtime/lastused 专项通过，旧壳新增7组/原26组回归通过；PG单列下方增量记录 |
| L11 | 稳定 ID 日志入口与统计应用区间深链均完成；统计发送字符串时间防公共helper过滤，校验响应区间 | `AccountStatsDrawer.vue`；生命周期第6组 |
| A01–A04、A07–A09 | OAuth新建/指定目标重新授权、浏览器PKCE回调、RT/mobileRT原clientID、session/PAT入口及加密ready会话重试完成；普通刷新保留 | `AccountOAuthDialog.vue`、`AccountReauthorizeDialog.vue`；授权前3组；后端 OAuth 本地假上游专项由后端负责人提供 |
| A05、A10 | 保留原文件/文本多格式预览导入；新增session授权入口；旧导入能力未被替换 | `Accounts.vue`导入区、`AccountOAuthDialog.vue`；原import测试+基础浏览器回归；agent_identity扩展归A06 |
| A06 | OAuth tokens 附带的 agent_identity 字符串/Record身份摘要、workspace+user 区分及重授权跨用户确认已接；**纯 Agent 私钥任务签名认证未实现，属于网关认证链路边界**，不将私钥当Bearer | `AccountOAuthDialog.vue`、`AccountReauthorizeDialog.vue`、`admin_account_identity.go`；授权第3组；后端 `TestManagedAccountIdentityPrivacyBatch` 本地及PG通过 |
| A11 | 显式POST导出，默认metadata不含凭据，凭据备份另有确认；选中/完整筛选两种目标 | `AccountExportDialog.vue`；基础第4组；后台附件/no-store/体积上限专项 |
| A12 | 已完成关联分组/代理定义的可选导出，代理密码单独确认；资源dry-run→明确创建/映射→账号dry-run/恢复，逐项失败可见、不覆盖同名资源。分阶段非原子，unknown资源创建不直接重试 | `AccountResourceRestorePanel.vue`、`AccountBundleImportDialog.vue`、`AccountExportDialog.vue`；资源迁移2组浏览器+320截图；后端 `TestManagedBundleResourceRestore` 本地及PG通过 |
| A13 | CRS Codex OAuth预览、明确选项、冻结目标、逐项同步与相同提交重读已完成；不复制不支持账号类型 | `AccountCrsDialog.vue`；生命周期第7组；后端假CRS登录/导出专项 |
| A14 | OAuth凭据复制不适用，未添加会暗示独立额度的复制入口 | 参考本身禁止OAuth复制 |
| E01–E04、E06–E09、E12、E13 | 现有单项编辑与新增明确字段批量编辑、分组/代理/数值/标签/备注/窗口、删除、10项分批刷新、部分结果/取消/安全失败重试完成 | `AccountBulkDialog.vue`；基础第1/2组；`account-management.test.js`；未知结果不自动重试 |
| E05 | 独立租期双版本、自动停用及精确版本安全恢复、冲突保留输入已接；不修改token过期 | `AccountLeaseDialog.vue`；生命周期第5组；后端持久任务专项 |
| E10 | **边界/部分**：复核当前网关已有真实冷却与准入状态，可只读展示；清除冷却会改变网关调度，因此本轮不提供解除操作。独立检查结果与业务错误历史分别保留 | H08–H10；此前“没有对应状态”的表述已纠正，不能把“不允许改变调度”说成“没有状态” |
| E11 | 按已选稳定ID+version批量替换完成：本地映射核对、显式身份变更、每批10、409不重试、成功不重放、unknown人工核对。OAuth新建另接notes/tags/enabled，目标重授权不发送这些字段 | `AccountBatchReauthorizeDialog.vue`、`accountReauthorization.js`；生命周期第10组+3单测；后端身份/批量专项 |
| H01–H04 | 真实模型目录、显式短提示词SSE、取消/终态、普通与compact结果分别保存完成；目录可见不宣称推理通过 | `AccountUpstreamDrawer.vue`、`accountTestStream.js`；授权第4组、SSE4单测；真实操作只以假上游验证 |
| H05–H06 | CRUD持久计划、cron/时区/模型/启停/保留数、unknown历史及下一运行已接；推理计划明确确认 | `AccountSchedulesDialog.vue`；生命周期第4组；后端并发claim/重启unknown专项 |
| H07 | **边界**：现有管理计划不会自动停用账号，不能“恢复”人工停用或上游额度。真实成功检查单独保存；未宣称有与参考相同的auto_recover调度语义 | 参考需求保留，不新增空开关 |
| H08、H11 | **部分对齐**：新增真实本机并发占满、连接/目录/指定模型冷却、容量保护、OAuth 刷新重试等待与来源/截止；共享读取失败为未知。没有全账号健康布尔值，不将 OAuth 等待误作现有访问令牌一定不可用，不查询/清除未指定模型。自动代理回退仍未实现 | `gateway_cooldown.go:41/95/119/155`、`gateway_refresh_cooldown.go:89` 提供只读事实；新管理实现不调用会清理/LRU更新的原读取函数；`AccountRuntimePanel.vue` |
| H09–H10 | **边界**：现有网关冷却截止可只读查看；解除/重置它会改变调度，用户要求不改网关行为，因此不提供解除动作。租期恢复仅处理自身停用 | 新增观测不能解除真实 upstream 限制；手动停用不被健康检查覆盖 |
| U01–U04、U10–U11 | 真实窗口/套餐/来源/陈旧快照、单项与批量读取、真实券列表/有效性/人工兑换确认、持久requestID重试、隐私关闭已接；缺失额度不填0，unknown兑换不声称未消费 | `AccountUpstreamDrawer.vue`、`AccountControlsPanel.vue`、`AccountBatchUpstreamDialog.vue`；授权第4/5组、生命周期第8组 |
| U05 | 独立自动重置策略完成：默认关闭、双版本、阈值/间隔、自动消费明确确认、unknown暂停需核对再启用；不改变网关调度 | `AccountAutoResetPanel.vue`；授权第6组、320截图；root自动重置PG专项通过 |
| U06 | 今日批量聚合≤100/批、明确本地时区边界、完整当前页ID、真实0与未取得区别、费用null显示未定价 | `Accounts.vue`、`accountManagement.usageBatch`；生命周期第2组 |
| U07–U08 | 保留明细范围的今天/7/30天/自定义统计、请求成功失败/用量/费用未知/TTFT未知、趋势/模型/端点/错误分布、日志深链完成；不宣称清理前全历史费用 | `AccountStatsDrawer.vue`；生命周期第6组；真实聚合见root后端专项 |
| U09 | 管理身份字段email/workspace/user/agent/plan及source/collected_at展示完成；JWT声明固定未验证，usage实时套餐另按真实上游快照展示；缺失声明显示未提供 | `AccountIdentityPanel.vue`；生命周期第3组；后端身份/隐私专项及私钥不保存/不返回验证 |
| G01–G07 | **边界，未全对齐**：G01 已补只读全局有效指纹/WS/HTTP/兼容/刷新/401恢复/会话/提示词策略及实际账号启用/并发/RT/到期参数。无每账号覆盖，开关不保证该账号具备上游能力。每账号模式/account-server、force/模型映射、load_factor/费率调度、quota拦截/Spark/自定义headers仍需改变网关语义 | `managementSettings()` 有效设置版本 + `codexFingerprintProfile()`；指纹只称应用层源码参考，不声称iOS/TLS仿真；原G表其他边界保留 |
| X01–X05 | 仅按原逐项证据标不适用；仍保留全部Codex可用管理项 | 不用多平台差异掩盖A/E/H/U未完成项 |

基线表中的“缺失”是审计时间事实，不与本节已验证实现状态混用。剩余边界明确为E10/H07/H09/H10、G01–G07、A06纯Agent私钥任务签名，以及H11自动代理回退机制；精确最近业务时间已新增，但PG仅覆盖已经成功持久化的事件、文件仅覆盖留存且有稳定ID的记录；这些不被计为全部完成。所有新管理写上游动作仍需用户在界面明确触发，没有预先启用真实自动策略。

### 只读运行状态增量（不覆盖前述已冻结验收）

本次源码新增 `GET /api/accounts/{id}/runtime?model=`、`POST /api/accounts/last-used`。前者只复制现有互斥锁内状态，Redis 只用 TIME/ZSCORE/GET；不调用会删除过期项或更新 LRU 的网关读取函数。`checked_at` / 冷却 `until` 是 Unix 毫秒；最近业务请求 `at` 是 Unix 秒。未指定的模型散列不可反推，明确为未检查。全局开关仅描述配置，不保证上游能力；账号停用、无刷新令牌等参数独立展示。

PG 最近时间直接聚合稳定 `account_ref` 的 usage_events，不使用 `usage_totals.last_at`，因为后者会混入旧按天累计迁移的桶时间；请求详情清理不影响持久事件。文件模式扫描留存日志，只匹配 `AccountID`，拒绝同名猜测。未匹配返回 null，读取错误不返回伪零。接口最多100个ID、10秒查询上限，页面仅在该可选列开启时请求。

本地 `TestManagedAccountRuntimeReadOnly`、`TestManagedAccountLastUsedStableID`、`TestManagedAccountRuntimeSharedReadOnly` 已通过；其中覆盖 Redis 仅只读命令、服务器时间偏移、不可用、过期项/LRU/准入不变和敏感字段不返回。PG对应两项由root独立环境验证。新浏览器入口 `frontend/tests/management-account-runtime-browser.mjs`，用 `MANAGEMENT_UI_DIST` 指向新的迭代产物，报告独立写 `.local/accounts-runtime/browser/report.json`，不覆盖原26组冻结报告。2026-09-18 05:41 新候选 `.local/management-parity/followup-20260918/dist`（账号 `accounts-managed-DeD8Vq7Q.js`）新增7组浏览器通过，`errors=[]`；运行状态/策略基础1440与320及四主题1440/390共20截图已完成巡检。原26组也在同候选通过，报告独立写 `.local/accounts-parity/followup-20260918/browser/report.json`，四主题/弹窗17张截图未覆盖旧冻结记录。PG两项已在root冻结源码的Linux73项race中通过（124事件、0skip/fail、source_changed=false），报告 `.local/management-parity/linux-race-20260917T214152-2d29dc/report.json`。

## 基线与判定

当前 `frontend/src/views/Accounts.vue` 是完整管理源码，但用户恢复的页面来自 `.local/ui-releases/7ca36331d1089e9a039f0949526dcb1a65fc28b2985a0f0521620646442f207b/frontend/dist`（45 文件，账号组件 `Accounts-CO5sQjFD.js`）。新版 Logs 的局部发布保留该旧 Accounts。下表“当前”指审计时可维护源码/API能力，不能据此推断现网页面已包含新版源码全部功能。实施应保留恢复后的蓝色主题、连续列表与原壳，仅替换 Accounts / Logs 懒加载页面。

状态定义：**已有**＝存在可执行路径，仍需发布回归；**部分**＝有基础但未达到参考能力；**缺失**＝没有对应管理路径；**边界**＝Codex 可用、必须记录和解决，但参考实现依赖本项目没有的运行时语义，不能增加无效按钮或私改网关；**不适用**＝确由账号类型决定，明确列出理由。边界不等于从范围删除。

来源缩写均相对于以下源码根，冒号后的数字为审计时一基行号（后续改动可能移动）：

- **S/**：`../sub2api/current-source/`。
- **C/**：本仓库根。
- **SV**：`S/frontend/src/views/admin/AccountsView.vue`。
- **SA**：`S/frontend/src/api/admin/accounts.ts`。
- **SH**：`S/backend/internal/handler/admin/account_handler.go`。
- **SR**：`S/backend/internal/server/routes/admin.go`。
- **ST**：`S/frontend/src/types/index.ts`。
- **CV**：`C/frontend/src/views/Accounts.vue`。
- **CA**：`C/frontend/src/api.js`。
- **CH**：`C/admin.go`；**CX**：`C/admin_extra.go`。
- **CP**：`C/accounts_pg.go`；**CI**：`C/accounts_import.go`。

主要可点击入口：[参考页面](../../sub2api/current-source/frontend/src/views/admin/AccountsView.vue)、[参考账号 API](../../sub2api/current-source/frontend/src/api/admin/accounts.ts)、[参考后端路由](../../sub2api/current-source/backend/internal/server/routes/admin.go)、[当前页面](../frontend/src/views/Accounts.vue)、[当前管理后端](../admin.go)、[后端对齐契约](MANAGEMENT-PARITY-BACKEND.md)。组件以下均指实际由 SV 导入的版本，尤其测试、统计、重授权、定时测试是 `components/admin/account/`，不能用同名未接入组件误判能力。

## 列表、筛选与选择

| ID | 逐项需求与参考契约 | 当前证据 / 差距 | 实现依赖与验收 |
|---|---|---|---|
| L01 | 列表、按稳定 ID 的详情；SA:41/173，SH:636 | 部分：CH:338、CX:165，旧接口游标页；详情通过 ID 取，支持保留元数据 | 管理列表独立接口；加载失败保留上一成功数据并标明失败，空结果不能等同网络失败；详情被删除显示确定错误 |
| L02 | 名称搜索与平台/类型/状态/隐私/分组筛选；`AccountTableFilters.vue:10` | 部分：CV:44-54、CX:28 只有名称、状态、组、代理；单一 Codex 不需无意义平台选择 | 名称/备注/标签/ID、状态、分组含未分组、代理含无代理、refresh 有无、过期范围；查询与导出/选择必须同契约；隐私筛选待真实元数据 |
| L03 | 服务端全量排序；SV:658/1315、SA:41 | 缺失：CV:82 的名称/到期只排当前页；CP:19 不接 sort | 白名单 sort_by/sort_order+稳定 ID 次排序；跨三页数据排序全局单调，分页无重复/遗漏，空到期一致 |
| L04 | 精确 total、页码跳转与页容量；SV:1299/1307 | 部分：CH:353 明确 total_kind=not_computed，只能游标前后 | 新管理接口 exact total；筛选/排序/容量变化一次回首页，后退恢复正确条件，不用本页数冒充总数 |
| L05 | 选本页/取消、全部筛选结果选择；SV:1114/2062，`utils/accountSelection.ts:17` 每页1000遍历且完整性校验 | 部分：CV:335 本页选中，换页/刷新会丢，无法处理所有匹配账号 | 专用 selection 返回固定 ID+version、total、filter_hash；超上限明确拒绝；筛选变化清选择，慢旧响应不得覆盖新条件 |
| L06 | 区分选中编辑与筛选全部编辑；SV:2094/2104、`AccountBulkActionsBar.vue:1` | 缺失：只有当前 selection 的五类动作 | 两种目标可见且确认数量一致；提交使用冻结快照，提交期间新增匹配账号不被误处理 |
| L07 | 自定义列及持久化；SV:948/1782/1813 | 已有基础：CV:34、`utils/accountFields.js:1` 有21列、12默认；旧发布页应单独核验 | 保留用户列选择、关键名称/操作不可全隐藏，新增列默认不破坏历史配置；320/390宽不横向溢出页面 |
| L08 | 自动刷新5/10/15/30秒、偏好、ETag与交互保护；SV:692/985/1439 | 缺失自动刷新/ETag；CV:146 有请求取消和最后请求胜出 | 后台刷新不能重置选中、打断弹窗、覆盖未保存编辑；隐藏标签页暂停；304不闪空表；失败明确且不无限toast |
| L09 | 账号状态、使用/并发、组、代理、优先级、期限、最后使用/创建、备注等列；SV:1782 | 部分：当前多数基础列有；原基线缺last_used、套餐/email、真实quota与今日统计、scheduler score；last_used已按本节增量补齐 | 逐字段真实来源和空值；“最近验证”不可用最后业务请求冒充；凭据明文不进入列表响应/DOM |
| L10 | 操作菜单边界、长名称/多组、弹窗定位、键盘与手机；SV:2499，`AccountActionMenu.vue:14` | 部分：新版源码移动卡片和详情已做，但不能重上此前被否定的整体卡片设计 | 延续旧壳与蓝色，桌面连续表、手机紧凑可读；菜单屏幕内、Escape返回焦点、操作忙状态防重复 |
| L11 | 账号详情到对应使用记录 | 已有：CV:76 accountLogs 传account_id；近期图CV:221只60条 | 保留稳定ID深链，可从统计时间区间携带from/to；删除或改名仍可查历史，不把60条称完整统计 |

## 创建、授权、迁移与导出

| ID | 逐项需求与参考契约 | 当前证据 / 差距 | 实现依赖与验收 |
|---|---|---|---|
| A01 | 新建 Codex OAuth 账号向导、通用默认字段；`CreateAccountModal.vue:352/2935/5489` | 部分：CH:364 创建复用凭据导入；缺独立创建向导 | 名称/备注/组/代理/并发/优先级/权重等统一校验；创建成功定位新账号，失败保留输入但关闭清敏感文本 |
| A02 | 浏览器 OAuth URL、PKCE、state、代理和回调交换；`useOpenAIOAuth.ts:55/101`，`openai_oauth_service.go:45/133` | 缺失管理OAuth；现有refresh不是新授权 | 管理侧短时一次性会话，绑定操作者/代理/目标；错误state、过期、重复code失败；URL复制/重新生成/取消/回调指引完整 |
| A03 | 直接 refresh_token 导入并校验，可指定client_id；SA:788，创建页:3479 | 部分：CI支持包含access的导入、现账号refresh；缺RT→有效凭据创建入口 | 独立验证/交换、真实返回expiry/account identity；空/失效RT逐项报错，不保存未验证伪造access |
| A04 | OpenAI移动端 refresh_token 授权分支；创建页:3480，`OAuthAuthorizationFlow.vue:77/968` | 缺失；不等同全局iOS指纹开关 | 审查参考真实client_id/交换契约并独立管理实现；明确token来源，不能用macOS客户端字段冒称iOS；网关指纹保持既有设置 |
| A05 | Codex auth.json/session/嵌套格式导入；创建页:3483/6303，SA:759，`account_codex_import.go:491` | 部分：CI:62/117/190多格式导入，元数据继承；需样本覆盖协议和字段 | 无secret合成样本验证auth.json、session JSON、数组、JSONL、封套、空文件、超限、错误行；有来源的expiry/clientID/identity正确 |
| A06 | agent_identity /工作区身份、同组织不同用户去重；创建页:3484，`account_codex_import.go:922/965/1063` | 部分/缺失：现有账号ID及名称导入去重不能证明覆盖agent identity | 拒绝把同organization的不同用户合并；同身份重复预览明确；workspace字段保留，不能仅按email/AccountID粗判 |
| A07 | Codex PAT验证创建；创建页:3485/6381，SA:766，`openai_oauth_handler.go:363` | 缺失独立PAT验证创建 | 参考验证接口管理侧实现；有权限/无权限/失效PAT三种结果；PAT不假定可刷新；返回元数据不泄漏原文 |
| A08 | 重新授权指定账号且保留身份与管理配置；SV:2337，SA:328，SH:1577 | 缺失专用入口，现同名replace不够明确 | target ID+version，替换凭据保留名称/组/代理/窗口/历史；身份变化明确确认；不同身份不能沿用旧refresh；版本冲突不覆盖 |
| A09 | 单账号刷新及无RT说明；SV:2353，SH:1526 | 已有：CH:548、CV:322，序列化refresh与token持久化 | 刷新是刷新凭据，不宣称模型连通成功；失效/无RT/上游异常明确，保持账号元数据且新expiry可见 |
| A10 | 批量文件/文本导入、预览、replace策略、逐项结果与重试；`ImportDataModal.vue`，`account_data.go:228/245` | 已有基础：CV导入、CI:361/402，文件上限/预览/脱敏回执 | 保留所有现有格式，不因新向导删成功链路；预览零写入，格式错/重复/部分成功/冲突可重试，回执无token |
| A11 | 完整数据导出：选中或全部筛选；SA:713，`account_data.go:100/527`，SR:411 step-up | 缺失：CV downloadImport仅导入结果，不是账号备份 | metadata和可迁移凭据分两个显式下载动作；凭据二次确认、POST/no-store/attachment，不后台预取；导出版本与账号数量准确 |
| A12 | 代理、分组等关系迁移与恢复；SA:748，`account_data.go:38/60/245` | 部分：导入defaults可绑定本地组代理，缺完整往返格式和映射 | 独立bundle type/version；代理凭据只在显式含凭据导出；关系按映射校验不静默ID误配；dry_run/replace/配额窗口往返 |
| A13 | 从CRS预览/同步；SV:111，SA:667/676 | 缺失 | 对现有Codex兼容源仍属范围；远端地址/凭据临时保存、预览选条目/映射代理组、同步逐项结果；不支持的源类型明确排除，不伪导入 |
| A14 | 账号复制 | 参考`AccountActionMenu.vue:121`只允许apikey/upstream/bedrock/service_account，**禁止OAuth复制**；当前无 | 对现有Codex OAuth不需要照搬“复制凭据创建独立额度账号”；可复制管理配置到新建向导，不能暗示额度独立 |

## 编辑、分组与批量动作

| ID | 逐项需求与参考契约 | 当前证据 / 差距 | 实现依赖与验收 |
|---|---|---|---|
| E01 | 名称/备注/启用状态及配置编辑；`EditAccountModal.vue:16/21/2840/4848` | 已有：CH:424/456，CV:281/293；version409 | 保留冲突提示与最新详情重读；未改字段不能意外清空；版本冲突提交不成功也不关闭编辑 |
| E02 | 组绑定/解绑/未分组；编辑页:2904，SH:137 | 已有单项groups数组；缺批量全结果与明确替换说明 | 单项/批量允许明确清空；失效group ID校验；更新组不能重置quota窗口；绑定后筛选和访问分组一致 |
| E03 | 代理绑定与无代理；创建页:2935，编辑页:1594 | 已有proxies数组（本项目多代理，不复制参考单proxy语义） | 保留多代理能力；批量替换/清空明确；代理名称失效显示ID而非空白；OAuth/测试/用量管理动作遵循选定代理 |
| E04 | 并发/优先级及调度参数；创建页:2946/2958 | 已有concurrency/priority/weight；参考load_factor另有含义 | 现字段编辑和批量一致，范围校验；weight不冒充load_factor；不改网关权重/调度行为 |
| E05 | 独立账号到期和到期自动暂停；创建页:2976/5869、`account_expiry_service.go:10/59` | 缺失：本项目expires_at是令牌到期，不能覆盖成账号租期 | 新字段独立于token_expiry；管理定时任务可到期改现enabled，需明确用户选择；重启补偿/时区/取消暂停可测，不能因刷新token延长账号租期 |
| E06 | 批量编辑字段显式勾选；`BulkEditAccountModal.vue:679/707/766/835/1407/2196`，SH:158 | 部分：CV:345仅enable/disable/proxies/tags/reset_window | 增加组、并发、优先级、权重、notes、quota/window；未勾选不发送；数组完整替换标明，空数组与未选择有区别 |
| E07 | 批量启用/停用或可调度状态；SV:2010/2451，SA:586 | 部分：本项目只有enabled，无独立schedulable | 提供当前真实启停批量；不可把enabled开关宣传为独立schedulable；若新增可调度层需先明确管理与现网选取关系 |
| E08 | 批量删除，确认目标范围与逐项结果；SV:1855，SH:1750 | 缺失，单项CH:533 | 按ID+version分批、错误不吞、取消停止后续批次；历史用量保留，成功/失败/未执行计数一致，不重试已成功删除 |
| E09 | 批量刷新凭据；SV:1892，SH:1953 | 缺失，仅单项 | 有界并发与每项错误；无RT跳过有理由；取消停止后续，旧selection不替换新credentials；不能无说明发测试推理 |
| E10 | 批量重置状态/清错；SV:1876、SH:1885 | 缺失真实可恢复状态；现apply enabled时清LastError不能改变历史失败 | 分离凭据状态、最后业务结果、管理检查；只确认/清除管理告警或实际检查成功恢复，不能删除历史来伪造健康 |
| E11 | 批量凭据更新/批量创建接口；SA:485/503，SH:2047/2196 | 部分：多格式导入、replace已有；缺按固定ID批量凭据更新 | 目标映射/版本/身份校验，敏感结果脱敏，部分失败定位但不回显凭据；UI不能用名称模糊匹配替换错误对象 |
| E12 | 批量完整筛选目标的可取消进度和失败重试 | 参考bulk接口返回success_ids/failed_ids/errors（SA:810）；当前batch逐条result | 前端冻结目标快照，100更新/删除、10刷新分块；停止不回滚已成功；只重试仍失败、先处理409重新确认；不显示虚假100% |
| E13 | 标签/备注和本地Token窗口（本项目已有增量能力） | CH:424、`accountFields.js:19`，quota/window变化会重置WindowStart | 对齐不能删现有能力；批量quota变更前说明窗口影响；普通备注编辑不发quota、不重置；只重置窗口不删除历史累计 |

## 测试、定时测试、健康与恢复

| ID | 逐项需求与参考契约 | 当前证据 / 差距 | 实现依赖与验收 |
|---|---|---|---|
| H01 | 指定账号可用模型列表；SA:598，SH:2768，测试组件:769 | 缺失管理账号models；网关模型列表不能证明每账号权限 | 管理侧读取真实catalog，展示来源/时间/失败；目录可见不等于推理通过；取消切换账号后旧响应不能写入 |
| H02 | 手动真实连通/推理测试及模型/提示词；`AccountTestModal.vue:828/847`，SH:1270 | 缺失；刷新token与查看请求记录不能替代 | 独立显式POST，有阶段、状态、脱敏错误与结果；默认安全短prompt，用户知道会产生请求；不更改业务网关链路 |
| H03 | 流式SSE输出、取消、失败终态；测试组件:880/941 | 缺失 | 支持分片跨行/多data、无完成事件断流判失败、401/429/5xx；切换账号/关窗取消并阻止旧回调；内容按文本渲染 |
| H04 | OpenAI普通/compact测试模式；测试组件:857 | 缺失，参考明确对OpenAI适用 | 对当前账号/服务真实支持才启用；独立测试适配层，compact成功与普通成功分别记录；不通过改生产请求转换来实现 |
| H05 | 定时测试创建/列出/修改/停启/删除；`ScheduledTestsPanel.vue:547/559/583/609/638`，`scheduled_test_handler.go:22` | 缺失模型/cron/计划存储与runner | 持久计划{account_id,model_id,cron_expression,enabled,max_results,auto_recover}；5字段cron校验、next_run、重启恢复、删除不再调度 |
| H06 | 计划结果历史/详情/保留条数；计划组件:656/677，`scheduled_test_service.go:72/80` | 缺失 | 最近运行/下次运行/成功失败/延迟/错误可查看；max_results裁剪只影响测试历史；手工/定时测试与业务日志标识清楚 |
| H07 | 测试成功后自动恢复开关；`scheduled_test_runner_service.go:122/150` | 缺失；现“最近请求失败”历史不应被覆盖 | 默认明确开关；仅真实成功能记录健康检查通过或恢复此前由该机制禁用账号；不能解除上游额度/人为停用，保留原错误历史 |
| H08 | 分层健康：active/error、限流、过载、临时不可调度及倒计时；`AccountStatusIndicator.vue:15/268/274` | 部分：CP:404/CX:146只有ok/error/disabled/expired/unverified；原审计未发现对应运行时状态，后续复核已确认gateway_cooldown现有状态并增加只读观察 | 显示credential状态、最近请求结果、check状态；真实存在的429时间才计时，无reset时间显示未知；不把HTTP200failed标成功 |
| H09 | 恢复状态、clear-error、clear-rate-limit；SA:361/399/411，SH:1301/1709/2545 | 边界：当前网关无参考限流/过载存储，不存在可清的截止值 | 可独立管理检查/确认告警；有实际状态才能恢复，返回变更清单；明确本地清错不恢复远端额度，历史请求不可篡改 |
| H10 | 临时不可调度原因、规则命中、过期与手动清除；SA:433/445，`TempUnschedStatusModal.vue` | 边界：本项目无此状态或规则执行器 | 需先说明用户所指可恢复对象；不新加网关错误策略，不生成占位恢复按钮；若独立管理暂停必须有来源与恢复规则 |
| H11 | 最后使用/最后检查/上游错误/代理回退恢复；SA:825，SH:1735 | 部分：有LastCheckedAt/LastError；没有参考自动proxy fallback机制 | 提供真实最后业务时间与检查结果，错误脱敏；没有自动回退时不显示“恢复代理回退”动作 |

## 用量、统计、套餐与隐私

| ID | 逐项需求与参考契约 | 当前证据 / 差距 | 实现依赖与验收 |
|---|---|---|---|
| U01 | Codex真实上游5小时/7日使用比例、窗口与reset；`AccountUsageCell.vue:119`，ST:1431，SA:371 | 缺失：CX:84/141只有网关本地Token窗口，不能冒充ChatGPT额度 | 独立上游读取服务与快照；实际window_seconds/reset优先、未知为空；本地token与远端百分比分开；401/429不得显示0% |
| U02 | 主动/被动用量来源、缓存与新鲜度、刷新；SA:371 `{source,force}`，`AccountUsageCell.vue:1391` | 缺失 | 首选官方只读quota查询；若参考通过推理采样需显式标明并授权动作，页面GET/自动刷新不隐式推理；stale/source/fetched_at/partial/error可见 |
| U03 | 批量用量与单项部分失败；SA:386，SH:2709，SV:745 | 缺失 | 批量有界、单账号失败不让全部归零；缓存去重，不每个单元格重复请求；滚动/翻页不会刷新全部远端 |
| U04 | OpenAI额度重置券数量、有效期、刷新、人工消耗确认；`OpenAIQuotaResetCell.vue:155/286/298`，SA:957/972，`openai_oauth_handler.go:475/502/576` | 缺失；**本地reset_window不是上游reset credit** | 管理侧真实quota接口/写接口；必须先实时查询有效券，失效缓存禁用消耗；确认一次只发一次，持久后处理，失败不伪成功 |
| U05 | 自动重置券开关与执行状态；`OpenAIQuotaResetCell.vue:201`、ST extra.codex_auto_reset_credit_state | 边界：参考与上游额度/调度阈值联动，当前没有 | 可独立管理任务评估但不能偷改网关；checking/available/resetting/success/no_credit/failed真实可追踪，避免重试重复消耗 |
| U06 | 账号今日请求/Token/费用，批量加载；SV:866，`AccountTodayStatsCell.vue:20`，SA:559/573 | 部分：当前只有窗口总量，缺今日/批量按日端点 | 以明确时区汇总全量事件；页面不能拿当前60条算今日；费用没有价格源时null/不可用，不填0 |
| U07 | 30天账号统计与摘要/日趋势；`AccountStatsModal.vue:657`固定30天，SA:349 | 缺失：CV:221散点为最近60请求，非完整统计 | 完整account_id时间汇总，实际覆盖区间/活跃天数、请求成功失败、Token/缓存/耗时；删明细后累计仍可信，覆盖不足要说明 |
| U08 | 模型与端点分布、成本口径、峰值日；统计组件:412、ST:2175/2185/2221 | 缺失；本项目历史事件粒度有限，费用未存 | 使用保留明细做分布时标coverage；不同cost类型不能混为一谈；没有证据的TTFT/账单/模型价格均不可伪造 |
| U09 | OAuth email、计划、订阅/组织/用户元数据；`useOpenAIOAuth.ts:9`，SV:1714，编辑页:2228 | 部分：JWT解析只供account/client ID、expiry，不展示来源完整元数据 | 管理侧可信payload保存source/collected_at；本地JWT解码不是远端验证；同组织多用户区分；可编辑标签不可冒充官方plan |
| U10 | OpenAI关闭训练隐私操作及结果状态；SV:2384/2405，SA:871，SH:3073 | 缺失，参考适用OpenAI OAuth非shadow | 核实真实上游契约后显式写；training_off/CF_blocked/failed区分，失败不显示已关闭；不收集新内容也不改网关 |
| U11 | 计划/额度采样未知、错误与状态刷新一致 | 参考ST:1381 usage source/errors，组件错误态 | 当前本地quota无upstream能力标签 | capability+source是后端事实；未实现/不支持/调用失败不同；支持项不因短暂失败从UI消失，保留上次可信结果与时间 |

## Codex 可用但需明确网关边界的扩展

这些不能因为难实现就从需求删除。可以独立实现的管理读写先做；只有会改变既有网关行为的部分需由根任务明确处理，不能在账号页加一个没有实际效果的开关后宣称对齐。

| ID | 参考能力与证据 | 当前事实 / 对齐边界 | 可验收结果 |
|---|---|---|---|
| G01 | 每账号OpenAI Responses/WebSocket/CLI-only/CLI-app-server/指纹模式；创建:3069/3243/3301，批量编辑:862/899/943/982/1099 | 当前有用户既有全局指纹设置，账号结构没有相应字段；新账号字段若要生效会涉及网关 | 不覆盖既有系统开关；可只读展示当前有效模式及来源；每账号覆盖必须先解决行为边界，不做假保存 |
| G02 | compact能力auto/force_on/off、最近探测和model mapping；SV:1726/1763，编辑:2245/2268 | 无账号级探测状态；mapping影响转发属于边界 | 管理显式测试可保存探测事实；auto/force模式及mapping只有实际支持才展示可写，普通测试不能标compact通过 |
| G03 | 独立load_factor、费率rate_multiplier、scheduler score；创建:2952/2970，SV:930/1782 | 本项目weight不是load_factor，Token计量没有Sub2API用户计费；score无相同算法 | 显示现有并发/权重/优先级事实；费用另以有来源估算，不声称真实扣费；如做管理评分须另命名和算法说明 |
| G04 | 每账号调度阈值/额外额度策略、auto-reset联动；编辑:1537，ST extra阈值 | 当前没有上游quota调度阻断；本地quota字段也不阻断 | 保存与执行一致；不能为UI对齐在网关增加隐蔽拦截；本地额度清晰标“统计窗口” |
| G05 | Spark影子账号创建/与父账号共享凭据/独立quota维度；SV:2431，SA:988，`openai_oauth_handler.go:547`、ST:1290 | 当前Account没有parent_account_id/quota_dimension；简单复制OAuth会产生错误的独立账号假象 | 需父子数据模型、凭据轮换与调度语义共同支持；父级删除/refresh/export规则完整（参考导出跳过shadow有说明）；未实现不能声称Spark可用 |
| G06 | 自定义上游Request ID header与请求头；创建:2939，编辑:1598 | 本项目未有该账号字段；会改出站请求 | 管理页不新增无效配置，不把本地event_id冒称上游request ID；原网关行为保持 |
| G07 | 模型限制/映射/自定义状态码/错误透传规则/TLS profiles；创建/编辑各平台分支，SV:148 | 一部分适用于Codex但改变请求路由/错误处理，其他为Anthropic专属 | 逐能力标适用性；Codex适用mapping不可用“多平台”一词一并排除，实际生效需独立范围决策；本轮不改gateway文件 |

## 明确不适用或不同口径的参考项

| ID | 参考项 | 适用性结论 |
|---|---|---|
| X01 | Anthropic setup-token/cookie/OAuth、Claude TLS指纹、warmup、缓存创建TTL、会话/RPM策略、Bedrock凭据 | 不适用于现有OpenAI/Codex OAuth，保留参考目录即可，不在UI堆无效选项 |
| X02 | Gemini service-account、Antigravity tier/allowOverages、Grok图片音视频/WebSocket独有测试、Ollama插件 | 不适用于Codex；H02-H04保留Codex真实文本/compact测试，不借此排除所有测试 |
| X03 | OpenAI API-key/upstream供应商余额、费率探测、模型同步与独立API-key总/日/周美元quota（SA:631/993；SH:2569） | 当前账号类型是Codex OAuth，API-key专属操作不照搬；真实Codex上游5h/7d、reset credit和统计仍适用（U01-U08） |
| X04 | 重复OAuth凭据作为独立账号池 | 参考UI本就不允许OAuth复制（A14），没有义务提供有误导性的独立配额副本 |
| X05 | 创建页组件虽有session_token/access_token选项，但实际OpenAI调用明确`:show-session-token-option=false`、`:show-access-token-option=false`（创建:3481/3482） | 不能把组件死入口误报为参考已上线能力；当前原始token导入仍保留，Codex session/PAT/RT/OAuth属于明确可用入口 |

## 实施依赖与完成门槛

第一组是完整列表契约、全结果选择、全部基础编辑/批量、定向重授权、metadata/凭据完整往返；后端已约定 `GET /api/accounts/manage`、`POST /api/accounts/selection`、`POST /api/accounts/actions`、`POST /api/accounts/{id}/reauthorize`、两类export、`POST /api/accounts/import-bundle`。这只是L/E和部分A的基础，不能当作全部账号功能完成。

下一组需要管理侧OAuth会话、指定账号models/test与SSE、定时测试持久计划和结果；随后是上游quota只读快照/人工刷新、真实可用reset/privacy管理动作、账号今日/历史统计。可以并行实现，但同一token的刷新/重授权/查询要有一致并发语义与脱敏审计。

所有条目均需满足：实际后端行为、真实UI入口、明确loading/empty/error/stale、版本冲突/部分失败与取消处理、键盘/390与320可用、凭据不出现在默认导出或日志、成功链路回归。浏览器使用无secret合成数据；会写上游的真实操作另受用户当前授权范围约束。上线需从旧45文件基线做局部可维护构建，并验证除指定页面和最小入口映射外旧壳/其它页面资源保持，不能用完整新版壳覆盖用户恢复的UI。

“网关不改”的验收应同时比较受保护源码哈希与行为回归，而非只搜索文件名。无法在不改变既有网关语义下实现的G类条目须明确未完成原因和可行独立管理方案，不能悄悄删行、改成已有或用静态按钮掩盖。

最终 Logs Batch / Geo 对比度候选构建清单 SHA-256 `8d9faa86fca4f023c939cc6a38fda26741bc35b44b717897822ae9a29e1eb5db`：28个实际加载的账号与共享运行时资源逐哈希一致，账号33组回归可复用。独立机器证据和报告/截图已固化到 `.local/management-parity/followup-20260918/account-verification.json` 及 `account-evidence/`，未覆盖旧冻结包。
