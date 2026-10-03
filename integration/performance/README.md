# 独立性能验收

第三阶段继续验证均匀千万级和复杂筛选。原有 1M 均匀 fixture 可原地追加到 10M：

```powershell
python tools/performance_uniform_extend.py --fixture-report .local/performance-uniform/20260928T224734-987eb8/report.json --source .local/performance-slo/20260928t234046-15a493/source --rows 10000000
```

该入口只允许 `perf_uniform_*` 合成 schema，预检已有迁移 checksum 和可用磁盘（至少 60 GiB），保留 i=0…999999，按原公式追加 i=1000000…9999999。每 10 万条用独立事务写入真实 events/logs，保留所有汇总触发器，并在同一事务更新 `performance_uniform_extend_checkpoint.next_i`。重复运行相同命令会跳过已提交的批次，不依赖本地日志猜进度；当前未提交批次回滚后可重做。末尾精确验证 events/logs/账号分钟/两类 retained 计数为目标值，随后还需显式真实 `RebuildAggregates` 才能测 durable 总量接口。批量准备可与独立功能检查共享测试实例，因此批次耗时不作为摄取吞吐结论；正式 HTTP/SQL 测量仍需排他窗口。

复杂筛选原版证据见 `results/complex-filters-20260929-baseline.json`：千万级突发数据的 16 个单次真实 HTTP 场景中 14 个失败或超过 3 秒，包括全匹配 model/key 统计、无匹配 IP/endpoint/key 列表、多个未索引排序和百账号批量用量。第一阶段／第二阶段的常用形状通过不能替代这些复杂参数组合的证据。

第二阶段最终性能报告：`results/slo-20260929-phase2-final.md` / `.json`，源码 `41d1ed1d38c1…`、同一二进制 `f8f905990fce…`。四组排他顺序实测全部通过：均匀 10 万最慢 549.786ms、均匀 100 万 793.906ms；突发 10 万 292.317ms、突发 1000 万 374.033ms。均匀各 6 场景，突发各 62 场景，去重后总计 5576 个读取样本；另外两次独立 schema 写入共 520 样本。兼容接口在突发迁移前先执行，因此两组均在 010–012 完成后单独补测 27 个场景，最终汇总采用补测结果替换而非重复计数。报告保留千万均匀、任意参数、任意并发、上游调用未验证的限制。

均匀分布／高基数第二阶段入口：`python tools/performance_distribution.py --rows 100000`，然后 `--rows 1000000 --source <100k-run>/source`。每个事件对应不同账号分钟，30 天共 43200 个分钟，独立 `perf_uniform_*` schema，不修改此前 `perf_slo_*` 的千万级突发 fixture。工具直接捕获冻结 Go 实现生成的统计 SQL，运行 `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)`，记录精确日志／账号分钟数量、维度行数、汇总 heap/index 体积、spill、实时进度与 container identity；它是 SQL 实验，不能单凭结果宣称 HTTP SLO 通过。

第二阶段原版 `2aea57c48ef3…` SQL 基线见 `results/uniform-20260929-sql-baseline.json`：100k / 1M 条均匀日志产生 281440 / 1181440 条分钟维度汇总，统计 SQL 约 1877.742ms / 9280.150ms，表明第一阶段突发分布达标不能推出均匀高基数达标。HTTP 对照使用 `TestPerformanceUniformHTTP`，会在该随机 schema 补全不可用合成凭据与账号文档、调用真实 `RebuildAggregates`，然后经 HTTP 测量；构造阶段不改原始 logs/events。旧实现的百万分钟逐行维护小时汇总所产生的维护耗时也保留，不计入接口延迟。

当前源码的 SLO / 千万级入口（2026-09-29）：

```powershell
python tools/performance_slo.py --rows 10000000 --samples 20
```

该入口冻结完整后端源码与迁移、编译 Linux 测试程序、分片上传并核验 SHA256，使用现有隔离 PG 的随机 `perf_slo_*` schema。生成 **1000 个账号、1000 万条 usage_events、1000 万条 request_logs**，完整应用迁移和真实 RebuildAggregates；账号均禁用、凭据不可用，不连接模型上游。数据按 30 天突发分布生成，含 10% 错误请求，形成约 3 万个账号分钟桶。它验证该分布的千万级事件/日志查询，不代表千万账号、每条记录维度各异的高基数场景或任意分布。

默认保留随机合成 schema，便于不同源码版本使用相同数据复测；报告记录 schema 和实际精确行数。普通规模阈值为 **每个请求 <1000ms**，千万事件规模为 **每个请求 <3000ms**，包括最慢请求；超时、非 200、JSON 错误与被检查的响应语义不符均记为失败。每个场景记录首次请求及并发 1/4 的逐请求样本、P50/P95/P99/max、状态、字节数和数据库连接获取次数。采样 20 次时 P99 等于该批最大值，不能推断长期分位数。

