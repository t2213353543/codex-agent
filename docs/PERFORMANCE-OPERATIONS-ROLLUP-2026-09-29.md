# 运维统计秒/分钟汇总与重建优化

这是继续推进全接口性能目标的第二阶段记录。上一阶段千万行突发分布的 operations 接口并发 4 最大约 1.81 秒，但仍需解析整个窗口的每条请求 JSON；更密集的窗口没有固定延迟保证。当前新增 011，将日常请求工作量改为窗口内的汇总桶数量。尚未把小数据语义测试当作千万行性能结论。

## 精确统计结构

`retained_operations` 以 `(resolution, bucket, category)` 为主键，保存总请求、成功、错误、取消、tokens、usage-known、latency/TTFT 样本及总和，以及 7 个延迟直方图桶。中间计数和总和使用 numeric，最终响应仍按原字段转换到 bigint 或 float8。

同时保存秒与分钟两种粒度：15 分钟图表每槽 15 秒，且接口窗口跟随当前时间而非整分钟。仅分钟汇总会让每个槽都需要回读明细。因此查询把每个槽拆为完整分钟与两侧不足一分钟的秒桶，互斥合并。负 epoch 和小数秒统一按 floor 归属；合法整数秒窗口的半开范围与原查询一致。最新 8 条错误仍通过错误时间索引取得，与汇总位于同一 SQL 快照。

批量 INSERT/UPDATE/DELETE 使用 statement transition tables，按秒/分钟桶与错误分类合并 delta 后固定顺序 upsert。TRUNCATE 同事务清空派生统计；没有异步延迟，也没有 TTL 缓存。每条单请求增加最多两层汇总更新，因此额外写锁和写入成本必须测量。

011 独立于永久保留的 `usage_events`，清理 request_logs 会同步扣除运维统计。索引键错误分类来自受控枚举，不把任意上游错误文本放入索引。

## 已验证的正确性

`.local/performance-slo/20260928t225927-868af3/report.json` 全部选中 PG 测试通过，无跳过，包含：

- 与优化前原始 SQL 结果逐列双向 EXCEPT 比较。
- 15/60/360/1440 秒图表槽、非对齐窗口、17 秒/1 秒额外内部边界、空窗口、负 epoch。
- 未测 latency、显式零、负值、TTFT unknown/零/正值、HTTP 错误分类、全部直方图边界。
- 小数秒时间、批量跨分钟 UPDATE、状态/用量变更、DELETE、各种事务回滚和 TRUNCATE。
- 已有明细执行真实回填，再写入新记录；8 并发共 64 次入库不丢贡献。
- 30,000 条密集窗口查询在强制 custom/generic 计划下，明细只执行最新 8 条错误的有界索引读取，不全表扫描。

相对旧查询，一个极端中间算术差异是单条 input+output 超过 int64、其他记录负数抵消时，新路径 numeric 中间和可能合法成功而旧 bigint 每行加法提前失败。正常非负合法总量和最终 bigint 溢出检查保持不变。

## 额外写入成本实测

`.local/performance-slo/20260928t232944-8ea71e/report.json` 在同一配置的独立 schema 中，对 011 INSERT trigger 开/关分别测 3 轮 × 100 次串行持久化事务，其它汇总层保持相同，每组前清空合成数据。全部结果与启用时 100 条秒汇总计数核对通过。

| 011 运维维护 | 样本数 | 均值 | P50 | P95 |
| --- | ---: | ---: | ---: | ---: |
| 关闭 | 300 | 4.484 ms | 4.232 ms | 5.118 ms |
| 开启 | 300 | 4.841 ms | 4.586 ms | 5.325 ms |

这一实验均值增加约 0.358 ms（8.0%）。它是控制变量的串行事务对照，不是饱和吞吐、写锁竞争或生产长期 P95；首请求准备与规划开销保留在样本中。新增秒桶的存储容量也随活跃时间和错误分类数增长，真实容量列在分布报告中。

## 聚合重建

均匀 1M fixture 的旧 `RebuildAggregates` 真实耗时暴露了 007 小时触发器的问题：每一个账户分钟行重复更新同一小时全局行。原始 HTTP fixture 准备总耗时 609.056 秒，主要停留在这一分钟表 INSERT。

新重建保留原 exclusive advisory lock，并在 TRUNCATE 已取得 ACCESS EXCLUSIVE 表锁后，仅在同一事务内暂时停用小时 delta 触发器，批量重建 canonical 分钟/日/总量及 legacy 基线，最后一次 GROUP BY 构造小时表，再恢复触发器并提交。失败回滚会恢复旧汇总及触发器状态；没有未锁定的停用窗口。

`.local/performance-slo/20260928t230321-5b8767/report.json` 验证失败注入、旧统计不变、触发器启用状态恢复、重复重建、继续写入，以及已有 legacy/hourly 回归全部通过。

相同 1M 均匀 fixture 实测：旧账号初始化加真实聚合重建为 **609.056 秒**；新代码真实重建 **14.783 秒**，约 41 倍改善（两个计时范围略不同，旧范围含账号准备）。前后 grand/minute/hourly 都为 1,000,000 requests、1,399,000,000 tokens，legacy 行数 0→0；非空 legacy 基线另由上述回归验证。候选报告 `.local/performance-slo/20260928t230946-df0ae2/report.json`，旧报告 `.local/performance-slo/20260928t225414-da3603/report.json`。

同一候选 uniform1M 的 operations24h 首次约 8.46 ms、并发 4 最大 10.174 ms（每档仅 5 个样本）。uniform100k 首次 7.354 ms、并发 4 最大 10.213 ms。后续 20 样本正式统一验收会独立记录；小样本结果不等于长期尾延迟保证。

## 复跑

```powershell
python tools/performance_slo.py --test-run '^(TestPGOperations.*|TestPGMetricsRebuildHourlyBulkRollbackAndLiveWrites|TestPGMetricsHourly.*|TestPGMetricsLegacyBaseline.*)$' --timeout-minutes 10
```

该入口只使用隔离数据库与合成 schema。生产迁移未执行；011 既有数据回填仍在事务中暂时阻止明细写入，需要记录实际耗时。
