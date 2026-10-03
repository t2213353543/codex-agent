# 渠道区间计价独立复核

2026-09-18。只读复核本机 `../sub2api/current-source` 实际 UI → 保存 → OpenAI 计价路径；不修改本轮其他负责人拥有的后端、价格编辑器或网关。参考含本地扩展，下文不是官方 OpenAI 报价。

## 真实调用链

- `frontend/src/views/admin/ChannelsView.vue:445` 使用 `PricingEntryCard`，明确传入 `enable-tier-multipliers` / `enable-time-pricing`；价格卡真实提供基础空价、Token intervals、Fast/Flex/Max倍率。`backend/internal/handler/admin/channel_handler.go:35` 的模型价卡请求以及 `:68` 的三个倍率字段有保存契约。
- `OpenAIGatewayService.calculateOpenAIRecordUsageTokenCost`（`openai_gateway_usage.go:699`）将 API Key 的真实组、请求时间、服务层、推理等级、账号长上下文开关传给统一 Resolver，并非仅其他平台使用。
- `ModelPricingResolver.Resolve`（`model_pricing_resolver.go:71`）优先组价；组 Token 卡先清除自定义 intervals。无组覆盖时取得模型基础价，再应用渠道覆盖。`applyTokenOverrides` 克隆基础对象，避免污染共享目录。
- `BillingService.calculateTokenCost`（`billing_service.go:1381`）区间基数是 `InputTokens + CacheCreationTokens + CacheReadTokens`。参考的 `UsageTokens.InputTokens` 已在 `openai_gateway_usage.go:180` 从总输入扣除缓存，所以本站等价基数为原 `RequestRecord.InputTokens`，不能加输出，也不能再次加缓存。
- `FindMatchingInterval`（`channel.go:185`）为 `(min,max]`，0不匹配最小下界0；未命中回到已应用渠道基础覆盖的价卡。区间不是累进税率，选中后应用整次四桶。

## 必须区分的语义

| 项目 | 参考实际行为 | 本站最小管理实现注意点 |
|---|---|---|
| 基础空值 | 未配置渠道桶继承模型基础桶；显式0覆盖为0。渠道缓存写价缺省不因为渠道input变价而自动改为channel input | `inherit_base` 缺基础时保持unknown，不把参考内部零对象当已知免费；四桶都显式给出才可独立报价 |
| 区间单桶 | `intervalToModelPricing` 的绝对价优先，其次本桶 multiplier，再继承本桶基础价；绝对0也优先 | null与0分开，不能用truthy决定是否覆盖 |
| multiplier=0 | 参考 `channel.go:validateIntervalPrices` 和handler `gt=0` 拒绝区间及Fast/Flex/Max零倍率；账号/组倍率另允许0 | 若本站合同允许0，须明确是本站兼容扩展；未知继承价×0仍unknown，不能填0 |
| 官方长上下文 | `len(resolved.Intervals)>0` 即关闭官方长上下文倍率，包含真实输入落在gap时 | 不能只在“命中interval”才停LC，否则gap重复计价；所有空段过滤后若无有效段，参考仍可LC，本站可以直接拒绝空段 |
| 组长上下文开关 | `GroupsView.vue:1488/:3138`真实checkbox → `group_handler.go:194/:269` → resolver gate；false时`pricingContext=1`，同时停官方LC | 可在管理sale group加默认true可选bool，只控制估算。关闭不是把全部interval删除，而是选择1Token对应最低段；该段不存在则回基础 |
| 账号开关 | 统一路径的账号bool true可开启，false不否决组true；额外账号开关来自API-key适用路径 | 不把API-key-only开关强加Codex OAuth；组开关本身适用Codex并应可管理 |
| priority比例 | `channelTierOverridePrice`用`newStandard×basePriority/baseStandard`；interval绝对价同样保留原比例，multiplier同时乘标准与priority桶 | 本站service_tier分行价不自动等同该比例继承，需显式source与priority基础配对合同，否则记录未等价边界 |
| Fast/Flex | 显式Fast令`usePriorityServiceTierPricing`不选catalog priority桶，改整桶按Fast倍率；Flex类似。仅`priority/fast`使用显式Fast，不自动套`ultrafast` | 必須声明作用tier；不能把精确tier价再次无条件相乘，或给缺配置Codex臆造官方系数 |
| Max effort | `maxReasoningEffortBillingMultiplier`仅`NormalizeMaxReasoningEffort(effort)==max`，优先显式模型配置；默认3仅Claude Fable5.1 | 显式管理倍率可用于真实记录max，Codex默认不得套3；不要把xhigh自动称max |
| 分时 | 仅`PricingSourceChannel`调用`resolvedChannelTimeMultiplier`，组覆盖直接胜出则不叠渠道时段；定价时刻来自请求起点 | interval/LC/服务层解析完后应用；不是读取报告时刻 |
| 销售倍率/高峰 | `ResolveUserGroupRateMultiplier`选择用户覆盖或组默认，本站用明确KEY覆盖替代；随后乘订阅组peak | KEY与组不是相乘；组peak独立；四桶明细与总价同步 |
| 显式跟随采购 | 参考取`TotalCost`，不取`ActualCost`；本站follow_sale_base使用未舍入base/Rat | 基数含interval/服务层/LC/渠道分时/显式Max，排除组或KEY销售倍率及订阅peak；不能通过最终销售金额反除，因其允许0 |

