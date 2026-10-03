# 管理系统性能优化报告 — 2026-09-27

本轮盘点了 86 个管理 HTTP 路由和 10 个前端页面，完成两轮有实测依据的读取、聚合、分页、格式化和加载优化，并修复编译阻塞、代理缓存副作用、导出版本元数据不一致等功能问题。推理网关由并行任务维护，相关收益不计入本报告。

下面的数字来自冻结源码与隔离合成数据。本报告完成时尚未部署；随后已完成[生产发布与线上复测](SYSTEM-PRODUCTION-2026-09-27.md)。不将局部基准换算成整站统一提升比例、实际模型速度或生产 QPS。

## 主要收益

| 场景 | 优化前 | 优化后 | 耗时下降 |
| --- | ---: | ---: | ---: |
| 账号列表50条 | 19.919 ms | 6.207 ms | 68.84% |
| 账号列表500条 | 60.637 ms | 29.996 ms | 50.53% |
| 分组列表：1000账号 | 11.968 ms | 1.520 ms | 87.30% |
| 代理列表：1000账号 | 10.073 ms | 0.982 ms | 90.25% |
| Key列表：100把Key | 7.678 ms | 1.568 ms | 79.58% |
| 日志第100页，时间排序 | 92.433 ms | 1.996 ms | 97.84% |
| 日志第20页，Token排序 | 715.049 ms | 53.623 ms | 92.50% |
| 日志详情：数字ID | 4.178 ms | 0.148 ms | 96.46% |
| 账号批量用量：10账号/3,000条记录 | 292.660 ms | 23.165 ms | 92.08% |

账号、资源列表使用 Go HTTP handler 计时，包含其各自处理与 JSON 编码；资源列表还经过管理路由/鉴权。日志列表与详情是内部存储方法计时，不含 HTTP 路由及最终响应编码。均不含公网传输、真实上游响应和浏览器绘制。

账号首屏延后下载 **601,607 bytes 原始 / 204,583 bytes gzip** 图表模块，图表资源请求由 2 个降为 0 个；实际打开请求图或日统计时再加载。此项有浏览器网络记录和关闭/重开测试。完整 bundle 同期还受其他任务修改影响，只将图表依赖变化单独归因。

## 实现变化

1. 账号摘要仅聚合需要返回的稳定 ID 用量，取消通用方法附带的身份反查和历史请求 JSON 解码；资源名称仅查询本页引用。管理列表复用生成 ETag 的 JSON 字节，消除重复序列化。
2. 分组、代理列表直接聚合关系表，概览直接统计账号总数/启用数，避免下载所有账号元数据。Key 用量改为一次批量查询；100 把 Key 的整个列表连接池获取次数由 103 降到 4。
3. PG 日志页码跳转由逐页往返、传输和解码改为单 SQL；cursor 仍使用原键集分页。详情查询移除主键的 text cast，保留规范 ID、event ID 和冲突时最大行 ID 的行为。
4. 账户批量统计只展开账户维度；文件聚合复用平均值存储，避免逐样本重复分配。筛选、unknown/nullable、Token 溢出、快照和排序语义由对照测试覆盖。
5. 前端复用中文自然排序器、时区/数字格式器和实体索引；账号图表按需加载；日志路径历史限制为 50 个，并保留当前路径的页间游标链。
6. 元数据导出不解密全部代理凭据、不重建共享代理索引；修复文件模式读取元数据可能清空运行缓存密码的问题。含凭据的显式导出仍保留原行为。

此外修复 Groups/Keys 同名 formatTime 导致的前端编译失败；coverage 的 export_schema 与真实 CSV v3 统一。原始失败、测试数据修正和过期浏览器断言调整单独保留，不计入性能百分比。

## 测量方法与数据

