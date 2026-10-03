# SQL 性能优化与验证记录（2026-09-29）

本记录覆盖日志查询、管理用量统计和全局时间汇总。已有证据证明若干 SQL 扫描、排序和编译开销已消除；**尚未证明所有接口低于 1 秒，也未证明所有千万行查询低于 3 秒**。所有数据库验证均使用隔离 PostgreSQL、随机测试 schema 和合成数据，没有修改生产数据库。

## 已完成的改动

| 路径 | 原有成本 | 当前实现 | 语义边界 |
| --- | --- | --- | --- |
| `request_id` 查询 | `id::text` 不能利用主键；多列 OR 与时间排序可能选择大范围时间索引遍历 | 文本身份分支分别走索引，`UNION` 得到候选 ID 后按主键取记录；规范数字额外加入 bigint 候选 | 保留事件 ID、请求 ID、响应 ID 的匹配；`01`、`+1`、超出 int64 的字符串不会错误匹配数字主键 |
| 请求/响应/客户端关联 ID | JSON 字段逐行比较 | 三个部分 HASH 索引，仅收录相应字段非 NULL 的记录 | HASH 用于精确相等匹配，PostgreSQL 保留碰撞复核；不受 BTree 长字符串索引项长度上限影响 |
| 总 Token、延迟排序 | 对全部候选行计算 JSON 排序表达式并排序 | 两个与实际 `(value, created_at, id)` seek 顺序一致的复合索引 | 升/降序、游标和页码语义保留；没有给所有可选字段堆叠索引 |
| 管理用量统计 | 每条请求先扩展约 6–14 个维度，再对宽记录做大量聚合；源物化保留整个 JSON | 源只保留解析后的计数与维度；独立单值维度使用 `GROUPING SETS`，多值 group 单独去重聚合 | avg 的样本分母、unknown、缺失观测、image 溢出、账号/KEY 稳定 ID、历史名称和日历桶语义保留 |
| 账号批量统计 | 即使只输出账号也物化全部观测维度 | 单账号分组路径使用可内联 source，避免无用维度解析 | 输出保持与完整统计的 Accounts 投影一致 |
| 全局长时间范围汇总 | 扫描 `usage_minute` 的账户 × 分钟行 | `usage_hourly_global` 保存全局小时汇总；整小时读小时表，最多两个不完整小时读原分钟表 | 保留原有 `bucket >= from AND bucket < to` 语义，同一 SQL 的 MVCC 快照覆盖所有分支 |
| 小时对齐序列 | 对账户 × 分钟行聚合 | from、to、step 均按整小时对齐时读取小时表 | 不对齐序列继续读取分钟表，不改变时间槽边界 |
| PostgreSQL JIT | 大量复杂 SQL 的即时编译在结果产生前消耗数秒 | 应用 `OpenDatabase` 连接默认 `jit=off`，可用 `PG_JIT=on` 显式恢复 | 只影响该应用连接的执行策略，不修改服务器全局配置 |

`usage_daily` 不能直接替代全局 `usage_minute`：历史导入分别维护独立的 `account_day` 和全局 `minute` 基线。直接替换会改变历史总量。因此新增小时表从 canonical minute 表派生，而非借用账户日表。

小时表通过同事务触发器维护 `INSERT`、`UPDATE`、`DELETE` 和 `TRUNCATE`。更新跨小时按固定桶顺序取得行锁；中间加减使用 numeric，最后保存 bigint，避免 `-INT64_MIN` 或先加后减产生的假溢出。日志重试幂等、历史基线导入、聚合重建和事务回滚均有验证。

## 已取得的实测

### 30,000 行真实查询计划

证据：`.local/performance-slo/20260928t211703-054add/run.log`，报告中的 PostgreSQL 限额为 1 CPU / 512 MiB。测试捕获真实方法执行的 SQL 与参数，然后运行 `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)`，同时断言必须命中指定索引、不能全表扫描。

