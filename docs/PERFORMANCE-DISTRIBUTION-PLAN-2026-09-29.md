# 性能证据的分布边界与下一种验证方案

此文件保留最初的只读设计与待执行方案。该方案后来已实际执行：uniform100k/1M 发现 SQL 分别 1.878/9.280 秒的瓶颈，随后 010 日层和查询优化后的正式 HTTP 均通过 1 秒门槛。最新实现与证据见 [日层报告](PERFORMANCE-DAILY-ROLLUP-2026-09-29.md)和 [第二阶段最终验收](../integration/performance/results/slo-20260929-phase2-final.md)。以下“待执行/未执行”描述的是设计记录当时状态，不覆盖后续验收。

## 当前千万行证据能够覆盖什么

当前合成数据有 10,000,000 条请求、1,000 个账户，分布于 30 天，但每日集中在一分钟附近。`usage_minute` 的账户 × 分钟组合只有约 30,000。模型/KEY 等维度也保持较低基数。这是高聚合压缩率的 burst 分布。

009 读取成本取决于选中范围中的 `retained_usage_minute` 实际行数、unsafe 分钟数、返回维度基数和排序/合并成本。原始 `request_logs` 行数只是影响因素之一：同样的千万行若均匀分布、账户维度每分钟很少重复，账户分钟汇总可能达到数百万或千万行。每行 34 个 numeric 指标及索引键的读取量可能进入数百 MB 至数 GB。

因此当前结果应表述为“千万原始请求、1000 账户、30 天 burst 分布的指定接口/并发/资源配置”，不能写成普适的“任意千万级数据全部低于 3 秒”。除了原始行数，最终报告应记录原始到汇总的压缩比、汇总行数和表/索引大小。

## 最小第二种分布

保持现有字段、账户数、模型数、KEY 数、错误率及资源配置，仅改变时间与账户组合分布。先 100,000 行 smoke，再 1,000,000 行；分别建立新的 synthetic schema，不能向原 burst fixture 混入数据。

对从 0 开始的 `i` 使用以下可复现公式：

```sql
minute_slot = (i * 7919) % 43200
account_no = ((i / 43200) + 37 * (i % 43200)) % 1000 + 1
created_at = to_timestamp(anchor - 30 * 86400 + minute_slot * 60 + (i * 17) % 60)
```

`7919` 与 `43200` 互质，因此每 43,200 个请求遍历一次 30 天的全部分钟。同一时间分钟在下一轮使用不同账户，而不是因两个简单取模的公约数让账户与时间重复关联。

已经在本地纯 Python 验证公式，无数据库请求：

| 原始行数 | 不同账户 × 分钟 | 不同分钟 | 每账户最少/最多请求 |
| --- | ---: | ---: | ---: |
| 100,000 | 100,000 | 43,200 | 99 / 102 |
| 1,000,000 | 1,000,000 | 43,200 | 995 / 1004 |

这使新增账户维度汇总的数量有清晰的下限，不会再次把“均匀时间”误做成高重复的账户分钟组合。首轮保持 groups/模型等字段与 burst 相同，以便把差异归因于分布；高 groups 基数或动态模型名应另列后续变量。

计划复用当前真实 HTTP 路由、认证、JSON 编码和同机隔离 PostgreSQL runner。先在全部迁移已安装的空 schema 写入，测带触发器的数据构造耗时；若另测旧数据回填，必须新建独立 schema，不能将 bulk 构造时间冒充逐请求 ingestion 吞吐。

最小请求集：完整 30 天 stats、30 天 overview、model/account options、coverage、默认日志页、请求 ID 精确检索、带精确总数的基础状态日志页。stats 各补 UTC 与 Asia/Shanghai 的日粒度，避免只验证 UTC 整日边界。每个请求验证总计、维度计数和 ID，错误响应不得计作“快”。

记录首次请求，再记录并发 1/4 的至少 20 个样本，保存 P50/P95/P99、最大值、失败率、DB acquire 数和响应大小。物理冷缓存未控制时应继续明确标注。正式请求阶段不得与 race、其他 fixture 构造或迁移重叠。

同时记录这些容量字段：

