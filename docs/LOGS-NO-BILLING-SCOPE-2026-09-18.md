# 日志页移除全部计费功能

2026-09-18。最新用户要求为项目不需要任何计费。本次范围覆盖日志页及其统计工作流，不继续此前价格/采购/销售对齐目标。

## 当前功能

保留请求列表、身份与模型筛选、URL和收藏、固定记录快照、游标与精确总量、Token输入/输出/缓存、总耗时/TTFT/独立阶段、真实模型链/路径/有界尝试、错误处理、清理任务、脱敏与完整管理导出、IP归属地手动查询、账号/KEY/成员组用量排行及移动端。

移除所有参考费率、分时价、采购成本、采购策略、配置售价入口和详情组件；统计不再展示任何金额、价格覆盖率、费用分布、费用趋势或金额排序。运行组件不导入计费helper或API，排行只允许请求数、Token各桶和成功率。错误处理与导出说明同步去除费用语义。

当前页导出原来就是非身份运行指标白名单，本次维持该白名单并新增测试：即使旧响应携带任意`cost/account_cost/sale_cost`及金额字段，也不会进入当前页JSONL。服务端全量CSV/JSONL由后端owner同步移除计费字段，独立验证。

## 验收范围

`frontend/tests/run-log-management.mjs`不再导入或执行 reference pricing、time pricing、procurement、sale policy、cost policy 五套计费测试。这些是根据新用户范围退役的工作流，不是跳过失败后声称旧计费测试全通过。报告明确记录`scope=operational_logs_without_billing`及退役原因。

新`log-no-billing-browser.mjs`验证日志首屏及展开统计没有计费菜单/金额，详情不显示旧响应金额，不发计费API，Token/成功率和链路可选列仍可操作，当前页导出不带金额，四主题及390/320布局可用。原非计费查询、清理、观测、错误处理、Geo和下载会话测试继续运行。

`log-no-billing.test.js`新增三项：非计费排行白名单与零/未知、当前页导出排除所有旧金额字段、URL/收藏规范化拒绝计费查询参数。合并现有非计费日志相关单测共47项通过。构建、浏览器最终执行结果及精确哈希以`no-billing-20260918`最终机器证据为准。

本子任务未修改Accounts或Go网关。并行账号与后端移除结果由对应owner汇总；之前冻结产物和报告是历史验收，不代表现在仍应保留其计费功能。

## 本轮执行结果

- 日志非计费浏览器 **35 组通过，页面异常 0**：查询/快照/完整导出11、清理3、新无计费工作流3、观测6、错误处理5、手动Geo5、下载会话2。旧计费套件明确退役，未用于拼接更大的通过数。
- 非计费日志相关 **47 项单测通过**。组件编译通过；首屏1440浅色/390暗色、手机成功率排行和其它关键区四主题截图已检查。
- `.local/retired-billing-20260918/frontend-source` 保留本owner退役18个计费组件/API/helper，`frontend-tests`保留9个纯计费测试文件，包括尚未完成的渠道区间编辑器；它们不再进入运行构建和npm顶层测试glob。账号侧归档由另一owner负责。
- 构建只追踪64个运行源码，计费源码可达数为0；54个产物保持48个旧壳基线资源字节。Accounts入口`accounts-managed-HUq5arXF.js`，Logs入口`logs-managed-8jXhj7Tb.js`。最后账号死样式和退役导入解析分支清理后，已在此最终候选重新完整运行Logs35组，122张截图均重新生成。
- [构建manifest](../.local/management-parity/no-billing-20260918/build-manifest.json) SHA-256 `dab4be81d838fa47f4224f1f68932c6f35c734a0814052d1ad5baa9055341618`，前端集合SHA-256 `54db4d3ac12e8652a23a07eb797fe9a83c771b317e07aa3771e11b0e03dc8be9`，64个运行源码哈希集合SHA-256 `5484887f9203b16433a2afabfc4a5e8f282577a1af4aa0853b615bc840fd99a5`。[浏览器报告](../.local/management-parity/no-billing-20260918/logs-browser/report.json)、[机器验证](../.local/management-parity/no-billing-20260918/logs-verification.json)。

浏览器验证运行真实旧Vue/路由/UI，网络为隔离合成响应；服务端去计费API和统计/CSV行为由root的实际Go测试另证。没有部署或改动固定预览服务。
