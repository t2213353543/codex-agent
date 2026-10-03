# 接口与千万级数据性能优化（2026-09-29）

目标为普通请求低于 1 秒、千万级数据查询不超过 3 秒。**第二阶段指定基准已达标**：同一源码和二进制的四种规模/分布共 5576 次读取、520 次写入全部通过。生产尚未部署；全部数据库性能实验使用隔离合成环境。所有参数组合、高基数分布及上游推理的普适保证仍未证明，目标保持进行中。

最新逐场景证据见 [第二阶段实测表](../integration/performance/results/slo-20260929-phase2-final.md)和 [JSON](../integration/performance/results/slo-20260929-phase2-final.json)。后文第一阶段记录保留原时间点的实现和结果，不自动代表最新代码。

## 第二阶段最终状态

| 分布/规模 | 读取形状 | 完整请求数 | 最慢请求 | 门槛 |
| --- | ---: | ---: | ---: | ---: |
| 均匀 10 万 | 6 | 246 | 549.786 ms | < 1 秒 |
| 均匀 100 万 | 6 | 246 | 793.906 ms | < 1 秒 |
| 突发 10 万 | 62 | 2542 | 292.317 ms | < 1 秒 |
| 突发 1000 万 | 62 | 2542 | 374.033 ms | < 3 秒 |

每个形状包含首次请求和并发 1/4 各 20 个样本，全部 HTTP 200 且数据语义核对成功。均匀分布横跨 43,200 分钟，每个请求对应不同账号分钟；因此已额外覆盖第一阶段高压缩 burst fixture 无法证明的扩展边界。均匀千万行、任意维度基数和任意组合过滤尚未测试，不能把均匀百万结果外推为均匀千万保证。

最终源码 `41d1ed1d38c1ed829bc0caec9e7193d74027c015fc6cb96b01c904840b789d90`；统一二进制 `f8f905990fcee2346721f479dd29ea475fb92b5915c888d2cf25a9e4731fb237`。新增 27 形状在迁移后另行补测并去重归并，未将迁移前后的重复采样相加冒充更多覆盖。

第二阶段消除了三个实际瓶颈：均匀百万统计 HTTP 原 8.292 秒降到并发 4 最大 793.906 ms；概览账号排行通用计划扫描百万分钟行的问题通过 canonical 日级派生修复，最终 uniform1M overview 并发 4 最大 91.447 ms；运维统计通过秒/分钟精确汇总把原千万 burst 的约 1.81 秒降至并发 4 最大约 9.1 ms。筛选后的 CPA 只解密选中凭证，别名解析也不再读取无关账号关联。

重建在同一均匀百万 fixture 上从旧准备/重建约 609 秒降到新重建 14.783 秒，grand/minute/hourly 均精确保持 1,000,000 requests、1,399,000,000 tokens。新 010/011/012 对既有千万 burst 的增量迁移共 112.406 秒；这仍是需要写入等待的一次性维护成本，不属于日常 API SLO。

同一最终产物的完整 PG/Redis 回归：`.local/performance-slo/20260928t234818-e20968/report.json`，380 项管理测试加两项性能辅助测试共 382 项顶层（978 项含子测试）全部通过，0 失败、0 跳过、0 缺失。最终本地 `go test ./...`、`go vet ./...` 和构建通过。

同一源码摘要 `41d1ed1...` 的 Linux race 验证 18 项全部通过，无失败、跳过、缺失：`.local/performance-slo-race/phase2-final-41d1/report.json`。覆盖日层/运维/排行并发与回滚、批量重建、snapshot、精确 count、批量管理文档和 Key 状态。实际测试 23.538 秒，构建上传等总计 139.697 秒；临时容器与凭据已清理。

`/health` 另做 Windows 真实 loopback HTTP 401 次采样，最大 0.525 ms。它基于最终源码，仅加临时测试 overlay，运行环境和有效测试源码摘要与 Linux DB 基准不同，报告 `.local/performance-slo/health-phase2/report.json` 单列，不冒称同一 Linux 二进制。按精确静态优先路由匹配，42 个本地 GET 入口均至少有一种请求形状实测；[覆盖清单](../integration/performance/results/local-get-coverage-20260929.json) 同时列明各入口未测的参数、状态和基数。

实现与边界分别见 [日级维度汇总](PERFORMANCE-DAILY-ROLLUP-2026-09-29.md)、[秒/分钟运维及重建](PERFORMANCE-OPERATIONS-ROLLUP-2026-09-29.md)、[账号排行](PERFORMANCE-ACCOUNT-TOP-2026-09-29.md)、[新增读取接口](PERFORMANCE-ADDITIONAL-READS-2026-09-29.md)。旧 [第一阶段报告](../integration/performance/results/slo-20260929-final.md) 保留用于对照。

