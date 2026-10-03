# 本地读取接口补充验收（2026-09-29）

第二阶段开始时，当前既有后端文件与首轮正式验收源码 `2aea57c48ef3f21cdbf6f5bde183ae4f63efda04e2dd1e805c61847067c132ba` 的 manifest 完全一致。该验收实际采样 35 种读取请求、13 组写入，不能据此把所有 127 个路由注册/分派入口都标记通过。

以下是逐 handler 核验后尚未覆盖的本地 GET 请求。所有新增形状均准备在 `TestPerformanceSLOAdditionalReads` 中通过真实 loopback HTTP、实际管理鉴权和完整响应读取验证；尚未执行的行不写成达标。

| 路由 | 具体输入/数据形状 | 原有成本与本轮状态 |
| --- | --- | --- |
| `/api/accounts/{name}` | 原生稳定 ID，返回完整 account summary | 首轮动态路由归因误把固定 `/manage` 当作该详情覆盖；实际应补测 |
| `/api/accounts` | limit=50/500；search=当前名称 | SQL limit 与批量 summary；与 `/manage` 是不同接口 |
| `/api/accounts/{name}/upstream` | 已保存 models 快照；相同 credential generation | GET 仅读管理快照，不调用上游；原 inventory 的 upstream 分类需纠正 |
| `/api/accounts/{name}/auto-reset` | 关闭状态的已保存策略 | 账号 metadata + 一个管理文档 |
| `/api/accounts/{name}/schedules` | 20 个关闭计划，每计划已保留 100 条结果 | 单账号文档读取；结果数组也会解码，需测真实文档尺寸 |
| `/api/account-schedules/{id}/results` | 同上文档，limit=50/100 | ID 可直接解析所属账号；无需枚举全部计划文档 |
| `/api/accounts/{name}/lease` | 已保存版本 1、未来到期租期 | 单账号 metadata + 一个租期文档 |
| `/api/accounts/import-leases/receipts/{receipt}` | 本管理员所有的已保存恢复回执 | 单文档读取与 owner 校验 |
| `/api/account-oauth/{id}` | 本管理员所有的 pending 会话 | GET 只读已保存会话；不执行 OAuth exchange |
| `/v0/management/auth-files` | 无筛选全部账号；name=稳定 ID；name=filename；auth_index=哈希 | 原先筛一个仍解密全部账号；本轮先筛 metadata，选中项才解密，并只查绑定代理 |
| `/api/v1/admin/accounts` | page_size=50/500 | 首轮已改页内凭证解密，但尚无该路由 HTTP SLO 样本 |
| `/api/v1/admin/accounts/{id}` | compatNumericID | 本轮解析仅读取 ID/显示名和兼容别名，消除全部账号关联与宽 metadata 读取 |
| `/api/v1/admin/groups` | page_size=100，非空资源 | 当前按返回规模读取、过滤和分页；基础 fixture 只有 10 个 group |
| `/api/v1/admin/groups/all` | 全量非空 group | 合同为完整输出，不能以固定 limit 代替 |
| `/api/v1/admin/groups/{id}` | compatNumericID | 当前仍取全 group 后计算哈希找一项 |
| `/api/v1/admin/proxies` | page_size=100，非空资源 | metadata 查询，不读密码 |
| `/api/v1/admin/proxies/all` | 全量非空 proxy | 完整输出合同 |
| `/api/v1/admin/proxies/{id}` | compatNumericID | 当前仍取全 proxy metadata 后计算哈希找一项 |
| `/api/logs/errors` | page_size=50；另 resolved=false | 不同于首轮 `/api/logs?status=error`；前者包含精确总数与 review 状态 |

`/v0/management/auth-files/models` 实际会请求上游 models，原 inventory 将它列本地读取不准确。本轮不会为了补覆盖产生真实上游调用。完整导出单独按输出规模验证，不归入普通固定大小读取。

## 已实施：CPA 筛选前置

`compatSelectAccounts` 在同一个 RepeatableRead 事务里读取 metadata 与兼容字段、执行文件名/ID/auth-index 筛选，然后只为结果集批量读取并解密 `account_credentials`。完整列表仍返回全部账号；缺失匹配仍成功返回空数组；名称筛选仍只接受原生 ID 或文件名，不误扩展成显示名搜索。

