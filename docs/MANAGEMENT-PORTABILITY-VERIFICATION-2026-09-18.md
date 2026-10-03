# 租期适配与成本备份验收

2026-09-18。本轮完成真实功能、导出隐私修复、联合验证和独立固定打包，属于持续目标的实质进展。**尚未上线，完整对齐目标仍未完成。** 旧 UI、历史候选与原本机预览不被覆盖。

## 实际完成

- 原始 Sub2API 租期适配：完整 openai/oauth 外层 expires_at 与 auto_pause_on_expired 由显式选项读取，Unix秒/RFC3339/null/0有准确语义，保持凭据内部到期独立；缺省自动暂停按参考真实默认值并展示来源。三输入形态、多文件/行号、逐项错误均保留。
- 已有账号租期：同凭据账号默认仍跳过，用户可明确“保留账号，仅准备租期确认”。账号导入不会自动应用租期，后续使用真实稳定ID和双版本回执，过期停用/覆盖分别确认，未知结果不重复执行。
- 账号成本备份：自家凭据 bundle 可选携带账号绑定的全部旧成本规则历史，以及当前启用有序策略的显式账号覆盖倍率，不携带全局渠道/组策略和默认值。
- 成本独立恢复：文件索引绑定实际导入的新ID、预览真实配置、复制历史但重建规则ID；拒绝覆盖同模型本地成本，账号变化/倍率变化不能被旧回执覆盖。倍率更新需要额外确认，否则费率可独立完成并明确 partial；审计失败、部分完成和未知写结果均有真实状态。
- 导出保护：继承网关新增的 sent/returned model、路径、usage来源及尝试摘要字符串补齐秘密样式过滤，导出不改变原记录或原尝试数组。只改管理导出，不修改网关采集。

契约：[外部租期](MANAGEMENT-EXTERNAL-LEASE-IMPORT.md)、[原始导入 UI](EXTERNAL-LEASE-IMPORT-UI-2026-09-18.md)、[账号成本备份](MANAGEMENT-ACCOUNT-COST-BACKUP.md)。所有新增写入限于管理状态，原账号访问权限、指纹和流量策略不变。

## 固定验证

| 检查 | 结果 |
| --- | --- |
| Linux 管理竞态，实际隔离 PG16/Redis7 | 115 顶层、256 含子测试通过，0 失败/跳过/缺失 |
| 同归档完整 Go | 430 顶层通过、93 环境跳过、0 失败；1114 通过事件、104 跳过事件 |
| go vet、Windows/Linux 构建 | 全通过，212个文件前后哈希相同 |
| 前端统一单测 | 169 通过，0 失败/跳过 |
| 账号含成本备份及每日统计旧流程 | 58 组通过，141 张截图 |
| 独立租期恢复 / 原始外部导入 | 3 / 5 组通过，同一个最终前端 |
| 使用记录完整回归 | 51 组通过，256 张四主题截图，0 JS 错误 |
| 原UI保护 | 最终54资源、48基线原字节不变，共用原Vue与登录会话 |

整体 117 组浏览器回归。多数行为用合成API；真实 file/PG 测试单独覆盖错误输入、来源、双CAS、同目标冲突、并发claim、TTL/管理员隔离、审计回滚、不复制所有权、成本历史与条件恢复。未知不伪零、不伪成功。没有生产账号/数据库或上游推理。

首轮 `f10839c` 已通过114管理race和全库；运行期间共享 `gateway_attempt_hooks.go` 被它的源码owner继续修改，报告如实记录共享树已变。为不追赶网关在途实现，最终在**原f108固定源码**上只叠加两份管理隐私文件，形成72c7362b，再完整重跑115race和全库。二者差异已在local-verification.json核对；没有回滚共享树或采纳未验的新网关改动。

## 固定交付物

- Go 集合 `72c7362bc12a4c1ee58b02b723de99bb715d7b06d1ceeed6c525d973a72bb69f`，212文件。
- [源码归档](../.local/management-parity/local-race-20260918t044112-156756/source.tar.gz)、[Linux race](../.local/management-parity/local-race-20260918t044112-156756/report.json)、[同源全库/构建](../.local/management-parity/local-race-20260918t044112-156756/local-verification.json)。
- [Linux 程序](../.local/management-parity/local-race-20260918t044112-156756/codex-agent-linux-amd64)，SHA-256 `1af84581543233d2656e5482f573d333a2ad4fa633a1b4e94674894d4e0cc96b`。
- [54文件前端包](../.local/management-parity/portability-20260918/frontend-93bb82ff3f34.zip)，SHA-256 `07623a93036c39e726a95492b8bd9b31d6237d0b43ef9325301e47800b22ca7c`。
- [前端源码](../.local/management-parity/portability-20260918/frontend-source-93bb82ff3f34.tar.gz)，清单 `93bb82ff3f34b04fe212c23f1fc76e2119e104c015d0dfd9d90dc11de1c6218e`；Accounts-DNUcEyc_、Logs-E0IrKgCL。
- [机器总表](MANAGEMENT-PORTABILITY-VERIFICATION-2026-09-18.json)与race目录management-evidence固定副本，包含成本、外部导入、租期及daily报告，未用新截图代替旧构建证明。

重跑前端单测：`node frontend/scripts/verify-management-unit.mjs portability-20260918`。

## 本机真实预览

[最新独立空库预览](http://127.0.0.1:52556/#/login)使用上述固定程序/页面副本，登录密钥仅在本机 [admin-key.txt](../.local/management-parity/portability-preview/admin-key.txt)。旧两个预览和生产均未改动。

真实 Cookie 登录/退出、账号/日志、外部租期默认关闭、成本导出默认关闭及价格目录共9检查通过，1440/390共16截图，零页面/静态/API/外部请求错误。仅打开导入/导出，不提交账号、不下载凭据、不发推理；[真实浏览器报告](../.local/management-parity/portability-preview/browser-verification.json)独立于合成套件。停止脚本 `.local/management-parity/portability-preview/stop.ps1` 只删除自有进程/容器，停止会清空该预览数据。

## 未完成项

原始 Sub2API 账号 rate_multiplier 的直接适配仍是明确差距，本轮自家成本段不是它的替代。该字段应经显式来源与管理模式映射，不能误作网关权重或无价格源的实际扣费。账号多项 extra 的适用性继续按矩阵逐项核对，不能一句“扩展字段”全部排除。

新的被动模型/路径/阶段/attempts字段已进入共享记录，但管理查询、维度分布、详情失败链及完整 CSV 展示还需依据最后真实采集合同接入；继承JSON字段或通过脱敏单测不等于已全部完成。仅账户级网关策略、冷却恢复/Spark/Agent认证等行为边界仍保持原约束，未添加无效按钮。

详 [剩余审查](MANAGEMENT-REMAINING-AUDIT-2026-09-18.md) 与账号/使用记录矩阵。网关/native改变由其他任务拥有，管理测试不替代其最终验收；生产003增量迁移与发布门禁不变，本轮没有切换生产服务。
