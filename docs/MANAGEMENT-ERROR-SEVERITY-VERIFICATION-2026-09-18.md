# 错误分级管理联合验收

本轮完成最后一项已确认、可独立于网关运行行为补齐的使用记录管理能力：错误严重级别。该结论来自当前源码与参考实际接入链审查，不等于所有网关、认证及采集能力已完成。计费继续完全排除，详 [当前目标审查](CURRENT-NONBILLING-GOAL-AUDIT-2026-09-18.md)。

## 实现与原始事实

- 服务端只读派生 `error_severity` 与 `error_severity_source=management_rules_v1`，提供P1/P2/P3/未知；成功不分级。
- HTTP200的明确限流/过载可为P1，但原HTTP、业务结果、错误文本、Token和处理历史不更改。规则与参考前处理差异明确列在 [API契约](MANAGEMENT-ERROR-SEVERITY-API.md)。
- 文件与PG实现相同规则，列表、统计维度、详情、错误处理、快照、CSV/JSONL、清理使用同一筛选。CSVv3尾部追加2列，总61列，不包含任何金额。
- 桌面/手机/详情与错误列表展示带来源解释的级别；未知来源不按上游原生等级展示。错误工作区级别筛选独立，切换从第一页读取，不修改主列表筛选。
- 级别分布只含失败/中断，支持请求/Token维度和点选筛选。未知用量和实际零继续区分。

## 固定 Go 验证

- 源码摘要：`ff4ce018b14afdc1f1549cb6e1881f36c029fdd58a80da8f9a9238dbf877f10e`，239文件，测试前后相同。
- 108顶层管理Linux PostgreSQL16/Redis7 race、323事件全部通过，0失败/跳过/缺失，临时容器、网络与凭据清理完成。
- 完整Go测试470顶层通过、113环境依赖跳过、0失败；含子测试1224通过/126跳过。vet与Windows/Linux构建通过。环境跳过保留原原因，不算通过。
- 新增798种状态/HTTP/错误组合的独立Go与SQL规则矩阵，另有文件/PG API全集成，覆盖分页、复核状态交集、快照、导出61列和精确清理；原记录无派生字段写入。
- 原始证据目录 `.local/management-parity/local-race-20260918t073333-6f6254`。局部PG首轮旧CSV位置断言失败保留；测试改为按字段名定位后完整专项通过，不删除失败证据。

## 前端候选

固定manifest `c3368f22919950b4ae2ca6de744714ccebeaaa21cb16bf4abc3eb9523fd37648`，Accounts `accounts-managed-BXbY_rsM.js`，Logs `logs-managed-COfEPxyG.js`。54静态文件、48既有框架资源原字节保持，旧包不覆盖。证据位于 `.local/management-parity/error-severity-20260918`，最终机器索引为 [固定包验收](MANAGEMENT-ERROR-SEVERITY-VERIFICATION-2026-09-18.json)。

最终账号55组、日志46组浏览器及154项前端单元全部通过，页面异常0。账号48张、日志162张有效截图绑定同一构建；新等级/来源/独立筛选与旧查询、快照、账号流程一起回归。67个可达源码文件和全部54资源逐SHA验证通过，固定包未包含退役计费模块。

导出包 `frontend-c3368f229199.zip` SHA256 为 `f44425c47c11e4eed48ea104d7416da95912dd15397c994f1cfb03da11095c4e`；源码包 `frontend-source-c3368f229199.tar.gz` SHA256 为 `2634d5ca645e4baa62a19c305293f8f174c0af703a0714592881c5ef027ea32f`，位于该候选目录。

## 发布与剩余范围

同候选真实预览：`http://127.0.0.1:65087/#/login`。新独立PG中保留3条明确标识的合成记录（成功、HTTP200过载、HTTP缺失），没有账号或真实上游请求。真实API验证成功不返回分级、200失败为P1、缺HTTP为unknown，以及失败维度、61列CSV；累计聚合已同步重建，概览3请求/12Token与日志相符，不将直接插入样本产生的空聚合当成产品事实。证据为 `.local/management-parity/error-severity-preview/api-verification.json`、`synthetic-severity-api.json` 和 `browser-verification.json`，独立于已固定源码包。root已检查ready200和实际日志桌面展示。

本管理任务未改网关运行文件。共享快照额外继承其他任务的 `response_output.go`、custom-tool及native-CLI测试变更，不据管理测试替代对应owner发布验收。没有部署生产，没有测试真实账号或上游。

下一步的真实缺口是图像用量、已有客户端关联来源、准入前错误及精确阶段等采集，以及另外的网关恢复/每账号协议/身份签名能力。前者仍需要网关代码边界决定，后者会改变运行行为；在用户当前约束下不悄悄加入。不把没有来源的历史记录填成0或伪完整，也不把项目宣布为全部对齐完成。