```sql
SELECT count(*) FROM request_logs;
SELECT count(*) FROM usage_minute;
SELECT kind, count(*) AS rows, count(*) FILTER (WHERE requests > 0) AS live_rows
FROM retained_usage_minute GROUP BY kind;
SELECT count(*) FROM retained_usage_unsafe_minute WHERE requests > 0;
SELECT pg_relation_size('retained_usage_minute'), pg_indexes_size('retained_usage_minute');
```

对 stats 真实 SQL 捕获一次 `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)`，特别关注汇总扫描行数、临时读写、Aggregate 内存、JSON 边缘扫描量。计划执行结果与 HTTP 样本分开记录，避免重复 EXPLAIN 预热后冒充首次请求。

## 后续多分辨率精确汇总

新增 010 或之后的迁移，保留 009 源文件及 checksum。新增每小时、每日维度汇总；建议从已解析的分钟 numeric delta 派生，避免重新解析整份 request JSON。新增触发器可在分钟表 transition tables 上合并 OLD/NEW 的差量，再分别聚合到小时/日。

计数仍使用相同 status、kind、resource_id、name 键以及 sum/sample 字段。删除、更新、truncate、旧名字消失、image overflow 恢复应与分钟表相同。多层锁顺序必须固定，且与 008/009 的顺序一致。每个事件多两层维度 upsert 会增加写放大，不能只测读取收益。

范围读取采用互斥分解：完整 UTC 日使用日表，首尾不完整日使用小时表，不完整小时使用分钟表，不完整分钟读取明细。每条请求只能贡献一次，不允许 day/hour/minute 重叠后相加。

总计及账户/KEY/模型等独立维度只需覆盖请求范围，可以直接使用上述分解。时间序列还需服从每一个请求日历桶：一个汇总单元只有完全落在同一个输出桶内才能使用。例如 Asia/Shanghai 的自然日可用整小时分解，纽约 DST 的 23/25 小时日按实际日历边界分解；月、周同理。历史时区偏移包含秒时，继续降到分钟或明细，不把 UTC 日粗暴视为本地日。

unsafe 分钟不能只在分钟层绕过后仍使用包含它的父层合计。可维护小时/日级 unsafe 计数；父单元存在 unsafe 分钟时向下递归，直到在受影响分钟读取原始明细。计数反映“非零 unsafe 子单元数”时，需要处理从 0 到非 0 与反向变化；也可读取分钟 guard 计算哪些父单元不能使用，避免额外热点更新。两种方案都必须和数据读取处于同一 MVCC 快照。

基础 status 可以在各层精确聚合；任意组合过滤、snapshot membership、复杂文本谓词仍需要原始过滤路径或额外覆盖设计。多层汇总不会自动解决这些条件的固定延迟保证。

建议先用第二种 100k/1m spread 分布确定现在的瓶颈，再以同一份 fixture 验证 hour/day 路径。只有汇总扫描量显著减少、短事务写入成本可接受，并发/回填/删除一致性全部通过，才应将其纳入性能承诺。

## 已准备的独立复跑工具

`tools/performance_distribution.py` 不修改现有 performance harness。它冻结当前源码，使用临时测试 overlay 从真正的 `managedUsageRollupQuery` 导出 SQL/参数，再生成一个全新的 `perf_uniform_*` schema、1m 均匀数据、容量计数和 `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)` 脚本。导出过程不连接数据库；SQL 实现没有在 Python 中复制一份。

已经运行本地 prepare-only 并通过 SQL 导出与 Python 编译检查：`.local/performance-uniform/20260928T220602-af85b8/report.json`，状态为 `prepared_not_executed`。`experiment.sql`、`query.json`、源码清单和摘要保存在同一目录。**没有执行该 SQL，没有该分布的性能结果。**

待正式性能采样完成后，一条命令运行独立 1m 数据实验：

```powershell
python tools/performance_distribution.py --rows 1000000
```

只重新生成当前版本的待执行材料、不使用 SSH/PG：

```powershell
python tools/performance_distribution.py --rows 1000000 --prepare-only
```

工具验证隔离数据库角色/地址，新建独立 schema，运行前拒绝与现有 performance/race 容器重叠。实验结果保留 synthetic schema，临时客户端容器和凭据文件自动清理。它只测 SQL 与 fixture 成本，不包含 HTTP、认证、编码或完整 durable aggregates；无论实验是否成功，`slo_proven` 都为 false。实际 HTTP 分布基准仍需后续将同样数据分布接入正式 runner 后验证。

