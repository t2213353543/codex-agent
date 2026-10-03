# 非网关管理剩余能力审查

> 当前范围修订（2026-09-18）：用户明确项目不需要任何计费。本文下方的渠道销售区间、价格覆盖、采购、倍率及费用相关缺口均已退出目标，不得继续据此开发。原始记录保留供追溯；当前范围以 [无计费范围](MANAGEMENT-NO-BILLING-SCOPE.md) 为准，网关行为边界继续有效。

2026-09-18。按阶段只读核对本机 `../sub2api/current-source` 与本项目源码；审查不修改 Go、前端或冻结候选，不运行参考服务、查询生产或上游。参考包含本地扩展，不代表所有 Sub2API 版本都具备这些功能。最初审查基线为 `5852e088...`，当前结论以最上方最新阶段为准，旧矩阵“缺失”行不能覆盖已验实施记录。

最初基线确认了4组可独立管理实现的工作，后续均有增量实现，不能将这个旧数量继续当作当前剩余数。销售/采购/基础参考价已经分开，完成的长上下文、分时、采购阶梯、快照和未知/零语义不重复列为缺口。以下 `S/` 指参考根，`C/` 指本项目根；符号优先于后续可能移动的行号。

## Observations 当前结论

本节优先于下面保留的旧阶段待办。管理候选 `307b183ce71b3582488fa57de28906a361b0d431d1ee646003778dc7d43ef490` 已完成原始 Sub2API 外层 `rate_multiplier` 显式迁移（精确 JSON number、默认关闭、null/缺失不造1、0真0、来源不伪策略历史），以及先前已完成的原始外层租期、自家租期/账号成本备份、R1/R2/R4。因此下文原始rate/lease差距现在只能作为历史记录。

Q10/D03/A06/A08 的 sent/returned/inbound/upstream 链路，D11阶段计时，D16错误截断，D17有界attempt，D06/D09 schema/source现已有真实 RequestRecord 采集字段、管理过滤与四维分布、三价域聚合、详情和CSV字段。本轮 `admin_observation_end_to_end_test.go` 用实际网关向假上游发送401→OAuth刷新→重试，保持返回字节并写入真实PG，再验证管理筛选/详情/统计/导出及不存prompt/token。不能继续称其“没有来源”；这证明所测HTTP链路，不替代其他源码owner的所有HTTP/WS/native分支验收，也不能回填旧记录。

### 仍缺的纯管理范围：渠道销售上下文区间

**参考实际链路**：`S/backend/internal/service/channel.go` 的 `ChannelModelPricing.Intervals` / `FindMatchingInterval`，`channel_service.go:791` 的 `checkIntervalsHavePrices`，`model_pricing_resolver.go:227/:305/:323` 的 `applyTokenOverrides` / `GetIntervalPricing` / `intervalToModelPricing`，以及 `billing_service.go:1379` 的 `calculateTokenCost`。OpenAI用量继续经 `calculateOpenAIRecordUsageTokenCost` 进入此统一路径；参考 `frontend/src/components/admin/channel/IntervalRow.vue` 真正编辑区间与各桶价/倍率，并非孤立类型定义。

参考渠道销售区间的基数是**总输入（含缓存、不含输出）**，本项目对应 `RequestRecord.InputTokens`；边界 `(min,max]`、可多段、未命中回退渠道覆盖后的基础价。命中某桶：显式绝对价优先，否则使用配置倍率乘基础价，否则继承基础价。普通输入、输出、缓存读、缓存写可以独立设置，不能把输入倍率强制套在所有缓存桶。存在渠道区间时不再额外叠加官方式长上下文倍率；渠道分时随后应用。组模型覆盖在参考 `Resolve` 中明确移除 intervals，不能给组模型误加渠道区间功能。

**本站当前证据**：`C/admin_usage_pricing.go` 的 `managedPricingRule` 只有四桶基础价、单 `LongContextThreshold` 与输入/输出两倍率以及 TimePricing，没有销售 intervals；`admin_usage_sale.go` 直接使用该结构选价，`LogSalePriceEditor.vue` 也只编辑单阈值。采购 `managedCostInterval` 已完成但其基数是 input+output，语义不同。一个单阈值无法表达三段不同价格或缓存独立覆盖，因此 X09不能据采购阶梯已经完成就称全量渠道价卡对齐。

**下一子任务**：限定管理 sale-channel 模型价增加有界非重叠输入区间，独立逐桶可选绝对价/倍率/继承及明确优先级；保持组价覆盖、服务层、分时、KEY/组倍率、峰值和采购 follow_sale_base 的顺序。空区间的现有价格规则不变，当前immutable版本和旧快照不改；返回实际选中区间、基数与桶来源。file/PG/三价域/快照/CSV/JSONL/编辑器一起验证，不能只加一个控制项。

