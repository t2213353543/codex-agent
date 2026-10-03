# Retained 日志计数与缓存统计优化（2026-09-29）

`008_retained_log_rollup.sql` 新增 `retained_log_minute`，将 overview 缓存观测和日志精确计数的常见时间范围读取成本，从匹配明细条数降为分钟桶数。汇总只描述仍保留的 `request_logs`；删除明细会扣除这些计数，不会更改永久保留的 `usage_events`。

## 数据与读取合同

每分钟保存 requests、ok、errors，缓存读取已知/未知请求数、已观测输入/缓存 token 总和、缓存写入样本与总和，以及小数秒请求数。桶键是 UTC 分钟起点；计数和中间总和均使用 PostgreSQL numeric，读取最终转换为 bigint，保留原有溢出失败行为。

缓存观测仍区分缺失、显式零和正值，忽略负值及 cached 大于 input 的无效读取样本。非推理 endpoint、WebSocket warmup 不计入缓存观测。未知 cache write 仍输出 null，显式已知零仍输出 0；缓存读取比例只以已知样本的输入 token 为分母。统计时间范围没有变成永久累计量，也没有使用 TTL 缓存。

`OverviewCacheUsage` 将每个输出时间槽拆为完整分钟与两端不足一分钟的范围：完整分钟走汇总，两端走明细时间索引。支持非整分钟 from/to 和不为 60 倍数的 step。少量直接导入的小数秒 timestamp 会触发该分钟回读明细，保留原 SQL 的 epoch 转 bigint 四舍五入行为。普通应用写入是整数秒。

正常分钟对齐查询的 EXPLAIN 回归断言所有 `request_logs` 节点 `Actual Loops = 0`，证明查询不会为了返回空边缘分支而扫描明细。

第一次千万行验收仍发现 overview 首次响应 4.205 秒。随后真实 handler 的 pgx trace 定位到 cache SQL：4.635 秒总耗时中，4.592 秒花在缓存统计。EXPLAIN 证实 PostgreSQL 将千万行 `request_logs` 作为 Nested Loop 外侧，实际为空的 edges 放内侧，结果读了 652,607 个块并将空内侧循环一千万次。小 fixture 的原回归计划选择了相反连接顺序，因此没有暴露这一大表计划问题。

当前 SQL 以 `edges CROSS JOIN LATERAL (... WHERE edge_range OFFSET 0)` 保留参数化查询，先读取边缘范围；没有边缘时不访问明细。没有全局关闭 sequential scan，也没有强制整个应用使用 custom plan。同一千万行 schema 的 `.local/performance-slo/20260928t221901-8d283e/report.json` 记录：custom plan 的完整 handler 44.61 ms，generic plan 27.13 ms；cache 查询的 EXPLAIN 分别 0.572/0.495 ms，两种计划的明细节点都为 0 次执行。此证据是同环境诊断样本，不替代完整 HTTP 并发分位数。

日志总数/coverage 可对 requests、ok、errors 求和；coverage 的最早/最晚时间仍从明细时间索引两端取得，避免零桶或已删除记录污染范围。完整接口的筛选和游标兼容性由管理查询层决定，汇总没有覆盖的过滤仍须走原查询。

## 写入与迁移

`INSERT`、`DELETE`、`UPDATE` 分别使用 AFTER STATEMENT transition tables 触发器。一次 SQL 先对受影响分钟分组、按 bucket 排序，再每分钟执行一次 upsert；UPDATE 对新旧行求差，因此跨分钟移动更新两侧。TRUNCATE 在同事务清空汇总。

普通单条写入增加一次分钟 upsert；同分钟批量插入仍只增加一次对应桶 upsert；跨多个分钟的批量操作按不同分钟数更新。排序减少多桶修改相反顺序的死锁风险，同一分钟写入仍会竞争同一行锁。读性能收益不能替代写入吞吐、WAL 和锁等待验证。

迁移在一个事务内通过 `SHARE ROW EXCLUSIVE` 锁阻断 `request_logs` 写入，回填已有明细并安装触发器；普通读取继续进行。失败整体回滚，没有先回填后安装触发器的丢写窗口。迁移器为 008 使用维护级 statement timeout；这不是无锁迁移，实际停写时长随历史规模、CPU、磁盘和并发事务而变。不要修改已经应用的迁移文件及 checksum。

