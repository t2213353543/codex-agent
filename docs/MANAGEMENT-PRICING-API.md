# 使用记录参考价、长上下文与分时倍率

更新：2026-09-18。本契约补充 `MANAGEMENT-PARITY-BACKEND.md` 的管理参考价；不代表订阅账单、余额扣除或实际结算，不改变网关、分组倍率或请求内容。系统不预置、猜测或自动抓取官方价格。

## 配置版本

继续使用 `GET /api/logs/pricing` 和 `POST /api/logs/pricing`，要求现有管理鉴权并返回 `Cache-Control: no-store`。原模型、服务档位、普通输入/输出/缓存读/缓存写价格、来源和生效时刻保持原有语义。新增可选字段：

| 字段 | JSON 类型 | 语义 |
| --- | --- | --- |
| `long_context_input_token_threshold` | 整数 | 0 或缺失关闭；允许 0–9007199254740991。等于阈值使用基础档，严格大于才可能选择长上下文档。 |
| `long_context_input_cost_multiplier` | 十进制字符串 | 长上下文普通输入、缓存读、缓存写统一倍率。空或数值 0 视为 1。 |
| `long_context_output_cost_multiplier` | 十进制字符串 | 长上下文输出倍率。空或数值 0 视为 1。 |

倍率沿用价格十进制格式：非负，不接受科学计数法、NaN、负数；最多 7 位整数与 8 位小数。阈值为正且至少一个有效倍率大于 1 时才启用阶梯。`0 < multiplier < 1` 保留原值：例如输入 0.5、输出 2 可启用并分别采用 0.5/2；两侧均不大于 1 时整体不启用。倍率 0 表示未配置，价格 0 则是真正零价格，两者不能混淆。

阶梯字段属于不可变规则版本。相同版本内容重放返回原 ID；同生效时间修改阈值或任一倍率返回 409。已存在模型的新版必须声明后续生效时间，原版本不会被改写。新字段缺失的旧规则保持原行为。管理持久 JSON 文档自动兼容这些可选字段，无新增数据库迁移。

## 输入基数与计算

实现依据是本地 Sub2API 参考源码 `backend/internal/service/billing_service.go` 的 `shouldApplySessionLongContextPricing`、`longContextMultiplierOrOne`、`computeTokenBreakdown`，以及 `pricing_service.go` 的显式阈值/倍率覆盖语义。

参考源码的普通输入桶不含缓存，因此参考表达式为普通输入 + 缓存创建 + 缓存读取。本项目 `RequestRecord.input_tokens` 已经包含缓存读写，阈值直接比较这个字段，绝不再次加缓存。

先按实际返回模型、实际服务档位和记录时刻选择已有参考价版本，再判断长上下文阶梯。超过阈值后，整个请求的普通输入、缓存读、缓存写分别按原价乘输入倍率，整个输出桶按输出价乘输出倍率；不是只对超过阈值的 Token 加价。普通输入数量仍为 `input_tokens - cached_tokens - cache_write_tokens`。所有运算使用精确有理数或 PostgreSQL numeric，金额输出为 12 位小数。

未记录 usage、负数/重叠桶、未配置实际模型价保持 unknown。缓存写数量为 null 且其价格与普通输入价不同时，继续返回 `cache_write_usage_not_recorded`；阶梯不把缺失桶当零。缓存写价缺失时沿用普通输入价，已记录的 0 写入量保持已知零。

## 单条费用解释

已有 `cost` 中的来源、规则 ID、生效时间、分桶金额保持原含义。配置了阶梯字段的规则额外返回：

```json
{
  "long_context": {
    "enabled": true,
    "tier": "long_context",
    "threshold": 100,
    "input_tokens": 101,
    "comparison": "gt",
    "input_multiplier": "2",
    "output_multiplier": "1.5"
  }
}
```

这是合成演示参数，不是任何模型的官方价格。`tier` 为 `base` 或 `long_context`；两个倍率是本次请求实际适用值，基础档为 1/1。零价格仍可命中长上下文档，`tier` 解释选价条件而非声称有实际扣费或正附加费。缺失 usage 或未找到费率时无法选档，不补猜测解释；写入桶不完整而费用未知时仍可解释已知输入对应的档位。完全未配置阶梯的旧规则省略整个 `long_context`。

