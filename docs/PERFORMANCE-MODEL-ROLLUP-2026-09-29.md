# 017：模型条件化的精确统计

100k、两模型各占 50% 的实际基线证明原始分支仍需读取 50k 明细；原单模型统计方法观察约 4.12 秒，详细证据见 `PERFORMANCE-JOINT-FACTS-DESIGN-2026-09-29.md`。017 因此增加模型条件化维度层，不再要求目标模型覆盖整个选择范围。

## 存储与覆盖

新增 `retained_usage_model_minute`、`retained_usage_model_daily`，键为 `(selected_model,bucket,status,kind,resource_id,name)`，保留原有 34 项 numeric 总和/样本/计数。每个模型内部仍有完整模型、账户、KEY、group、端点、路径、错误分类等输出维度，因此 `model=A` 可以直接生成精确的所有输出，而不是从独立模型边际猜测其它维度。

读路径复用现有日/分钟/明细边缘互斥分解、avg 和溢出投影、时区日历桶与 first/last 查找。`model=A` 内部再附 KEY/group/endpoint 条件时，先在选定模型内部证明为空或普遍满足；若在该模型内部仍部分匹配，则继续原始联合过滤。没有宣称所有 co-occurrence 条件都被覆盖。

模型轴增加到旧维度键上会扩大 BTree 项，因此新层仅纳入 `retained_usage_safe=true` 且模型不超过 512 个 UTF-8 字节的记录。旧合法长模型仍写入原表与旧汇总；513–1700 字节的旧 safe 模型另外计入 `retained_usage_model_unsafe_minute`，更长或其它不安全字段仍由原 unsafe guard 覆盖。模型读路径取两个 guard 的并集，整分钟回读准确明细，既不截断也不拒写。

## 不重扫单模型明细的回填

旧 009 每个 safe 请求恰有一个 model 边际贡献。若 `(minute,status)` 内只有一个正引用 model，该分钟状态的每项旧维度都必然来自该模型，可以直接逐列复制到新层。即使该分钟存在未纳入旧汇总的 unsafe 请求，复制 safe 子集仍准确，读取时 guard 继续回读完整分钟。

017 用事务临时表记录各 `(minute,status)` 的模型种数与单模型名称。单模型且模型长度安全的桶直接复制旧分钟维度；只有真正混合且存在安全短模型的桶，才按时间范围索引探测原始记录、解析计数并构造新层。模型额外 unsafe 计数同样直接从旧 kind=model 边际总和生成，不需要再扫描 JSON。

回填先锁 request_logs，再锁旧分钟汇总，与写入顺序一致。表创建、回填、维护函数替换在同一事务提交。017 SHA256 暂定稳定为 `f282da85f542489bc9f594f7dcc9cff0795150648c53c884da99ba6c4979f28a`，009/010 及其它旧迁移 checksum 没有修改。

## 同步维护与成本边界

017 替换已有三项 request transition 维护函数，继续共享一次 classified/counted JSON 提取。锁顺序固定为旧 guard → 模型 guard → 旧 minute → 旧 daily → 模型 minute → 模型 daily；每层按完整键排序，避免分别依赖 UPSERT 的 INSERT/UPDATE 后置事件造成交叉拿锁。

numeric 计数解析一次；维度文本/group 展开保持 NOT MATERIALIZED，多个层重复读取维度表达式，避免把十余倍宽 numeric 中间行物化到临时磁盘。这个取舍需要实际写成本验证。新层约增加一套模型条件化维度写入与索引，不能只报告读收益；高模型/资源基数仍可能使汇总接近逐记录大小。

request_logs TRUNCATE 触发清空新 minute/daily/guard，事务失败或显式回滚同步恢复。DELETE/模型切换/时间移动/名称变更均先撤销 OLD 再加入 NEW，image 超界状态仍由 numeric 和最终范围判定，删除后可恢复。

## 已通过的语义验证

`.local/performance-slo/20260929t011743-b584ff/report.json`：017 首轮迁移与旧回填链全部通过。

`.local/performance-slo/20260929t011952-5e02e7/report.json`：完整联合验证通过，包含：

- 原始明细直接重算全部 34 项字段，与模型 minute/daily 的双向 EXCEPT 差集为零。
- 同一分钟单模型、混合模型、同分钟不同 status 各自单模型的回填精确性。
- 将 counter 提取函数临时替换为抛错版本，单模型回填仍成功，证明快捷复制路径没有重新解析 JSON。
- 512 字节模型叠加长账户/KEY 名称的索引安全；513 字节、UTF-8 超字节预算、极长模型、长 group、亚秒 guard。
- 模型/status/时间修改，跨日移动，删除，回滚及 TRUNCATE。
- 8 个并发写入者反复交换模型，维护无应用重试仍完成且结果准确。
- 模型读取与原始 SQL 比较：多个时区、DST、6 种 step、对齐/非对齐边缘，以及模型内部部分/不相交的 KEY/group 组合。
- 原 009/010 回填测试适配重放 017 后仍通过。

这些是小数据语义与结构证明。017 的 100k 混合模型前后速度、burst/uniform 大表回填时间、体积和新增写入成本尚需协调中的后续实测，不把上述验证通过提前写成整体 1 秒/3 秒目标已达成。
