# X09 剩余定价能力只读复核

2026-09-18。审查对象为本机 `../sub2api/current-source`，以下 `backend/`、`frontend/` 路径均相对此目录。只读核对实现、路由、保存校验和现有测试，没有运行参考程序、调用上游或修改已冻结候选。

结论：**组倍率、渠道模型售价、独立账号成本、组高峰和渠道分时倍率均有真实配置及 OpenAI/Codex 用量调用路径，不能因本站目前没有对应配置域而排除。** 但它们不是同一个功能，不能把渠道售价直接称为进价，也不能把 DeepSeek 官方峰谷预设套用到 Codex。

## 已确认的实际功能

| 能力 | 保存入口和关键符号 | Codex 路径的实际语义 |
| --- | --- | --- |
| 组倍率及用户专属组倍率 | `backend/internal/handler/admin/group_handler.go:188` 的 `rate_multiplier`；`backend/internal/server/routes/admin.go:343` 的组 POST/PUT、`:349` 的 `PUT /groups/:id/rate-multipliers`；UI `frontend/src/views/admin/GroupsView.vue:617`。 | `OpenAIGatewayService.RecordUsage`（`openai_gateway_usage.go:196`）选系统默认，再以 `ResolveUserGroupRateMultiplier` 解析用户×组覆盖；`user_group_rate_resolver.go:44` 明确用户覆盖优先，否则组默认，不是把用户与组倍率再乘一次。`billing_service.go:1541` 将分桶总价乘最终倍率得到 ActualCost，`openai_gateway_usage.go:418` 将倍率快照写日志。OAuth/Codex 未被类型条件排除。 |
| 渠道模型售价与组模型价覆盖 | `channel_handler.go:35` 的渠道创建/更新请求含 `model_pricing`；`channel.go:93` 的 `ChannelModelPricing`；`ChannelsView.vue` 的定价编辑器。`ModelPricingResolver.Resolve` 在 `model_pricing_resolver.go:71`。 | `calculateOpenAIRecordUsageTokenCost`（`openai_gateway_usage.go:700`）传 GroupID/Group 给统一计价。优先组模型价，之后关联活跃渠道模型价，再基础目录；组模型价命中后直接返回，故不会再叠加渠道时段价。`ChannelService.GetChannelForGroup/GetChannelModelPricing`（`channel_service.go:494/:544`）只取活跃渠道，组不能同时归属多个渠道。该价格首先是客户参考/销售计价基础。 |
| 独立账号成本，最接近“渠道进价” | `Channel.AccountStatsPricingRules`、`ApplyPricingToAccountStats`（`channel.go:69`）；`channel_handler.go:40` 保存 `apply_pricing_to_account_stats`、`account_stats_pricing_rules`；UI `ChannelsView.vue:267/:462` 可编辑；规则可选账号/分组和独立模型价格。账号倍率由 `account_handler.go:126` 的 `rate_multiplier` 保存，`Account.BillingRateMultiplier`（`account.go:158`）读取。 | `RecordUsage` 在 `openai_gateway_usage.go:473` 调用 `applyAccountStatsCost`。`resolveAccountStatsCost`（`account_stats_pricing.go:23`）顺序：先匹配自定义成本规则；否则开关开启则采用此次售价倍率前 TotalCost；否则尝试上游模型目录价；最后回退 TotalCost。规则按顺序，账号 ID **或**组 ID 命中，模型精确优先、后配置顺序的前缀通配。最终报表公式是 `COALESCE(account_stats_cost,total_cost) * COALESCE(account_rate_multiplier,1)`（`repository/usage_log_repo_stats.go:283`），与用户 ActualCost 分开。 |
| 订阅组高峰倍率 | `group_handler.go:205` 的 `peak_rate_enabled/peak_start/peak_end/peak_rate_multiplier`；`GroupsView.vue:1173` 仅订阅组显示；`Group.PeakMultiplierAt/ValidatePeakRateConfig`（`group.go:315/:336`）。 | **只有 subscription 类型组**启用后生效；使用服务器全局时区、分钟精度、当日 `[start,end)`，不支持跨天；允许 0 倍。`computePeakAwareMultipliers`（`group.go:393`）将组高峰因子乘 token 倍率；图片按次倍率不受影响。OpenAI `RecordUsage` 在 `:209` 调用，Codex 文本 token 请求确实可用。 |
| 渠道模型分时倍率 | `ChannelTimePricing`（`channel.go:113`）；`channel_handler.go:75` 接受 `time_pricing`；`TimePricingSection.vue` 经 `PricingEntryCard.vue:191` 提供 UI；`channel_service.go:693` 保存校验。 | `custom_channel_time_pricing.go` 的 `MultiplierAt`/`parseChannelTimePeriods`：显式 IANA 时区、每天或仅工作日、多段秒级 `[start,end)`、兼容 HH:mm；结束 00:00 表示当日 24:00；不支持跨天单段，禁止重叠；倍率至少 0.01、最多两位小数。`billing_service.go:254/:1428` 只在 `PricingSourceChannel` 下，将命中时段倍率应用于整个 token 费用。仅 token 模式允许；无时间或未命中为 1。与订阅组高峰可以叠加，但先命中组模型价则渠道时段不参与。 |