## 可复跑入口

```powershell
python tools/performance_slo.py --rows 10000000
```

入口冻结完整后端源码，记录源码和二进制 SHA256，在隔离 PostgreSQL 中创建专用 schema，生成日志与事件，核对行数，再以真实 loopback HTTP 调用管理路由，包含鉴权、查询、JSON 编码和完整响应读取。每个场景记录首次请求、1/4 并发逐请求样本、P50/P95/P99、最大值、状态码、响应长度及数据正确性。未配置上游，不产生真实推理。

已有 fixture 可用 `--reuse <report.json>` 复测；`--source <snapshot/source>` 保持旧版业务源码、只更新同一套采样工具，可做前后对照。通用 PG 回归用 `--test-run '<测试正则>'`。工具拒绝空匹配，并明确保留跳过和失败。

“首次请求”指新测试进程内该接口首次调用，其他接口可能已预热共享状态，PostgreSQL/OS 缓存没有清空，不等于物理冷盘测试。端点超过阈值或出错时停止其额外压测，保留已有失败，不用较短 timeout 伪造达标。

## 已完成的主要优化

| 链路 | 改动 |
| --- | --- |
| 日志身份与排序 | 请求/响应/客户端关联 ID 精确索引；保持数字主键可用；Token/延迟与时间 ID 复合排序索引 |
| 管理统计 | 消除多维宽 JSON 展开；紧凑投影与 GROUPING SETS；业务连接默认关闭高启动成本 JIT，可 `PG_JIT=on` 恢复 |
| 长跨度累计统计 | 从 canonical 分钟表派生全局小时汇总，兼容独立 legacy 基线；部分小时保持分钟边界 |
| 日志写入 | 用量事件幂等门与明细 INSERT 合并为单个 SQL，保持同事务汇总/回滚，减少一个数据库往返 |
| 账号最近使用 | 排序和 runtime/last-used 批量读取使用同事务维护的 account_request_state，避免扫描全部历史事件 |
| Key 鉴权 | 每次查询当前数据库元组身份；未变化时复用解密结果与 token 索引，减少整个 Key 文档传输、解密与线性查找 |
| 管理资源读取 | Group 单项主键查询；只读取所需绑定名称；概览计数不读取全量 Group 文档 |
| Sub2 兼容分页 | 同一 RepeatableRead 快照中筛选元数据并分页，只解密该页凭证、构造该页 DTO |
| 运维统计 | 在 PostgreSQL 内汇总，最多返回图表桶和最近 8 条错误，避免所有 JSON 传回 Go |
| 错误复核页面 | 未指定 resolved 筛选时仅批量读取当前页 review；指定筛选保留全局 resolved-set 和游标签名语义 |
| JSON 输出 | 根据 JSON 类型解析，避免把每个计数反复尝试解码为字符串、对象和数组 |
| 明细保留统计 | 新增分钟 retained 汇总，与日志 insert/update/delete/truncate 同步，独立于永久保留的 usage_events |
| 全维统计与候选筛选 | 新增 retained 分钟维度精确汇总，基础时间/status 直接聚合，其他筛选保留原始路径；同一数据源服务 options |

Key 快照校验同时包括 `version`、`updated_at`、`xmin` 和 `ctid`，覆盖无版本递增的 UPDATE 及 delete/recreate。数据库错误或取消不会使用本地快照放行。返回数据与缓存分离，调用方修改 Groups 不影响其他请求。

SQL 计划、精确历史语义和迁移细节见 [SQL 报告](PERFORMANCE-SQL-2026-09-29.md)。

## 已有实测证据

| 场景 | 原路径 | 优化路径 | 范围 |
| --- | ---: | ---: | --- |
| 100,000 事件的账号最近使用排序 | 23.94 ms | 3.04 ms | 单进程内部 SQL/存储基准，约 7.9 倍 |
| 10,000 Key 当前鉴权读取 | 41.91 ms | 0.172 ms | 每次均验证 PG，约 244 倍；10.97 MB/50,051 次分配降到 1,808 B/34 次 |
| 100,000 行全维统计，相同 SQL | JIT on 4929.973 ms | JIT off 572.388 ms | 单次 EXPLAIN，不是 HTTP 分位数 |
| 500 条管理响应的公共错误字段处理 | 约 5.33 ms / 4.04 MB | 约 2.38 ms / 1.30 MB | Windows 微基准，无错误内容场景 |

账号和 Key 基准证据位于 `.local/system-perf-20260927/20260928T211912-2f23e9/`。同一 variant 的 legacy/new 子场景用于比较；baseline/candidate 两份目录均为当时工作树，不把它们之间的差异解释为版本收益。迭代次数有限，不能当作生产 P95。

