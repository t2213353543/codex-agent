# 渠道与配置售价契约

2026-09-18。该目录是管理员配置的销售费用估算，不是实际收款、用户余额或 ChatGPT 订阅扣费。它独立于基础参考价 `cost` 和采购成本 `account_cost`，输出第三费用域 `sale_cost`；不改变网关选路、凭据、访问权限或账号组关系。

## 目录与原子版本

`GET /api/logs/sale-policy` 返回 `{version,items,kind:"configured_sale",currency:"USD"}`；items 为全部历史版本，version 是最新 CAS 版本。

`POST /api/logs/sale-policy` 提交整个新目录，body 如下：

```json
{
  "version": 0,
  "effective_from": 1789680000,
  "source": "Administrator-entered sales agreement",
  "timezone": "Asia/Shanghai",
  "channels": [{"id":"channel-1","name":"Sales channel","enabled":true,"model_pricing":[]}],
  "groups": [{
    "group_id":"stable-group-id","name":"Pricing group","enabled":true,
    "mode":"subscription","channel_id":"channel-1","rate_multiplier":"1.5",
    "peak":{"enabled":true,"start_time":"09:00","end_time":"18:00","multiplier":"0.5"},
    "model_pricing":[]
  }],
  "keys": [{"key_id":"stable-key-id","group_id":"stable-group-id","rate_multiplier":""}]
}
```

返回 `{policy,kind}`，policy 增加不可变 `id`、新 `version` 和 Unix 秒 `created_at`。第一版本允许历史生效时刻；后续版本必须基于最新 CAS，生效时间不早于提交时刻且严格晚于前一目录。旧版本不可修改。草稿删除条目只影响新目录；旧请求仍按旧目录匹配，不能物理删除历史后冒称费用不变。

全目录保存与 `usage_sale.update` 审计同一事务。CAS/生效时间冲突返回 409；审计失败不提交配置。所有路由使用现有管理员鉴权、同源会话和 no-store。网络提交结果不明时 UI 保留草稿并禁止自动重复发布。

限制：单次 body 最大 4 MiB；每版本最多 50 个渠道、100 个计价组、500 个 KEY 归属，总模型价格最多 1000 条；最多 100 个历史版本且整个管理文档不得超过 8 MiB。超限明确报错，不截断或自动删历史。稳定 ID 允许 1–100 个 ASCII 字母、数字、下划线与连字符。名称最多 200 UTF-8 字节，source 必填且最多 500 字节；自由文本进行秘密样式脱敏。

渠道、组和 KEY ID 各自在目录内唯一。组的 channel_id 必须为空或存在；KEY 只能绑定目录中的一个明确计价组。删除渠道或组前必须处理引用，不静默解绑。可以按已删除资源的稳定 ID 保留历史规则，显示名称只是人工快照，不做同名猜测。

## 模型价格

model_pricing 元素复用 [基础参考价格字段](MANAGEMENT-PRICING-API.md)：model、service_tier、四桶单价、长上下文阈值/输入输出倍率和可选 time_pricing。model 可精确或仅末尾 `*` 前缀；同容器内 model+service_tier 不能重复。四桶美元价格为非负十进制字符串，缓存写入可留空表示与普通输入同价。

渠道模型允许分时规则；组模型覆盖不允许携带渠道分时规则，避免命中组价格后仍叠加渠道时段价。每次发布由服务端生成价格版本 ID 并绑定目录生效时间和来源，不信任客户端传入内部 ID/生效时间。

## 实际选择顺序

