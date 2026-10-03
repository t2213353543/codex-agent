# 部分匹配联合过滤：下一项可执行设计

当前精确 gate 只证明“空”或“全部满足”。当模型 A 占 50%，且每个账户同时存在 A/B 请求时，账号边际、模型边际和全局边际都不足以恢复 A 对各账户/KEY/group 的贡献。没有额外的共同出现信息，就不能精确拼接出结果。以下设计没有创建新表，没有执行新的 PostgreSQL 实验，不能视为性能通过证据。

## 先测一个无法被常量证明优化的实例

新增 opt-in `TestPerformanceMixedModelStatsBaseline`，使用现有隔离 PG test fixture、自带随机 schema，结束自动清理。每条记录只属于 A/B 一个模型，两个模型各 50%；模型由 `(i/1000)%2` 决定，账户由 `i%1000` 决定，确保全部 1000 账户都含两模型，而非模型与账户取模意外相关。

时间采用 30 天 spread 公式，group 为 `['shared','shared']`，验证 group 去重；两个测试请求为 `model=mix-a` 和 `model=mix-a&key_id=mixed-key&group_id=shared`。两者 gate 均须得到 matched=true/different=true，确保实际走部分匹配原始分支。

测试验证请求/成功错误/Token/缓存/样本/平均值、模型/KEY/group/1000 账号、所有账号请求总和、时间序列总和，然后捕获真实方法 SQL 的 `EXPLAIN (ANALYZE,BUFFERS,SETTINGS,FORMAT JSON)`。输出存储方法时间及计划，不把它当成 HTTP 延迟。读取 timeout 专为基线提高至 120 秒，避免把原始路径超时截断后失去工作量证据。

本轮只执行 Go 编译与 `-list`，没有运行测试、没有 PG 请求。正式窗口空闲后可先运行 100k，再 1m：

```powershell
python tools/performance_slo.py --rows 100000 --test-run '^TestPerformanceMixedModelStatsBaseline$' --timeout-minutes 15
python tools/performance_slo.py --rows 1000000 --test-run '^TestPerformanceMixedModelStatsBaseline$' --timeout-minutes 30
```

基线必须保留真实吞吐和汇总体积。100k/1m 能精确验证模型占比和账号分布；不能简单把 1m 时间乘十当作千万行实测。

### 后续已执行的 100k 基线

在 root 授权的 uniform 构造维护空档执行了 100k 版本，报告 `.local/performance-slo/20260929t010338-6bf4e3/report.json`，源摘要 `f13448f68de0b39c488fd4b8718da112d5a66228d5f015ccdeb2d92e0ba833fa`。功能断言全部通过：100,000 条中每个模型 50,000 条、1000 账户每个都包含两个模型，所有输出计数/观测及账号/series 总和正确。

| 精确筛选 | 存储方法墙钟 | 随后 EXPLAIN Execution Time | 原始匹配行数 |
| --- | ---: | ---: | ---: |
| `model=mix-a` | 4117.669 ms | 1341.709 ms | 50,000 |
| `model=mix-a&key_id=mixed-key&group_id=shared` | 4167.804 ms | 1643.396 ms | 50,000 |

实际计划使用原始 Bitmap Heap Scan，3 个并行 loop 各约 16,666.67 行；原始分支没有被错误短路。临时读写分别约 9.1 MiB、17.1 MiB。fixture 构造耗时 91.598 秒。

这轮与 uniform 大表构造维护并行，不是独占 SLO 样本，也不包含 HTTP/auth/编码；第一次方法调用与后续 EXPLAIN 不可当作同一条件的 A/B。它已经提供工作量与当前未满足场景的真实证据：即使只 100k 原始行，50% 部分匹配仍需要聚合 50k 明细，本轮观察时间超过 1 秒。没有继续创建 1m 数据，避免重复重负载；新 schema 未落地。

## 第一选择：模型条件化的既有维度汇总

若基线证明实际主慢点就是 `model=...`，最小新增覆盖不是 36 字段全 cube，而是现有维度表增加一个条件轴 model：

```text
retained_usage_model_minute
  minute, status, selected_model, kind, resource_id, name
  34 × numeric count/sum/sample

retained_usage_model_daily
  day, status, selected_model, kind, resource_id, name
  同上；total 仍按分钟供图表与 first/last 边缘
```

