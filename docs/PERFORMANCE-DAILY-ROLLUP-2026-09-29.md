# 高基数分钟分布与每日维度汇总（010）

前一轮千万行 burst fixture 只有约 30,000 个账户分钟组合，不能代表均匀分布。第二种 `uniform30days` 合成数据把每个请求放进不同账户分钟组合，已经验证出原 009 的扩展瓶颈。

| 原始行数 | 009 活跃汇总行数 | 汇总 heap / indexes | 原 SQL Execution Time |
| --- | ---: | ---: | ---: |
| 100,000 | 281,440 | 55.10 / 17.25 MB | 1877.742 ms |
| 1,000,000 | 1,181,440 | 331.49 / 119.60 MB | 9280.150 ms |

100k 原始 SQL 临时读写约 205 MiB；1m 约 972 MiB。100k 的真实 stats HTTP 首次约 1.796 秒，也已明确超过普通场景 1 秒目标。原始计划证据分别为 `.local/performance-uniform/20260928T224556-877e14/plan.json`、`.local/performance-uniform/20260928T224734-987eb8/plan.json`。这些旧版基线冻结于 `2aea57c...`，没有包含 010/011。

## 最小多分辨率实现

`010_retained_usage_daily.sql` 新建 `retained_usage_daily`，仅汇总非 `total` 维度。高基数的账户、KEY、模型等维度在完整 UTC 日使用日表；首尾不完整日读取分钟表。图表与 first/last 仍使用低基数的 total-minute 行，并新增部分索引，使其不会扫描全体账户分钟行。

因此不必增加每小时层：任意时区的日/周/月图表、DST 和 60/3600 秒粒度继续由分钟 total 精确生成，高基数维度与图表采用不同的粒度来源。日表并不假设 UTC 日等于本地自然日，也不会把日表时间戳直接用于本地图表。

unsafe 分钟所在的完整 UTC 日排除日表，回退到该日分钟；具体 unsafe 分钟及秒级边缘继续回读原始明细。日表、分钟和原始记录来源互斥，处于同一 SQL MVCC 快照。历史名称保留独立键和正引用数过滤，因此删除最大名称后仍能回退；numeric sum/sample、image overflow 恢复语义不变。

010 首次从既有 009 分钟计数回填，无需重新解析 request JSON。它不修改 009 文件或 checksum，而是在同一迁移事务中替换三项 request transition 维护函数：单个语句共享已解析的 `classified`/`counted` 数据源，按 guard → minute → daily 的依赖顺序维护两层。

直接在 minute 表的 `INSERT ... ON CONFLICT` 之后分别挂 UPDATE/INSERT 日汇总触发器，可能使两个并发 status 互换请求先后取得不同日锁。最终实现避免该分裂路径，统一请求全部 delta 后按固定日键顺序 upsert。没有全局或每日 advisory 锁，也没有通过应用重试掩盖维护层死锁。`dimensions` 不额外物化整份多维宽行，避免将约十余倍展开结果写入临时表；numeric 观测只解析一次，维度文本投影由各层读取。

迁移先取得 request writer 边界，再锁分钟表，保持现有写锁顺序。回填/函数替换原子提交；minute truncate 触发清空 daily，事务回滚一并恢复。该迁移当前稳定 SHA256：`aa88e1a13f32572b7b88bacc8d78a24d4e6c38eb4858405705dbd83874a3efc5`。

## 已完成验证

`.local/performance-slo/20260928t225927-868af3/report.json` 联合 PG 验证通过，其中新增日层测试覆盖：

- 六天数据，UTC、上海、纽约 DST、加德满都；分钟/小时/日/周/月步长与非对齐首尾。
- 所有基础状态过滤、长维度与亚秒 unsafe 回退。
- 删除旧最大历史名称、跨日移动、truncate 回滚。
- 并发写入、从既有分钟数据重新回填；每个 numeric 字段与分钟表聚合差集比较。
- 与原始明细 SQL 完整 JSON 结果逐项比较，并确认完整天使用日表。

`.local/performance-slo/20260928t230216-196a6a/report.json` 通过：两个并发 writer 在不同分钟、相同每日维度上进行 20 轮相反 status 修改，没有应用重试或死锁；009 回填接缝与新 010 连续迁移仍通过。

同一报告包含日层写入成本实验：其它汇总保持不变，临时 schema 每组清空，三轮各 100 个真实 `persistRecord` 串行事务，对比原 minute-only 维护函数与新 minute+daily 维护函数。

| 300 个样本 | 均值 | P50 | P95 |
| --- | ---: | ---: | ---: |
| minute-only | 4.153 ms | 3.921 ms | 4.673 ms |
| minute+daily | 5.181 ms | 4.984 ms | 5.556 ms |

均值增加约 24.75%。该实验当时与旧 uniform fixture 的维护重建并行，属于额外写放大的初步证据，不能作为无干扰吞吐或生产延迟保证。日层增加一层维度写入与同日热点锁，读取收益须与此成本一并评估。

## 尚待同 fixture 验证

010 已通过独立正确性测试；第一轮 uniform 同数据应用后的结果已经得到：100k 的 stats 首次约 764.5 ms、并发 4 最大约 820.8 ms；1m 首次 875.971 ms、并发 4 最大 1043.773 ms，5 个样本中 3 个超过 1 秒。因此第一轮日层候选仍未达到稳定的普通场景 1 秒要求。