同模块应核对**平价部分覆盖**：参考 `billing_service.go:1267` 的 `applyChannelTokenPriceOverrides` 对 nil 保留基础值，显式0覆盖；当前本站输入/输出/缓存价格必须全部填写，只支持write空值沿input。可在管理渠道价新增明确继承已有有来源目录的模式，按记录时间选择基础版本；基础目录未配置仍unknown，不为此自动抓价或编造官方默认值。这是价卡语义的一部分，不要求引入实际账单、路由渠道或实时扣账。

测试须覆盖三个输入区间、上下边界/间隙/无上限、缓存不重复计入、输出不影响选档、逐桶0/绝对价优先/倍率/继承、缺基础价和缺写缓存量、区间与long-context互斥、group优先压过channel、分时/服务层/KEY与组峰值组合、follow_sale_base精确基数、多版本与导出一致。现有RequestRecord已能支撑普通文本Token路径，所以不能以“无字段”将此项排除。

### 其余范围

本轮未确认其他新的纯管理必补功能；不把已声明范围的计划历史页数、独立价卡文件传输、图表装饰或无reference调用链的额外仪表盘列为硬要求。E10/H07/H09/H10 的冷却/错误清除、H11自动代理回退恢复和Agent私钥任务认证仍改变网关运行行为，不因接口放admin文件而被视为获准修改。X08多模态Token细分及缓存TTL子桶仍需要对应实际观测字段与Codex路径证据，当前不能由text桶推断；此处保留来源边界，不声称参考里没有相关功能。

此归档独立验收结果：220文件哈希前后不变，full Go 454顶层通过/100环境跳过、含子1191通过/111跳过，vet及Windows/Linux构建通过。证据为 `.local/management-parity/local-race-20260918t050711-9d1fa7/local-verification.json`。这是固定管理候选的验证事实，不是“所有管理功能完成”或生产上线结论；本轮前端最终冻结和预览另由主任务汇总。

## Portability 复核增量

2026-09-18 portability 已在原始 Sub2API 普通导入中新增显式 `include_external_leases`，保留完整平台包装并区分外层管理租期与 OAuth 凭据到期；三输入形态、同凭据仅准备租期回执、逐行错误及独立双确认均有真实 file/PG 与旧壳浏览器证据。自家 bundle 可显式携带账号绑定的全部旧单目标成本历史及当前已生效有序策略的显式账号倍率，使用实际新 ID 回执独立预览/恢复，不复制全局策略或覆盖本地费率。下述 completion 的原始租期缺口因此已经推进，不再当当前未实现。

仍须保留**原始 Sub2API rate_multiplier** 的直接适配差距：参考 account_data.go 的真实导入导出包含该字段，而本站普通 raw 导入仍将其列为未自动映射。自家成本段不是它的等价替代；下一步应以显式选项映射到已存在的管理采购策略/账号倍率，并保持不改网关、默认不消费、不猜成本来源。

另一个源码任务已在共享 RequestRecord 加入 sent_model/returned_model、inbound/upstream path、阶段计时、attempts/truncated/schema/source 元数据，原“没有字段”只能作为历史事实。管理端当前自动继承 JSON 可选字段，并补导出秘密样式过滤；完整查询/维度分布/详情失败链和 CSV 字典仍需接入真实契约并验证，不能因为字段进入 JSON 就宣布使用记录对齐。具体采集最后冻结及分支证据由源码 owner 提供，管理任务不编辑网关。

保持旧候选与报告不可变。最新证据以 portability 固定总表为准，旧阶段的待办文字保留用于审计，不覆盖当前已经完成的行为。

## Completion 复核增量

上述四项是 `5852e088` 阶段的基线，保留用于追踪，不代表新候选仍全部缺失。2026-09-18 的 completion Go 候选 `cff3981d7efc24c7652faee5a7c725568c57039de9606f9982a46fd57b942f59` 已补 R1 显式跟随销售基数、R2 有序多目标规则、R3 **本系统 bundle** 租期往返；R4 每日统计/深链由账号统计负责人补齐并单独验收。定价策略源码只读复核未发现新增实际错误：跨规则先后、同规则模型精确/通配顺序及同模式服务层覆盖与 PG 一致；销售倍率为0仍通过未格式化基数计采购；已匹配但用量未知不降级到另一条规则；脱敏导出移除自由来源并摘要化规则、组和渠道标识；三个费用域沿同记录快照和只读事务计算。该结论不替代外部上游或生产验证。

