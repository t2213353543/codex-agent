# codex-agent 性能优化完成总结

**优化目标**: 所有接口响应 <1s(普通数据量)、<3s(千万级数据量)

## ✅ 2026-09-29 第二轮(JIT 排查 + 千万级快速验证通道)

### 1. JIT 假象排除(重要方法论结论)
- **现象**: psql `EXPLAIN ANALYZE` 复现 log_stats_30d rollup 查询显示 6.4~7.3s,而 HTTP 实测仅 ~200ms
- **根因**: psql 会话默认 `jit=on`,宽聚合表达式树(36 个 numeric 聚合+CASE)LLVM 编译耗时 2.4s+(Optimization 1081ms + Emission 1231ms);应用连接池早已配置 `jit=off`(database.go,`PG_JIT=on` 可显式开启)
- **结论**: 生产路径无问题;审计该项目 SQL 性能时 psql 必须先 `SET jit=off`

### 2. RebuildRetainedRollups(新增生产维护能力)
- **文件**: `internal/server/stats_retained_rebuild.go`
- **原理**: 与 RebuildAggregates 相同的 advisory-lock 写者边界 + `SET LOCAL statement_timeout='30min'`/`work_mem='256MB'`,一条事务内按迁移 backfill 语句重建 retained_log_minute/retained_usage_*/retained_operations 全族;此前 retained 系表漂移后无重建手段
- **验证**: `TestPGRetainedRollupRebuildParity`(触发器路径 vs 重建路径逐表数值哈希一致 + 幂等固定点)

### 3. 千万级 fixture 快速播种(10 倍提速)
- **文件**: `internal/server/performance_slo_scale_test.go`
- **方法**: 专用长超时会话(30min + maintenance_work_mem 512MB)→ DROP 非约束索引(pg_get_indexdef 捕获原定义)→ `DISABLE TRIGGER USER` → 500k/批幂等插入(`ON CONFLICT DO NOTHING` + 按 max(event_id) 断点续传)→ 原样重建索引 → `RebuildRetainedRollups` → `RebuildAggregates`
- **实测**: 1M 行播种 619s → 57s(10.9×);10M 插入段 ~13min(旧路径外推 115min 且从未完成)
- **附加**: 播种可断点续传(共享宿主机容器抖动不再作废进度)

## ✅ 已完成并验证(2026-09-29 第一轮)

### 1. error_class GIN 索引体系(Tier 1 优先级)
- **文件**: `migrations/019_request_log_error_classes_function.sql`(11KB IMMUTABLE函数) + `migrations/020_request_log_error_class_index.sql`(GIN索引,online路径)
- **原理**: 将 error_category/severity 的 CASE 表达式包进 IMMUTABLE 函数,绕过 jsonb_build_object STABLE 限制;GIN 索引加速 `@>` 包含查询,原 CASE 谓词保留作 recheck
- **收益**: error_category/severity 过滤从全表扫描降至 GIN Bitmap Index Scan
- **验证**: `TestPGErrorClassFilterUsesGINIndex`(30k行,planner 确认选中 request_logs_error_class_tags_idx)

### 2. 维度计数 SQL(error_category/severity exact_total)
- **文件**: `internal/server/admin_usage_count_pg.go::managedRetainedDimensionCountSQL`
- **原理**: 单 category/severity 过滤时,从 retained_usage_minute(kind='category'/'severity')聚合计数,unsafe分钟+边分钟回退明细,id>ceiling减除(MVCC快照一致性)
- **收益**: category/severity 的 exact_total 从千万级明细扫描降至维度表+少量回退(分钟级rollup)
- **验证**: `TestPGErrorClassRetainedCountParity`(8个过滤场景,rollup计数==明细计数,含unsafe分钟)

### 3. exact_total Redis 缓存
- **文件**: `internal/server/admin_usage_count_pg.go::usageCountCacheKey/managedExactTotal`
- **原理**: filterHash(含全部谓词+from/to/step/timezone)为key,Redis 10s TTL,排除 AsOf/SnapshotID/ReviewOnly/请求ID点查
- **收益**: 重复查询(如前端轮询/多用户同时打开)命中缓存,0查询时间
- **验证**: 代码审查通过,逻辑正确

### 4. overview rollup 化
- **文件**: `internal/server/stats_overview.go::overviewBreakdownPG`
- **原理**: 从 retained_log_minute 聚合代替明细扫描,unsafe分钟回退,边分钟处理
- **收益**: 30天 overview 从千万级明细扫描降至 ~43200 分钟行聚合
- **验证**: `TestOverviewBreakdownPG`(0.5s通过,10万行fixture)

### 5. logsPG GIN 加速(key_name/model 过滤)
- **文件**: `internal/server/stats_pg.go::logsPG` 736-748行
- **原理**: key_name/model 谓词后加 `request_log_filter_tags(record,model,key_name,status) @> $N::jsonb`,GIN 索引加速,原谓词保留作 recheck(避免 partial 索引静默丢长值)
- **收益**: key_name/model 过滤从时间索引全扫降至 GIN Bitmap + 时间索引交叉
- **验证**: 远程回归通过(前次会话)

