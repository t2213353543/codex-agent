# 账号采购成本管理接口

2026-09-18。人工配置的 USD 采购估算独立于参考售价、订阅扣费和网关计量。没有修改网关账号调度、请求内容或额度；不向 OAuth 上游自动探测价格。当前实现复用现有加密管理状态，不增加数据库迁移。

## 规则与版本

`GET /api/logs/cost-rules` 返回 `{items,kind:"configured_procurement",currency:"USD"}`，items 包含全部不可变版本。复用管理认证、同源会话和 no-store。

`POST /api/logs/cost-rules` 创建一个新规则或追加后续版本：

```json
{
  "rule_id": "",
  "version": 0,
  "enabled": true,
  "target_type": "account",
  "target_id": "stable-account-id",
  "model": "gpt-*",
  "service_tier": "",
  "effective_from": 1789680000,
  "source": "Administrator-entered purchase agreement",
  "supplier": "Supplier name",
  "input_per_million": "1",
  "output_per_million": "4",
  "cached_per_million": "0.1",
  "cache_write_per_million": "",
  "multiplier": "1"
}
```

结果 `{rule,kind}`。rule 包含稳定 `rule_id`、该版本独有 `id`、递增 `version`、`created_at` 和全部配置。时间为 Unix 秒；单价/倍率是最多 7 位整数、8 位小数的非负十进制字符串，倍率允许真实 0。留空缓存写入单价表示与普通输入相同，不代表写入量为 0。

- 首版必须 `rule_id=""`、`version=0` 且启用，可指定历史生效时间。
- 后续必须提交原 rule_id 和最新 version，生效时间不得早于提交时刻且必须晚于前一版本；身份字段 target_type/target_id/model/service_tier 不可改变。
- 同一身份只能有一个规则序列；重复新建、过期版本或修改身份返回 409。前端保留草稿并要求重新核对，网络结果不明时不自动重发。
- 停用是新增 `enabled=false` 版本，不删除历史，也不停止账号本身。历史请求仍按当时已生效版本计算。
- source 必填，supplier 可空，两者是人工文本而非认证/采购协议连接。秘密样式会脱敏，不应输入账号密码或令牌。
- 整个目录最多 2000 个版本。修改与 `usage_procurement.update` 审计同事务；审计失败不提交目录。

## 匹配与计算

每条请求首先按请求时间选每个规则当时的版本，再考虑启用状态。目标仅稳定账号 ID 或明确分组 ID，不用名称匹配；账号规则优先于分组规则。模型先精确匹配，再选择最长末尾 `*` 前缀（单独 `*` 匹配全部模型），同模型模式的特定服务层优先通用服务层。已观察上游模型优先，缺失才使用请求模型。

请求 Groups 是账号成员快照，不是已经证实的唯一计费组。无账号规则且多个组同时匹配时返回 `ambiguous_cost_rules`，不能按顺序选择、全部相乘或冒称真实路由计价组。单一组规则的结果也是明确的管理采购估算。

普通输入 = input_tokens - cached_tokens - cache_write_tokens。四个互斥桶分别按费率计算，再乘采购倍率；精确有理数运算完成后才格式化到 12 位美元小数。缓存写入量未知且价格不同、桶非法、没有 usage 证据时保持 unknown，即使倍率是 0 也不能把未知变成零。

采购规则不自动回退参考售价或官方订阅账单，不启用渠道售价分时配置；参考实现的自定义账号成本规则也不允许分时价格。参考长上下文价与本采购成本是两个独立规则域，不能混用阈值语义。采购的自定义费用阶梯、独立渠道实体、显式售价计费归属及用户/组售价倍率仍属于后续范围。

## 输出与预览

`POST /api/logs/cost-rules/preview` 接受 `{record_id}`，仅读取真实留存请求及当前已保存规则，返回 `{record_id,quote,scope:"retained_request_details"}`；不存在/已清理返回 404，不发送推理，不将未保存草稿当已生效规则。

列表、详情、JSONL 中独立 `account_cost` 包括：

- `kind:"configured_procurement"`、`status:known|unknown`、`reason`、`currency:"USD"`、`amount_usd:string|null`。
- `rule_id/rule_version`、`pricing_id`（版本 ID）、`target_type/target_id`、`source/supplier`、`multiplier`。
- `base_amount_usd` 为倍率前总额，`breakdown` 为应用倍率后的四桶费用；不能再乘一次。

统计总量、模型/端点/上游模型/错误类别、账号/KEY/分组与趋势分别追加 `account_cost_usd`、`costed_requests`、`uncosted_requests`。金额只含可计价部分，unknown 不参与金额相加；空请求桶是真实零，存在请求但全部未知时金额 null。原 estimated_cost_usd/priced_requests/unpriced_requests 仍只表示参考售价，不能改名替换。

PostgreSQL 采购聚合在数据库内完成，与原统计在同一只读 REPEATABLE READ 事务中，避免两次 SQL 之间新请求导致计数与金额口径不一致。文件模式在同一次明细扫描中累计，未限制为当前页。

使用记录的 `as_of` 同时冻结参考价和采购目录。列表、统计、导出及携带相同过滤/as_of 的详情都使用该版本；详情还校验记录属于快照，后增记录返回 404、筛选不符或过期返回冲突。独立错误处理工作区和采购预览没有快照，明确使用当前已保存规则。

CSV 末尾追加采购金额、状态、原因、规则/版本、目标类型/ID、倍率和来源。默认脱敏导出清除采购来源与供应方、摘要化目标 ID；显式身份导出仍做文本秘密脱敏和公式转义。

## 验证边界

`TestManagedProcurementArithmeticAndMatching` 验证微额金额不提前舍入、零与未知、稳定 ID、多组歧义、模型优先级和停用时间。`TestManagedProcurementContract` / `TestPGManagedProcurementContract` 验证真实路由、CAS 并发、审计回滚、快照计价与详情成员约束；`TestManagedProcurementStats` / `TestPGManagedProcurementStats` 对照全量维度、导出及同 MVCC 快照。运行结果另以最终候选清单为准，不能用测试名称代替实际已执行证据。