| SQL 场景 | 单次 Execution Time |
| --- | ---: |
| 事件 ID 详情 | 0.043 ms |
| 数字 ID 详情（保留事件 ID 冲突语义） | 0.074 ms |
| 请求 ID 精确检索 | 0.165 ms |
| 数字请求 ID 精确检索 | 0.118 ms |
| 客户端关联 ID | 0.035 ms |
| 第 100 页，每页 50 条，时间升序 | 1.675 ms |
| 第 100 页，每页 50 条，时间降序 | 1.411 ms |
| 第 100 页，每页 50 条，总 Token 排序 | 2.197 ms |
| 第 100 页，每页 50 条，延迟排序 | 3.092 ms |

这些是单次数据库计划执行时间，**不是 HTTP 延迟、并发吞吐或 P95**。详情查询已有旧式 `id::text` 对照：事件 ID 3.951 ms，数字 ID 4.566 ms；详情优化在本任务开始前已存在，本次仅扩展相同索引原则到管理请求 ID 过滤，不能将已有详情改动算成本次新增收益。

### 100,000 行 JIT A/B

证据：`.local/performance-slo/20260928t212139-28c97c/run.log`。同一进程、同一 100,000 行合成 fixture、同一 SQL，依次将该独占连接的 JIT 设置为 on/off。报告记录 PostgreSQL 限额为 4 CPU / 4 GiB；这与最初 HTTP smoke 的 1 CPU / 512 MiB 不同，不能把两次资源配置混为同一基准。

| SQL 场景 | JIT on | JIT off |
| --- | ---: | ---: |
| 全维管理用量统计 | 4929.973 ms | 572.388 ms |
| 仅账号投影 | 504.787 ms | 456.016 ms |

全维统计的 JIT on 计划生成 99 个函数，编译时间累计为 6654.004 ms。该累计包含并行进程时间，不能与墙钟 Execution Time 直接相加。此次同 SQL 对照约减少 88.4% 执行时间，证明主要固定延迟来自编译。

这不是旧版展开 SQL 对新版 GROUPING SETS 的完整 A/B，也不是千万行结论。物理冷缓存未被控制，验证期间隔离实例可能同时执行其他合成工作；需要重复样本才能给稳定的延迟分位数。

最初 100,000 行 HTTP smoke（`.local/performance-slo/20260928t211314-1330a4/report.json`）在 JIT 尚开启、PostgreSQL 1 CPU / 512 MiB 时，全维 stats 首次响应 9548.748 ms，约 1.09 MB。该结果是明确的未达标证据，不能被后续 SQL 单次结果替代成“所有接口已达标”。

### 语义与持久化验证

`.local/performance-slo/20260928t211230-d29bb1/report.json`：`TestPGManaged.*`、小时范围/变更验证、cache read/create 统计和 overview breakdown 通过，无跳过。

`.local/performance-slo/20260928t211703-054add/report.json`：新增真实索引计划、canonical request ID、多排序游标/页码、完整统计与账号投影、小时回填、并发写入、重建、独立 legacy 基线、跨小时 update/delete、truncate rollback 和 signed bigint 中间算术测试通过，无跳过。报告中的通用 `requested_rows` 字段不代表这些单元/集成测试实际创建了千万行；本节规模以各测试 fixture 为准。

小时汇总写入成本已准备 benchmark，但首次尝试被当时在线索引迁移的锁超时阻止（`.local/performance-slo/20260928t212258-51f085/run.log`）。**目前没有可用的写入增幅结论**；不能用读取收益隐去额外小时全局行锁和 upsert 成本。

## 迁移与部署边界

`006_metrics_query_indexes.sql` 保持原 SQL 文件内容及 checksum。迁移器仅针对这一已知索引脚本选择在线执行；已完成的同 checksum 迁移仍直接跳过。全新空表在 SHARE NOWAIT 锁内确认为空后使用普通 CREATE；非空表使用 CREATE INDEX CONCURRENTLY。

