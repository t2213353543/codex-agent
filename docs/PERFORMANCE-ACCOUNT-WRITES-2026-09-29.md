# 本地管理写入性能审查与批量导入

第三阶段开始时，既有后端文件对照正式基线 `41d1ed1d38c1ed829bc0caec9e7193d74027c015fc6cb96b01c904840b789d90` 的 manifest 无差异。之前 13 组写入包含 9 个 group/key/proxy CRUD、单账号 metadata、last-used/usage-batch 两种 POST 读取，以及直接存储 API 的并发 durable ingest；不是所有管理写路由已完成性能验收。

## 三个最重的账号管理路径

1. `pgImportDetailed` 逐账号检查 token 指纹，再按显示名读取 metadata/凭证，新账号继续 account、credentials、两种关联、审计、active 设置等多次数据库操作。1000 个全新账号会产生约数千至上万条 SQL 往返；同 token/名称的替换和 refresh rotation 又有顺序语义，不能简单并行改变结果。
2. `POST /api/accounts/actions` 更新 100 项时，每项先 GetMetadata/CAS 校验，再进入 UpdateAccountVersion 重新读取、更新、重写关联、审计、重读和提交。响应是逐项成功/失败，不能把一个失败改成全部回滚，也不能直接并发含名称/版本冲突项。
3. `pgSaveMetadata` 即使只变 Enabled/Notes 也删除并重插全部 group/proxy 关系，额外产生索引/FK 工作；随后读取完整 metadata。该成本在批量更新中被放大。当前先修最重的全新导入路径，其余保留明确待测而非未经验证改写事务合同。

## 已实现的新账号批量路径

`pgImportNewAccounts` 只处理有效记录至少 2 个、批内名称/token 指纹各不重复、数据库无相同名称/token 的批次。调用方继续持有原 import/name-rename advisory lock。一次 ANY 查询确认边界后，按表批量 COPY accounts、credentials、非空 groups/proxies 和逐账号 audit；最多 5 次 COPY，随后必要时为原空库设置第一个 active account。

COPY 位于原事务，保留 FK、row trigger、每项审计、owner revision、credential version=1、代理顺序和 ID。DryRun 执行后回滚，没有预览写入。存在任何已有项/批内重复时完全回到原有逐项有序逻辑，保留 name conflict、ambiguous name、duplicate、显式 replacement 与 refresh-token rotation。

**没有降低 10000 行导入上限，没有跳过验证，没有提前返回假成功。** 优化后数据正确性仍由提交后读取和审计条数验证。

## 已取得验证

`.local/performance-slo/20260929t000540-e841f3/report.json`，源码 `08e16b2c7b208d29b9269927c919c7e01e666c7db8aedb4fdc96cc7f6c2810c9`：5 项顶层 PG 测试、7 项含子测试全部通过、无跳过。

- 100 条新账号 + 1 条无效输入，预览报告/回滚与正式提交一致。
- account metadata、凭证、owner revision、初始代数、关联去重及代理位置保持。
- 100 条审计完整写入；外键无效或审计 trigger 抛错时整个导入回滚。
- 重复 token/同名/显式 replacement 的有序 fallback。
- 既有 PG import conflict/dry-run、Sub2 导入 metadata/partial validation 回归。

真实 HTTP 性能 harness `TestPerformanceAccountBulkWrites` 已在排他窗口执行。`.local/account-bulk-writes-20260929/report.json` 使用同一已验证源码/二进制，独立 synthetic schema，18 个请求全部成功、语义正确且小于 1 秒；测试总执行 2.89 秒。完整读取响应并核对落库数量，不修改已有千万行读取 fixture，不调用模型/授权提供方。

| HTTP 输入规模/动作 | 3 次样本范围 |
| --- | ---: |
| 100 账号预览 | 10.013–12.621 ms |
| 100 账号全新导入 | 10.560–11.172 ms |
| 100 账号重复导入（原有逐项分支） | 46.123–53.033 ms |
| 1000 账号预览 | 90.763–188.890 ms |
| 1000 账号全新导入 | 95.800–186.387 ms |
| 1000 账号重复导入（原有逐项分支） | 461.588–507.723 ms |

