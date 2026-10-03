# C 组：观测口径、前端语义和发布门禁

Owner task: `01a0e7ed-5abb-7690-8d5d-c11b4273b156`。

本组已实际修改共享源码并完成本地回归；生产统一发布由主任务 `01a0dfcf-70a3-7713-a3b6-449eb2a0fad5` 负责。没有启动真实模型请求、修改生产设置、更新远端 monitor 或重建共享前端。

## 已完成修复

| 范围 | 原问题 | 当前行为 |
| --- | --- | --- |
| 缓存审计 | 手动审计把未知读取量请求计入分母；控制请求和 WS 预热可能计入缓存观察 | 已知读取和对应输入共同计算，创建量独立；models/warmup 排除在缓存观察之外，请求/错误/raw token 总数仍保留 |
| 错误监控 | 漏识别 `usageLimitExceeded`，此前失败尝试可掩盖最终错误，截断的 attempts 被当成最终状态 | 补齐限流 alias，仅用未截断的最后尝试辅助归类；保留实际 attempt 的 status/code/phase/timing 字段 |
| 前端 | 缓存创建量已经显示却提示曲线留空；header-only 模型被标成返回模型 | 趋势按所选指标及符合缓存统计条件的请求数解释；头部声明标为“上游声明”，真实返回模型仍显示“返回” |
| postdeploy | `--smoke` 返回 403 或 failed 仍退出 0 | 保留失败报告并退出 1，关闭连接；成功与非 smoke 路径不变 |
| PG 验收指纹 | 冻结后新增源码漏过检查 | 比较完整路径并集，新增、删除、修改均能识别 |
| acceptance 指纹 | 删除受测 fixture 可漏过单向比较 | 比较同范围的完整文件集合，保留既有明确批准的单文件测试例外 |
| 前端证据 | acceptance 重读共享浏览器结果并绑定重新计算的当前 dist | 使用 work 内封存的报告与七份证据，核对源码、文件路径、内容哈希和完整产物集合 |
| 前端验收过程 | 失败重跑遗留旧 pass；浏览器期间产物变化未检查 | 新轮先失效旧汇总；build 后冻结产物，浏览器完成后再次核对 |

保留主任务在 `cache_rate_report.py` 新增的 `tenant_partitions` 和 monitor 的 `bps_prompt_cache_scope`。未修改 Settings.vue 或主任务持有的核心缓存/协议文件。

## 本地验证

2026-09-28 12:25:21 UTC 的合并检查：Python 共 45 项，其中 **39 通过、6 项原生 PostgreSQL opt-in 明确跳过**；前端 Node **44 通过、0 失败**。受测文件在执行期间哈希不变。

观测 SQL 的纯本地用例执行实际聚合表达式，并将 PostgreSQL 标量语法适配至 SQLite；这证明合成输入下的口径，不代替原生 PostgreSQL 运行。主任务应在最终统一验收时运行原生 PG 和新版只读观测脚本。前端生产 build/浏览器结果须重新生成，旧封存会因本轮源码变化被正确拒绝。

一条离线复跑命令（不部署、不构建 dist、不调用模型）：

```powershell
python .local/coordination-20260928/team-c/verify_team_c.py
```

完整文件 SHA 和执行日志索引：[final-verification.json](../.local/coordination-20260928/team-c/final-verification.json)。新增测试会改变整体源码指纹；当前候选必须重新冻结。

## 修改文件

- `tools/cache_rate_report.py`
- `tools/recent_failures_cache_audit.py`
- `tools/codex_agent_live_monitor.py`
- `tools/cache_reliability_postdeploy.py`
- `tools/frontend_release_gates.py`
- `tools/cache_observation_pg_linux_check.py`
- `tools/reliability_acceptance.py`
- `tools/test_observability_contract.py`
- `tools/test_cache_reliability_postdeploy.py`
- `tools/test_frontend_release_gates.py`
- `tools/test_cache_observation_pg_linux_check.py`
- `tools/test_reliability_acceptance_sources.py`
- `frontend/src/components/LogUsageAnalytics.vue`
- `frontend/src/views/Logs.vue`
- `frontend/tests/log-display-observation.test.js`

最终指纹也包含未修改的 `tools/test_codex_agent_live_monitor.py`，因为其现有回归参与本组验证。

## 子任务证据

- [前端语义审查](../.local/coordination-20260928/team-c/frontend/review.md)
- [postdeploy / frontend gate 审查](../.local/coordination-20260928/team-c/release/REPORT.md)
- [观测工具审查](../.local/coordination-20260928/team-c/observability/review.md)
- [PG 指纹缺陷原始复现](../.local/coordination-20260928/team-c/pg-source-drift-result.json) 与 [修后检查](../.local/coordination-20260928/team-c/pg-source-drift-verification.json)
- [acceptance 删除 fixture 的原始复现](../.local/coordination-20260928/team-c/acceptance/source-deletion-before.json)

本组完成状态以统一协作文档的 **SOURCE FROZEN** 条目为准；这份报告不代表生产已部署。