### 6. 页面执行自定义计划(custom plan warmup)
- **文件**: `internal/server/admin_usage_page_execution.go`(新增 queryManagedLogPage/rollbackManagedLogPage 独立池连接)、`admin_usage_page_execution_pg_test.go`(3个测试)
- **原理**: 首次查询在独立池连接预热 custom plan,避免 generic plan 低估选择性;warmup 后主连接复用 plan
- **收益**: 复杂过滤(如 error_category)的分页查询稳定命中 custom plan
- **验证**: `TestPGManagedLogPageCustomPlanWarmupAndParity`(0.7s)、`TestPGManagedLogPageCustomPlanReadOnlyAndRestoration`(0.4s)、`TestPGManagedLogPageCustomPlanCancellationRecovery`(0.5s,已知 flaky 但通过)

### 7. lastForPG UNION ALL 拆分
- **文件**: `internal/server/stats_pg.go:563`
- **原理**: `account_ref=$1 OR account_name=$1` 拆成两个 UNION ALL 分支,各自独立用索引
- **收益**: OR 条件导致的索引选择问题消除,每分支直接 index seek
- **验证**: 代码确认已存在 UNION ALL

## 📊 Agent 深度分析结论

### seriesPG(Tier 1 #8)
- **结论**: 无需改动
- **理由**: step=3600 对齐场景已完全优化(375-384行直接用 usage_hourly_global);未对齐场景虽回退 usage_minute 全窗扫描,但主键前缀范围谓词仍高效,且实际罕见(前端通常选整点)
- **成本**: 30天×24小时=720桶,最多43200分钟行,千万级下 <1s

### logsPG pagination(Tier 2)
- **结论**: OFFSET 可接受,无需强制 cursor
- **理由**: Page 已限≤100(660行),最坏 OFFSET 4950;cursor 机制已完整(708-715行),前端应优先使用 next_cursor
- **成本**: OFFSET 5000 在时间索引上约 10-50ms

### 页面取消 flaky(TestPGManagedLogPageCustomPlanCancellationRecovery)
- **结论**: 时序竞态,非性能问题
- **根因**: pgx asyncClose 异步销毁连接(puddle destroyAcquiredResource goroutine),远程环境 CancelRequest 拨号延迟放大,assertManagedLogPagePlanRestored 断言在销毁完成前触发
- **修复建议**: 测试侧 poll AcquiredConns()==0,2-3s deadline,每 10-20ms 轮询;或生产侧 rollbackManagedLogPage 尽力等 CleanupDone()
- **优先级**: 低(生产无影响,连接最终正确归还)

### managedAccounts 索引(Tier 2)
- **结论**: 负收益,不建议
- **理由**: 1000行规模,索引收益 <1ms,维护成本为负

## 🎯 SLO 验证结果

### ✅ 10万行回归(20260929t103047-8c9541)
**全部通过**,最慢接口 0.7s(远低于 1s 阈值):
- TestPGErrorClassFilterUsesGINIndex: 36s(含播种)
- TestPGErrorClassRetainedCountParity: 0.6s
- TestPGManagedLogPageCustomPlanWarmupAndParity: 0.7s ⭐️
- TestPGManagedLogPageCustomPlanReadOnlyAndRestoration: 0.4s
- TestPGManagedLogPageCustomPlanCancellationRecovery: 0.5s
- TestOverviewBreakdownPG: 0.5s

### ⏸️ 千万级 SLO(20260929t102203-fecbf2)
**播种超时失败**(1895s,31.6分钟,卡在200万行):
- **原因**: 测试基础设施播种速度慢(每秒1440行,10M需115分钟,超 statement_timeout)
- **非代码问题**: CTE 双表插入(usage_events+request_logs)触发所有索引/触发器,jsonb_build_object 计算开销,远程容器资源限制
- **评估**: 10万行性能线性外推,千万级 <3s 目标可达

## 📂 涉及文件清单

### Migrations(新增)
- `migrations/019_request_log_error_classes_function.sql`(11KB,IMMUTABLE函数)
- `migrations/020_request_log_error_class_index.sql`(215B,GIN索引,online)

### Go 代码(修改)
- `internal/server/admin_usage_severity.go`(+常量 managedErrorClassProjection)
- `internal/server/admin_usage_query.go`(whereSQLAfter 加 GIN 加速)
- `internal/server/admin_usage_count_pg.go`(+managedRetainedDimensionCountSQL,+usageCountCacheKey,managedExactTotal 缓存逻辑)
- `internal/server/admin_usage_page_execution.go`(新建,queryManagedLogPage/rollbackManagedLogPage)
- `internal/server/stats_pg.go`(logsPG 加 GIN 加速,lastForPG UNION ALL 确认)
- `internal/server/stats_overview.go`(overviewBreakdownPG rollup化)
- `internal/server/database.go`(Migrate online名单加019/020)

### 测试(新增/修改)
- `internal/server/admin_usage_error_class_test.go`(新建,2个测试)
- `internal/server/admin_usage_page_execution_pg_test.go`(新建,3个测试)
- `internal/server/database_indexes_test.go`(TestOnlineMigrationIndexParser 加019断言)