**R3 的跨格式边界仍须保留**：`accounts_import.go:345` 对原始 Sub2API `auto_pause_on_expired/rate_multiplier/extra` 返回 `sub2api_settings_not_imported`，`:351` 对平台封套外层租期 `expires_at` 返回 `account_expiry_not_imported`；`accountCredentialObject` 不将这种外层账号租期混入 token expiry。本轮只在 `codex-agent-accounts` v1 可选 `management.leases` 中提供租期配置往返及独立确认，没有为原始 `sub2api-data` 适配租期。不能将“自家备份已支持”写成“跨格式租期完整支持”。下一可执行子任务是给 Sub2API 原始导入增加带来源的外层租期适配，在原导入实际回执生成稳定ID/双版本后复用同样的独立租期预览与确认，不把 expiry 自动写入 Token 字段，不静默应用到重复/同名账号。

最终 completion 前端清单 `ebd3d90fbeae6debe7ba78ea3f80f20575982646b2d5daa4c192ddd6a7b19ebb` 已对租期3组浏览器重跑，54静态文件逐hash、清单前后不变、errors=[]；报告为 `.local/management-parity/completion-20260918/lease-browser/report.json`。这次前端重建只影响新的管理候选，不更新既有 commercial-preview 服务。

## 必补任务

本机真实空库预览另发现一个轻微呈现缺口：`LogSalePolicyDialog.vue` 读取空目录后 selectedVersion 为字符串 `"0"`，但“尚无目录” option 值为空串，导致空下拉框不显示占位。创建流程正常，不影响费用或保存；下一 UI 增量应统一空选择值并加真实空目录断言。当前固定商业包不原地替换。

### R1 显式跟随售价用于账号统计

**真实参考链路**：`S/backend/internal/handler/admin/channel_handler.go:40` 接受 `apply_pricing_to_account_stats`，`S/frontend/src/views/admin/ChannelsView.vue:267` 有实际开关；渠道服务保存后，`OpenAIGatewayService.RecordUsage`（`openai_gateway_usage.go:473`）调用 `applyAccountStatsCost`，后者进入 `account_stats_pricing.go:23` 的 `resolveAccountStatsCost`。先尝试自定义成本规则，未命中且开关启用时取此次客户计价的 **TotalCost（客户倍率前）**，最后报表再乘账号成本倍率。不是复制用户 ActualCost，也不是对缺成本一律默认售价。

**当前状态**：`C/admin_usage_sale.go` 的 `managedSaleQuote.BaseAmount` 已区分销售倍率前金额；`C/admin_usage_procurement.go:19` 只支持四桶绝对采购价及 multiplier，没有显式成本来源模式，也不读取销售选价。当前“无采购规则保持 unknown”是正确缺省，但还没有参考中的主动跟随选项。

**下一子任务**：新增默认关闭、带不可变生效版本的账号统计来源策略，例如 `explicit_rates` / `follow_sale_base`；先匹配明确采购规则，再应用显式跟随策略。沿已有 KEY→管理计价组→渠道关联确定来源，不从账号多组任选。跟随基数应包含选中的模型价、服务层、长上下文及渠道分时，排除组/KEY销售倍率及组高峰倍率，然后再乘明确的账号统计成本倍率。内部保留精确有理数基数，不能复用已四舍五入的 `BaseAmount` 字符串，尤其不能用除以销售倍率来回算，因为倍率允许为 0。

**完成条件**：quote 返回实际来源模式、销售/采购规则版本和应用倍率；跟随源未知仍 unknown，已知零仍 0。file/PG、列表/详情/汇总/CSV/JSONL、冻结快照一致；新旧版本和“明确规则优先于跟随”有手算测试。参考 `TotalCost<=0` 会回退，但本站不能机械复制而覆盖真实零。全部为管理估算，不写 usage_events 扣费、账号 quota 或网关策略。

### R2 有序多目标成本规则集

**真实参考链路**：`S/backend/internal/service/channel.go:76` 的 `AccountStatsPricingRule` 具有 `GroupIDs[]`、`AccountIDs[]`、`SortOrder` 和 `Pricing[]`；`channel_handler.go:478` 校验范围非空并按请求数组设置 `SortOrder=i`。UI `ChannelsView.vue:501/:522` 实际选择多个组/账号。`account_stats_pricing.go:99` 的 `tryCustomRules` 按顺序先命中；`matchAccountStatsRule` 为账号 **或**计价组匹配；每条规则内的 `findPricingForModel` 才做精确优先、之后配置顺序的前缀匹配。规则范围匹配但模型未匹配会继续下一规则。