每个请求只有一个 model，所以本层每请求贡献的维度数与当前 009 一样，而不是模型数 × 维度数。现有 `(minute,status,kind,id,name)` 中原来混在一起的模型会分裂成多个键；对一个持续使用单模型的账户，账户维行数可能不增加，但独立低基数维会被模型数放大。

该层准确覆盖 model + time/status 的所有公开输出维度。在选定模型内部，还可重新执行现有 Endpoint/Key/Group 普遍成立或缺失证明，因其边际此时已**条件化于模型**。如果模型内 KEY 仍部分匹配，依旧不能直接用两个边际得出联合结果，必须使用更细层或原始路径。

对于每分钟每账户可能使用 M 个不同模型、A 个账户、D 个分钟、K 个维度的范围，最坏模型条件层活跃行数近似 `min(N × K, D × A × M + 其它维度的条件基数)`，而不是简单 `D×A`。必须在上述混合模型 fixture 上记录日/分钟行数和压缩比，不能只看原始行数。

新增写成本约为再维护一套维度层：每事件十余个 minute upsert、非 total 的 daily upsert，以及更长事务持锁。可以保留当前基础表来维持轻量总览读，也可在迁移后逐步让模型条件层汇总取代其一部分；后者涉及更大切换边界，不能在一次迁移中悄然删除旧语义。

接入点应明确：`managedLogStatsPG` 检查 `q.Model` 与其它条件覆盖；专用 source 构造读取模型条件日/分钟/unsafe；最终复用当前 `managedUsageRollupAggregateSQL`，不再重写 42 项输出解码。触发维护应像 010 一样一次解析 source，再按确定的层级/键排序写入，不能从 UPSERT 的 UPDATE/INSERT 两种后置事件各自拿锁。

## 第二选择：窄联合事实投影

要覆盖大量任意 AND 条件，模型条件边际仍不足。应建立一份保留每请求联合属性的窄事实投影，先消除重 JSON 解压/重复字符串查找，再决定可聚合的粒度：

```text
retained_request_fact
  log_id PRIMARY KEY、created_at（保留原 timestamptz 精度）
  account_ref、json_account_id、account_name、key_id、key_name
  model、upstream_model、sent_model、returned_model
  endpoint、inbound_path、upstream_path、request_type
  service_tier、reasoning_effort、status、http_code
  stream、websocket_warmup?、native_compaction?、error_truncated?
  error_category、error_severity、error_phase
  groups（按当前语义规范化/去重）、ip
  34 项可加 count/sum/sample
```

可选布尔的 NULL 与 false 必须保留不同状态；account_ref 与 JSON ID 是不同字段；上游/发送/返回模型也不能合并。错误 prose 不必完整复制：派生 category/severity 用当前固定规则计算，但 repair 改变 prose 或 final error code 时必须重算。`request_id`、`response_id`、`client_request_id`、snapshot/review membership 等通常高度唯一，应直接先从已有原表索引/成员表取得候选 log_id，再 join 窄 fact，而不是把它们加入聚合键。

投影每原始请求一行，容量下界仍为 N；它节省 JSON CPU 与无关字段 I/O，不等于把千万行半数筛选变成常数成本。按 250–600 字节平均紧凑行粗估，10m heap 原始体积约 2.5–6 GB，另加行头、页空隙、索引、WAL、TOAST；这只是容量预算，必须用 `pg_column_size`/`pg_relation_size` 实测。把所有字段做组合 BTree 会膨胀且遇到长值上限；优先固定 time/log_id、常见 model/account/key 引导索引，以及紧凑 GIN 等值候选，不覆盖任意排序。

查询路径是“已有索引缩小候选 → 窄 fact 上一个 filter → grouping sets”。比如模型占 50%，即使每行仅 300 字节仍需读约 1.5 GB 逻辑数据；在冷盘或 4 并发下无法仅凭窄投影承诺 3 秒。此时才需要将可重复的联合属性折叠成 cube。

## 可重复联合键 cube 的精确边界

一个精确可聚合 cell 的键必须至少包含本次支持的所有筛选字段与所有输出维度字段，且时间桶不能跨越需要区分的图表边界。不能省略 model/key/account/groups 的共同关系后仍声称任意筛选精确。

