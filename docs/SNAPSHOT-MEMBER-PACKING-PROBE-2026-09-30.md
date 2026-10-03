# 固定快照成员：4096-ID 分块 SQL 原型

首轮先 prepare-only，之后按 root 明确授权，在另一隔离测试完全结束后串行运行默认 100000 行原型并通过。后续轮修正公平基线，完成交替 100k 和 1M 隔离测量，结果与限制分别列在本文最后两节。未改应用、migration、生产设置或生产数据。工具为 `tools/performance_snapshot_members_probe.py`；首轮证据 `.local/snapshot-members-probe/` 保留不改，后续证据在 `.local/snapshot-members-fair/run100k/` 和 `run1m/`。这里是后续存储实现的设计证据，不是已集成的应用优化。

## 保留的现有语义

`internal/server/admin_usage_snapshot.go` 当前在一个 INSERT…SELECT statement 的 MVCC 快照内，将符合过滤条件的精确 ID 集合写入 `management_cleanup_targets`，随后在同一事务写 snapshot owner/filter/TTL 元数据。后续查询使用该 ID 集合与当前仍保留的请求行及当前过滤条件的交集。`Snapshot.Total` 是捕获时总数，不因后续删除回减。

原型仅替代该成员集合的物理表示，不用最大 ID、事务号或捕获时间推断成员。实际应用集成还必须保持 owner/filter 校验、空集合、同事务元数据提交、有效期清理、列表/详情/导出/cleanup 的统一读取以及 rollup dirty-minute 守卫。这个原型没有替换这些应用入口。

成员删除后没有存储的请求内容可被返回：批量结果始终 INNER JOIN 当前请求行。另有一个必须明确的旧语义限制：现有集合仅存 ID，若管理员明确删除后用完全相同 ID 插入另一个 event，旧集合也会匹配该新行。单纯 bigint[] 不能额外保证禁止这种 ID 复用；需要事件身份或墓碑才可强化。本原型的 `40_same_id_reuse_characterization.sql` 将明确测出两种表示的相同行为，不把它包装成更强保证。

## 表示和查询

单 statement 中只选 ID，按 bigint 的有符号自然顺序编号，以 `(row_number()-1)/4096` 分组，每组 `array_agg(id ORDER BY id)`，写一条块记录。主键为 `(snapshot_id,first_id)`，保存 `last_id`、有界 `bigint[]` 和到期时间。每数组最多 4096 个 ID，检查一维、无 NULL、严格递增；捕获回归另验证块之间不重叠和与逐行参考集合完全相等。

ID 本身不做加减、绝对值或分桶偏移运算，因而负数、零及 bigint 两端不会溢出。ID 下界使用带引号的 bigint literal，避免一元负号与正值强转的解析边界。min/max 仅用于块定位，不替代数组中的精确成员判断。

详情先用 B-tree 定位 `snapshot_id=$1 AND first_id<=$2 ORDER BY first_id DESC LIMIT 1`，再验证 `last_id >= $2 AND $2=ANY(ids)`。这样最多展开一个数组，不扫全部块；`ANY` 仍最多比较 4096 项且可能需要读取/解压 TOAST。本轮实际详情和候选页计划都使用 `packed_members_pkey`，但不据此承诺所有规模/参数计划都相同。

批量读取用每块 `unnest(ids)` 后与当前请求表按主键集合关联；不强制逐 ID 的 LATERAL lookup。原型同时生成 100 条候选页的索引成员判断，以及用于 dirty-minute 思路的集合 anti-join。本轮全量关联选中 hash join，没有对请求表做 N 次主键 lookup；anti-join 的输出估计则有明显偏差，见实测分析。

## 已准备的测试

默认构造 100000 条合成记录及 6 个 bigint 边界 ID；可配置 100 到 1000000，脚本拒绝更大数据量。只测 all 和 selected 两种可控谓词，不声称涵盖全部线上动态过滤成本。

生成的 17 个 SQL 文件涵盖：

