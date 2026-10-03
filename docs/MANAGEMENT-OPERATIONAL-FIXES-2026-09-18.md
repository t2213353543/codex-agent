# 非计费管理对齐与故障修复

2026-09-18。在无计费候选基础上继续审计当前源码和本地 Sub2API 实际接入流程。上一轮移除计费是有效进展，不是全量管理对齐完成；本轮仍不更改 Codex 网关转发行为、不部署生产。

## 日志查询与身份

默认时间排序原为 `extract(epoch FROM created_at)::bigint,created_at,id`，现有 `request_logs_time_idx(created_at DESC,id DESC)` 无法直接满足这个表达式排序。保留原时间顺序和同时间 ID 次序，改为原始 `created_at,id`，游标比较同步使用原始时间类型；模型和其他数值排序不变，不新增或改写迁移。

本地独立 PostgreSQL 16、5 万条合成日志、51 条首屏的 EXPLAIN ANALYZE：

| 顺序 | 原查询 | 当前查询 |
| --- | --- | --- |
| 升序 | Gather Merge / Sort / Seq Scan，10.810 ms，1253 shared hit blocks | request_logs_time_idx Index Scan，0.044 ms，6 blocks |
| 降序 | Gather Merge / Sort / Seq Scan，12.932 ms，1252 shared hit blocks | request_logs_time_idx Index Scan，0.044 ms，5 blocks |

这是本机单次执行计划证据，不是生产延迟保证。`TestPGManagedUsageTimeIndexPlan` 可重复验证计划结构，不把固定耗时当测试阈值。文件/PG 的 7 类排序×2方向、相同时间、多页游标、页码跳转、全量导出和旧 v0 时间游标已通过。

账号搜索候选原 PG 使用 `account_ref`，会把 `legacy:<name>` 内部归集键当成真实账号 ID，且历史标签的 `max` 受数据库排序规则影响。当前仅从记录中读取真实 `account_id`，标签使用 C 排序规则，与文件路径一致；无 ID 历史请求继续可按名称查，不伪装成已删除资源 ID。

## 最近业务请求排序

参考 `sub2api/current-source/frontend/src/views/admin/AccountsView.vue` 和 `backend/internal/repository/account_repo.go` 的 `last_used_at` 服务端排序是实际管理能力。本站此前只有可选展示列，本轮新增 `sort_by=last_used_at`。

- PostgreSQL 一次按稳定 `account_ref` 聚合 durable usage_events，再关联账号排序；不使用可能含旧日桶时间的 usage_totals。
- 文件模式完整扫描留存明细，只匹配稳定账号 ID，不按名称猜测；数据读取失败返回错误，不把失败当未使用。
- 升降序均将无匹配记录的账号置后，相同时间以稳定 ID 同方向排序。列表、全筛选选择和导出复用同一服务端规则。
- 文件/PG 跨页、同值、改名/同名旧记录、已删除异 ID、未知时间，以及清理详情后 PG durable 事件仍可排序均有测试。

专项原始输出：`.local/management-parity/operational-fixes-20260918/root-query-tests.log`，7 个顶层测试通过；所有该专项临时容器已清理。以下为最终同候选联合验收，不用局部通过代替其他功能验证。

## 管理操作可靠性

CRS 单账号凭据、来源链接及审计现在原子提交，审计失败不再留下已经变更的凭据。代理创建和多账号批次仍是独立结果，不冒称整个批次有单一事务。文件模式只新增可选 `managed_crs_links` 管理字段，兼容旧文档；未修改账号调度或网关算法。

定时计划创建支持稳定 `request_id`：网络结果未知时前端冻结原参数并保留浏览器会话草稿，重试同一请求不会新增第二个计划；不同参数或已删除计划返回冲突。刷新计划列表与读取历史使用独立请求代次，不再把历史面板卡在加载中。单个/批量删除账号后清理计划，旧孤儿计划在管理 worker 扫描时补清；已停用的过期租期不重复写入或增加版本。

完整事务、幂等、部分失败与重复请求限制见 [管理可靠性契约](MANAGEMENT-RELIABILITY-2026-09-18.md)。

## 前端边界

- 账号日期 URL 超出 JS/后端支持范围不再抛出 RangeError；界面阻止查询并明确提示清除无效日期，其他有效条件保留。
- 日志与账号日统计将合法时区别名规范为服务端可接受的 IANA 名称；拒绝仅浏览器支持的数字偏移等输入。
- Token 趋势、分布和排行对全未知用量显示未知，趋势断开；真实零保留 0，部分采样显示已记录合计及覆盖请求数。空时间桶仍表示真实零请求。
- 清理文案只描述累计用量/账号额度，不再称项目有账务功能。所有计费实现保持退役。

## 同候选验证

- Go 源码 `bdfbf92ba3102028440c00f75fb8d728f117fc5f4f3f227782429198b2147c82`，225 文件；管理 Linux PostgreSQL16/Redis7 race 94 顶层、277 事件通过，0 失败/跳过/缺失，源码前后未变，临时资源清理完成。
- 固定 Go 全库测试 454 顶层通过、103 环境依赖跳过、0 失败；含子测试 1176 通过、116 跳过。vet、Windows/Linux 构建通过。跳过项单独记录，不算通过。
- 前端 manifest `0e286edbfa6e169513ab62d8bdfa57552867e8a7a7ad3cdf664a140805ab36af`；54 个产物、48 个既有框架资源保持字节一致。账号入口 `accounts-managed-DDB1Q0kq.js`，日志入口 `logs-managed-Bgcf_x3-.js`。
- 当前 146 项前端单元测试通过；账号 53 组、日志 38 组浏览器回归通过，页面异常 0。桌面/手机/四主题截图包含未知创建请求、真实零/未知用量和趋势断点，关键截图经交叉目视核对。
- Go 证据：`.local/management-parity/local-race-20260918t065425-d36576`；前端证据：`.local/management-parity/operational-fixes-20260918`。归档索引见 [机器验收](MANAGEMENT-OPERATIONAL-FIXES-VERIFICATION-2026-09-18.json)。

## 尚未证明完成

固定包生成后，另启动相同 Go/前端哈希的真实空库预览 `http://127.0.0.1:50666/#/login`；证据在 `.local/management-parity/operational-fixes-preview/{api-verification,browser-verification}.json`。Cookie 登录、真实最近业务请求升降序 API、CSVv3、计费路由关闭，以及 11 项桌面/手机浏览器检查、18 张截图通过，页面/API/静态资源错误为 0。未导入真实账号、未发送上游请求；root 独立访问 ready 为 200。此前 57451 预览和数据库不变。本补充证据未覆盖写入已冻结报告。

本轮没有宣称全部 Sub2API 能力对齐：参考账号列表的 ID/状态全局排序仍未接入本站；恢复网关错误/清除冷却、自动恢复、独立 schedulable、Spark 等行为仍超出当前“仅已复现兼容错误可改网关”的授权边界。多模态 Token 及部分网络阶段/完整上游失败链仍需实际采集来源，不能用现有管理字段推造。

共享快照相较前轮另包含其他任务所有的 native 启动与 WebSocket budget 文件变更，本管理任务未编辑这些文件，也不以本管理测试为其独立发布背书。旧固定候选与预览数据保留，本轮不部署生产。