- 后端：Windows Go 1.26.7 编译 Linux amd64 测试程序，同机连接独立 PostgreSQL 18.4，两个版本串行执行。测试驱动容器 2 CPU / 2 GiB；独立 PG 容器 1 CPU / 512 MiB，二者配额分别记录。不存在生产账号、模型调用或业务库访问。
- 每个后端场景 3 轮，每轮 30 次，表中耗时/分配为 3 个轮次均值的中位数。源码/二进制 SHA256、原始日志全部保留。
- 资源数据：1,000 或 10,000 个禁用合成账号、100 个分组、100 个代理、100 把 Key。概览基准统计表为空且未配置 Redis，仅测账号规模造成的开销；不能套用到有大量历史聚合或缓存命中的概览。
- 账号摘要：500 账号、2,000 分组、2,000 代理、15,000 日聚合、每条历史请求约 4 KB 无关内容；分别测试 50/500 条和有/无绑定。
- 日志：PG 30,000 条，file 10,000 条；每页 50 条。页码仍限制 100，OFFSET 仍需数据库扫描/排序被跳过的匹配行，不宣称任意深度为常数成本。
- 前端：Node v24.19.0 / Windows x64；2 次预热、7 轮交替采样取中位数；每项比较输出深度相等。浏览器为本机 Chrome、mock API、新 context；不将本机 ready 时间作为生产时延承诺。
- `db_reads/op` 计量连接池 AcquireCount。账号列表的一次获取对应包含多条 SQL 的 repeatable-read 事务，不能称为 SQL 总条数。

## 全部有效后端基准

负的“耗时下降”表示该次测量变慢，完整展示未筛掉低收益项。handler 不含真实网络；具体是否经过鉴权见原始 benchmark 实现。

| 场景 | 计时层级 | 前 ms | 后 ms | 耗时下降 | 加速比 | 分配字节下降 |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| `AggregationSamples` | 内存微基准 | 1.551 | 0.685 | 55.86% | 2.27× | 99.98% |
| `Endpoints/file_10k/account_batch_10` | handler | 97.824 | 92.608 | 5.33% | 1.06× | 4.32% |
| `Endpoints/pg_30k/detail_15000` | 内部读取/组装 | 4.178 | 0.148 | 96.46% | 28.22× | -5.44% |
| `Endpoints/pg_30k/detail_perf-15000` | 内部读取/组装 | 3.996 | 0.144 | 96.39% | 27.73× | -5.44% |
| `Endpoints/pg_30k/page_100_time` | 内部读取/组装 | 92.433 | 1.996 | 97.84% | 46.31× | 99.43% |
| `Endpoints/pg_30k/page_1_time` | 内部读取/组装 | 1.160 | 1.171 | -0.96% | 0.99× | 42.49% |
| `Endpoints/pg_30k/page_20_time` | 内部读取/组装 | 19.311 | 1.394 | 92.78% | 13.85× | 97.14% |
| `Endpoints/pg_30k/page_20_tokens` | 内部读取/组装 | 715.049 | 53.623 | 92.50% | 13.33× | 97.14% |
| `AccountBundleMetadata` | 内部读取/组装 | 12.163 | 11.270 | 7.35% | 1.08× | 33.86% |
| `AccountSummaries/page50/bindings_false` | 内部读取/组装 | 16.624 | 2.901 | 82.55% | 5.73× | 94.67% |
| `AccountSummaries/page50/bindings_true` | 内部读取/组装 | 16.392 | 3.401 | 79.25% | 4.82× | 94.03% |
| `AccountSummaries/page500/bindings_false` | 内部读取/组装 | 55.797 | 24.268 | 56.51% | 2.30× | 83.30% |
| `AccountSummaries/page500/bindings_true` | 内部读取/组装 | 54.234 | 26.101 | 51.87% | 2.08× | 79.73% |
| `ManagedAccountsList/page50` | handler | 19.919 | 6.207 | 68.84% | 3.21× | 91.52% |
| `ManagedAccountsList/page500` | handler | 60.637 | 29.996 | 50.53% | 2.02× | 69.05% |
| `ResourceEndpoints/accounts_10000_keys_100/groups` | handler | 142.269 | 3.064 | 97.85% | 46.43× | 99.38% |
| `ResourceEndpoints/accounts_10000_keys_100/keys` | handler | 9.361 | 1.551 | 83.43% | 6.03× | 25.96% |
| `ResourceEndpoints/accounts_10000_keys_100/overview` | handler | 132.036 | 3.100 | 97.65% | 42.60× | 99.14% |
| `ResourceEndpoints/accounts_10000_keys_100/proxies` | handler | 129.834 | 2.458 | 98.11% | 52.83× | 99.51% |
| `ResourceEndpoints/accounts_1000_keys_100/groups` | handler | 11.968 | 1.520 | 87.30% | 7.87× | 92.78% |
| `ResourceEndpoints/accounts_1000_keys_100/keys` | handler | 7.678 | 1.568 | 79.58% | 4.90× | 26.13% |
| `ResourceEndpoints/accounts_1000_keys_100/overview` | handler | 11.621 | 2.339 | 79.87% | 4.97× | 90.25% |
| `ResourceEndpoints/accounts_1000_keys_100/proxies` | handler | 10.073 | 0.982 | 90.25% | 10.26× | 94.19% |
| `Endpoints/pg_30k/account_batch_10` | 直接handler | 292.660 | 23.165 | 92.08% | 12.63× | 42.45% |