- 相同稳定数据上的逐行/分块 all 与 filtered 捕获；保存完整 `EXPLAIN ANALYZE BUFFERS WAL SETTINGS` 计划。
- 单 statement 内精确参考与分块集合同时捕获并对称 EXCEPT 验证；负 ID、零、最大最小 bigint 及块间缺口的详情判断。
- 成员删除、成员变成不匹配、非成员变成匹配、后插入低 ID 与高 ID；当前过滤后的关联结果不扩张。
- 空集合。
- 两连接 MVCC 竞争：writer 先插入未提交低 ID 并更新已有成员，capture statement 先取得 MVCC 快照后在 advisory barrier 阻塞；确认真实锁等待后才提交 writer。低 ID 不能进入旧快照，旧成员身份仍在集合内。
- 当前表的批量 join、详情/page 索引查询、成员 anti-join，以及明确的同 ID 复用边界表征。

4 项离线独立检查验证了准备分支不会导入 SSH 模块、执行目标和 schema/owner/marker guard 固定、数据规模上限、有符号极值的分块/定位和单 statement SQL 结构。随后实际 PG 验证通过捕获集合对称 EXCEPT、成员/非成员修改、删除、late-ID、空集合及真实两连接 barrier，不以离线检查替代 PG 回归。

## 固定隔离与运行方式

准备命令（不连数据库）：

```powershell
python tools/performance_snapshot_members_probe.py
```

root 通知共享隔离 PG 空闲后，未来一次命令：

```powershell
python tools/performance_snapshot_members_probe.py --execute
```

工具不提供自定义 DSN/container/database/schema 参数。执行只允许 `codex-agent-integration-postgres`，校验 compose project=`codex-agent-integration`、service=`postgres`，数据库 `codex_agent_integration`、执行角色 `codex_agent_test`，schema 固定 `perf_snapshot_members_probe`。schema 已存在直接拒绝覆盖；获得专用 advisory lock 后创建带唯一 owner marker 的 schema，结束只清理 owner 和 marker 完全吻合的该 schema。不会启动容器、建数据库或调整共享数据库设置。首次连接验证发现非特权角色不能在 PGOPTIONS 设置 `temp_file_limit`；runner 因此仅在固定 integration 容器以 postgres 建立带资源限额的会话，随即 `SET ROLE codex_agent_test` 执行 fixture SQL。并发 barrier 的一次 metadata 轮询临时 RESET ROLE 后恢复，不授予或改变任何持久权限。

会话限制：statement 30s、锁等待 5s、work_mem 8MiB、临时文件 256MiB、禁 parallel/JIT，`enable_hashagg=off` 防止聚合同时保留所有块的数组状态。并发边界仅 3 个连接，测试以外不执行宽查询。错误或超时不自动重试。完整计划与成功/失败结果写入 `report.json`，没有实际运行时不生成通过报告。

## 性能判断边界

分块把 N 条成员 heap/index/TTL 写入减少为约 `ceil(N/4096)` 条；1000 万个真实 ID 约 2442 个块。它仍要读取全部 N 个 ID、执行窗口/分组排序，并保留约 8N 字节原始 ID 信息（1000 万约 80MB，另有 tuple/index/TOAST/WAL）。每个数组有界不等于整个查询没有排序或 materialized tuplestore；受 work_mem 限制的排序可能落盘，当前原型也有结构完整性检查成本。

对于大量独立详情判断，B-tree 定位后扫描数组可能慢于原来的单成员主键判断；批量 stats 仍需展开 N 个成员。尤其旧 dirty-minute 守卫如果机械替换成逐行 ANY 查询，可能扩大 CPU 工作量，本原型提供集合 anti-join 仅作为计划比较，不提出直接替换建议。大集合创建与精确全量聚合仍有 O(N) 下界。

目前没有 10M 数据或 3s 延迟证明。100k/最多1M 的隔离测量只用来判断是否值得继续；只有相同数据、资源、过滤条件的执行计划、WAL、表体积和时间结果支持，才进入应用设计与 migration 讨论。

## 本轮 100k 实际结果

环境：PostgreSQL 18.4；integration 容器限制 4 CPU / 4GiB；查询 `work_mem=8MB`、parallel=0、hashagg=off；合成表 100000 条常规 ID 加 6 个有符号边界 ID。每种捕获只测一次，packed 先于 row，OS/PG 缓存未清；这是同数据的 SQL 原型对照，不是线上过滤成本、HTTP 延迟或统计置信结论。首次运行在任何 schema 创建前因登录角色无权设置 `temp_file_limit` 停止；保留 `first-failed-before-schema.json` 与 `first-failure-options.json`，一次有依据的会话角色修正后整套通过，用时 6.8 秒，`fixture_removed=true`。