1m 的日表约 60,240 行、19.7 MB。对应 `.local/performance-uniform/20260928T231236-6bc00c/plan.json` 执行时间 923.922 ms：103,440 行 source 包含 60,240 个日维度行和 43,200 个总量分钟；为总量与图表再次复制总量分钟后，146,640 个宽行进行 external merge，约 27 MB 磁盘排序，总临时读写约 49 MB。

据此继续修改读取 SQL，而没有增加 schema/写入层：

- source 使用显式可选 `NOT MATERIALIZED`，让 kind 和字段投影向各数据源下推；options 只需要身份/名称时不再先物化全部 34 个 numeric 字段。
- 维度单独聚合；总量分钟通过 `GROUPING SETS((), point)` 直接产生全局总量和时间序列，不将它们复制到维度大排序中。
- 空总量仍返回零计数、NULL 平均值及原有空数组；所有慢路径、unsafe 和日历分桶继续使用同一来源合同。

新 SQL 在完全相同 1m fixture 上的只读计划执行 **617.121 ms**，比上一轮 923.922 ms 减少约 33.2%；临时读写降至约 10.6 MB。证据 `.local/performance-uniform/20260928T231526-b591cd/plan.json`。这个值是单次 SQL 计划时间，不是并发 HTTP 验收。`.local/performance-slo/20260928t231516-33311e/report.json` 的日层全时区/边缘/mutation、原始 rollup、options 身份/日历、并发和回填验证全部通过。新版本同源正式 HTTP 采样正在另行进行，本记录不提前宣称其分位数已达标。

随后正式 uniform1m HTTP 重复采样中，stats 和 options 已通过 1 秒阈值：证据 `.local/performance-slo/20260928t232220-62f4b1/report.json`，源码摘要 `1a31da3c1cc9ba7a4b378989f7b909d1bd3f2a81abbf4404fab7be8646514753`。每个场景并发 1/4 各 20 个样本，0 错误；首次 stats 为 **626.962 ms**。

| uniform1m 读取场景 | 并发 1 P95 / 最大 | 并发 4 P95 / 最大 |
| --- | ---: | ---: |
| 30 天 stats | 642.857 / 644.897 ms | 794.311 / 806.341 ms |
| account options | 46.074 / 49.603 ms | 51.448 / 51.470 ms |
| model options | 13.234 / 16.579 ms | 12.580 / 12.584 ms |

同轮 overview 仍有两次并发 4 请求超过 1 秒，因此该报告整体为 fail，不能把上述三个场景通过写成“全部接口已达标”。overview 的独立定位与最终统一验证结果由总体报告记录。

## 第二阶段最终同源验收

后续 overview 账户排行增加独立的 canonical 日层（012）后，最终统一源码 `41d1ed1d38c1ed829bc0caec9e7193d74027c015fc6cb96b01c904840b789d90` 在四种已定义规模/分布通过测量，没有复用不同源码的结果拼接：

| 分布/规模 | stats 首次 | stats 并发 4 P95 / 最大 | 同源报告 |
| --- | ---: | ---: | --- |
| uniform30days 100k | 470.587 ms | 542.170 / 549.786 ms | `.local/performance-slo/20260928t234200-2dbd0c/report.json` |
| uniform30days 1m | 660.345 ms | 793.852 / 793.906 ms | `.local/performance-slo/20260928t234046-15a493/report.json` |
| burst 10m | 207.710 ms | 321.024 / 374.033 ms | `.local/performance-slo/20260928t234319-fda3a5/report.json` |

uniform1m 的 overview 并发 4 最大 91.447 ms，前一轮慢点已在同源验收中解决；account options 最大 92.354 ms。表中 stats 每并发档 20 个样本、0 失败；物理冷缓存仍未强制驱逐。burst100k、两规模附加管理接口和写入验收由总体报告列出。

相同源码的完整功能回归 `.local/performance-slo/20260928t234818-e20968/report.json` 为 pass：382 个顶层测试（380 个管理/存储回归与 2 个受控验收辅助测试），无失败、跳过或缺失。此前旧候选的失败记录保留，本段明确标识最终通过版本，不覆盖或删除失败证据。

最终相同源码 Linux race 也已通过：`.local/performance-slo-race/phase2-final-41d1/report.json`，18 个顶层测试全部通过，0 失败/跳过/缺失，实际测试 23.538 秒。覆盖 008/009/010/011/012 的关键并发与变更、日层反向 status、bulk rebuild 回滚、snapshot/count/GetMany、KEY 缓存与授权、兼容接口引用。race 使用与上述最终性能/功能验证相同的 `41d1ed1d...` 源码，无 overlay，验证后工作区与 manifest 零差异；临时容器和凭据文件已清理。

这些结果证明了指定条件下所测路径的改善，不把 1m 均匀分布外推为任意 10m 均匀分布，不覆盖未测组合过滤、所有写入负载或外部推理耗时。

第一版日层对全部范围可压缩高基数分钟数据，但某个查询的所有数据都在不足一天的高基数边缘，仍会读取分钟数据。大量 unsafe 日会失去日层收益。是否有必要继续增加小时层，应由同 fixture 的边缘范围结果决定，不能盲目增加第三层同步写放大。