索引迁移逐项持久化 statement checksum claim。中断后只有属于该迁移的对象才允许恢复；无 claim 的同名对象拒绝自动收编。已 claim 的 invalid/not-ready 索引通过 DROP INDEX CONCURRENTLY 后重建。所有索引成功后才记录完整迁移完成标志。迁移临时提高 statement timeout，退出时恢复；恢复失败则关闭连接，避免维护超时流入应用连接池。

在线创建仍会等待旧事务/快照。最初沿用请求的 3 秒锁等待时，中断恢复受到同库旧快照影响并返回 55P03（`.local/performance-slo/20260928t212359-de0a37/run.log`）。迁移现单独使用 30 秒 lock timeout、结束后恢复原设置。`.local/performance-slo/20260928t214118-e1edc7/report.json` 的 `TestPGOnlineIndexMigrationResumeAndTimeout` 已验证中断后恢复、完整迁移重跑、无 claim 对象拒绝和超时恢复全部通过。仍可能在更长事务存在时失败，失败保留 claim 供重跑，不把 CONCURRENTLY 解释为无需等待。

`007_metrics_hourly_global.sql` 在一个事务中锁定分钟表写入、回填小时表并安装触发器，普通读取仍可进行。它需要计划内的回填时间和写入停顿，不是无锁在线回填。迁移 statement timeout 已单独延长至 30 分钟；CLI 总 context 也是 30 分钟，不能据此保证任意历史规模都能完成。失败会整体回滚，重跑不会留下半份小时汇总。

升级现有大表还需验证磁盘空间、五个新索引的实际体积/构建时间、WAL 和写入成本。源码与 synthetic schema 测试已完成的项目不能替代目标实例升级验证。

## 第一阶段结束时仍需解决的千万行边界

完整 stats 仍须读取匹配记录并计算全部维度，关闭 JIT 消除了固定启动成本，没有消除 O(N) 工作。任意组合过滤、快照成员筛选、精确总数、完整导出、某些未建索引的排序和文件模式全量扫描均不能根据上述结果承诺固定 1 秒/3 秒。

第一阶段据此提出 retained-detail 专用的分钟维度汇总，而非复用永久保留的 `usage_events` 总量。第二阶段已实现下述基础查询路径，见后续章节；复杂组合过滤仍有相同边界：

1. 保存 `(bucket, kind, resource_id/name)` 的计数、各项 sum 和 sample count；avg 在读取时计算；image sum 使用 numeric 并在输出时判定 bigint 溢出。
2. `request_logs` 的 insert/update/delete 与汇总同事务更新；按请求去重 groups；删除 retained detail 必须同步扣除该 scope 的统计。
3. 账号/KEY 的历史最大 label 需要可逆的 label 引用计数或删除后回查；first/last 根据仍有数据的桶和索引边界查找，不能保留已删除记录的时间。
4. 完整分钟读取汇总，秒级不完整边缘读取原始记录；日、周、月与 DST 桶在查询层按请求时区重分组。
5. 任意组合过滤需要明确的汇总覆盖或更细 cube。未覆盖条件仍回退扫描时，该条件仍属于尚未达到固定延迟目标的范围；若改成异步统计任务，需要明确修改接口合同。
6. 约十余维度的同步更新会显著增加写放大，应先对真实分布测吞吐、热点锁和恢复成本，再确定维度与粒度。

## 可复跑命令

最新源码的 SQL 语义与真实索引计划验证：

```powershell
python tools/performance_slo.py --test-run '^(TestPGMetricsHourly.*|TestPGManagedRequestIDFilterCanonicalIdentity|TestPGManagedUsageReadIndexPlans|TestPGManagedUsageAggregationParityReview|TestPGManagedAccountProjectionParity|TestPGManagedNumberedPageParity)$' --timeout-minutes 10
```

在保留的 100,000 行合成 fixture 上复跑 JIT A/B EXPLAIN：

```powershell
python tools/performance_slo.py --reuse .local/performance-slo/20260928t211314-1330a4/report.json --test-run '^TestPerformanceManagedUsageStatsPlan$' --timeout-minutes 5
```

