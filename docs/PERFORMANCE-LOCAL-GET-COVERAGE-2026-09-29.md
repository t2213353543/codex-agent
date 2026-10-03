# 本地 GET 路由覆盖核对

只读核验当前实际注册入口与 35 + 27 个管理 HTTP 请求形状，并加入真实 `/health` TCP 样本后，结果为：**42 个本地 GET 路由均已有具体请求样本，共 63 种输入形状。** 这只是入口/请求形状覆盖，不能推导每种参数、对象规模或故障状态已达标。

完整逐路由清单：

- [CSV](../integration/performance/results/local-get-coverage-20260929.csv)：每行一个路由，包含 `measured/unmeasured`、实际完整请求路径、case 名称、证据报告和未测输入形状。
- [JSON](../integration/performance/results/local-get-coverage-20260929.json)：同时记录报告及源码/二进制摘要、样本数、并发档、状态和当前路由注册文件摘要。

当前 127 个注册/分派入口中，本清单提取 45 个显式 GET 注册，以及通过 ANY 注册但 handler 仅允许 GET 的 `/health`。其中 42 个为本地读取，3 个为完整导出，1 个需调用外部 models。其余写入方法、网关协议分派前缀和静态资源不能混进这 42 个路由。

## 动态路由归因

每个实际请求只归属一个路由。匹配时先去掉 query，要求相同路径段数，并以静态段更具体的注册优先：

| 真实请求 | 应归属 | 不应误计 |
| --- | --- | --- |
| `/api/accounts/manage?page_size=50` | `/api/accounts/manage` | `/api/accounts/{name}` |
| `/api/accounts/slo_a_1` | `/api/accounts/{name}` | `/api/accounts/manage` |
| `/api/logs/errors?page_size=50` | `/api/logs/errors` | `/api/logs/{id}` |
| `/api/logs/options?field=model` | `/api/logs/options` | `/api/logs/{id}` |
| `/api/logs/coverage` | `/api/logs/coverage` | `/api/logs/{id}` |
| `/api/logs/slo_e_500/review` | `/api/logs/{id}/review` | `/api/logs/{id}` |
| `/api/v1/admin/groups/all` | `/api/v1/admin/groups/all` | `/api/v1/admin/groups/{id}` |

Sub2 的 groups/proxies 各有 list、all、detail 三个 GET 注册，总共 6 个不同读取路由。代码循环还会注册 POST/PUT/DELETE；这些不是新的 GET，按 `(method,path)` 去重后当前清单没有重复。

GET `/api/accounts/{name}/upstream` 仅读取已保存快照、必要时补本地窗口观测，属于本地读取；同一路径 POST 才执行上游检查。`/v0/management/auth-files/models` 内部调用 `client.Do`，明确排除本地性能覆盖。OAuth GET 只读已保存会话，同样不能因为路径包含 oauth 就误标外部请求。

## 当前证据绑定

该版本清单以三份已完成报告的实际 FIRST/RESULT 路径生成，共 **63 个唯一具体请求形状**。组合报告中迁移前执行的 AdditionalReads 样本由较晚的迁移后报告替换，不重复计数：

1. `.local/performance-slo/20260928t234319-fda3a5/report.json`：取原 35 形状；同报告较早的 27 个 AdditionalReads 样本不纳入本清单。
2. `.local/performance-slo/20260928t234622-6e6cb8/report.json`：取迁移完成后的 27 个 AdditionalReads 形状。
3. `.local/performance-slo/health-phase2/report.json`：实际 `webHandler` 的 GET `/health`，Windows loopback TCP，首次 1 次及并发 1/4 各 200 次，共 401 次，全部通过，最大 0.525 ms；该恒定存活入口不查询数据库。

前两项运行同一 Linux 二进制 `f8f905990fcee2346721f479dd29ea475fb92b5915c888d2cf25a9e4731fb237`，有效源码摘要为 `41d1ed1d38c1ed829bc0caec9e7193d74027c015fc6cb96b01c904840b789d90`。health 报告声明同一 `base_source_sha256`，但加入仅测试 overlay 后的有效源码为 `ea8d2d60652c55b5ea3fd4709abe2250e89b62e1c534c9a1ee6204cb60643021`，运行环境也为 Windows，不能说成同一 Linux 产物。

JSON 保留 `mixed_sources=true`，并另增 `business_source_reference` 表达共享的业务源码基线；有效源码 hash、测试 overlay hash、报告原始 scope 和运行环境均保留，`same_effective_binary_asserted=false`。每条 measurement 均能追溯自身报告，未用业务基线字段替换真实测试产物身份。

## 有实际价值的剩余输入缺口

下列输入会走不同查询分支或显著改变工作量，即使所有路由已有至少一个样本，也仍属于未覆盖范围：

- **日志与统计**：model/account/key/group/status 等组合筛选，option 的 key/group 字段和 q 搜索，resolved=true 与大量 review 文档、游标续页及游标失效，所有可选排序、snapshot、calendar/DST、长标签与高维度基数。
- **账号与资源规模**：当前账号约 1000、group/proxy 各 10；不是千万账号。需要更大账号/Key/资源数、深页、status/privacy/group/proxy 筛选、复杂绑定和 large compat 文档。
- **管理状态**：OAuth ready 需解析凭证和目标身份；现只测 pending。租期到期/错误归属，调度 running/error 快照，大型恢复回执和大结果文档，也不是同一输入。
- **快照与依赖状态**：已保存 upstream 快照目前只测 models；usage/reset 快照及窗口费用补全仍需单独覆盖。Redis 缺失、延迟、不可用，PG 失败、Cookie 会话失效等需测失败路径预算。
- **输出规模明确不同的入口**：三个 GET 导出（logs export、CPA download、Sub2 data）保留为 `bulk_export`，不把普通页大小 SLO套到整表输出。上游 models、推理和 OAuth exchange 独立于本地读取目标。

CSV/JSON 每个路由均附有具体未测形状，`unmeasured=false` 的含义仅是“至少有一种已采样请求”，不是“该路由所有可能请求已证明满足 1 秒/3 秒”。

## 复跑

```powershell
python tools/performance_route_coverage.py --prefer-later-case --reports .local/performance-slo/20260928t234319-fda3a5/report.json .local/performance-slo/20260928t234622-6e6cb8/report.json .local/performance-slo/health-phase2/report.json
```

这是纯文件分析工具，不访问数据库、SSH 或网络，不修改 Go 业务与采样 harness。