文件模式单条/汇总与 PostgreSQL 聚合采用相同计算。JSONL 导出保留同一 `cost`，CSV 金额沿用同一计算；CSV 档位解释列由导出模块追加。现有使用记录快照一并保存当时的完整费率版本，包含可选阶梯字段。

## 验证范围

`admin_usage_pricing_long_context_test.go` 验证阈值前一位、等于阈值、超过一位、全缓存输入、普通/读/写/输出各桶全量倍率、零价格、0 倍率回退、缺一侧倍率、正小数倍率、禁用条件、未知及显式零写入、旧规则和未来版本、API 校验与不可变重放。相同合成记录同时用于文件和真实隔离 PostgreSQL 的全量汇总、模型/账号/KEY/组/时间桶、单条与 JSONL/CSV 一致性测试。

这组测试不使用真实账号、不调用上游、不连接生产。运行证据位于 `.local/management-parity/long-context-pricing/`；最终 PG、联合源码归档与发布状态以主任务验收记录为准。

## 分时参考价

规则版本可选 `time_pricing`。这是有来源的管理参考价策略，不是官方 Codex 时段价或账户采购成本规则；来源继续由规则顶层 `source` 保存。依据 Sub2API `custom_channel_time_pricing.go`、`billing_service.go` 的渠道 token 时段语义实现，应用于本站既有管理参考价，不新增订阅结算或改变网关。

```json
{
  "time_pricing": {
    "timezone": "Asia/Shanghai",
    "weekdays_only": true,
    "periods": [
      {"start_time": "09:00", "end_time": "12:00", "multiplier": "1.25"},
      {"start_time": "22:00", "end_time": "00:00", "multiplier": "0.50"}
    ]
  }
}
```

样例只是运营规则演示。timezone 必须为明确 IANA 名称，拒绝空、Local 或未知名称；periods 为 1–48 段，缺失、null 或空列表表示关闭。时间允许 HH:mm 或 HH:mm:ss，保存统一为 HH:mm:ss、按开始排序。区间左闭右开，结束 00:00 表示当日 24:00；单段不支持跨天，不能起止相同或重叠，可配置相邻时段。全天策略可拆成 00:00–12:00、12:00–00:00 两段。倍率是 0.01–9999999.99 的十进制字符串，最多两位小数，规范保存时去掉尾零；0 不表示免费，也不允许以 0 关闭时段。

按 `RequestRecord.Time` 转换到配置时区后判断本地星期及秒钟，`weekdays_only` 排除当地周六/周日。夏令时重复时刻按各自 UTC 偏移落到相同墙钟窗口，跳过的墙钟时刻不会凭空创建记录。未命中窗口或周末实际倍率为 1。命中则在服务层和长上下文选价之后，对四个完整金额桶乘同一个精确倍率；按记录起点计价，不读取当前浏览器或查询时刻。

单条 `cost.time_pricing` 返回 `enabled`、`matched`、`timezone`、`weekdays_only`、`pricing_at`、含偏移的 `local_time`、实际 `multiplier`、固定来源类型 `source:"configured_reference"`、`reason`。reason 为 matched/weekend/outside_period；匹配时另有 `period:{start_time,end_time,multiplier}`。旧无规则省略整个字段。正常 API 拒绝非法配置；损坏的历史配置会保留 `invalid_configuration` 提示、enabled=false、实际倍率 1，并省略 local_time/period，避免 PG 与文件模式不一致。未知用量仍未知，时段规则不能补造 Token。

规则时区、工作日和任一窗口都属于不可变版本；同一生效时间修改返回 409，格式不同但规范化等价的重放仍返回原版本。使用记录快照保存完整分时配置，新增后续费率不会改变已有快照；file/PG 汇总、单条及 JSONL/CSV 使用相同金额计算。嵌套 source 仅固定类别，不复制可能含身份信息的管理员来源备注，保持脱敏导出约定。

新增 `admin_usage_time_pricing_test.go` 覆盖秒级边界、相邻窗口、午夜、工作日时区、纽约春季跳时及秋季重复时刻、长上下文组合和四桶金额、零价、未知写入桶、配置校验、版本不可变和持久冻结快照。新验证输出使用 `.local/management-parity/pricing-next/`；先前已冻结目录不变。
