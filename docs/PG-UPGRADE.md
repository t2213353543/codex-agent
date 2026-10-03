# 已迁移实例的程序升级

本文用于现有 codex-agent PostgreSQL/Redis 服务的无 schema 变更升级。首次 JSON→PG 迁移仍使用 MIGRATION-DEPLOYMENT.md；已迁移实例使用独立 `tools/upgrade.py`，不会重复导入旧数据。

1. 固定源码并运行 Go 测试、vet、Linux 构建、前端单元/浏览器测试和实际隔离 PG/Redis 验证。保存 `status: pass`、`source_sha256` 及各项证据的 `upgrade-acceptance.json`。源码哈希使用 `tools/acceptance.py` 的 `source_manifest()`；跳过的真实账号/CLI选跑项必须明确记录，不能写成已执行。
2. 执行 `python tools/upgrade.py prepare --acceptance REPORT`，生成 `.local/upgrades/<hash>/upgrade-manifest.json` 与不可变后端/前端包。
3. 对同一清单依次执行 `stage`、`preflight`、`activate`，参数均为 `--release MANIFEST`。现有用户授权的项目 SSH 传输例外沿用 MIGRATION-DEPLOYMENT.md，不改变共享生产技能。
4. `status` 查看当前运行身份；实际部署后另验管理 Bearer、Cookie 会话、设置、诊断、浏览器页面及未授权拒绝。验收不得输出真实密钥、账号令牌、代理密码或数据库连接串。
5. 需要回退时对同一清单执行 `rollback`。脚本恢复备份的完整旧 systemd unit，使用保留的旧程序和页面，继续使用当前 PG 数据库；不会回灌历史数据库而丢失新凭据或请求记录。

远端脚本严格限定 `/opt/codex-agent/releases/<hash>`、`codex-agent.service` 及本项目独立 PG/Redis 地址。它核验当前 migration 名称/checksum、所有静态资源哈希、实际 PID 程序哈希及 DynamicUser 读取权限；任何 drop-in 或不认识的启动参数都会拒绝切换。`activate` 持部署锁，先在候选目录 `.upgrade-private`（0700）保存私密配置和校验过的 PG dump，再停止排空、替换 ExecStart、启动及检查 live/ready/根页面/入口资源。启动失败自动恢复旧服务。

验收必需布尔项：`go_tests`、`go_vet`、`backend_build`、`frontend_unit`、`frontend_build`、`frontend_browser`、`storage`、`schema_unchanged`。这些是本次无 schema 升级的检查项，不表示重复执行了首次迁移性能和数据导入验收。报告应保存具体命令、结果、证据哈希及适用范围。