每个请求包含管理鉴权、JSON 解析、数据库操作、JSON 输出和完整 TCP 响应读取。库中账号随每个成功批次累计，因此后续样本不代表固定表规模；每项仅 3 次，不宣称长期 P99 或更高并发保证。尚无同 fixture 旧/新 A/B，不能把上述当前延迟换算为未经测量的加速比。10000 行受支持上限保持，但本轮只实测 100/1000。

## 合法 10000 行上限与纯重复优化

后续专门使用 `PERFORMANCE_IMPORT_SIZES=10000` 验证现有合法批量上限，JSON 请求体为 1,386,684 字节，完整 HTTP 返回并核对 10000 个 added/duplicate 结果。没有缩小接口上限，也没有用截断报告降低输出工作。

首轮 `.local/performance-slo/20260929t004945-8abc64/report.json` 暴露明确热点：10000 项重复导入耗时 **21995.112 ms**，HTTP 200 且计数正确；新导入/预览分别 674.883/676.411 ms。一次事务虽只占一个 pool connection，仍会逐项执行指纹查找、metadata、凭证读取，不能把低 `db_acquires` 误认作低 SQL 往返数量。

新增 `pgImportUnchangedAccounts` 用单条 `DISTINCT ON(token_fingerprint) ... ORDER BY token_fingerprint,account_id` 批量读取候选密文。原冻结版指纹查询同样 `ORDER BY account_id LIMIT 1`，且指纹索引非 UNIQUE，因此新路径明确保留该选择规则。每个不同指纹只解密一次，并执行原有 accountSecret JSON/Identity 解码与 owner revision 校验。

只有所有已存在有效输入的 refresh token 为空或与当前相同时才进入 duplicate 快路径。该条件与原 `importedRefreshRotation` 的无写分支相同；元数据/Identity 不同但 refresh 相同时仍不更新。refresh token 改变时整个批次回原顺序分支，不提前修改 report。重复输入指纹的输出顺序、identity conflict、两次连续 refresh rotation 都保持。

`.local/performance-slo/20260929t005511-9fece8/report.json`，源码 `5b268cb7e9bccbbdfaa25f02aa6fd1b92efcd2d1aa15e806e70b160a47de4d6e`：9 项顶层/14 项含子测试全部通过，无跳过；同规模 10000 行 preview/new/duplicate 诊断分别 **805.583/818.699/365.469 ms**。

回归用拒绝 INSERT/UPDATE/DELETE 的 trigger 证明纯重复不改账号/凭证/审计；覆盖数据库重复指纹的最小 ID、重复输入报告顺序、空 refresh、ignored metadata、旋转顺序与 credential version +2、identity conflict、密文/owner/Identity JSON 损坏。两次 10000 行试验与另一 schema 的长期维护并行，均标记 `functional_only=true`、明确 `interference` 且 `slo_pass=false`；这些是结构与正确性证据，不能据单个非独占样本宣称正式 60 倍加速或长期 1 秒保证。最终静默窗口仍需补最大批量采样。

**10000 行 mixed replacement/refresh rotation/名称冲突批次仍走逐项 fallback，尚未证明其小于 1 秒。** 不是“10000 行所有导入情况均达标”。

随后修复一个明确 N+1 断崖：9999 项无变更 duplicate 加 1 项全新账号原先会同时不满足两个批量分支，整批退回约 30000 条查询。新有界混合路径只允许批内 name/token 各自唯一、已存在项全部无 refresh 变化、未知项全部为独立的新名称/token；完整判断后 COPY 新子集，再按原输入 index 合并报告。未知项对应已有名称、批内重复或旋转时仍回原顺序逻辑，非法凭证也保留原路径报错，不提前写入。

`.local/performance-slo/20260929t011443-004941/report.json`，源码 `be3b350f39f538cd0dc142bf26a5b335278c5a4e130a4601573402b00e04b6a0`：PG 验证有界 mixed 报告顺序/预览/active/审计、5 类不可优化条件判断前零写、审计回滚及 Context 链。10000 行维护并行单样本诊断为 preview 824.071 ms、new 867.944 ms、pure duplicate 392.373 ms、9999 duplicate + 1 new **392.077 ms**。继续保留 functional/interference 标记，不计 verified SLO。metadata Context 超时也已修正回原有 15 秒，未延长至 20 秒。