OpenAI 请求级定价时刻不是读取报表时刻。`openai_gateway_handler.go:822/:1404` 将 HTTP 的 PricingAt 传给记录任务，`:2967` 将 WebSocket 本轮 turn 的时刻传入；`openai_gateway_usage.go:128` 使用该时刻，缺失才回退记录时刻。因此跨峰谷边界的长请求不会中途换价。

## 必须保留的边界

- 自定义**账号统计成本规则不支持时段价**：`channel_service.go:715` 的 `validateAccountStatsPricingRules` 对含 periods 的 `TimePricing` 返回 `ACCOUNT_STATS_TIME_PRICING_UNSUPPORTED`。不能因结构复用 `ChannelModelPricing` 就宣称进价规则也支持分时。售价跟随开关可间接采用已经含渠道时段的 TotalCost，但那是另一条明确来源。
- 参考成本规则的自定义区间基数是输入+输出+缓存（`account_stats_pricing.go:214` 的 `calculateTokenStatsCost`），不同于 Codex 官方式长上下文的仅总输入阈值；不能复用本站新长上下文字段而改变含义。参考成本的 `<=0 → nil` 回退也不是本站“真实零与未知分开”的等价语义，新实现需明确保留零值，不能机械复制此回退。
- 账号 `BillingRateMultiplier` 支持 OAuth 账号手工配置；但 `upstream_billing_probe.go:77` 明确自动探测仅 API-key 账号，协议为上游中转 `/v1/sub2api/billing`。**这不是官方 ChatGPT OAuth/Codex 进价查询接口**。本站应允许带来源的手工成本配置，不伪造 OAuth 自动同步。
- `billing_service.go:1404` 的 DeepSeek 官方高低峰仅针对 DeepSeek 且默认目录来源；不能作为 Codex 官方时间价依据。订阅组高峰和渠道分时是管理员运营规则，不是官方 Codex 自动价格。
- 参考有持久支持：`migrations/101_add_account_stats_pricing.sql`、`158_add_group_peak_rate_multiplier.sql`、`225_channel_model_time_pricing.sql`。已读 `group_peak_rate_test.go`、`account_stats_pricing_test.go` 等对应断言；本次没有重跑参考库测试，结论属于源码调用链核验。

## 下一阶段可独立管理实现的范围

| 可交付模块 | 最小数据与来源 | 不改变网关的实现方法 |
| --- | --- | --- |
| 组/KEY 售价倍率版本 | 稳定 group_id、明确本次计价组、十进制倍率、生效时刻、来源。本站没有 Sub2API User 实体，不能直接冒充用户专属倍率。 | 新增管理计价策略文档与 CRUD/预览，保留不可变版本。KEY→唯一计价组或明确账号/组规则作为**管理估算归属**，逐条解释来源；重叠或无法确定时 unknown。本站 RequestRecord.Groups 在 `gateway.go:579/:738` 与 `gateway_websocket.go:494` 是账号多组成员快照，不能任选首组、所有组相乘或当作真实路由计价组。 |
| 渠道/账号成本账本 | 人工合同/采购价格来源、供应来源 ID、关联账号 ID、模型、输入/读缓存/写缓存/输出价格、可选账号成本倍率及生效版本；真实账单若有则另带采集来源。 | 复用 encrypted managementState 存独立成本规则与来源资源，不把代理名当渠道、不探测地址、不触账户调度。先按稳定 account_id 命中，再显式管理组规则；参考成本与用户侧估算分别输出，未配置成本不默认等于收入。提供完整 CRUD、顺序/冲突预览、导入导出和版本说明。 |
| 参考售价时段规则 | IANA 时区、工作日开关、有界非重叠 periods、十进制倍率、来源、版本。本站原生 HTTP/WS 的 RequestRecord.Time 已在请求/turn 起点记录，可用作明确标注的估算定价时刻；历史导入数据需声明时刻来源。 | 管理价格规则增加 `time_pricing`，按记录时间计算，冻结 quote 中实际时区/窗口/倍率，与已完成长上下文和服务层规则组合；零/未知不混淆。无需新增订阅扣费、预算或利润门。组高峰若要逐项等价复刻，先增加明确的管理计价模式字段，不能给普通组套“订阅”语义。 |

三个模块可共用版本化管理状态，无需为了它们先改网关。最终必须覆盖 file/PG 全量聚合、单条、同快照 CSV/JSONL、历史版本、价源/倍率/时段解释；在同一 quote 明确分开基础参考价、用户侧配置估算和账号采购成本。测试至少包括多组冲突、已删资源历史绑定、用户替代组而非相乘、输入含缓存、缺写入桶、价与倍率为零、窗口起止/午夜/周末/DST、跨窗口请求、未匹配及无来源。新功能未实现前，X09 这些子项应标为**已确认待实现**，而非“不适用”或“已完成”。