JIT 对照证据位于 `.local/performance-slo/20260928t212139-28c97c/`。最初 1 CPU/512 MiB PG 的 100k HTTP smoke 中，全维统计首次 9.55 秒、4 并发失败；30 日概览 4 并发约 1.71 秒。这些是真实失败，后续改动需要新的 HTTP 实测证明改进。

隔离测试库之后调整为 4 CPU/4 GiB，主机只读检查为 80 CPU、128 GiB、约 79.8 GiB available；资源变化保存在 `resource-profile.json`。不同资源档位结果分别记录。

### 千万行基线与当前迭代

fixture 实际含 request_logs、usage_events 各 10,000,000 行，账号 1,000 个，usage_minute 30,000 行，usage_daily 29,517 行。插入用时 1545.27 秒，含聚合重建 1585.87 秒。日志按 30 天的每日突发模式分布，不等于均匀分布一千万个不同账户分钟桶。

`.local/performance-slo/20260928t214307-098eee/report.json` 为旧业务冻结版本的真实千万行对照：33 个读取场景，26 通过、7 未通过。它已包含本任务早期部分改动，因此不能称为开工前原版。精简证据保存在 `integration/performance/results/slo-20260929-10m-before.json`。

| 未通过的场景 | 基线观察 |
| --- | ---: |
| account runtime | 4 并发最大 3195 ms |
| model options | 首次 4692 ms，HTTP 200 |
| account options | 首次 15379 ms，HTTP 500 |
| request ID | 首次 15002 ms，HTTP 500 |
| 全维 stats | 首次 15039 ms，HTTP 500 |
| 30 日 overview | 首次 12706 ms，HTTP 200 |
| 24 小时 operations | 4 并发最大 3590 ms，9/20 超过 3 秒 |

之后 100k fixture 应用 008+009 共 11.227 秒。5 个失败热点的迭代 smoke 全部通过：stats 首次 270 ms，概览首次 32 ms，model/account options 首次约 68/73 ms；每并发档仅 3 次采样，正式复验会增加到 20 次。

千万行候选初验 `.local/performance-slo/20260928t215440-75aee2/report.json`：008+009 一次性回填 **1040.72 秒（17 分 20.7 秒）**；retained_log_minute 约 32 KiB、retained_usage_minute 约 9.73 MB。这是在既有合成大表上的实际维护耗时，不是日常 API 延迟，回填事务期间对应明细写入等待。

此候选初验 33 个读取场景中 31 个通过：全维 stats 首次 283.8 ms、并发 4 P99 402.9 ms；operations 并发 4 P99 1697.7 ms。仍有两个失败：request_id 首次 3.766 ms，但重复执行第 5/11/17 个样本遇到约 15 秒 HTTP 500；overview 首次 4205 ms。重复请求揭示 prepared generic plan 问题，不能只取首个快速样本认定优化成功，正在修复并将重新冻结验证。

同轮独立 schema 中 13 组写入/批量操作全部通过（各 20 次，阈值 1 秒）：group/key/proxy CRUD 最大 2.861 ms，账号批量用量 6.665 ms，4 并发持久入库最大 75.608 ms。入库后严格核对 events/logs/requests 各 20、tokens 2400；写测没有改变千万行读取 fixture。

### 最终性能验收

随后修复 request/client ID 的通用计划退化，以及 overview 空边缘的错误连接顺序。前者用候选驱动的 LATERAL 主键探测，后者用边缘范围驱动的 LATERAL 时间索引探测；不依赖全局强制 custom plan。回归分别验证强制 custom/generic 和重复调用，正确空结果也纳入采样。

| 规模/链路 | 场景/样本 | 阈值 | 实际最慢 | 结果 |
| --- | --- | ---: | ---: | --- |
| 100k 日志与事件 | 35 读取场景，1435 次 | 1000 ms | 312.450 ms | 全部成功、语义正确 |
| 10M 日志与事件 | 35 读取场景，1435 次 | 3000 ms | 1808.511 ms | 全部成功、语义正确 |
| 独立 schema 写入与持久化 | 13 组，260 样本 | 1000 ms | 68.334 ms | 全部通过 |

性能验收源码 SHA256：`2aea57c48ef3f21cdbf6f5bde183ae4f63efda04e2dd1e805c61847067c132ba`。两种规模和写入/管理回归实际使用同一二进制 `3f505b9e2e9affb2a3a1bc645808e2fbf8ced9538cce4fcd25688e282f9a434c`，复用了已上传产物，没有重新编译冒充同一产物。

- 千万级：`.local/performance-slo/20260928t223935-af1ba8/report.json`。
- 十万级：`.local/performance-slo/20260928t224025-82433d/report.json`。
- 完整管理与写入：`.local/performance-slo/20260928t223513-9c8dcb/report.json`。