**当前状态**：`C/managedCostRule` 只有单 `target_type/target_id` 和单模型；`managedAccountCost` 固定账号优先、精确/最长前缀优先，没有管理员规则顺序。没有账号规则且两个账号成员组都命中时返回 `ambiguous_cost_rules`。该保守行为符合本站当前合同，但不等同参考的有序多目标规则；复制多条单目标规则也不能等价表达“一个版本更新整组目标、某条较早通配规则优先于较后精确规则”。

**下一子任务**：增加版本化的有序采购规则集，支持稳定账号/计价组目标数组、每条多个模型价卡及已完成的 Token 区间；整体 CAS、明确顺序调整、冲突预览、命中解释。计价组用现有显式管理绑定，不能把账号成员组自动解释成参考的请求 GroupID。保留旧单目标规则的历史含义，以显式新策略版本切换，不在升级时重排旧账。

**完成条件**：多目标 OR、规则先后顺序、规则命中但模型不命中、同规则模型精确/通配、停用与历史版本、零/未知值、PG/file 有相同候选选择。自定义采购规则仍不加 `time_pricing`：参考 `channel_service.go:715` 明确拒绝 `ACCOUNT_STATS_TIME_PRICING_UNSUPPORTED`。渠道停售、模型路由或选账号策略不是此子任务。

### R3 A12/E05 租期随账号备份往返

**真实参考链路**：`S/backend/internal/handler/admin/account_data.go:60` 的 `DataAccount` 包含 `expires_at`、`auto_pause_on_expired`、账号 `rate_multiplier` 和 `extra`；`ExportData` 在 `:202` 写出，`ImportData` 在 `:436` 传给 `CreateAccountInput`（`:448/:449` 为租期与到期暂停）。这是真实迁移路径，不只是字段声明。

**当前状态**：本站 `C/admin_account_leases.go` 已有独立 `managedAccountLease.ExpiresAt/AutoDisable/RestoreOnExtend`，令牌 expires_at 与租期正确分开；但 `C/admin_accounts_management.go:725` 的 `managedAccountBackup` 和 `:741` 的 bundle 只包含账号基础配置/凭据及可选组代理资源，不包含租期。`exportManagedAccounts` 没有调用 `getManagedLease`，恢复流程也没有迁移租期。`docs/MANAGEMENT-ACCOUNT-API.md` 已诚实记录此限制，因此 A12 的“完整管理关系往返”仍不能覆盖租期。

**下一子任务**：升级或增加向后兼容的可选账号管理备份段，显式导出租期配置，dry-run 显示到期影响、通过导入回执新稳定 ID 映射恢复。不能复制 `disabled_version/disabled_by_lease` 等旧实例的停用所有权证明；若目标已手工修改或租期已过期，需明确显示处理结果，不伪造“可安全自动恢复”。凭据、代理密码仍沿现有独立确认与 no-store。

**完成条件**：无租期、未来租期、已到期、auto_disable=false、目标重名/映射/版本冲突、租期保存失败与账号恢复部分完成均有真实结果。参考还携带成本倍率和 extra；本站成本目前独立于账号，因此与账号绑定的成本配置若纳入备份，必须带来源并映射新 ID，不能把规则 multiplier 静默转成网关字段。自动消费券策略可备份配置，但恢复执行前须重新明确授权，不复制运行中 claim/幂等消费记录为新任务。

### R4 U07/U08 账号日统计摘要与费用趋势

**真实参考链路**：`S/backend/internal/handler/admin/account_handler.go:1678` 的 `GetStats` 调用 `AccountUsageService.GetAccountUsageStats`，进入 `repository/usage_log_repo_stats.go:919`，按日汇总历史并计算 `ActualDaysUsed`、日均成本/请求/Token、`HighestCostDay`、`HighestRequestDay`、今日摘要及三种费用。`S/frontend/src/components/admin/account/AccountStatsModal.vue:110/:217/:277/:518` 实际展示日均、峰值日和费用趋势。

**当前状态**：`C/frontend/src/components/AccountStatsDrawer.vue` 已显示基础参考、独立采购、独立配置售价三种总额及已知/未知覆盖，也已有请求曲线和模型/端点/错误分布，这些不再是缺口。仍缺活跃日数、对应日均、最高费用日/请求日和费用日趋势；趋势只画 `total_requests`。`load()` 仅传 account_id/from/to，未固定日历时区与每日 step，不能从自动生成的小时/月桶直接推导“峰值日”。

