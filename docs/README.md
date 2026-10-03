# 文档索引

先从当前入口和架构阅读，按日期命名的报告用于追溯当时的验证结果。历史报告中的路径、模型、部署状态与测试数量不自动代表当前版本。

## 当前入口

| 内容 | 文档 |
| --- | --- |
| 安装、运行与接入 | [项目 README](../README.md) |
| 当前 PowerPoint 网关实现 | [请求、会话和维护边界](architecture/POWERPOINT-GATEWAY.md) |
| 三种推理模式与服务器托管任务 | [CLI、App Server 与 Responses 接入](INFERENCE-MODES.md) |
| 当前 0.4.0 修复与上线结果 | [BPS、三模式、额度和耗时验收](RELIABILITY-MODES-2026-09-27.md) |
| 本轮迁移交付 | [实现、验收与发布记录](POWERPOINT-MIGRATION-2026-09-27.md) |
| PowerPoint HTTP/WS 性能优化 | [实验、改动与性能报告](GATEWAY-BPS-PERFORMANCE-2026-09-27.md) |
| 目录与构建维护 | [架构索引](architecture/README.md)、[结构迁移清单](architecture/STRUCTURE-MIGRATION.md) |
| PowerPoint 接入 Codex 的实测边界 | [2026-09-27 验证报告](POWERPOINT-CODEX-VALIDATION-2026-09-27.md) |
| App Server 与 OpenAI 兼容文本接口的质量 | [两账号推理与流式实测](APP-SERVER-QUALITY-2026-09-27.md) |
| 管理 API 契约 | [API-CONTRACT](API-CONTRACT.md)、[账号管理 API](MANAGEMENT-ACCOUNT-API.md) |
| Sub2API / CPA 账号同步兼容 | [接口与差异说明](ADMIN-COMPATIBILITY.md)、[生产上线记录](ADMIN-COMPATIBILITY-RELEASE-2026-09-27.md) |
| 管理系统性能与功能审查 | [实测对比与验证](SYSTEM-PERFORMANCE-2026-09-27.md)、[86 个接口审查清单](SYSTEM-API-REVIEW-2026-09-27.md) |
| 全接口与千万级数据性能 | [目标、当前结果与复跑入口](PERFORMANCE-SLO-2026-09-29.md)、[SQL 计划与语义验证](PERFORMANCE-SQL-2026-09-29.md) |
| 分散数据与未覆盖接口优化 | [日级精确汇总](PERFORMANCE-DAILY-ROLLUP-2026-09-29.md)、[运维统计与批量重建](PERFORMANCE-OPERATIONS-ROLLUP-2026-09-29.md)、[账号排行](PERFORMANCE-ACCOUNT-TOP-2026-09-29.md)、[补充读取接口](PERFORMANCE-ADDITIONAL-READS-2026-09-29.md) |
| 复杂筛选与批量写入 | [筛选统计](PERFORMANCE-FILTERED-STATS-2026-09-29.md)、[筛选/排序索引](PERFORMANCE-FILTER-INDEXES-2026-09-29.md)、[账号写入与覆盖](PERFORMANCE-ACCOUNT-WRITES-2026-09-29.md) |
| 大规模清理任务 | [固定成员与进度扫描优化](PERFORMANCE-CLEANUP-2026-09-29.md) |
| 系统优化生产上线 | [发布、线上验证与回滚记录](SYSTEM-PRODUCTION-2026-09-27.md) |
| 后端交付与数据库迁移 | [后端交付](BACKEND-HANDOFF.md)、[迁移部署](MIGRATION-DEPLOYMENT.md)、[PostgreSQL 升级](PG-UPGRADE.md) |
| 网关配置 | [网关设置](GATEWAY-SETTINGS.md)、[CLI 测试](CODEX-REAL-CLI-TEST.md) |
| 当前前端与 BPS 配置字段 | [前端交接](../frontend/FRONTEND-HANDOFF.md) |
| 控制台、上海时间与运维监控 | [10 项调整及复验](console-polish-2026-09-27.md)、[账号窗口美元估算](account-window-usd.md) |
| 测试、验收和发布工具 | [工具索引](../tools/README.md) |
| 独立集成验证 | [合成 API 测试](../integration/tests/README.md)、[网关环境验证](../integration/gateway_environment/README.md) |

## 研究与历史记录

`audit-20260924/`、`quality-20260924/`、`quality-20260925/`、`quality-20260926/` 保存专题材料。文档目录根下的日期报告暂时保留原路径，避免破坏交付清单、归档哈希和已有链接。

后续归档到 `history/YYYY-MM/` 时，应同时登记旧路径、修改仍在使用的链接，并保留不可变验收材料的原始路径和摘要；不能只按文件名批量搬走。执行顺序见[结构迁移清单](architecture/STRUCTURE-MIGRATION.md)。

已完成的少量根文档归档、原文保留位置和旧根编译产物处理见[历史材料索引](history/README.md)。