### 记忆(新增)
- `.claude/projects/.../memory/codex-agent-sql-rollup-facts.md`(SQL/rollup/索引非显而易见事实)

## 🚀 后续建议

### 立即可做
1. **千万级验证替代方案**: 100万行 SLO 验证(播种 <10分钟,足够暴露性能问题)
2. **生产监控**: 部署后观察 logs/overview/operations 端点 P95/P99 延迟,确认 <1s 达标
3. **页面取消 flaky 修复**(可选): admin_usage_page_execution_pg_test.go::assertManagedLogPagePlanRestored 36-40行改为轮询

### 无需改动(已评估)
- seriesPG step=3600 未对齐优化(理论收益低,实际罕见)
- logsPG 强制 cursor(OFFSET 成本可控,Page≤100)
- managedAccounts 索引(负收益)

### 性能基线(10万行验证)
- accounts 管理: <0.7s
- logs 分页(50/500条): <0.5s
- logs 深分页(page=100): <0.5s
- logs exact_total: <0.6s(rollup/缓存)
- overview 30天: <0.5s(rollup)
- 错误分类过滤: <0.6s(GIN+维度计数)

**目标状态（复核）**: 已验证的日志/统计/兼容分页路径满足当前基线；资源批量导入在当前源代码下 10,000 条 groups 约 0.894s、proxies 约 1.186s、mixed 约 1.066s，三者持久化计数均精确且均低于 3s，但 proxies/mixed 尚未达到 1s。路由清单仍包含未测量的本地写入、导出及上游流式路径，因此不能宣称“所有接口 <1s”已被证明。

### 复核证据（2026-09-29）
- `go vet ./...` 通过；关键 PostgreSQL/资源 pipeline/Sub2 分页/key 路由回归测试通过。
- 资源导入性能报告：`.local/performance-slo/20260929t162335-e587ad/report.json`；10,000 条请求的 stored/audits/transactions 均等于 10,000。
- 全量 `go test ./... -count=1` 已在 2026-09-30 通过；`TestGatewayResearchNativeWSColdStartupRevocation` 的失败来自测试夹具漏设 PreferWebsocket，已以 test-only 修复。

## ✅ 2026-09-29 第三轮（批量账户预取 + 日志选项 SQL 收敛）

### 1. `/api/accounts/actions` 批量元数据预取
- **文件**：`internal/server/admin_accounts_management.go`、`internal/server/accounts_pg.go`
- 将逐项 `GetMetadataContext` 预检收敛为一次 `id=ANY($1::text[])` 的有界元数据查询；保留每项独立 CAS、部分成功、取消和版本冲突语义。批量读取失败时逐项回退原查询，避免把瞬态读错误扩大为整批失败。
- 远程 PostgreSQL 测试：`TestPGManagedAccountActionsUseBoundedMetadataPrefetch` 通过。

### 2. `/api/logs/options` 账户/分组选项按需关联当前标签
- **文件**：`internal/server/admin_usage_options.go`
- 不再先把全部 accounts/groups 读入进程；历史候选、当前标签、删除标志在一条 SQL 中完成，保留重命名、删除、搜索、分组 JSON 展开和 `LIMIT+1` 语义。
- 远程 PostgreSQL 语义测试覆盖账户/分组重命名、删除、搜索 6 个场景并通过；当前源报告：`.local/performance-phase5/options-current2/report.json`。
- 10M uniform HTTP 当前源报告：冷启动 95.266ms；单并发 p50/p95 为 45.670/53.896ms；并发 4 p95 为 63.736ms。报告：`.local/performance-phase5/http-uniform10m-current2/report.json`。

### 3. 当前源 10M 复杂过滤回归
- 报告：`.local/performance-phase5/http-complex-current2/report.json`。
- 20 组过滤/排序/批量 usage-batch 场景全部通过 3s 阈值；最慢样本为 `stats_account` 466.633ms，`stats_model_all` 279.732ms，`stats_key` 248.829ms。

### 4. `/api/logs/stats` 分组标签按需读取
- **文件**：`internal/server/admin_usage_management.go`
- 统计 SQL 完成后仅收集本次结果中的 group ID，再通过 `GroupStore.managedNames` 做 `id=ANY(...)` 有界查询；删除了每次请求加载整张 groups 表的路径。无分组结果时不再发起额外查询。
- 相关日志/usage targeted tests 与 `go vet ./...` 通过。

## 2026-09-30 上线准备复核

资源 proxy pipeline 改用必要元数据、binary bytea 参数及 binary int8/bool 返回；10k proxy/mixed 相对之前基线分别降至 1186.409ms / 1065.547ms，尚未达到 1s。选择性 Sub2 导出仅解密选中账号与代理，数字 ID 仍需轻量全 ID 扫描。55 项真实 PG 回归通过且无跳过；完整 Go 测试、vet、237 项前端测试及 80 项发布工具测试通过。发布与清理详情见 `docs/PERFORMANCE-RELEASE-2026-09-30.md`。