两条命令均冻结源码、保存 source/binary checksum、限制到已有隔离环境，并生成独立报告。千万行 HTTP 测量及其最终资源配置、覆盖和未达标项目由总体性能报告记录。

## 第二阶段：精确 retained 用量维度汇总

新增 `009_retained_usage_dimensions.sql`，稳定 SHA256 为 `18f9779019904742413ad1df0a79cc7500657452fb7bf6dc000fcd58960a0895`。它与 008 的 retained 概览/cache 汇总分工独立，复用 `retained_log_cache_observation(record)`，没有修改既有迁移 checksum。

`retained_usage_minute` 按分钟、原始 status、kind、资源 ID 和历史名称存储 34 个 numeric 计数、样本数和总和。单值维度每请求贡献一次，group 先去重；总量行兼供时间序列。avg 始终由总和/样本数计算，缺失与零样本分开保存，image overflow 在最终输出判断。因此删除大样本后，溢出状态能够恢复，不会留下不可逆的布尔标记。

历史名称独立成行；查询只选择 `requests > 0` 的行后取最大名称。删除旧最大名称会恢复到剩余名称。无真实 ID 的账户/KEY 使用 `account_legacy`/`key_legacy` 类型；options 只选择真实 ID 类型，因此合法的 `name:...` 真实 ID 不会被当作 legacy 丢弃。stats 读取时规范化 legacy 类型，维持原有分组合同。

同事务 statement transition-table 触发器维护 INSERT/UPDATE/DELETE，先合并正负 delta 再按固定键序 upsert；TRUNCATE 清空派生表。零计数行作为不可见的 tombstone 保留，避免每次写入全表清扫或删除锁放大；长期大量重命名后的空间回收仍需独立维护策略。

基础时间和状态过滤走汇总；任意其他过滤、账号批量投影、as_of/snapshot、review 仍走原始 SQL。两个不完整分钟只读实际明细边缘，整分钟读汇总。两种来源在同一 SQL 的 MVCC 快照下合并，未用 TTL、近似结果或异步 API。

合法长文本不被截断或拒写：超出索引键安全字节阈值的记录进入 `retained_usage_unsafe_minute` 计数。亚秒时间戳同样标记。查询在这些分钟排除全部汇总，改读完整分钟明细，防止双计；删除最后一条异常记录后恢复汇总路径。历史时区在一个 UTC 分钟内改变日历桶时，也精确回读该分钟。原有 epoch 转 bigint 的舍入、时区、DST 日/周/月边界保留。

顶层 first/last 从有记录的最早/最晚选中分钟中按时间索引定位；没有为每个维度维护不可逆的 min/max。空结果仍返回原有零总量和空数组。

### 第二阶段验证

`.local/performance-slo/20260928t215242-2f820c/report.json` 全部通过、无跳过，覆盖：

- 现有 options identity/calendar、snapshot 测试。
- 汇总与原始 SQL 完整 JSON 对照，覆盖基础 status 和各种分钟边缘。
- 删除最大历史名称、image 超界后删除恢复、修改 status/计数/分组/时间、truncate 回滚、删除全部后的空结果。
- 超长 group、亚秒跨分钟舍入、纽约 DST 日/周和加德满都月桶。
- 8 个并发写入者共 32 条短事务后统计与明细完全一致。
- 在已有明细且没有 009 的状态执行真实回填；使用 `pg_locks` 确认等待 DML 边界，普通读仍观察旧提交状态，写事务提交后回填包含该变更，后续写入也正确维护。

Snapshot 测试保留“较小 ID、相同时间戳、稍后提交的记录不得进入已捕获快照”场景。其 fixture 改为先预留较小 sequence ID、提交正常请求后才插入未提交记录，避免测试自己持有同分钟汇总锁又同步等待另一写入者的人工自锁。同步汇总确实引入同键写入串行化，此成本没有被隐藏。