| 成员捕获 | 实际成员数 | 分块数 | 逐行执行 ms | 分块执行 ms | 逐行 WAL bytes | 分块 WAL bytes |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| all | 100006 | 25 | 498.649 | 321.473 | 21894311 | 363261 |
| selected | 66673 | 17 | 330.926 | 174.416 | 15753247 | 236324 |

在这些原型 SQL 内，all 捕获约 1.55 倍、selected 约 1.90 倍加速。比较还有一个重要限制：row baseline 为生成同 statement 的总数，加了 `RETURNING 1` 的 N 行 CTE 和 count；实际应用用 INSERT command tag 的 RowsAffected，没有这段额外物化。row all INSERT 节点 466.45ms、整个 statement 498.65ms，相差约 32ms；两种原型还都显式 MATERIALIZED selected，不是应用的全部真实选取形态。所以上述比例不能称为应用精确加速比，下一轮应以应用原始 INSERT 直写作为计时基线。WAL 和表体积不因这段只读计数产生额外 N 行持久写入。

分块 all 路径有两次排序，全部为内存 quicksort，temp read/write 均 0；聚合子计划约 140ms，包含检查、TOAST、索引和 WAL 的 INSERT 约 320ms。节点时间包含子节点，不能将它们相加，也不能把 INSERT 与聚合的差值全归到某一检查函数。100k 没有排序落盘不能推断 1M/10M 也没有。

存储对照覆盖所有实测快照，共 300026 个成员。逐行 300026 行含索引 32735232 bytes；分块 76 行、相同 300026 个精确 ID、最大数组 4096 项，含索引和 TOAST 1155072 bytes，约减少 96.5%。这是规律排序 ID 的压缩结果，不能当成任意稀疏 ID 分布的固定压缩比。

| 后续查询 | 执行 ms | 实际计划与边界 |
| --- | ---: | --- |
| 最大 bigint 单条 membership | 0.328 | `packed_members_pkey` Index Scan → LIMIT 1；末块 1702 项，不是 4096 项最坏样本。 |
| 连续候选页 100 条 | 7.519 | 100 次 Index Scan，每次约 0.004ms；含数组成员校验的 Subquery Scan 每次约 0.072ms；99 条匹配，另 1 条为晚插入非成员。 |
| 分块展开后批量关联 | 93.910 | 25 次 unnest + hash join，实际返回 100005 条；捕获成员中 1 条已删除，不复活。 |
| 逐行参考批量关联 | 94.198 | 同样 100005 条，hash join；没有证据证明分块能加速这一统计读取阶段。 |
| 分块集合 anti-join | 55.188 | 返回 3 条 late 非成员；输出估计 50004 条，对实际约高估 16668 倍。 |

详情页的成本已经可见：这 100 个候选均落在同一个末块，计划没有 Memoize；重复读取数组并做 membership 校验比单次 B-tree 定位占用更多时间。数组上限仍是 4096，故一般 100 次独立校验最多面对 409600 个元素比较，但本次末块只有 1702 项，没有实测这个最大成本。未来可比较“页内先去重 chunk，再只展开每个块一次与 page IDs 做集合关联”，不能直接把当前每行 ANY 路径用于全量 dirty-minute 判断。

批量展开的输入估计本次较接近：98700 对 100006；anti-join 输出选择率却严重高估。所有后续读取计划是在手动 ANALYZE 两种成员表后测量，尚未覆盖生产新快照刚建完、未 ANALYZE、prepared generic plan、过期快照占比变化时的估计。dirty-minute 查询还带 bucket 聚合，这个简单 anti-count 的 55ms 不是完整 rollup 守卫耗时。

实测也确认显式同 ID 重建的边界：`legacy_visible=true` 且 `packed_visible=true`。这只能证明与当前固定 ID 集合一致，不能宣称防止事件身份被同 ID 替换。

下一轮值得继续验证存储表示，但本轮不写 migration、不集成应用、不扩容。下一步应优先：新建块表未 ANALYZE 时的计划、完整 4096 项与稀疏/高跨度 ID、候选页去重块、dirty-minute anti-join 的估计和总体成本；随后经排程确认再考虑最多 1M 及排序落盘。100k 的有限捕获提升不足以证明 10M 捕获低于 3 秒。

