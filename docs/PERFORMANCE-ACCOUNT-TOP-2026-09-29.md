# 账号排行精确日汇总（012）

均匀分布一百万日志的第二阶段验证中，overview 的 4 并发最大响应为 1109 ms，尚未达到普通规模 1 秒目标。真实 handler profile 将主要成本定位到 `accountTopPG`：custom plan 下约 129.9 ms，generic plan 下约 420 ms，均扫描当前范围全部 `usage_minute` 账号分钟行。

## 来源与语义

排行原合同是 `usage_minute.bucket >= from AND bucket < to AND account_name <> ''`，再按 account_ref 分组，对名称求数据库默认排序规则下的 MAX，并按 token 总和降序、account_ref 升序取前若干项。

不能替换成既有 `usage_daily`：历史 `account_day` 基线只写 daily；全局 minute 基线的名称为空，本来就不参与排行。普通 minute UPSERT 会把当前名称改成最新写入名称，而重建 minute 时名称来自该分钟 event 名称的 MAX；新排行只派生当前 canonical minute 行，完整保留这些既有行为。

`012_account_top_day.sql` 新增 `usage_account_top_day(day,account_ref,account_name,minute_rows,requests,errors,tokens)`。day 是 UTC 日编号，使用 floor(bucket/86400)，避免 MinInt64 对应日首时间戳溢出。所有中间计数为 numeric；查询输出仍按原合同 cast bigint。

`minute_rows` 记录真实分钟行引用数，查询按其大于零判断名称仍存在。不能用 requests>0 替代，因为真实的零计数分钟行同样影响最大历史名称与零用量排行。名称更新、删除以及跨日移动按旧行减、新行加，删除最大名称会正确回退到剩余最大名称。

名称/ref 总编码长度超过安全 BTree 键预算的合法分钟行不写复合索引，改为记录 `usage_account_top_unsafe_day` 引用计数。查询对该整日回读原分钟行，不截断或拒绝合法长字符串。

完整日读取派生表；至多两个不足一天的边缘保持原 minute bucket 谓词。unsafe 完整日通过参数化 LATERAL 范围回读。所有分支属于同一 SQL 快照，没有 TTL 缓存，不将历史合并为近似值。

## 维护与并发边界

迁移使用 SHARE ROW EXCLUSIVE 锁阻断 minute 写入而允许读取，在同事务完成回填与安装维护触发器。INSERT/UPDATE/DELETE 使用 statement transition tables 分组后定序 upsert；TRUNCATE 同事务清空两个派生表。批量重建是纯 INSERT，不会逐分钟重复更新派生行。

普通业务 `queueMetricAggregates` 每次提交单行 minute UPSERT；Rebuild 是批量 INSERT；legacy minute 基线名称为空，012 不接收这些行。真实 `persistRecordOnce` 多 worker、同账号不同分钟交替名称的测试不使用重试且通过。

外部任意多行 `INSERT ... ON CONFLICT` 仍可能遇到 PostgreSQL 死锁：同一个语句的 AFTER UPDATE 和 AFTER INSERT transition 分批执行，两个语句可以在不同名称上的锁顺序相反。确定性实验确实复现 `40P01`，因此没有宣称任意 SQL 永不死锁，也没有为未暴露 API 的批量路径加入全局串行锁。外部调用方应完整回滚并重试事务；测试捕获仅 `40P01`，整体重试失败语句，核对每个贡献与最终精确总数，不忽略失败行。正式性能验收仍需包含当前业务持久写入成本。

## 验证与冻结

`.local/performance-slo/20260928t233744-1302aa/report.json`：7 项顶层 PG 测试全部通过，无跳过。

- 精确原始 SQL 对照，含默认 collation、名称/日期变更、零/负计数与独立 legacy daily。
- 非整天范围、空范围、MinInt64/MaxInt64 边界、合法超长名称与 unsafe 引用撤销。
- 回填后继续写入、聚合重建、删除回退与 TRUNCATE 回滚。
- 普通单行并发写和真实 persistRecordOnce 并发业务，无重试成功。
- 外部相反 mixed UPSERT 的 `40P01` 后整体重试与精确贡献恢复。
- root 的小时序列末尾部分小时范围回归同步通过。

源码 SHA-256：`41d1ed1d38c1ed829bc0caec9e7193d74027c015fc6cb96b01c904840b789d90`。
012 迁移 SHA-256：`a2ead9a8ac5b7176b961d1dda8c343075dd37c5bf4a7beb58f59519202167c8d`。

此时功能与语义已验证；均匀一百万及其他规模最终 HTTP 性能由统一冻结后的四规模验收记录，不用小 fixture 的成功替代该结论。