100,000 行既有 fixture 应用 008+009 的实测回填耗时 **11.227 秒**。随后 5 个 HTTP 场景、每并发档 3 个样本全部成功；stats 首次 **270 ms**、并发 4 最大 **292.5 ms**，model options 首次 **67.8 ms**、account options **72.9 ms**。新增 `retained_usage_minute` 大小约 **9.49 MB**。证据 `.local/performance-slo/20260928t215220-b6f840/report.json`，源码摘要 `e286221d5...`，隔离 PostgreSQL 配额 4 CPU / 4 GiB。这是小样本 smoke，不是稳定 P95 结论。

千万行现有 fixture 的 008+009 回填与正式 HTTP 验证正在另行执行，完成前不将上述 100,000 行结果外推为千万行保证。此结构的读取成本与活跃分钟 × 维度基数相关；若每请求都有不同资源/名称，压缩率会下降。极多 unsafe 分钟或不受支持的复杂过滤仍可退化为明细扫描。

回填需要锁定 retained 明细写入直至事务提交；普通读可继续。同步维护增加各维度 upsert、numeric 算术与热点锁成本，需将最终写入吞吐/回填时间与读取收益一起评估。

复跑第二阶段的精确性与回填边界验证：

```powershell
python tools/performance_slo.py --test-run '^(TestPGManagedUsageRollup.*|TestPGManagedUsageOptionIdentityParity|TestPGManagedUsageOptionsCalendar|TestPGManagedUsageSnapshot)$' --timeout-minutes 5
```

## 千万行真实采样发现的 prepared generic plan 退化

首轮千万行 HTTP 重复采样发现：`request_id` 查询首次只有 3.766 ms，但后续定期出现约 15 秒 HTTP 500。首次查询或单次 EXPLAIN 没有覆盖这个故障。PostgreSQL 从 custom plan 切换到 generic plan 后，对稀疏 JSON 身份各自默认估计 50,000 行，选择 Hash Join，主侧按 `id <= ceiling` 扫描大范围主键。

基线计划已保存于 `.local/performance-slo/request-id-generic-diagnostic/baseline-plan.jsonl`。custom 是候选 ID 驱动的 Nested Loop；generic 是主表范围扫描与候选 hash 匹配。这里不是 SQL 注入、超时配置或网络故障，而是参数化计划的行数估计改变。

最终读取形态改为候选身份 UNION → `CROSS JOIN LATERAL (SELECT ... WHERE id=candidate.id OFFSET 0)`，保留每个候选的主键等值查找边界。未设置全局 `force_custom_plan`，也未把所有候选加载到应用内数组。日志页、精确总数、原始统计、导出、options 过滤、snapshot/cleanup 目标读取共享该数据源；request/client ID 同时出现仍为交集。

客户端关联 ID 的 force_generic 基线虽未选择时间全扫，也产生 HASH 等值位图与 `id <= ceiling` 的大范围 PK 位图相交，计划估计主键侧超过 333 万行。因此同样使用候选驱动。不存在的 client ID 在千万 fixture 的修复后 `EXPLAIN ANALYZE` 为 0.307 ms，主键条件变成 `id = candidate.id`；证据 `client-baseline-plan.json`、`client-fixed-plan.json` 位于相同诊断目录。

`.local/performance-slo/20260928t222001-10b8a0/report.json` 在原有千万行 fixture 上只读验证实际日志方法，包含 ExactTotal：强制 custom 20 次为 3.336–27.023 ms，强制 generic 20 次为 2.384–14.505 ms；对应 EXPLAIN 为 1.057/1.101 ms，主键实际仅访问 1 行、1 loop。取消请求后连接归还，下一请求正常。

新增回归故意使用没有 request/response JSON 身份的稀疏数据，避免“每行都有身份”的测试隐藏默认估计问题；覆盖 canonical 数字 ID、重复调用、精确总数、stats/export/options、client 多命中、client 缺失以及两类 ID 的匹配/不匹配交集。正式 HTTP 35 场景同源重测及最终判断由总体报告记录，强制计划的存储方法验证不冒充 HTTP 验收。