日志首页 1.160 → 1.171 ms（约慢 0.96%），各轮范围重叠，不能据此声称显著退化或加速；分配字节下降 42.5%。详情查询多分配约 144 bytes 和 3–4 个对象，换来约 96.4% 耗时下降。文件批量统计仅改善约 5.3%，没有使用 PG 的倍率替代。资源元数据导出主要收益是分配减少约 33.9%，耗时只下降约 7.35%。

### 账户批量用量补测

补测已通过：**292.660 → 23.165 ms，耗时下降 92.08%，加速 12.63×**；每次分配 135,419 → 77,938 bytes，分配次数 803 → 417。基准在计时前确认 10 个账号各 300 条、合计 3,000 条请求，并验证账号 ID、Token 与 known 用量。计时包含直接 handler 的请求解析及响应编码，不经过管理鉴权路由。

首轮 PG fixture 遗漏 account_ref，实际匹配零行，旧的 3.764 → 2.539 ms 已作废。[定向补测报告](../.local/system-perf-20260927/20260927T023722-8912f0/report.json)替代该行；两轮候选仅 benchmark 测试文件不同，生产实现逐文件完全一致。

## 资源列表原始样本分位数

以下 P50/P95 从每版全部 90 次已记录的 handler 样本重新合并计算，**不是**三轮 P95 的中位数。它们仍是隔离串行处理耗时，不是生产用户网络 P95。

| 场景 | 前/后样本数 | P50 ms 前 → 后 | P95 ms 前 → 后 |
| --- | ---: | ---: | ---: |
| `accounts_1000_keys_100/groups` | 90 / 90 | 11.934 → 1.474 | 13.454 → 1.810 |
| `accounts_1000_keys_100/proxies` | 90 / 90 | 10.052 → 0.938 | 10.453 → 1.339 |
| `accounts_1000_keys_100/keys` | 90 / 90 | 7.700 → 1.502 | 9.142 → 1.988 |
| `accounts_1000_keys_100/overview` | 90 / 90 | 11.540 → 2.290 | 13.835 → 2.898 |
| `accounts_10000_keys_100/groups` | 90 / 90 | 141.189 → 2.980 | 156.072 → 3.623 |
| `accounts_10000_keys_100/proxies` | 90 / 90 | 126.829 → 2.408 | 150.723 → 2.893 |
| `accounts_10000_keys_100/keys` | 90 / 90 | 9.312 → 1.521 | 10.322 → 1.936 |
| `accounts_10000_keys_100/overview` | 90 / 90 | 132.848 → 3.116 | 148.979 → 3.734 |

## SQL 执行计划核对

[6 份完整计划](../.local/system-perf-20260927/logs/query-plans.json)由 tracer 抓取真实读方法 SQL 后执行 EXPLAIN ANALYZE BUFFERS，避免只验证手写的理想 SQL。

- event ID 详情使用唯一索引，返回 1 行，4 个缓存块命中。
- 数字 ID 详情使用主键/event 索引的 BitmapOr，返回 1 行，6 个缓存块命中。
- 基线两例实际为主键倒序扫描加过滤，均丢弃 15,000 行、命中 410 个缓存块；不能描述为完全没有索引。
- 第 100 页升/降序均走时间索引、无额外 Sort；扫描 5,001 行后返回 51 行，OFFSET 仍跳过 4,950 行。

这些计划均为热缓存，read blocks 为 0；单次 Execution Time 未用于计算提升百分比。

## 前端 CPU 基准

| 工作负载 | 优化前 | 优化后 | 耗时下降 |
| --- | ---: | ---: | ---: |
| `resource_name_sort_10000` | 650.185 ms | 22.933 ms | 96.47% |
| `key_csv_10000_with_2000_groups` | 59.422 ms | 13.902 ms | 76.60% |
| `log_time_parts_2000` | 99.070 ms | 8.961 ms | 90.95% |
| `account_daily_summary_2000` | 87.597 ms | 4.626 ms | 94.72% |
| `log_percent_10000` | 188.555 ms | 5.058 ms | 97.32% |

