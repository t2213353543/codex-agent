# 管理查询与保留机制审查

已完成独立合成库性能采样、修复前后 EXPLAIN 对比、实际 prune-logs / rebuild-usage 命令验证；生产未迁移、未部署。以下候选数据对应 Linux SHA256 `9be0b2156bf522a52eabf36b0f2762fd9332c6480f34916c2b841b1a907c074e`，包含批量统计修复和概览缓存，早于后端最终 main / refresh / settings 收尾，不自动等同最终发布版本。源码快照哈希见 `results/linux-candidate-build-current.json`。

## 真实性能样本

PG 18.4 专用数据库 codex_agent_performance；合成账号 1,000、分组 10、禁用代理 10、日志和持久事件各 100,000，覆盖约 120 日；分钟聚合 100,000 行、日聚合 92,057 行。应用独立容器为 1 CPU、512 MiB、pids 128，与测试 PG/Redis 同机。HTTP 脚本在服务器 localhost:28880 运行，真实 cookie 会话，每请求新建 HTTP 连接，不含 SSH 往返，不调用网关。

每场景预热 3 次、正式 30 次，nearest-rank 百分位。每并发档 12 场景、360 请求，共 720 请求全部 HTTP 200。没有业务 SLO，以下是实测值，不擅自按阈值判定已达到生产目标。

| 场景 | 单并发 P50 / P95 ms | 4 并发 P50 / P95 ms |
|---|---:|---:|
| 账号列表 50 条 | 6.890 / 7.277 | 7.728 / 71.729 |
| 账号列表 500 条 | 52.171 / 74.897 | 199.247 / 231.282 |
| 账号筛选 | 7.342 / 8.105 | 8.329 / 80.516 |
| 日志 50 条 | 1.153 / 1.494 | 1.991 / 3.497 |
| 失败日志 50 条 | 1.219 / 1.469 | 1.997 / 2.625 |
| 单账号日志 | 1.293 / 1.862 | 1.848 / 2.499 |
| 默认概览（预热后缓存） | 0.803 / 1.308 | 1.585 / 2.623 |
| 1 日概览（显式范围） | 68.512 / 70.623 | 295.219 / 396.057 |
| 7 日概览（显式范围） | 70.228 / 90.269 | 298.758 / 407.392 |
| 30 日概览（显式范围） | 96.298 / 116.275 | 380.892 / 405.893 |
| 账号下一游标 | 6.623 / 7.101 | 8.052 / 60.025 |
| 日志下一游标 | 1.243 / 1.470 | 1.846 / 2.968 |

逐请求样本、响应字节和状态码见 results/http-server-c1-session.json、http-server-c4-session.json。其中 recorded_at_utc 继承自先前 DB 元数据，表示元数据时间，未单独精确记录这轮 HTTP 起止时刻；不得用它声称 HTTP 测试时间。工具已补 http_started_at_utc/http_finished_at_utc，下一轮才有该证据。原始候选样本未改写。

Windows app 通过 SSH 访问远端 PG/Redis 曾触发 Redis 一秒 read timeout/readiness 503，因此未用该环境的 HTTP 延迟作为应用性能。服务器本机结果与本地隧道现象分别记录。

## SQL 与可维护性