定义 `dimensions` 为规范 JSONB/typed tuple，内容包括上面的联合属性，组以排序去重集合保存。`minute + dimensions` 的 cell 存 34 项 numeric 加法状态。每请求只写一个 cell（而不是 12 个边际），读取匹配 cells 后再展开多个输出维度。若 `(minute,dimensions)` 大量重复，收益明显；若每请求 IP、request type、组集合、名称或模型都不同，cell 数 C 接近 N，仍只是较窄的原始扫描。

不能仅用 `md5(dimensions)` 作为唯一键当作精确聚合。无碰撞语义需要字典：digest 只索引候选，原 tuple 全等判断；同 digest 并发插入时按 digest advisory 锁序列化、查原值，再分配 bigint dimension_id。碰撞可以存在多条不同原值，计数绝不能混合。长键不能悄然截断，字典应存原值并使用哈希候选/原值复核。这会增加写锁、字典存储、GC 和迁移复杂度。

一种更可控的首版是不支持任意长/非规范 tuple 的 cube，像现有 unsafe 机制那样将该分钟标记为不可完全汇总，并回读准确 fact。正常键按明确 byte budget 索引，任何合法超限记录仍被保存，不能为了建索引拒写历史合法字段。

日级 coarsening 可减少跨分钟相同维度 cells，但图表 total 仍需按条件联合属性的分钟计数；不能用日 cell 回答 60 秒 step。推荐保留 minute cells，追加 daily cells仅供完整日、跨高基数维度聚合，再沿当前 disjoint 日/分钟/秒边缘方案分解。每增加一层都会增加同步写入成本。

## 一致性与上线顺序

1. 从现有真实慢请求选择最小覆盖：先模型条件维度层，或窄事实。不要同时创建三层，以免无法归因写放大。
2. 明确 typed NULL/default/JSON 标量语义，单独保存 stable account_ref 与 JSON identity，复用当前 cache eligibility、latency known、image numeric overflow 规则。
3. request_logs 的 INSERT/UPDATE/DELETE 同事务生成 OLD/NEW delta；更新模型、KEY、group、error prose、time 必须撤销旧贡献再加入新贡献；固定所有层级及键锁顺序。
4. 回填先锁接缝，复用一次解析 source，比较所有计数/样本字段差集；truncate、rollback、repair、最后引用删除后 max label 回退均验证。
5. 明细保留期删除必须同步删除 fact/cell贡献，永久 usage_events 不可作为 retained stats 替代品。first/last 用 exact selected candidate 的时间边界，不能取未过滤全局 min/max。
6. 测量原始 50% 混合模型、四维部分交集/不相交、全匹配、零匹配、长值/亚秒、MVCC 并发提交，检查真实 cached generic plan，而非只看一次带常量 EXPLAIN。
7. 同 fixture A/B 记录读 HTTP first/P95/P99、写串行/并发吞吐、WAL/索引/表大小、回填停写时间。若 C/N 接近 1 且读取仍超目标，应明确这份分布未达标，不用 TTL 缓存或错误响应替代精确结果。

具体新增 schema 编号暂不占用。当前慢路径入口在 `internal/server/admin_usage_statistics_pg.go`；counter/unsafe/daily/guard 复用点在 `stats_rollup*.go` 与已冻结的 009/010/013，通过新增迁移追加，既有 checksum 不改。

## 当前明确未满足或未验证的目标

- 混合模型/KEY/group 的部分匹配联合统计：仍执行精确 raw 分支；上述 100k/50% 场景已观察到超过 1 秒，未证明普通条件目标。
- 任意 10m 高联合基数过滤：没有 cell/fact/模型条件层实测，不能用 burst 基础统计或单模型全匹配 gate 的通过结果替代。
- 全量大文件导出、超大 snapshot/cleanup 预览和写入：输出/持久化工作量随匹配数据增长，必须分别测量，不能以首字节或空结果冒充完整完成时间。
- 任意硬件、并发、磁盘冷态与外部上游推理：当前实验只覆盖报告中的环境和选择器，未提供普适延迟保证。

以上是实际剩余工作范围，不是将用户目标缩小成已测样本。后续应先复跑独占 mixed 100k，再以模型条件层或窄事实的明确覆盖设计比较读写成本；只有真正完成这些场景，才可扩大“已达标”结论。
