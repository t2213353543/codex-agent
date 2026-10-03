# 第三阶段：筛选统计与账户批量精确加速

前两阶段的基础 time/status 汇总不能回答任意组合过滤。不同维度的边际统计不含共同出现关系，不能把“模型 A 的计数”直接套到账号、KEY、group 等其它维度。

第三阶段 burst10m 基线确实发现未覆盖的慢路径，`.local/performance-slo/20260929t000113-3d9c1d/report.json` 保留失败证据：全匹配模型和 KEY 统计约 15 秒 HTTP 500；error+model 约 11.642 秒；100 账号批量用量约 7.960 秒。此前基础查询通过不代表这些筛选已通过。

## 受支持筛选的精确等价证明

`stats_rollup_filter.go` 没有新增模型组合 cube，而是在同一 SQL 快照中读取精确 rollup source，证明单个筛选的实际情况：

| 证明结果 | 执行路径 |
| --- | --- |
| 目标维度没有请求 | 返回原有空统计结构 |
| 选中范围所有请求都满足筛选 | 使用基础全维汇总，结果与原筛选完全等价 |
| 只有部分请求匹配 | 执行原始精确过滤 SQL |

支持 Model、Endpoint、KeyID、GroupID 最多四个 AND 条件，可叠加 time/status。所有条件共用一个 source 聚合：只要任一条件的目标在边际中不存在，就可证明 AND 结果为空；只有每个条件都对整个范围普遍成立，才能证明完整选择等于基础范围。若各目标分别存在但至少一个只部分匹配，仍执行原始联合过滤，不能将“边际都存在”误当作共同出现。

其它字段、snapshot/review、内部诊断 queryer 等仍保持原始路径。正常的混合模型范围仍会触发原始查询，本改动不能宣称任意模型或多维过滤都具有常量延迟。

Model/Endpoint 是每请求单值维度，证明同时检查有目标值与没有其它值。KeyID 的 true-ID 和 legacy-name 类型分开检查：legacy 请求一定阻止全匹配证明，合法以 `name:` 开头的真实 ID 不会被误认为 legacy。

GroupID 是多值维度，不能使用“没有其它 group”作为判断。已有维度生成函数对每请求的 group 去重，因此目标 group 的请求总数等于总请求数，才证明每个请求都属于这个 group。其它 group 可以同时存在；无 group、重复 group、变更或删除均由精确 source 反映。

Gate、基础汇总和原始分支在同一条 SQL 中通过 MATERIALIZED gate 与 guarded LATERAL 连接，没有先查询后读取的竞态、TTL 缓存或近似结果。未选分支必须在实际计划中为零 loops，不能只依赖小 fixture 测试看起来快。

## 账户批量投影

账户 batch 原筛选条件为 `account_ref = ANY(ids)`，而 009 账户维度基于 JSON `account_id`。历史导入或人工修正可能使两者不同，直接对维度 ID 筛选会漏记或混入别的账户。

013 仅新增一个部分索引：`request_logs_account_identity_mismatch_time_idx(created_at)`，索引条件为 `account_ref IS DISTINCT FROM COALESCE(record->>'account_id','')`。迁移由在线索引执行器处理，不修改旧迁移。稳定 SHA256 为 `ff313efec91ed62417f1b5a504b92523b36e45d006eb45a7ffb8f849245e15ca`。

同 SQL 快照中若选定时间/status 范围不存在任何身份不一致记录，`accountsOnly + AccountIDs` 才直接读取账户维度；否则整个 batch 保留原始 SQL。Guard 保守地检查范围内所有身份不一致，而不仅是请求 IDs，防止某个未选择 account_ref 的 JSON ID 冒充被选择账户。legacy ref、反向漂移、空 ID 和真实 `name:` 前缀均有回归覆盖。

该索引只存异常记录，正常一致写入不增加索引项；迁移仍需读取旧表判断条件，不能把在线构建称为免费或立即完成。没有新增高维 cube 或每请求额外同步聚合。

## 已完成的验证

`.local/performance-slo/20260929t000614-21645e/report.json` 全部通过：Model/Endpoint 的 6 种时间粒度和 3 个时区；空表、全匹配、混合值、长 group/亚秒 unsafe、删除后恢复、未命名字段；真实 KeyID 与 legacy 同名区分；repeatable-read 快照下并发提交；账号身份双向漂移、legacy、缺失、原 account projection。

`.local/performance-slo/20260929t002655-284b34/report.json` 在原 burst10m fixture、`force_generic_plan` 下验证实际统计方法捕获的 SQL：

- 全模型、全 KEY、error+model：原始大扫描分支均为 0 loops；只有 first/last 时间索引各实际读取 1 行。
- 缺失模型：全部 request_logs 节点均为 0 loops。
- 该轮与 uniform 构造维护并行，EXPLAIN 的 161/172/22 ms 等时间只属于结构诊断，不作为独占 HTTP/SLO 验收。

GroupID 新增回归涵盖重复成员、多组共存、非成员加入/删除、成员改写、groups 字段缺失与恢复。真实最终过滤 HTTP、prepared statement 多次切换与 batch100 分位数需由协调中的最终重复采样给出；完成前不把上述计划结构证明当成全部过滤响应达标。

多条件回归进一步构造“模型 main 只出现在 responses、模型 other 只出现在 compact”的数据：main 和 compact 两个边际分别有值，但 `model=main AND endpoint=compact` 必须走原始分支并返回 0。还覆盖四条件全部成立、部分成立、任一缺失、未知 KEY/名称/组、真实 `name:` ID，以及删除冲突记录后恢复全匹配证明。对应短功能验证记录在最终总体报告引用，不用小数据功能通过替代千万行延迟验收。

更一般的 co-occurrence 筛选仍需要精确联合投影、具备正确条件覆盖的汇总或原始 SQL。下一步应按实际慢请求与写放大成本选取设计，而不是直接构建几十字段的巨大 cube。

100k、两模型各 50%、全部 1000 账户都同时包含两模型的后续基线已经执行，精确功能通过但 raw 分支实际聚合 50k 明细。单模型方法观察 4117.669 ms，模型+KEY+group 为 4167.804 ms；随后 EXPLAIN 分别 1341.709/1643.396 ms。该轮与 uniform 构造维护并行，不能称独占 SLO 样本，但明确不能声称部分匹配过滤已经达到 1 秒。详细证据、联合事实设计及剩余目标列表见 `docs/PERFORMANCE-JOINT-FACTS-DESIGN-2026-09-29.md`。