## 后续轮：公平基线与新建快照计划（100k）

原始 100k 报告和生成 SQL 保留在 `.local/snapshot-members-probe/`，report SHA256 仍为 `2f8bf5a2d218576fc5f4853812c3c0732f65c780c78a921d7342a4b4f1ef909a`。后续轮使用 `.local/snapshot-members-fair/run100k/report.json`；工具现在默认生成新的带时间目录，并拒绝覆写已有 report 的目录。

逐行计时改为应用的直接 `INSERT INTO … SELECT … FROM fixture_logs WHERE …`，没有 WITH、RETURNING 或 count。持久 psql 连接的 statement timing 与 `INSERT 0 N` command tag 一起采集，确认 `:ROW_COUNT` 为真实 N。分块仍在同 statement 内返回每块长度并记录总数；逐行总数不额外扫描进入计时。每轮先清空两种成员表与元数据，再交替 row→packed / packed→row；all、filtered 各 6 轮。这样消除上一原型的 N 行返回计数偏差，但仍是合成简单谓词，不是完整线上 HTTP 测量。

| 捕获（6 轮） | 直接 INSERT 中位 ms | 分块中位 ms | 直接 INSERT 范围 ms | 分块范围 ms |
| --- | ---: | ---: | ---: | ---: |
| all 100006 | 411.398 | 217.757 | 397.472–450.486 | 214.479–226.005 |
| selected 66673 | 282.274 | 144.565 | 276.111–289.643 | 140.346–152.232 |

收益分别约 1.89 倍、1.95 倍。独立 EXPLAIN 的 all WAL 为逐行 23621191 bytes、分块 354660 bytes。整套 14.413 秒，语义测试和 chunk-page 去重对称 EXCEPT 均通过，schema 已清理。

新增成员表仅在这个一次性 fixture 内关闭 autovacuum，确保 fresh 阶段没有后台 ANALYZE；实测两表 `reltuples=-1,relpages=0`。随后明确 ANALYZE，并分别用 `force_custom_plan` 和 `force_generic_plan` 的实际 PREPARE→EXPLAIN EXECUTE 对照，共 40 个读取计划，不把普通 bind 的 EXPLAIN 当 generic plan。

| 读取 | fresh custom / generic ms | ANALYZE 后 custom / generic ms |
| --- | ---: | ---: |
| packed batch 100006 | 256.420 / 249.831 | 83.263 / 77.952 |
| row batch 100006 | 158.829 / 159.796 | 67.352 / 62.237 |
| 连续页 100：逐项 ANY | 12.829 / 11.528 | 11.253 / 11.096 |
| 连续页 100：块去重后展开 | 2.608 / 2.281 | 2.337 / 2.322 |
| 4096 项稀疏块尾页 100：逐项 ANY | 9.590 / 9.660 | 10.579 / 10.469 |
| 4096 项稀疏块尾页 100：块展开一次 | 5.737 / 5.480 | 5.168 / 5.944 |

fresh packed batch 估计仅 40 行、实际 100006，选择按成员逐项关联请求主键的 nested-loop，慢于直接成员表约 1.6 倍；ANALYZE 后估计改善，但仍未胜过 row batch。这是实际应用集成阻断点：不能只发布“捕获变快”而忽略新快照立即读取的退化。不能通过每个快照创建后全表 ANALYZE 掩盖该问题，需后续验证可维持的基数/读取设计。

本轮 4096-ID 稀疏块覆盖最小到最大 bigint 的大跨度，尾项命中与相邻空隙未命中都通过。单项完整块 ANY 0.112–0.139ms；这次确实覆盖完整 4096 项，但 ID 是规则跨度而非随机分布，不是最坏压缩率证明。连续页的去重方案先只定位块 first_id，再对唯一块读数组并展开；100 个候选在一个完整块内，从约 11–13ms 降到约 2.3–2.6ms。跨多个块、稀疏 page 与复杂排序仍未覆盖，不能宣称所有页都同样加速。

anti-join 在 fresh 时估计 99966 条结果、ANALYZE 后估计 50003，实际 0；本轮读计划在插入 late 非成员之前采集，因此不能与首轮 actual=3 混为一组数据。实际执行约 48–54ms，没有解决选择率估计问题。