## 已完成验证

隔离 PostgreSQL 合成数据报告：`.local/performance-slo/20260928t214118-e1edc7/report.json`，全部通过且无跳过。

- 90 条混合缓存观测批量插入、跨桶 UPDATE、变更 eligibility、批量 DELETE、TRUNCATE。
- 回填迁移后继续写入，与原始明细 GROUPING SETS 查询逐字段一致。
- 不对齐时间、37 秒/1 秒步长、负 epoch、小数秒输入、空范围。
- warmup、控制接口、未知/显式零/负值、无效 cached/input，以及桶内和跨桶 bigint 溢出。
- 8 个并发写入者，各插入 20 行；insert/update/delete/truncate 回滚与持久事件独立性。
- 正常完整分钟查询不执行任何明细扫描；root 的精确 count、review 批读和在线索引恢复回归在同轮通过。

这份报告的通用 `requested_rows` 参数不代表这些回归各创建了一千万行；上述是语义、事务及执行路径验证。千万行 HTTP 延迟和覆盖见总体性能报告。

## 写入成本的证据状态

首轮独立基准 `.local/retained-rollup-bench-20260929/report.json` 使用 PostgreSQL 4 CPU / 4 GiB、测试进程 2 CPU / 2 GiB，3 轮、每轮 5 次。单条 event+detail 写入的每轮均值中位数，从关闭 008 时 0.446 ms 变为开启时 0.984 ms，增加约 0.538 ms。这是低样本的单写事务结果，不是并发 P95。

该轮 1000 行批量样本在连续累积表规模时单调变慢，无法分离 trigger 与关系规模/统计信息影响，不能拿前后比值当作写放大。第二轮尝试每次样本前清空并 ANALYZE，但旧版基准裸 `ANALYZE` 会访问同数据库其他 schema，撞到同期千万行回填锁，返回 `55P03`；`.local/retained-rollup-bench-20260929-v2/report.json` 明确为失败。基准现已限制 ANALYZE 到本 fixture 表。

第三轮在全部正式 HTTP 测量结束后的独占窗口完成，`.local/retained-rollup-bench-20260929-v3/report.json` 为 PASS。每个样本前在停止计时区清空 event/detail fixture 并只对这些表 ANALYZE，使初始关系规模一致。3 轮、每轮 5 次，以下是各轮均值的中位数：

| event + detail 单事务写入 | 008 trigger 关闭 | 008 trigger 开启 | 增量 |
| --- | ---: | ---: | ---: |
| 1 条 | 1.642 ms | 5.239 ms | +3.597 ms |
| 1000 条批量 | 39.751 ms | 49.700 ms | +9.949 ms，约 +25.0% |

单条样本每次 reset 会包含关系初始化/重规划成本，与首轮热表样本不是相同实验条件。1000 条批量的增量平均约 9.95 微秒/条。样本较少，没有测并发 P95、WAL 字节量或长期热点锁吞吐。

该基准冻结于已验证的 008 源码并仅覆盖 benchmark 修正，source SHA-256 为 `710c5fce3dc6269c8f3135cbb1fc6fc13f10100d16831118d786424ac9a1333c`；**不含 009 维度汇总**，不能将这组数作为最终全部触发器的写入增幅。最终整套源码的并发持久写入证据见总体 SLO 报告。

## 复跑

```powershell
python tools/performance_slo.py --test-run '^Test(PGRetainedRollup.*|PGOverviewCache.*|PGCacheObservation.*|CacheReadCreationStatisticsPG|PGManagedRetainedCountParity)$' --timeout-minutes 10
```

读取与写入对照基准为 `BenchmarkSystemPGRetainedCacheReads`、`BenchmarkSystemPGRetainedRollupWrites`。后者在相同合成数据库中分别关闭/开启 008 INSERT trigger，对单条和 1000 条批量写入取样；它测量包含 event/log 插入的事务成本，不代表完整网关推理吞吐。