当前 35 个读取场景同时检查行数、统计总量、错误/成功数量、过滤身份及响应协议。其中包含有匹配与无匹配的请求 ID 查询、无匹配的客户端请求 ID 查询，空结果也必须返回 200 且满足耗时阈值。测量准备会在同一个隔离 schema 中增加 100 个禁用合成密钥、100 条 ingress 事件和 10 条已完成清理任务元数据，让辅助接口返回实际非空内容；不执行清理任务、不删除日志。密钥规模是 100，独立的万密钥基准见 `BenchmarkSystemPGKeyCurrentSnapshot`，不能把这里的密钥读取称为千万密钥验证。早期基线为 33 个场景，最终新增的两个空匹配场景没有旧版对照，不应计入前后总场景数比较。

使用 `httptest.NewServer` 真正经过同机 loopback HTTP、管理路由、鉴权和 JSON 编解码。首请求仅指新测试进程首次访问该端点，前面其他端点可能已预热共享状态；不清除共享主机的操作系统和 PG 缓存，因此不声称物理磁盘冷缓存性能。该入口默认未配置 Redis 应用缓存；独立 Redis 服务不参与这些延迟。HTTP 客户端复用连接，不含公网或模型推理时间。

```powershell
# 先构造千万级 fixture，随后在同一个 schema 上冻结新源码复测
python tools/performance_slo.py --rows 10000000 --phase seed
python tools/performance_slo.py --reuse .local/performance-slo/<run>/report.json --samples 20
# 使用旧业务快照与当前同一套 SLO 测试对照；有效混合源码会重新冻结并逐文件记录哈希
python tools/performance_slo.py --source .local/performance-slo/<old-run>/source --reuse .local/performance-slo/<seed-run>/report.json --samples 20
# 缩小到指定场景，仍保留真实失败
python tools/performance_slo.py --reuse .local/performance-slo/<run>/report.json --cases overview_30d,log_stats_30d,operations_24h
# 不造规模数据，仅执行当前源码的真实 PG 回归
python tools/performance_slo.py --test-run '^TestPGOperationsSQLParity$' --timeout-minutes 10
# 常规本地写请求及并发持久化：使用独立临时 schema，不改千万级事件基数
python tools/performance_slo.py --test-run '^TestPerformanceSLOWrites$' --samples 20 --timeout-minutes 15
# 已有验收二进制：核验源/产物 SHA 与目标 schema 全部迁移 checksum，直接复用，不编译上传
python tools/performance_slo.py --binary-report .local/performance-slo/20260928t223513-9c8dcb/report.json --remote-binary /opt/codex-agent-integration/checks/performance-slo-20260928t223513-9c8dcb/server.test --reuse .local/performance-slo/20260928t211543-5a5a53/report.json --samples 20
```

2026-09-29 最终证据见 `results/slo-20260929-final.md` / `.json`。十万级与千万级各 35 场景、1435 次读取，同一源码 `2aea57c48ef3…`、同一二进制 `3f505b9e2e9a…`，各自最慢 312.450ms / 1808.511ms；同版本另有 260 次写入/持久化采样、362 项管理回归。13 项 race 使用此前源码，最终报告明确列出最后 Group.create 持久化时间戳修复的差异，没有将不同源码的证据混称完全一致。

证据写入 `.local/performance-slo/<run>/`：`source-manifest.json`、`build.log`、`run.log`、`report.json`。报告包含路由清单及本地读/写、上游/流式、批量导出分类，未采样路由明确标记 `not_measured`。`all_routes_slo_proven` 始终为 false：这组读取样本不替代所有接口、所有输入、所有并发的保证。首请求错误或超标会保留失败样本并跳过该场景后续压力；并发 1 失败后也不继续并发 4，不把快速错误响应当作通过。完整采样只对实际运行阶段统计分位数，未执行阶段保持缺失。

旧入口与历史证据：

工具：`python tools/performance.py`。只接受 `.local/integration/performance-environment.json` 中明确标记为合成环境的 `codex_agent_performance` 数据库；HTTP 只接受 `127.0.0.1:28880`。不会将规模数据写入 `codex_agent_integration`，不会输出 DSN、凭据、cookie 或管理密钥。

实际端到端采样使用同机模式：测试候选容器、PG、Redis 都在当前主节点的独立测试环境，本机 loopback 28880；Python HTTP 采样同机执行。Windows 上通过 SSH 访问 PG/Redis 曾触发 Redis 1 秒超时，因此不把该网络环境测出的延迟当作应用性能。`28882` 仅为该测试 HTTP 的本地隧道，也被工具允许；不同网络路径必须分别标记。

已建好专用 fixture 后的可重复同机入口：