完整首屏包大小受并行变更影响，不据此汇总整站收益。日期缓存有 32 个条目的上限，DST/午夜/多时区交替测试通过；资源 Map 保留重复 ID 取第一条及缺失回退行为。

## 验证与边界

- `go test ./...`：通过（无 live 配置的测试不表示真实推理验收）。
- `go vet ./...`：通过。
- 冻结候选普通管理 PG/Redis 回归：**271/271 通过，0 跳过**。
- 同版生产源码 Linux `-race`：**272/272 通过，0 失败/跳过/遗漏**。仅额外增加真实查询 EXPLAIN 测试；临时测试容器和凭据文件已清理。
- 前端 Node 测试：**185/185 通过**；优化前派生基线与候选 Vite build 通过。
- 浏览器：资源管理 **18 项**、账号图表生命周期 **4 项**、管理功能 **22 项**、设置 **12 项**、Dashboard **10 项**、日志 **12 专项/55 断言**通过。Dashboard 使用源代码 Vite 检查真实图表实例；其余对应隔离构建和 mock API。日志套件前 7 专项及剩余 5 专项的原始结果逐项关联，未将未执行场景算入通过数。
- 86 路由与 10 页面逐项审查见[接口清单](SYSTEM-API-REVIEW-2026-09-27.md)及[前端记录](../.local/system-perf-20260927/frontend/frontend-report.md)。审查后保留的路径没有各自独立提速数字。

仍需按实际数据规模决定的候选包括：全局 last-used 对长期 usage_events 的聚合、清理历史任务与 review 文档扫描、新增运维页的大窗口扫描。现有事务、幂等、凭据代际、取消和一致快照约束保留；本轮没有未经量化的数据库索引/迁移，也没有改动网关请求处理路径。

本工作区同期存在控制台时间/Token 展示、运维页和网关任务。基线原始 829 个文件的 hash 完整性已复核；仅为两版比较添加共用 benchmark 文件，未改基线实现。新源码快照及所有可归因场景均有证据，不能把其他任务的功能加入或网关收益算入本轮。

## 证据与复现

- [合并后的有效性能与验证 JSON](../.local/system-perf-20260927/final-report.json)，已使用非空 batch 补测替换无效场景。
- [首轮性能报告 JSON](../.local/system-perf-20260927/20260927T022740-9ed1ba/report.json)，含 24 个首轮场景，已作废的空 PG batch 单独标识在[无效样本记录](../.local/system-perf-20260927/logs/invalidated-samples.json)。
- [原始 baseline 输出](../.local/system-perf-20260927/20260927T022740-9ed1ba/baseline/benchmark.log)、[candidate 输出](../.local/system-perf-20260927/20260927T022740-9ed1ba/candidate/benchmark.log)、[271 项管理测试](../.local/system-perf-20260927/20260927T022740-9ed1ba/correctness.log)。
- [race 报告](../.local/system-perf-20260927/race-20260927T023158/report.json)、[race 与真实查询计划日志](../.local/system-perf-20260927/race-20260927T023158/race.log)。
- [前端报告](../.local/system-perf-20260927/frontend/frontend-report.md)、[CPU 原始样本](../.local/system-perf-20260927/frontend/benchmark.json)、[浏览器网络与加载样本](../.local/system-perf-20260927/frontend/account-loading.json)。

后端完整重跑（使用已修复的非空 batch fixture）：

```powershell
python tools/system_performance.py --baseline .local/system-perf-20260927/baseline-src --benchtime 30x --count 3
```

前端完整对照：

```powershell
node frontend/scripts/benchmark-system.mjs --browser
```

性能候选源码 SHA256：`39aa19a335a557da619438f07d10317195654427185a9a4eece509dd99984d4f`。同版 Linux 构建产物位于 `.local/system-perf-20260927/20260927T022740-9ed1ba/candidate/codex-agent-linux-amd64`，SHA256 `279e3d231e7619616837eaa7c0977dd0da1b7aafb27cd1337b46bdfeb1b6d2f5`；后续生产使用另行冻结、完整验收的组合发布包，见上线记录。