| 项目 | 修后观察 / 结果 | 边界 |
|---|---|---|
| 批量账号统计 | 已移除日聚合 ID/name OR JOIN；先解析身份，再按 account_ref MATERIALIZED 选取一次，最后查状态 | 50账号 194.995 → 2.334 ms；500账号 2028.685 → 26.952 ms。修前500次扫描，每次过滤23,338行、shared hits296,500；修后CTE一次扫描11,680行、hits458 |
| 账号页网络 N+1 | 账号1 + 分组1 + 代理1 + 批量统计3，共固定6次业务查询 | 不含认证，静态路径；未装 pg_stat_statements，未实测每请求 SQL 调用数 |
| 关系查询 | 单次 SELECT 内两个子查询使用主键/索引 | 501行页 SubPlan 各循环501次，是库内逐行工作，没有逐账号网络往返。50条元数据2.283ms、500条16.930ms |
| 凭据分离 | 管理列表不读 account_credentials | 元数据仍含 notes/tags/last_error；本轮未压最大长度文本 |
| 概览账号计数 | 仍 ListMetadata 读全部账号/关系用于计数 | 1000账号该SQL32.322ms、关联SubPlan各1000次；规模增长的CPU/内存问题未消除 |
| 日志游标 | created_at DESC,id DESC，limit+1，total=-1，无每页全量COUNT | 首屏/账号/错误/深游标SQL0.294–0.366ms。辅助选取深游标的一次OFFSET不在应用查询内 |
| 旧 page | 最多100页，连续游标读 | 无大OFFSET，但最多100次SQL；新界面应使用cursor |
| 密钥 N+1 | handleKeysList逐key调用KeyUsage | 未修；未做大规模密钥压测，不能声称所有管理列表无N+1 |
| PG池 | MaxConns默认12、MinConns0、连接5秒、statement15秒、lock5秒 | 有界；100参数上限仍须服从部署总连接预算 |
| Redis池 | PoolSize10、dial2秒、read/write1秒、无无限重试 | 正常会话已测；故障拒绝放行由主集成证据证明 |
| 概览缓存 | 已实现无query概览GET/SET、TTL10秒、异常回源PG、写路径invalidate | 缓存与显式范围分别采样；失效/故障场景由主集成故障测试负责 |
| 全量COUNT | 日志翻页不COUNT；离线fixture校验COUNT | 不把离线数量核对当作列表查询 |
| 保留范围 | 仅request_logs清理；usage_events及分钟/日聚合保留，tokens_retained范围筛选 | 所有表不会在92天后自动缩小，需容量监控 |
| 保留调度 | CLI prune-logs存在，无应用内定时器 | 部署需定时任务与执行记录 |

results/explain.json 是修后完整 EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)，修前为 explain-candidate-ebe1a068.json。explain-id-only-proposal.json 是修复前对照实验，不是最终SQL。SQL数字是单次计划服务端执行时间，不是SQL P50/P95；HTTP才有重复采样。

## 保留与重建证据

实际候选执行 rebuild-usage → prune-logs --before 2026-06-17T09:48:04Z → rebuild-usage，见 results/retention.json：

- 精确删除23,298条到期明细，100,000→76,702；CLI数量与DB一致。
- 持久事件仍100,000；累计token139,900,000；usage_totals九字段清理前/后及重建后相同（成功90,000、错误10,000）。
- 初次重建2348.348ms、清理212.297ms、清理后重建2474.138ms。
- CLI每批5000行上限仅源码审查，未逐批插桩。重建独占advisory lock，事件写入共享同一锁；大规模重建会暂停提交。

**旧证据限制：** PsqlConnection row_to_json 对未命名SUM列生成同名JSON key，Python折叠字段；旧 minute/daily 数组只有最后latency_n=100000。旧报告 pruning_preserves_all_aggregates / rebuild_matches_preprune 的true完整覆盖usage_totals九字段，但不证明分钟/日九字段。不得用旧报告声称全部聚合完整比对通过。工具已加唯一列别名，并新增只读补测：

```powershell
python integration/performance/remote_candidate.py retention-check
```

补测逐项核对grand/minute/daily九字段，并以双向EXCEPT核对所有分钟/日时间桶和账号维度与usage_events一致；不删数据、不重建、不还原丢失的旧快照。补测尚未执行，只有生成的retention-check.json可作为补足证据。原始retention.json未改写。

候选新增accounts.ordinal导致迁移checksum不一致时，服务正确拒绝启动。仅专用性能库执行data-only合成备份→重建public→真实migrate→恢复合成数据→数量核对，没有修改checksum绕过，没有动主集成库或生产。

## 复跑与发布边界

运行说明见README.md。已经清理的性能库现在是76,702条明细；后续采样须记录此规模，不把它与清理前100,000条无说明合并。最终候选与本报告SHA不同，须记录差异和有限复测。性能通过、迁移完成、生产部署分别判定。