## 千万行 burst 同源重测结果

008+009 对现有千万行 fixture 的回填最终完成，耗时 **1040.72 秒**；009 维度汇总约 **9.73 MB**。这说明回填是长维护操作，不能因读请求变快而省略部署停写时间。没有修改已应用的 009 文件或 checksum。

修复 generic plan 后，正式 HTTP 报告 `.local/performance-slo/20260928t222504-b6417f/report.json` 为 pass，源摘要 `7c7c5be84770d510fdea628254c99e98f08580af4a12ec5ff725fb68225926e8`。35 个读取场景每场景包含首次请求、并发 1/4 各 20 个样本，另有 13 组写入/持久化操作。与 SQL 直接相关的结果如下：

| 千万行 burst 场景 | 并发 1 P95 / 最大 | 并发 4 P95 / 最大 | 失败 |
| --- | ---: | ---: | ---: |
| 全 30 天 stats | 260.787 / 261.879 ms | 373.342 / 376.654 ms | 0 |
| request_id 命中 | 2.367 / 2.565 ms | 3.384 / 3.430 ms | 0 |
| request_id 不存在 | 2.173 / 2.440 ms | 2.906 / 3.031 ms | 0 |
| client_request_id 不存在 | 1.026 / 1.080 ms | 1.242 / 1.401 ms | 0 |
| 30 天 overview | 34.060 / 48.724 ms | 29.875 / 30.708 ms | 0 |

stats 首次响应 264.342 ms，响应体约 1.12 MB。完全相同源码也在 100,000 行 fixture 重测成功：`.local/performance-slo/20260928t222645-38fba7/report.json`。这些成绩限定在报告中的资源、分布、参数和并发条件，不扩大到未测路由或任意高基数组合过滤。

该千万行 fixture 只有 30,000 个账户 × 分钟组合；高压缩率是上述 stats 结果的重要条件。第二种均匀分布的范围审查、hour/day 后续设计和可运行但尚未执行的 1m 实验工具见 `docs/PERFORMANCE-DISTRIBUTION-PLAN-2026-09-29.md`。

同源 Linux race 13 项通过，无失败、跳过或缺失，证据 `.local/performance-slo-race/final-7c7c5be/report.json`。覆盖 008/009 变更与并发、回填接缝、snapshot、count、GetMany、KEY 缓存/授权关键行为。另一次全管理 361 项回归暴露了四项失败，不能把 HTTP/race 通过写成全回归已通过；后续测试 fixture/创建时间修复和最终结果以总体报告为准。

### 最终回归及 race 源码差异

四项失败修复后，`.local/performance-slo/20260928t223513-9c8dcb/report.json` 为 pass：**363 个顶层测试**（362 个管理/存储回归加写入验收），947 个含子测试的通过事件，0 失败、0 跳过；13 组写入操作均成功。最终源码摘要为 `2aea57c48ef3f21cdbf6f5bde183ae4f63efda04e2dd1e805c61847067c132ba`。

与已经通过 13 项 race 的 `7c7c5be...` 逐文件比较，最终仅一个生产文件变化：`store_pg.go` 的 Group 创建由应用本地时间改为 `INSERT ... RETURNING` 返回数据库实际持久化的创建/更新时间和 version。其他差异是四个测试文件：Redis fixture 预期、Group 时间边界断言、cleanup 的较小 ID 晚提交 fixture 顺序，以及 time-index 测试仅批量构造事务的 60 秒 deadline。索引读取、008/009、小时汇总、KEY 缓存、GetMany 等相关生产实现与 race 来源逐字节一致。

差异清单、两份 manifest 摘要及逐行 diff 已保存于 `.local/performance-slo/final-validation-delta.json`。**race 并非运行于最终摘要**，该逐文件比较不会被描述为新的最终摘要 race 通过。最终版本的同源两规模 HTTP 重测由总体性能报告给出。