最初只读审查发现 store 原有 `ImportDetailed`/`UpdateAccountVersion` 使用内部 Background timeout，HTTP 已取消仍可能继续等待连接或处理整批。后续通过保留旧方法包装、增加 Context 入口的方式修复该管理链，见下节。事务冲突/FK/审计错误不被伪装成成功，回滚与报告排序保持。快照与并发刷新边界也没有新增缓存或跳过数据库读取。

## HTTP 取消贯穿账号管理写入

新增 `ImportDetailedContext`、`UpdateAccountVersionContext`、`GetMetadataContext`，旧签名仍以 Background 调用新入口，保持后台/旧调用方可用。HTTP import/preview、单账号 PUT、batch/actions update 和 actions 前置 metadata 读取接入请求 Context。内部仍保留原 2 分钟/15 秒上限，但客户端取消可以更早终止等连接/SQL/事务工作；解析每个账号、COPY 构造及 duplicate 解密循环也检查取消。文件模式 callback 在完成前检查取消并沿原原子写入路径回滚。刷新 token 的独立持久化路径未改。

`accountDBError` 保留 `context.Canceled/DeadlineExceeded`，避免将 caller cancellation 擦成不明存储错误。提交成功的工作不被事后描述成未提交，取消前未提交的事务按原 rollback 逻辑退出。

`.local/performance-slo/20260929t010521-73d8e0/report.json`，源码 `aac06959612daf8d732cbe4a71011b0cad9e12b8ef1f8caa5c7f22d8fc8caeac`：12 顶层/24 含子测试全部通过、0 跳过。完整 admin router 回归先让真实管理员鉴权成功，在首次 body read 处暂停，再占据唯一 PG 连接并放行 body，确认真正写入或 actions metadata preflight 等待 acquire 后取消。import/PUT/batch/actions 均及时退出，数据库账号总数和 version 不变；不是仅在认证层提前失败的伪取消测试。Store 直接调用同时验证 `errors.Is(err,context.Canceled)`，文件 callback 取消也未写入。相关批导入、凭证和 metadata 原子性回归在同轮通过。

## 非 GET 路由分类需按处理分支

新增 metadata 优化只用于持有旧账号快照的 `UpdateAccountVersion` 路径：成功 UPDATE/CAS 获得行锁后，Groups 以去重集合比较，Proxies 按去重后的顺序比较；未改变的关系分别省去 DELETE/INSERT 两条 SQL。只有 Notes/Enabled 改动时共省 4 条关系写入语句。callback 前分离旧 slice，调用方原地调整代理优先级也不会被误判为未变。其他没有可靠旧快照的 `pgSaveMetadata` 调用者保持原行为。

`.local/performance-slo/20260929t001951-479cb5/report.json`，源码 `7c58023efc2ec25962167ecdb0f947c0e2fcb3b40ed00c593529aec902b79f01`：联合 6 项顶层/8 项含子测试 PG 全部通过。用拒绝任何关系表 statement 写入的 trigger 证明未变绑定的 metadata 更新不触碰关系表；代理原地重排、CAS 冲突、变更外键校验、审计失败回滚均验证。测试与另一 synthetic schema 的维护并行，因此只用作功能证据，不计算 SLO。

`TestPerformanceAccountBulkMetadata` 已准备真实 100 项 actions update 和 batch update，提交后核对每项 version 增量、enabled、notes。该性能测试仍待最终排他窗口，不把减少 SQL 次数换成未经实测的延迟收益。

## 新增 15 个管理写入形状已完成真实功能验证

新增单个测试文件 `performance_management_local_writes_test.go`，通过真实 `p.adminRoutes()` 和 loopback TCP 执行：账号选择、settings 保存、绑定/解绑、租期修改、关闭 auto-reset 策略、schedule 创建/更新/删除、error review 更新、1000 记录 snapshot、1000 记录 cleanup preview、cleanup job 创建/取消、管理员 Key 轮换，共 15 个形状。

