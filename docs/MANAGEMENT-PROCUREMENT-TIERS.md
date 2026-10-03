# 采购参考成本 Token 阶梯

2026-09-18。仅增加管理采购成本规则能力，不改变网关、实际扣费、销售规则、分组调度或默认官方价格。

## 参考语义

本机 Sub2API `backend/internal/service/account_stats_pricing.go:214` 的 `calculateTokenStatsCost` 以普通输入 + 输出 + 缓存写入 + 缓存读取为区间基数；`channel.go:185` 的 `FindMatchingInterval` 实际条件为 `total > min && (max == nil || total <= max)`，即 **(min,max]**，不是左闭右开。本项目输入已经含缓存，等价基数为 `RequestRecord.InputTokens + OutputTokens`，不能再加缓存。

命中区间后按该档四桶绝对价计算整个请求，之后乘规则已有的 `multiplier`；不是逐段累加，也不是官方长上下文仅输入阈值。未命中，包括总量为 0 或落在区间空隙，沿用原规则基础价格。无 intervals 的旧规则行为不变。

## 规则接口

已有 `GET/POST /api/logs/cost-rules` 增加可选 `intervals`，null/省略/空数组为关闭。示例为合成配置，不是官方报价：

```json
{
  "intervals": [
    {
      "min_tokens": 0,
      "max_tokens": 100,
      "input_per_million": "1",
      "output_per_million": "2",
      "cached_per_million": "0.2",
      "cache_write_per_million": "1"
    },
    {
      "min_tokens": 100,
      "max_tokens": null,
      "input_per_million": "2",
      "output_per_million": "4",
      "cached_per_million": "0.4",
      "cache_write_per_million": "2"
    }
  ]
}
```

每条规则最多 32 个区间。min 是 0–9007199254740991 的整数，max 为 null 或大于 min 的同范围整数。保存按 min 升序规范化；禁止重叠、重复区间及无上界区间之后仍有区间，允许相邻区间和空隙。四桶价格全部必填非负十进制字符串，沿用最多 7 位整数/8 位小数，允许真实 0；阶梯内空写入价格不会沿用基础价。旧 flat 的 cache_write 留空沿用 input 的行为保持不变。

参考采购路径把缺价字段默认为 0，并会把零成本退回后备价格；本站要求四价完整且保留真实零，防止缺失配置被误当免费或真实零被覆盖。未加入参考采购路径没有实际采用的区间倍率、time_pricing 或缓存 TTL 子桶。

区间属于原有不可变成本版本。新增规则 version=0；追加版本使用最新 version CAS、保持 target/model/service_tier 身份、未来生效时间；改变、移除 intervals 或停用都通过追加版本，不更新已存在历史版本。相同时间/CAS 冲突仍返回 409。

## 报价解释与未知值

配置了 intervals 时，`account_cost` 增加 `token_tier`：

```json
{
  "basis": "input_plus_output",
  "boundary": "(min,max]",
  "total_tokens": 110,
  "matched": true,
  "reason": "matched",
  "interval": {
    "min_tokens": 100,
    "max_tokens": null,
    "input_per_million": "2",
    "output_per_million": "4",
    "cached_per_million": "0.4",
    "cache_write_per_million": "2"
  }
}
```

未命中为 matched=false、reason=outside_intervals，省略 interval。无用量、桶为负或重叠、总量超过安全整数上限时，total_tokens 为 null，reason 分别为 usage_not_recorded/invalid_usage_buckets，成本保持 unknown；先检查再求和，避免 int64 溢出。缓存写量未记录且命中区间写价与普通输入价不等时，仍是 cache_write_usage_not_recorded，不能擅当 0；已知总量的实际区间仍可展示。

零价格或规则 0 倍率仍是已知 0，并保留真实档位说明。`base_amount_usd` 表示应用已选基础/区间四桶价后、规则倍率前的金额，不是未命中档的基础价格金额。报价的 rule_id、rule_version、来源和 supplier 语义不变。

## 一致性与验证

文件计算和 PostgreSQL numeric 使用同一总量、严格边界和四桶价格，继续在销售/采购同一只读事务快照内聚合。单条、列表、统计及 JSONL/CSV 金额一致；JSONL 自动保留 token_tier，CSV 的可解释阶梯列由导出 owner 接入。已有管理快照保存完整区间版本，后续改价不改变该快照；删除关联账号也不按名字重新绑定规则。

`admin_usage_procurement_tier_test.go` 覆盖区间上下界、缓存基数、全缓存、空隙回退、无上界、真实零、未知写入、桶错误与大整数、配置规范化/拒绝、API 版本冲突，以及 file/PG 全量维度、详情、JSONL/CSV 和持久快照。输出使用 `.local/management-parity/commercial-20260918/`，旧冻结目录保持不动；最终验证状态以该轮测试报告为准。

本轮专项结果：本地采购 6 个顶层 / 29 个含子测试用例全部通过；真实 PostgreSQL 16.15 的 `TestPGManagedProcurementTiers`、`TestPGManagedProcurementContract`、`TestPGManagedProcurementStats` 全部通过，无跳过。数据库为现有本机镜像启动的一次性 tmpfs 容器，绑定随机 loopback 端口，密码仅内存传递，执行后已删除；没有连接生产、真实账号或上游。证据为该目录 `local-procurement-tier-summary.json`、`local-docker-pg-procurement-tier-summary.json` 与各自 tests.jsonl，绑定本轮三个 Go 文件的 SHA256。最终联合销售模块、CSV 解释列与浏览器仍由主任务合并验收。