1. 按 `RequestRecord.Time` 选择当时已生效的最高目录版本；无目录为 `sale_policy_not_configured`。
2. 按 `RequestRecord.KeyID` 查明确计价组。没有绑定为 `sale_group_unassigned`，没有或停用组为 `sale_group_unavailable`。不使用账号成员 `Groups`、名称或当前账号分组猜测归属。
3. 价格依次选择组模型覆盖、关联且启用的渠道模型价格、基础参考价格目录。组/渠道内先精确模型，再最长末尾前缀，再同模式特定服务层；基础目录保留原模型/服务层/生效版本规则。
4. KEY rate_multiplier 非空时覆盖组倍率；为空才继承组倍率。两者不相乘。允许真实 0，但用量或价格未知时不能变成零费用。
5. `mode=subscription` 且 peak.enabled 时，按目录显式 IANA 时区的同日 `[HH:mm, HH:mm)` 选择高峰因子，再与上述倍率相乘。非订阅组不能启用；跨天、相同起止和负值被拒；高峰倍率 0 有效。此模式是管理计价规则，不创建订阅账本。
6. 选中价格自己的长上下文/分时倍率先用于整次互斥 Token 桶，再应用组或 KEY 倍率与高峰因子。精确有理数计算后统一格式化到 12 位小数，不先舍入分桶再放大。

输入量已含缓存；普通输入=输入-缓存读-缓存写。reasoning 是输出子集。无 usage、负数或超范围缓存桶、独立缓存写价但无写量时均保持 unknown。

## 报价与统计

`POST /api/logs/sale-policy/preview` 接受 `{record_id}`，读取真实留存请求与当前已保存目录。结果 `{record_id,quote,scope:"retained_request_details"}`；不存在/被清理返回 404。草稿不参与预览，不发送推理或上游请求。

`sale_cost` 复用 status/reason/amount_usd/currency/pricing_id/pricing_model/source/breakdown/long_context/time_pricing，并追加：

- kind=`configured_sale`，policy_id/policy_version。
- group_id/group_name、channel_id/channel_name、key_id。
- price_source=`group|channel|catalog`；source 是**实际选中价格**的来源，基础目录回退时不是销售目录 source。
- rate_source=`key_override|group`、rate_multiplier、peak_multiplier。
- base_amount_usd 是应用组/KEY/高峰之前、但已含选中价格长上下文/分时的金额；breakdown 是最终全部倍率后的四桶费用。
- pricing_at 为请求 Unix 秒、local_time 为目录计价时区的 RFC3339 当地时间。

列表、详情、全量统计、趋势、分布、账号/KEY/分组排行都提供独立 `sale_cost_usd`、`sale_priced_requests`、`sale_unpriced_requests`。统计保留相同查询范围；部分费用可知时只加已知部分，并展示未定价数。无请求桶为零，有请求全未知为 null。

原 Groups 分布仍表示请求记录的账号成员组，并非唯一销售计价组；不能把多个成员组金额相加当去重收入。KEY 的计价组事实在报价里明确展示，记录 Groups 不被重写。

PostgreSQL 采用参数化目录及 numeric 在数据库聚合，不把全量请求搬到应用求和；参考、采购、配置售价三种费用共享只读 REPEATABLE READ。文件模式同扫描累加。五分钟 as_of 同时冻结记录成员、基础价、采购价和销售目录，详情/统计/导出不会在旧列表中偷换新版本。

CSV 追加 sale_* 金额、状态、来源和倍率字段；JSONL 输出完整白名单报价。默认脱敏清除来源/渠道组名称，并将组/渠道/KEY ID 摘要化；显式身份导出仍秘密脱敏并防 CSV 公式。

## 与参考的对应及剩余

已实现参考可用于 Codex 的渠道模型价格、组模型覆盖、组默认倍率、KEY 级明确归属与覆盖、订阅高峰、渠道分时及真实历史版本。本站没有终端 User 实体，因此 KEY 覆盖是明确管理映射，不冒称已有用户专属账务；无真实账本、余额或收款来源时不能宣称实际结算。

采购自定义阶梯见 [采购阶梯契约](MANAGEMENT-PROCUREMENT-TIERS.md)。本目录不启用模型映射/调度策略，也不把售价配置当账号网关规则。与参考渠道关联的账号成本“跟随售价”开关及规则排序/多目标覆盖语义仍应单独审查，不能因为已有独立采购规则就说完全相同。

实际执行证据由本轮 commercial 候选最终报告提供。单独的 API/helper/合成浏览器通过不代替 file/PG、不可变快照、并发 CAS/审计回滚及整套回归。
