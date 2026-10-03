# 有序采购策略与售价跟随

2026-09-18。本策略是现有采购费用 `account_cost` 的显式版本化选择器，不增加第四类金额，不改变网关路由/计量/账号状态。旧单目标规则仍保留历史语义；没有生效策略或策略 `enabled=false` 时继续使用旧规则。

## 完整目录

`GET /api/logs/cost-policy` 返回 `{version,items,kind:"configured_procurement_policy",currency:"USD"}`，包含全部不可变策略版本。`POST` 同一路径提交完整目录：

```json
{
  "version": 0,
  "effective_from": 1789680000,
  "enabled": true,
  "source": "Administrator-entered procurement agreement",
  "default_multiplier": "1",
  "rules": [{
    "id": "rule-1", "name": "Priority rule", "enabled": true,
    "account_ids": ["stable-account-id"], "group_ids": ["explicit-pricing-group-id"],
    "prices": [{
      "model": "gpt-*", "service_tier": "",
      "input_per_million": "1", "output_per_million": "4",
      "cached_per_million": "0.1", "cache_write_per_million": "",
      "intervals": []
    }]
  }],
  "follow_channels": [{"channel_id":"channel-1","multiplier":"1"}],
  "account_multipliers": [{"account_id":"stable-account-id","multiplier":"0.5"}]
}
```

结果 `{policy,kind}`。服务端生成策略 id、价格 id、created_at 和新 version。首版可以历史生效；后续必须基于最新 CAS，生效时间不早于提交时刻且严格晚于上一版本。发布与 `usage_cost_policy.update` 审计同事务，失败不提交；整个目录的增删和顺序一起保存。

限制：body 4 MiB、目录历史 8 MiB、100 版本；每版最多 100 条规则、1000 个总模型价格、每规则账号/组各 100 个、50 跟随渠道、500 账号倍率。稳定 ID 为 1–100 个 ASCII 字母/数字/下划线/连字符且相应范围内唯一；每规则目标至少一个、价格至少一个。来源最多 500 UTF-8 字节且必填，名称最多 200 字节；密钥样式会脱敏。单价/倍率沿用非负十进制字符串，真零有效。全部接口复用管理认证、同源会话和 no-store。

每条价格可携带 [采购 Token 阶梯](MANAGEMENT-PROCUREMENT-TIERS.md)，四桶与区间规范相同。自定义采购价不接受渠道分时字段或长上下文销售倍率，防止相同结构误用；JSON 严格拒绝未知字段。

## 真实匹配顺序

1. 以请求时间选择已生效的最高策略版本。没有或 enabled=false 时输出 `cost_source=legacy`，不重新解释旧单目标规则。
2. 规则按提交数组顺序扫描。账号 ID 或**显式 KEY 计价组**任一命中即属于目标范围；组来自同请求时刻生效的售价目录绑定，不从账号成员 Groups 选第一项。规则停用或范围不符则继续。
3. 每规则内先精确模型，再匹配通配价格的配置顺序，而非最长前缀；同模型模式的特定服务层优先通用层。不匹配当前服务层的条目不能提前占据顺序。规则范围匹配但无模型价时继续下一规则。
4. 找到明确价格后停止选择。缺用量/缺独立写缓存量等产生 unknown，不继续拿后面更便宜价格或售价掩盖。命中阶梯替换整次四桶后，乘账号覆盖倍率；未配置账号覆盖才用 default_multiplier，二者不相乘。
5. 没有匹配自定义价格时，只有显式绑定计价组关联的**启用渠道**列在 follow_channels 才跟随售价基数。渠道 ID 不取代理名、不要求选中价一定来自渠道：若组覆盖了渠道价，则跟随实际组价基数。

跟随基数包括实际选择模型/服务层的价格、长上下文和渠道分时，但排除售价的 KEY/组倍率及订阅高峰。采购最终金额 = 此精确基数 × 跟随渠道倍率 × 账号覆盖或默认采购倍率。不得从最终售价除回基数，因为售价倍率允许 0；不得把格式化的 base_amount_usd 重新转数字来放大。本实现保留精确有理数基数及四桶，中间不提前舍入。

没有显式跟随或源价格/用量未知时保持 unknown。采购倍率 0 仅在来源已知时得到真实零，不能将未知变为零。模式停用是新版本明确恢复旧规则，历史版本不删除。

## 报价、快照与统计

`POST /api/logs/cost-policy/preview` 接受 `{record_id}`，读取真实留存请求及当前已保存策略，返回 `{record_id,quote,scope}`。404 表示不存在/清理；未保存草稿不参与，无上游调用。原 cost-rules/preview 仍明确预览旧单目标规则，不把它说成有序策略报价。

`account_cost` 保留原状态、金额、四桶、实际阶梯，并追加 policy_id/policy_version、cost_source（legacy/ordered_rule/follow_sale_base）、matched_rule_id/matched_rule_order（从 1 开始）、group_id/channel_id、sale_policy_id/sale_policy_version。有序规则 target_type 为 ordered_rule，跟随为 channel。来源说明是采购策略来源，跟随的售价版本单列用于核对。

列表、详情、审核错误列表、账号/KEY/成员组/模型/端点统计、趋势、排行及全量导出使用同一选择逻辑。PostgreSQL 用参数化 JSON 目录和 numeric 在数据库聚合，复用旧采购/售价选择 CTE；不把全部明细传到应用求和。三费用域在同一个只读 REPEATABLE READ 中，新增/清理不会在本次多次聚合之间改变集合。

五分钟 as_of 额外保存有序采购目录；升级前生成的快照没有此字段时继续其旧采购语义。JSONL 输出可选来源字段，CSV 末尾增加九列策略/命中/关联版本。默认脱敏摘要化人工规则 ID、关联组和渠道，移除 source/supplier；显式身份导出仍秘密脱敏和公式转义。

## 验证

源码用手算覆盖销售倍率与高峰都为 0 时采购仍正确跟随、账户覆盖而非相乘、规则顺序与模型顺序、精确优先、服务层过滤、unknown 禁止回退、没有绑定禁止猜组、多版本恢复 legacy、微额精确放大与真实零。文件及 PostgreSQL 契约验证原子 CAS、审计回滚、全维度/CSV/JSONL、价格与记录快照。最终执行结果以 completion 候选报告为准，不用契约文件宣称已上线。