`.local/performance-slo/20260929t002715-c09518/report.json`，源码 `d04b6a6ba9c515f2a7eda05916943fdf1240f3b67306aa9a19f8f7693603c186`：samples=1，17 个完整 HTTP 请求及所有提交后断言通过。每条 `WRITE_RESULT` 明确 `functional_only=true`；测试与大表维护并行，**不作为排他环境 SLO 验收**。

该 fixture 创建独立 schema 的 1000 个错误 event/detail。先证明 canceled job 不删除记录，再显式执行单个 worker batch，最终核对 detail=0、durable events=1000、retained count=0、维度 total=0。worker 完成耗时单独发 `WORKER_COMPLETION`，不会把创建 job 的响应时间冒充删除全部记录耗时。schedule 全部关闭，不启动任何后台 worker；提供方 RoundTripper 拒绝外部请求，并断言调用数为零。管理员 Key 每轮更新后由后续请求使用新 Key，保持实际鉴权路径。

使用 `PERFORMANCE_SLO_SAMPLES=5/20` 可扩大重复样本；正式排他性能运行不设置 `PERFORMANCE_LOCAL_WRITE_FUNCTIONAL`，所有请求继续使用 1000 ms 阈值。功能模式通过临时 runner envfile 注入，未修改机器全局环境。

另新增 `performance_compat_writes_test.go`：CPA upload/status/fields/delete 4 形状，Sub2 group/proxy 各 CRUD 共 6 形状，Sub2 account create/update/schedulable/clear-error/delete 5 形状，共 15 个真实兼容 HTTP 生命周期请求。复用现有有效 payload，验证密文不含明文 token且可解密、metadata修改不改变凭证代数、关联持久与资源删除解绑、暂停状态保留、最终账号/凭证/关系无遗留，提供方调用数为零。

`.local/performance-slo/20260929t012117-368c35/report.json`，源码 `27fc9600af2f7cd625345935008d9f8c972ddff76fe1d5861cea9782c44f1373`：15 形状 samples=1、全部断言通过，均 `functional_only=true`。与大表维护并行，不能计作 verified SLO；正式版本可 samples=20 且启用 1 秒断言。此次只新增测试文件，未为这些路线修改生产逻辑。

工具按路径推断的 61 个 local_write 不是可靠的全部本地写入集合：

- `POST /accounts/actions` 中 update/delete 为本地，refresh 会调用上游。
- privacy、reset-credit、Sub2 test/set-privacy、公共 IP geo 请求包含外部调用。
- OAuth start 只生成 URL/保存 session，是本地；exchange 调用上游；import 按 session/refresh-token/PAT 分支分别判断；apply 已保存凭证主要是本地提交。
- selection、last-used、usage-batch 是 POST 读取；cleanup preview/snapshot 是写入快照成员的工作，不等同于普通小文档 CRUD。
- 异步 cleanup create 返回 job 不代表实际删除任务在 1 秒内完成，必须分开记录提交耗时与总工作耗时。

后续扩展批量更新/解绑/删除、schedule/lease/compat 生命周期、review 和 cleanup snapshot 时，应记录 handler、请求 body 分支、实体与批量规模、事务/审计结果，而不是仅把 POST 方法当作同一性能形状。

纯文件逐路由清单已生成：[非 GET CSV](../integration/performance/results/non-get-coverage-20260929.csv)、[JSON](../integration/performance/results/non-get-coverage-20260929.json)。共 74 个非 GET 注册：52 个纯本地写路由已有 39 个实际调用形状，其中 **12 个具有排他窗口已验证样本，27 个仅功能/诊断证据**，另 13 个入口未调用；3 个 POST 读取都有样本，其中 2 个已验证 SLO、1 个仅功能；3 个按输入混合本地/外部；14 个外部调用；2 个导出。

每条 measurement 带 `evidence_status`、`functional_only`、`interference`、`slo_verified`，10000 行维护并行诊断不会提升某路由的 verified SLO 状态。即使同一路由在 100/1000 形状已有已验证样本，也不意味着其 10000 或 mixed 形状已验证。直接 durable ingest 存储 API 没有 HTTP route，不占任何路由覆盖名额。清单合并不同阶段证据，仍为 `mixed_sources=true`、`all_parameter_shapes_proven=false`。

重新生成：`python tools/performance_write_coverage.py`。该脚本不调用 PostgreSQL、SSH 或网络。