代理 URL 仅读取结果账号引用的 `proxyColumns`，不扫描无关代理、不读取密码密文、不重建在线路由索引。

新回归覆盖 21 个账号下 ID、保留文件名、auth index、错配和空结果的公开 JSON 等价；AEAD 计数断言凭证解密次数等于显示项数。损坏无关账号凭证不会拖垮定点结果，选中损坏账号或请求全列表仍报告存储失败。文件模式契约测试已在本地通过；新增 PG 测试已纳入 `.local/performance-slo/20260928t230321-5b8767/report.json`，全部通过。

兼容详情的别名解析也不再读取全部账号的 notes/tags、group/proxy 关联和 credential version。新 `compatAccountReferences` 在同一个 RepeatableRead 事务内只读 `id,display_name` 与兼容记录，仍保留原有全局 numeric collision 检查、稳定 ID 优先与文件别名匹配。它仍需枚举账号引用，尚未引入持久化别名索引；不要把字段投影优化描述为 O(1) 查询。

## 第一轮实际 HTTP smoke

`.local/performance-slo/20260928t230646-078f5c/report.json`：27 种新增输入形状、189 次完整 HTTP 请求全部成功且语义断言通过。使用既有 10,000,000 条日志/事件、1,000 账号的每日突发 fixture，每形状 1 次首次请求和并发 1/4 各 3 个样本；不是长期分位数验收。

| 形状 | 首次完整响应 |
| --- | ---: |
| 原生账号详情 | 7.912 ms |
| legacy 列表 500 账号 | 44.465 ms |
| 已保存 schedule 文档（20 计划 × 100 结果） | 8.124 ms |
| CPA 全部 1000 账号 | 43.369 ms |
| CPA 稳定 ID / 文件名 / auth_index 筛选 | 15.205 / 13.439 / 14.904 ms |
| Sub2 500 账号页 / numeric ID 详情 | 32.856 / 6.777 ms |
| 错误复核列表 / resolved=false | 11.380 / 388.269 ms |

本轮全部请求中最慢为 388.269 ms。源码 SHA-256 为 `449edd70fa624ba0f4f423255f6be70f308f2ffbde2beab9880b1b60bb631719`。没有对既有大表运行迁移、重建或 ANALYZE，也没有触碰日志/事件人口；仅准备少量管理状态文档。

后续 `.local/performance-slo/20260928t231554-45649b/report.json` 对 27 形状各首次 1 次、并发 1/4 各 20 次，共 1,107 个请求全部成功且断言通过，最大 365.800 ms。此轮与另一个 agent 的只读计划诊断/独立功能测试可能重叠，不能称作独占环境最终 SLO 证据。其源码为 `e88f498469aaed048e9e55807f12b246a50cd8247a8f931e300f05a9eb228eb9`；最终由总任务使用统一源码与迁移版本、排他窗口同时复验 35 + 27 形状和持久写入。

上述 burst schema 实际只应用 000–009。当前源码含尚未应用的 010/011，因此严格二进制复用检查曾拒绝 source/DB 迁移集合不一致（`.local/performance-slo/20260928t231435-105a0c/report.json`），该失败没有执行性能请求。不能据此推断数据库已经升级。这些补充接口不使用 010/011，新测试明确不自动迁移；最终统一验收会记录真实已应用迁移集合。

## 复跑

复用已保留合成 fixture：

```powershell
python tools/performance_slo.py --reuse .local/performance-slo/20260928t223935-af1ba8/report.json --test-run '^Test(PerformanceSLOAdditionalReads|PGCPAFilteredRead.*)$' --samples 20 --timeout-minutes 10
```

新增 HTTP 测试会创建独立名称的少量管理状态文档用于非空场景，不修改既有日志、事件或聚合计数，不运行迁移，不调用上游。每形状包含首次请求、1/4 并发、完整响应语义断言；准备文档不计入请求耗时。数据库/操作系统缓存不强制清空。