## 手算验证表

除特别说明外，使用本站记录 `input=1000, cached=300, cache_write=100, output=200`。互斥普通输入600；区间基数1000，不是1200或1400。基础单价（USD / 百万Token）按输入/输出/读缓存/写缓存为 `2 / 6 / 0.5 / 3`，基础总额 `(600×2+200×6+300×0.5+100×3)/1e6 = 0.00285`。这些都是合成价。

| 编号 | 配置与触发 | 精确预期（USD）/要验证的事实 |
|---|---|---|
| C01 | 只有渠道基础input覆盖4，其他null | `0.00405`；write仍继承3，不能随input变4 |
| C02 | interval `(500,1500]`：input绝对4且multiplier9，output multiplier0.5，cached null，write绝对0且multiplier2 | `0.00315`；absolute优先，显式0优先，总额不是0.02475 |
| C03 | interval `(1000,2000]`，全部桶×2，真实input=1000 | 未命中，`0.00285`；上一区间若为`(0,1000]`则恰命中其上界 |
| C04 | 同C03，input=1001，其余缓存/输出不变 | 普通输入601，基础`0.002852`，命中后`0.005704` |
| C05 | input=1000、output改100000；只配置`(1000,2000]`×2 | 不命中；输出不能把输入区间推高 |
| C06 | 全缓存input=1000,cached=900,write=100,output=0；区间`(999,1000]`读缓存绝对1、write绝对2 | 基数1000命中；`0.0011`，不把cache再次加到基数 |
| C07 | 无catalog基础，四桶全null；interval仅input×0（本站若允许） | unknown；即使用量0或倍率0也不能凭空造完整价格 |
| C08 | 无catalog，命中interval显式四桶`4/3/0.5/0` | `0.00315`；显式完整价可报价，gap则unknown |
| C09 | 配置LC input×2/output×3，真实input已超过LC阈值；没有interval | `0.0069`；缓存读写均随input倍率 |
| C10 | 同C09但存在有效interval `(2000,3000]`，真实input1000位于gap | `0.00285`；存在interval便抑制LC，未命中不恢复LC |
| C11 | 组gate=false；最低interval `(0,500]`只有input绝对1，高段从500开始 | 实际1000仍按selection_context=1取最低段，`0.00225`，不是高段；quote同时报告实际1000/有效选择1 |
| C12 | base standard/priority input `2/5`，渠道input覆盖4，再interval input绝对7；其余priority output12/cached1/write6 | priority输入保比例`7×5/2=17.5`；最终`0.0138` |
| C13 | 同C12但显式Fast=3 | priority不再取17.5，四桶标准`7/6/0.5/3`整体×3；`0.01755`，不能再叠catalog priority ratio |
| C14 | 基础整桶Flex=0.25；service_tier=flex | `0.0007125`；不叠既有默认Flex0.5 |
| C15 | C02区间价后渠道时段×0.5；组默认3、KEY覆盖1.5、订阅peak2 | base=`0.001575`，sales=`0.004725`；不是再乘组3 |
| C16 | C15，follow渠道×0.5、account采购override0.4（默认采购2被替代） | procurement=`0.000315`；不乘销售KEY1.5/peak2/默认采购2 |
| C17 | C16但KEY销售倍率0 | sales=0，procurement仍`0.000315`；不得反除0 |
| C18 | 基础价显式max-effort倍率4，真实effort=max | base=`0.0114`；effort=xhigh/缺失则`0.00285`，不能推断max |
| C19 | write未观测；选中价write≠input | unknown `cache_write_usage_not_recorded`；interval选择可展示已知总输入，不能把缺write当0 |
| C20 | base/interval价格`0.00000001`×倍率`0.00000001`，仅普通input=1 | 精确内部桶价`1e-16 / 百万`，费用Rat=`1e-22`；字符串展示可按12位舍入0，但follow/后续大倍率必须使用未舍入Rat |
| C21 | 历史snapshot后新增base价或渠道新版本 | 原列表/详情/统计/CSV/JSONL报价及source版本不变；新snapshot才采用新版 |

优先测试 Go per-record 与 PostgreSQL numeric 聚合逐条同值，并验证零/unknown覆盖计数。只有浏览器mock显示正确不证明继承公式正确；当前源码owner负责实际实现与最终freeze测试，本表用于独立交叉核对。
