# 有序采购、租期和日统计增量验收

2026-09-18。本轮完成四组已审查管理功能的具体实现和验证，属于目标实质进展；**候选未上线，完整 Sub2API 对齐目标仍未完成**。目录名 completion 只标识本次迭代，不代表整个目标已完成。旧蓝色 UI、前轮固定包和独立预览服务未被覆盖。

## 本轮实现

- 有序多目标采购策略：规则顺序优先、稳定账号或显式 KEY 计价组 OR 匹配；规则内精确模型优先、通配按配置顺序，含服务层与采购 Token 阶梯。整目录 CAS、不可变历史、显式恢复旧单目标模式，未匹配模型继续下一规则，已匹配但用量未知不回退改价。
- 显式跟随售价基数：只有指定的启用渠道才启用，无配置不自动跟随。使用实际所选价格的精确基数，包含长上下文/分时，排除 KEY/组售价倍率及高峰；不从零售价除回基数，不放大已舍入金额。采购最终按明确渠道与账号倍率计算，保持真零/未知区别。
- 自家账号 bundle 租期往返：凭据导出可单独包含租期配置；账号恢复后取得稳定 ID 回执，租期独立预览、确认、双版本 CAS。过期停用/覆盖旧租期分别确认，不复制旧实例的恢复所有权；部分失败和未知结果可读取回执，不自动重复执行。
- 账号日统计：明确 IANA 和每日步长、完整留存日桶；活跃日/自然日分母、精确费用日均、并列峰值日、今日摘要和请求/Token/三费用趋势，未知留缺口。日志入口保留账号/区间/时区，由日志页独立建立新快照，不伪称跨页固定同一集合。
- UI 修复：空售价目录显示正确占位；租期弹窗使用旧 runtime 支持的响应式绑定；新日期/租期/策略交互继续旧壳，无整站重新美化。

契约：[有序采购](MANAGEMENT-COST-POLICY-API.md)、[租期备份](MANAGEMENT-LEASE-BACKUP-API.md)、[账号日统计](ACCOUNT-DAILY-STATS-2026-09-18.md)、[导出字典](MANAGEMENT-PARITY-USAGE-API.md)。列表、详情、统计和导出共享冻结策略；数据库端仍做精确 numeric 聚合，三个费用域共用只读事务，不拉全量请求到前端求和。

## 同候选验证

| 验证 | 结果 |
| --- | --- |
| Linux 管理竞态，真实本机隔离 PostgreSQL16 / Redis7 | 105 顶层、220 测试事件通过，0 失败/跳过/缺失 |
| 同归档完整 Go | 404 顶层通过、87 环境跳过、0 失败；1054 通过事件、98 跳过事件 |
| go vet / Windows / Linux 构建 | 全通过，202 文件前后哈希一致 |
| 统一前端单测 | 155 通过，0 失败/跳过 |
| 账号原流程及日统计 | 52 组通过，0 JS 错误，117 截图 |
| 独立租期导入导出 | 3 组通过，绑定同一最终前端清单 |
| 使用记录、采购策略及全部旧流程 | 51 组通过，0 JS 错误，256 张四主题截图及窄屏边界 |
| 旧 UI 保留 | 最终54资源，48个基线资源原字节，共用原Vue/登录运行时 |

真实数据库用例覆盖两种存储算法对照、手算价格、同规则服务层排序、旧模式切换、审计回滚/并发 CAS、snapshot/详情/导出、租期并发 claim/过期确认/未知写结果。价格缺值与真实零分别验证；价格阶梯和跟随使用精确金额而非 UI 估算。日统计图表检查多个指标 canvas 非空、DST23/25小时日、并列日期、部分费用与没有活跃日。

测试没有使用真实账号、生产数据库或上游推理。临时测试容器、网络和凭据已清理。页面行为数据为合成 API，不能把它当生产请求或者真实付费账单。

## 固定交付

- Go 集合 SHA-256：`cff3981d7efc24c7652faee5a7c725568c57039de9606f9982a46fd57b942f59`。
- [源码归档](../.local/management-parity/local-race-20260918t040142-bf2a74/source.tar.gz)，202 个文件；[Linux race](../.local/management-parity/local-race-20260918t040142-bf2a74/report.json)及 [全库/构建报告](../.local/management-parity/local-race-20260918t040142-bf2a74/local-verification.json)。
- [Linux 程序](../.local/management-parity/local-race-20260918t040142-bf2a74/codex-agent-linux-amd64)，SHA-256 `aeefb478d704127f4e213e70df67b8d1a99416c82366328ed43747e76e185066`。
- [54资源前端包](../.local/management-parity/completion-20260918/frontend-ebd3d90fbeae.zip)，SHA-256 `1e01450c02ef8f71f4e6e3e3a44a9a5496bc2d2d0992989b8e1d94aa67add620`。
- [前端源码](../.local/management-parity/completion-20260918/frontend-source-ebd3d90fbeae.tar.gz)，构建清单 SHA-256 `ebd3d90fbeae6debe7ba78ea3f80f20575982646b2d5daa4c192ddd6a7b19ebb`；Accounts-CuBOfqxs、Logs-E0IrKgCL。
- [机器总表](MANAGEMENT-COMPLETION-VERIFICATION-2026-09-18.json)及 race 目录 management-evidence 固定副本，涵盖新日统计和租期报告，不只引用旧套件。

前端重跑：`node frontend/scripts/verify-management-unit.mjs completion-20260918`。

## 独立本机预览

[最新预览](http://127.0.0.1:61858/#/login)使用本轮固定 Windows 程序和前端副本，独立空 PostgreSQL/Redis，未改原 commercial 预览或生产。登录只需本机 [admin-key.txt](../.local/management-parity/completion-preview/admin-key.txt)，不提供数据库或加密主密钥。

实际 Cookie 登录/退出、账号/日志及三种计价目录空态均通过，含已修空目录占位；1440/390 共12截图，页面、静态、API和外部请求错误为零。证据 [真实API验证](../.local/management-parity/completion-preview/api-verification.json) 和 [真实浏览器验证](../.local/management-parity/completion-preview/browser-verification.json) 单独保存，不替换固定候选报告。没有导入账号或发送推理。

服务刻意留作用户试用；停止脚本为 `.local/management-parity/completion-preview/stop.ps1`，只停止自身进程和两个容器，停止会清除预览数据。重启 `start.ps1` 后采用新端口和新密钥，以 state.json 为准。上线仍未执行。

## 仍需继续

原始 Sub2API 外层账号 `expires_at/auto_pause_on_expired` 仍由普通导入返回未映射警告；本轮只完善本系统 bundle 的租期段，跨格式租期适配是下一项真实可执行工作。不能把外层租期误当 OAuth token expires_at，也不能自动应用到同名账号。详 [剩余审查增量](MANAGEMENT-REMAINING-AUDIT-2026-09-18.md)。

账号关联采购规则/倍率备份及已核查参考额外字段仍需逐项映射，不能用“有 JSON 导出”替代完整往返。每账号网关指纹/映射、恢复冷却/调度、Spark/私钥认证仍受保持网关行为的约束，未用无效开关假装实现。完整发送/返回模型、端点链、阶段耗时、重试链/截断和多模态计量来源仍需源码 owner 交接真实观测。

本任务未编辑网关。固定 Go 归档继承其他任务的网关/native 更新，机器总表单列，管理测试不替代它们的专项或生产门禁。本轮无新增迁移；旧生产升级仍须已存在的003增量迁移及协同发布。本任务不并发切生产服务。