同轮生产只读巡检核对 b631 / binary `cf20f619…` 与 `/proc/exe`、registry 一致，无 panic/persistence。最新 5000 行只覆盖约 60 秒，以 HTTP header429 和 attachment429 为主；无 WS 样本，不能据此推断 WS 修复后的自然流量表现。只复用已生成的定时器报告和固定32诊断，没有发生成请求或调整生产设置。

## 后续轮：1M 边界实测与结论

100k 公平捕获有稳定收益后，按 root 排程等待其他 PG 测试明确 terminal，使用完全相同工具 SHA `7d4a9e9dc902a9dec1170e275af04627016dad81bd31cec90ac4f3d3cdd1b14d` 运行 `--rows 1000000`，未改超时、work_mem、并行度或容器资源。报告 `.local/snapshot-members-fair/run1m/report.json`；实际 118.961 秒，5 项语义场景及 42 个计划全部完成，fixture_removed=true，随后释放 PG 窗口。未测试 10M。

| 捕获（6 轮交替） | 原始 INSERT 中位 ms | 分块中位 ms | 原始 INSERT 范围 ms | 分块范围 ms |
| --- | ---: | ---: | ---: | ---: |
| all 1000006 | 3306.884 | 2177.225 | 3191.759–3591.157 | 2067.145–2307.424 |
| selected 666673 | 2319.165 | 1408.957 | 2153.509–2434.692 | 1355.214–1494.918 |

1M 捕获加速比降至约 1.52、1.65。分块 all 的独立 EXPLAIN 已出现两次 external merge sort，磁盘排序空间分别约 25MiB 和 11MiB；根节点累计 temp read 4652 blocks / written 6857 blocks（写约 53.6MiB），直接 INSERT 无临时落盘。EXPLAIN 带逐节点计时，分块总 2673.686ms；应与六轮 psql statement 的 2177ms 中位分开看，不能混用为同一次耗时。

相同 all EXPLAIN 的 WAL：逐行 236078371 bytes，分块 3548336 bytes，仍有明显持久写入量收益。最终成员表体积为逐行 362307584 bytes、分块 12058624 bytes；后者额外包含独立稀疏测试块 4096 个 ID（3337455 对 3333359），并非完全相同计数，故这里只给实际体积，不计算严格等量压缩比。

| 读取（1M） | fresh custom / generic ms | ANALYZE 后 custom / generic ms |
| --- | ---: | ---: |
| packed batch | 2394.596 / 2436.442 | 984.563 / 991.165 |
| row batch | 1595.333 / 1494.815 | 902.889 / 870.066 |
| 页 100：逐项 ANY | 12.632 / 11.631 | 14.053 / 10.700 |
| 页 100：块去重后展开 | 2.419 / 2.194 | 2.347 / 2.309 |
| 集合 anti-join | 599.582 / 573.259 | 712.748 / 725.627 |

fresh packed batch 的估计仍是 40，实际 1000006，逐项 nested loop 是明确退化来源。ANALYZE 后两种 batch 都使用分批 hash join，累计 temp read/write 5988 blocks，分块仍略慢。anti-join fresh 是落盘 merge anti，ANALYZE 后是分批 hash anti；估计 999966/500003、实际 0，计划成本不稳定问题继续存在，ANALYZE 也没有自动带来更快的 anti 路径。

完整稀疏 4096-ID 尾部页 100 的逐项校验约 8.3–10.6ms，展开一次约 4.5–6.3ms。但 analyzed generic 的稀疏页去重结果估计高达 30878289，实际只有 100；当前一次快测没有变慢到秒级，不等于这个估计能安全用于更复杂的下游关联。普通连续页的去重约 2.2–2.4ms，在 100k/1M 都有收益，仍只覆盖单块候选页。

本轮到此冻结原型。捕获减少 heap/index/WAL 的方向有真实证据，但新快照读取明显退化，不能直接替换应用成员表示。推荐后续先验证能稳定避免 fresh per-ID lookup 的批量读取设计，并单独评估页内去重块；不要通过每个快照强制全表 ANALYZE 或简单提升资源掩盖问题。1M 的捕获已约 2.18 秒，且有外部排序，不能外推成 10M 低于 3 秒。应用、migration、鉴权、过滤和删除语义本轮均未修改。