千万级并发 4 P99：全维 stats **339.447 ms**、overview **45.276 ms**、request ID **2.910 ms**、operations **1808.511 ms**。同一档只有 20 个样本，nearest-rank P99 等于该档最大值，不能解释成大样本长期分位数。

最后完整回归修复 Group 创建返回时间：改为读取 INSERT RETURNING 的实际持久化时间/版本，避免应用 Unix 截断与数据库小数秒舍入差一秒。SQL 往返数量不变。上述最终性能、完整回归和写入结果均已包含该修复。

009 维护 34 个 numeric 计数/样本总和与历史维度名称；删除可正确回退最大历史名称和观测溢出状态。基础时间/status 使用汇总，复杂筛选仍保留原始查询。超长维度或亚秒时间戳按整分钟精确回读，既不截断也不近似。详细验证见 [SQL 第二阶段](PERFORMANCE-SQL-2026-09-29.md)。

### 网关自身处理开销

`.local/performance-slo/gateway-loopback.json` 记录 17 个合成场景、460 次完整校验通过的真实 loopback TCP 请求，涵盖 HTTP SSE、JSON、compact、models、WebSocket、256 KiB 输入/历史、256 帧与 4 并发。最大总耗时 61.803 ms，其中该场景人为加入两段各 20 ms 上游等待；无人工等待场景最大 42.873 ms。

此测试使用临时文件存储及内存历史，没有 PG/Redis 和真实模型网络，不代表真实推理完成时间。第三方推理、OAuth、代理探测、导出整个大表与普通本地读接口分别列明，不能把流式首帧时间冒充完整结果耗时。

### 正确性和并发

首轮与中途 `go test ./...` 全部通过，`go vet ./...` 通过。缺少环境时被跳过的 PG 项目由独立远端报告补充，不把本地成功当作 PG 实测。

`.local/performance-slo-race/20260928T213212-e03ca0/race/report.json`：Linux Go 1.26.8、CGO、race detector，15 个明确选中测试全部通过，0 失败/跳过/缺失，实际测试 8.044 秒。覆盖 Key 当前校验/并发、Metrics 幂等/小时汇总、Sub2 页凭证和管理状态批读。绑定源码 SHA `9e6529a20d02440660362790c8abaf31d312a6516f2dec0b76b9124c490c22ba`；后续 retained 汇总变动需要另行验证。

`.local/performance-slo/20260928t214118-e1edc7/report.json` 的全部 PG 测试通过、无跳过，补充验证 retained 汇总任意边界/37 秒和 1 秒步长/负 epoch/亚秒精确回退，批量写入/跨分钟更新/删除、回填、8 并发写入、insert/update/delete/TRUNCATE 回滚、数值溢出、精确总数游标边界、批量复核读取及在线索引中断恢复。执行计划证明正常整分钟 cache 汇总的 request_logs 节点 Actual Loops 为 0。

## 第一阶段收尾时的验证边界（历史）

千万级 fixture、旧业务 HTTP 对照与同一源码的两种数据规模复测已完成。完整管理回归已通过：362 项管理顶层测试，加独立写入验收共 363 项顶层/947 项含子测试，0 失败、0 跳过、0 缺失。`all_routes_slo_proven` 保持 false：清单识别 127 个路由注册/分派入口，本次读取覆盖 35 个请求形状，未采样路由或参数组合不自动视为通过。上游推理、OAuth、完整大表导出及更高并发没有统一 1 秒/3 秒结论。

容量结论必须同时记录原始行数、时间分布、账号/Key/模型基数与汇总行数。若每分钟有大量不同资源，或每次请求都有不同名称，维度汇总可能接近明细规模；基础 burst fixture 通过不能证明该分布也低于 3 秒。第一阶段结束时[第二分布方案](PERFORMANCE-DISTRIBUTION-PLAN-2026-09-29.md)尚未执行，第二阶段已经取得均匀 100k/1M 实测，见本文开头与最新结果表；任意组合过滤仍需单独验证。

生产迁移和发布未执行。大表普通 CREATE INDEX 的阻塞问题通过专用在线迁移器处理；小时及 retained 汇总仍需回填和事务写入锁，执行时间与额外写入成本必须实测后记录。

最终本地 `go test ./...` 通过（server 150.983 秒），`go vet ./...` 与生产入口编译通过，前端 237 项测试通过。最后 13 项 Linux race 通过的是先前 `7c7c5be...` 摘要，和最终摘要之间只有 Group 创建返回值这一处生产差异及四个测试文件；008/009、KEY、GetMany、索引读取和统计实现字节一致。差异证据 `.local/performance-slo/final-validation-delta.json`，不将较早 race 冒称最终摘要的 race 结果。