```powershell
python integration/performance/remote_candidate.py deploy --binary .local/integration/performance-server-linux
python integration/performance/remote_candidate.py explain
python integration/performance/remote_candidate.py benchmark --samples 30
python integration/performance/remote_candidate.py benchmark --samples 30 --concurrency 4
python integration/performance/remote_candidate.py retention
python integration/performance/remote_candidate.py retention-check
```

该入口只管理 `/opt/codex-agent-integration/performance` 与带标记的 `codex-agent-integration-performance` 容器，使用已存在的官方 postgres:18-alpine 运行静态 Go 二进制，read-only、非 root、cap-drop ALL、no-new-privileges、1 CPU、512 MiB、pids 128。数据库端口/账号/命名空间仍来自私有配置，不采用现有业务 PG/Redis。运行前按 `sub2api-production` 技能确认当前主节点；不修改其他服务。上传切块并验证 SHA256；重复部署只替换此标记容器。`explain` 使用现有 PG 容器内 psql，无需在服务器安装 Python 包。

依赖准备：

```powershell
python -m pip install --target .local/performance-python "psycopg[binary]>=3.2,<4"
```

数据库须由交付版本的 `migrate` 显式建立；配置文件的环境变量须传给该命令及独立测试进程。使用同一冻结二进制，记录 SHA256。不要在 Go 实现仍变化时把结果称为发布版性能。

数据准备与运行顺序：

```powershell
python tools/performance.py seed --accounts 1000 --events 100000
# 用 performance-environment.json 的环境变量执行交付二进制 rebuild-usage，再启动独立测试进程 28880。
python tools/performance.py explain
python tools/performance.py benchmark --admin-key-file .local/integration/performance-data/client-key --samples 30
python tools/performance.py benchmark --admin-key-file .local/integration/performance-data/client-key --samples 30 --concurrency 4
python tools/performance.py retention --binary .local/integration/performance-server.exe
```

`seed` 只接受空库并在事务中生成 1,000 个账号、10 个分组、10 个禁用代理、100,000 条跨 120 日的事件及明细。所有令牌由固定合成模板生成，以真实 AES-GCM 存储格式加密，无刷新令牌，禁止调用任何网关或刷新路径。该批量装载仅建立查询数据集，不代表逐事件摄取性能或导入 CLI 验证。账号、关系、凭据和日志都不取自生产。

`explain` 对交付代码对应查询执行 `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)`，保留完整计划、参数、数据量、PG 版本、源码哈希。深游标的选取在测量前用一次 OFFSET 定位；被测应用查询仍是 `(created_at,id)` 游标，无 OFFSET。手工镜像 SQL 应与最终交付源码核对，变化时更新脚本再测。

`benchmark` 默认先建立真实 cookie 管理会话，测列表（50/500/筛选/游标）、日志（首页/失败/账号/游标）和 1/7/30 日概览。记录逐请求样本、状态码、成功/失败数量、P50/P95（nearest-rank）和响应字节。未指定业务 SLO，因此报告真实延迟，不擅自用阈值判定性能已达标。HTTP 在本机，PG/Redis 经 SSH 隧道，网络往返包含在 API 延迟内；SQL 服务端执行时间单独记录。每请求新建 HTTP 连接；预热次数明确记录。

`retention` 先调用实际交付二进制 `rebuild-usage`，保存累计/分钟/日聚合，再调用实际 `prune-logs --before <92日前>`，最后再次调用实际 CLI 重建。验证明细精确减少、CLI 删除数量一致、持久事件不变、累计 token 与事件相符、聚合重建前后一致。该步骤会改变专用性能库，适合性能采样后执行；CLI 每批 5,000 行的上界通过源码审查，没有对每批做计数插桩。重跑须另建空的专用性能库，工具不会自动清库。

所有结果写入本目录 `results/`。`fixture.json`、`explain.json`、`http-c1-session.json`、`http-c4-session.json`、`retention.json` 是实际证据；没有生成的项目就是未执行，不能替换成示例数字。查询数/N+1 与保留调度审查记在 `REVIEW.md`；若没有 `pg_stat_statements`，明确只做静态调用数审查，不声称实测每请求 SQL 数。

旧候选 retention.json 的分钟/日 SUM 列存在测试适配器同名 JSON key 折叠，完整限制见 REVIEW.md。已修正为唯一列别名。`retention-check` 是独立只读补测，逐字段并逐时间桶对照持久事件；不会清理或重建。已清理过的 fixture 可用 `remote_candidate.py retention --allow-no-expired` 再执行实际 CLI 重建及清理命令；若没有到期明细会显式记录 `nonempty_prune_exercised=false`，不冒充再次执行了非空清理。新的同机 deploy 会读取实时数量，绑定 acceptance.source_manifest 的总 SHA；开始测试后不要改实现文件。