**下一子任务**：在明确 IANA 时区下请求/聚合每日数据，提供真实活跃日/日均/峰值日及三费用趋势切换，保留范围和保留明细覆盖说明。可以复用已有 stats 聚合；长区间若不适合直接返回全部日点，可增加有界账号摘要，不能拿当前页或粗月桶冒充日统计。

**完成条件**：跨时区午夜/DST、无请求日、无数据、部分未定价/未配置采购、真实 0、删明细、峰值并列与不同费用口径分别验证。日均分母须明确是活跃日或自然日；部分未知时显示“已估算部分”的峰值和日均，不能称完整账单。无需修改网关累计计量。

## 导入导出核验结果

**未找到参考的独立渠道/成本价卡文件导入导出，因此不把它列为必补项。** `S/backend/internal/server/routes/admin.go:767` 的实际渠道路由只有 list/get/create/update/delete、model-pricing 和 sync-models；`S/frontend/src/api/admin/channels.ts` 与 `ChannelsView.vue` 无对应文件传输工作流。ModelTagInput 的批量粘贴只是模型名，不是完整价卡导入。当前本站 sale/cost GET+版本 POST 也不是可迁移备份，二者应如实描述。

参考 `/accounts/data` 的实际账号/代理迁移则确实存在，故 R3 依据的是其有源码的租期/倍率路径。未来完整备份所有管理计划、日志审核、销售策略、OAuth 暂存等可另设项目备份需求，但参考账号 DataPayload 本身也不备份独立 scheduled-test 表或全量渠道，不应为本次对齐凭空扩成新的硬要求。

## A/E/H/U 其余复核

| 范围 | 参考真实路径与当前状态 | 本次结论 |
| --- | --- | --- |
| A01–A11、A13 与 E01–E09、E11–E13 | 参考 account OAuth/import/update/bulk 路径；本站对应 `admin_account_oauth.go`、`admin_accounts_management.go`、identity、CRS、lease 与 Accounts 对话框均有实现，当前矩阵实施记录与代码吻合。 | 本次未发现新的必补非网关项；A12 租期往返单列 R3，不把整个账号管理再次算缺失。 |
| H01–H06 | 参考 `scheduled_test_runner_service.go:122` 保存真实结果；本站 `runManagedSchedule` 保存含模型、耗时、错误及文本的 Snapshot，UI可展开。本站保留最多100但显示最近50，参考 `ScheduledTestsPanel.vue:668` 本身固定读取20。 | 先前“未显示全部100”疑点撤回为非参考硬差距；当前明确最近50，不能仅据这个数字再造必补分页。 |
| E10、H07、H09–H10 | 参考 runner `:133/:155` 的自动恢复调用 `RateLimitService.RecoverAccountAfterSuccessfulTest`，`ratelimit_service.go:2102/:2139` 真正清除 error/rate-limit/temp-unsched/model-rate-limit 并通知调度。本站独立检查已保存真实成功，runtime只读显示真实冷却，故没有与参考等效的清除动作。 | **仍是未对齐的运行时行为边界**；即使按钮放admin文件，清除也会改变网关选择，不能在“管理代码独立”名义下绕过用户禁改网关。无需新增只清文字的伪恢复开关。 |
| H11 | 参考 `account_handler.go:1735` 的 `RevertProxyFallback` 调 `admin_account.go:1309` 的 repository `RevertProxyFallback`，修改实际代理配置；本站有多代理编辑、真实只读状态和最近使用，没有同自动回退机制。 | 运行时边界保留；不把缺少机制说成不适用，也不增无效恢复入口。 |
| A06 纯Agent私钥、A14 OAuth复制 | 参考 `openai_agent_identity.go:395` 的 `buildAgentIdentityAuthenticationHeaders` 调 ensure/register task，参与请求认证；本站只支持有来源身份摘要。参考 `AccountActionMenu.vue:108` 的 canDuplicate 不允许OAuth复制。 | 私钥链路仍需网关能力；OAuth复制不是当前类型应照搬功能。 |
| U01–U06、U09–U11 | 当前真实quota/券/隐私、自动重置、批量今日、metadata来源对应 `admin_account_upstream.go/controls.go/auto_reset.go/identity.go`；参考相应管理调用已核验且矩阵实施记录准确。 | 本次未确认新非网关缺口；统计三费用已经接入，不再重复要求。U07/U08 日摘要差距仅 R4。 |

建议按 R1+R2（同成本选择域）、R3（账号迁移）、R4（统计展示）三个独立子任务推进。每项都可限定于管理状态、管理接口和页面；旧策略默认值、不可变版本、同快照聚合及未知值语义继续保持。完成这些之前，只能说明当前商业候选已通过其验证，不能把“候选测试全绿”写成全部参考管理功能已对齐。
