# 无计费管理候选验收

2026-09-18。响应用户“本项目不需要任何计费”，本轮删除管理计费功能，而不是继续补齐旧定价需求。旧阶段报告保留，仅作历史证据。完整范围见 [无计费说明](MANAGEMENT-NO-BILLING-SCOPE.md)。

## 完成内容

- 账号与使用记录页面不再出现参考价、采购、售价、倍率、金额统计或费用趋势；保留此前恢复的界面框架。
- 后端计费路由关闭，计算实现退出活动包；列表、详情、全量统计、账号批量用量、快照、CSV/JSONL 只提供非计费信息。CSV 版本为 `codex-usage-v3`。
- 旧账号备份中的成本段忽略；账号凭据、关联资源与租期仍独立校验、预览、确认恢复。计费 opt-in 请求明确拒绝，不偷偷执行部分写入。
- 历史已存计费文档和原始请求记录保持原样。退役代码与测试归档在 `.local/retired-billing-20260918`，不计入新功能通过数。
- PostgreSQL 与文件统计在稳定身份、同名不同 ID、无 ID 历史记录、分组去重/空值、空日历桶上对齐；请求、Token、时延、TTFT、模型/路径/错误维度保留。

## 固定候选

- Go 源码：`c35280b7aa6ca1a5cae86d79feb15333cb58d241e65bd6448547672213923ad6`，214 个文件，测试后不变。
- 前端 manifest：`dab4be81d838fa47f4224f1f68932c6f35c734a0814052d1ad5baa9055341618`。
- 前端产物：`54db4d3ac12e8652a23a07eb797fe9a83c771b317e07aa3771e11b0e03dc8be9`，54 个文件，48 个既有框架资源原字节保留。
- 账号入口 `accounts-managed-HUq5arXF.js`；日志入口 `logs-managed-8jXhj7Tb.js`。归档和逐文件证据见 [机器验收](MANAGEMENT-NO-BILLING-VERIFICATION-2026-09-18.json)。

## 验证结果

| 验证 | 结果 |
| --- | --- |
| 全库 Go 测试 | 顶层 439 通过、92 环境依赖跳过、0 失败；含子测试 1121 通过、105 跳过。跳过不算通过。 |
| 独立 Linux PostgreSQL 16 / Redis 7 管理 race | 81 顶层、212 事件通过，0 失败/跳过/缺失；临时资源已清理。 |
| go vet / Windows、Linux 构建 | 全部通过。 |
| 当前前端单元 | 137 通过，0 失败/跳过。 |
| 账号浏览器 | 47 组通过，0 页面异常；原非计费管理、授权、运行状态、租期和资源恢复保留。 |
| 日志浏览器 | 35 组通过，0 页面异常；筛选/快照/统计/错误/导出/清理与无计费断言。 |
| 视觉证据 | 账号 48 张、日志 122 张；覆盖桌面、手机及四主题，重点页面另经目视核对。 |

本地浏览器主体回归使用合成 API 与凭据；真实管理服务另用本地独立数据库验证 Cookie 登录、页面加载、账务路由 404 与 CSVv3，不发送真实推理或访问生产。固定 Go 测试证据位于 `.local/management-parity/local-race-20260918t060131-0f095d`，前端证据位于 `.local/management-parity/no-billing-20260918`。

可重复运行当前前端单元验证：

```powershell
node frontend/scripts/verify-management-unit.mjs no-billing-20260918
```

## 发布边界

当前本地真实预览：`http://127.0.0.1:57451/#/login`。登录密钥只保存在 `.local/management-parity/no-billing-preview/admin-key.txt`，未写入报告。空 PostgreSQL/Redis、10 项真实浏览器检查、16 张桌面/手机截图及 API 检查通过，页面/API/静态资源错误、外网及计费请求均为 0；root 独立检查 ready 返回 200。对应报告为该目录的 `api-verification.json` 和 `browser-verification.json`。

四个旧本地预览只停止已核验的 HTTP 进程，8 个原数据库容器及全部旧文件保留，见 `retired-http-stop-report.json`。没有执行旧预览的数据清理脚本。

本轮未部署生产，不修改 Codex 网关运行行为，不覆盖旧发布包。共享 Go 快照包含其他任务所有的网关文件；管理验收不等于其全部功能已通过发布验收，也不宣称所有参考系统能力均已完成。后续管理优化继续按不含计费的范围执行。