## 009 既有数据回填的只读审查

正在执行的 009 不应修改或中断。它已在 100k fixture 应用，checksum 固定。以下是未来全新部署/后续重建流程的优化方案，不是已经获得的性能结论。

当前回填执行两次 `classified AS MATERIALIZED`：一次维护 unsafe marker，一次生成计数。第二次又对每条安全记录执行 `retained_usage_counters()`，其中的局部 MATERIALIZED CTE 可能阻止 SQL 函数内联，再执行多行维度函数扩展及宽 numeric 聚合。即使 JSON 只解析一次，10M 次函数调用/每次执行器启动和每维重复的聚合转换函数仍可能昂贵。不能仅凭 SQL 文本断言真实 planner 是否内联，需用计划确认。

后续重建可以在持有同样的 DML 接缝锁后，执行一次扁平提取，将每条明细需要的 safe flag、紧凑维度、34 个计数与样本保存到事务临时 staging 表。先从 staging 聚合 unsafe marker，再从同一 staging 分别聚合单值维度和 groups。这样避免第二次读取 request JSON、第二次执行安全检查和多次 JSON 解压；代价是写入/读取 staging 的磁盘量，应与内存可控的单条数据修改 CTE 方案比较。

单值维度可使用 `GROUPING SETS`，account/key 保留 `(resource_id,name)` 组合键与 legacy 类型。group 数组仍需独立去重展开，不能把同一请求的重复 group 变成重复贡献。先计算 numeric 计数一次再聚合；`GROUPING SETS` 仍必须为每个实际输出维度执行相应的聚合状态更新，不会神奇地把所有 numeric sum 工作量减少为 1/12。它主要减少扩大中间行集、元组复制、重复表达式和函数执行器启动，真实收益取决于计划。

若单条 GROUPING SETS 不能并行聚合，可比较固定维度的 `UNION ALL` 聚合分支或按不重叠时间分段读取 staging。并行度上限、临时磁盘、排序/hash 内存和最终索引插入应一起测量，不能简单把 work_mem 提高到按连接数无法承受的值。完全在线、无需长 DML 锁的回填还需要并发变更日志与明确的快照/重放边界，这是另一项一致性设计。

下面的诊断命令仅生成 JSON 计划，不执行任何扫描/聚合/写入，没有 `ANALYZE`。通过已配置的隔离 `TEST_DATABASE_URL` 连接，可在当前回填结束后检查函数是否以内联表达式或 Function Scan 形态出现。当前任务没有执行此命令：

```powershell
$planSql = @'
SET search_path=perf_slo_20260928t211543_5a5a53;
SET jit=off;
EXPLAIN (VERBOSE, SETTINGS, FORMAT JSON)
WITH classified AS MATERIALIZED (
 SELECT floor(extract(epoch FROM created_at)/60)::bigint*60 AS bucket,status,model,account_name,key_name,record,
 retained_usage_safe(created_at,status,record,model,account_name,key_name) AS safe
 FROM request_logs
), counted AS MATERIALIZED (
 SELECT l.bucket,l.status,l.model,l.account_name,l.key_name,l.record,c.*
 FROM classified l CROSS JOIN LATERAL retained_usage_counters(l.record,l.status)c WHERE safe
)
SELECT bucket,status,d.kind,d.resource_id,d.name,sum(requests),sum(input_tokens),sum(latency_samples),sum(latency_sum)
FROM counted CROSS JOIN LATERAL retained_usage_dimensions(record,status,model,account_name,key_name)d
GROUP BY bucket,status,d.kind,d.resource_id,d.name;
'@
$planSql | psql "$env:TEST_DATABASE_URL" -X -v ON_ERROR_STOP=1
```

该命令是针对瓶颈形态的投影计划，只列出部分 sum，不能用其 cost 代替完整 34 字段回填耗时。要做可信 A/B，应在独立同分布 fixture 上比较完整旧/新回填，记录函数调用、temporary I/O、wall time、CPU 和结果差集，而不是在正式回填同时执行 `EXPLAIN ANALYZE` 争用资源。
