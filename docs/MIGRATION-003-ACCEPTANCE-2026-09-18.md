# 003 管理清理目标表：独立迁移验收与发布边界

本验收只在 `codex-agent-integration-postgres` 的 `codex_agent_integration` 数据库随机 schema 中执行。测试用户为 `codex_agent_test`，临时 schema / NOLOGIN 角色均使用 `codex_agent_integration_` 前缀；生产 PostgreSQL、Redis、服务、配置与 SQL 文件不在修改范围。**独立测试迁移不等于生产已迁移或部署完成。**

## 变更与真实迁移器语义

`003_management_cleanup.sql` 只新增 `management_cleanup_targets` 表、复合主键 `(preview_id, log_id)` 和 `expires_at IS NOT NULL` 的过期索引；不删除或修改现有账号、凭据、日志与统计表。它刻意不对 request_logs 建立外键，因为冻结预览目标后，其他清理任务可能先删除日志；目标集不应因为后来的日志变化而扩展。

本次 SQL SHA-256：`31ca0e1e58bb846c9691bd505718a1c13ef3aba03f9abf157034317e85cde56e`。实际 `database.go` SHA-256：`0237213d6c9405a117a6bd4ae17076b89d3d1f8565c430c414efa1ba6465d42a`。

`Database.Migrate` 使用 schema 名派生的 advisory lock，按文件名排序逐项执行。**每一个 SQL 文件单独开启事务**，SQL 成功后在同一事务写入 name/checksum，随后提交。已有 checksum 相同则跳过，不同则失败；这不是“000–003 全部文件放在一个事务”。因此 003 失败时，之前成功的 000–002 不会回滚。

`003` 本身不含 `IF NOT EXISTS`；幂等性来自真实迁移器的 ledger/checksum，而不是重复手动执行 SQL。本验收直接调用项目的实际 `Database.Migrate`，没有用 Python 另写一份迁移逻辑。

## 一命令复跑

已有独立 PG SSH tunnel `127.0.0.1:25432` 时：

```powershell
python integration/gateway_environment/migration_003.py
```

需要保留禁用的合成 schema，供精确旧 binary 兼容验证使用：

```powershell
python integration/gateway_environment/migration_003.py --keep-schema
```

测试结果与 SQL 副本保存在 `.local/migration-003/<UTC>/`，不会覆盖旧报告。保留模式另产生私密 `fixture.json`，含合成环境 DSN/密钥，不能发布、打印或并入发布证据正文。默认模式在结束后删除随机 schema 与临时角色；保留模式仍清理临时角色，仅留下禁用合成账号所在 schema，由兼容 smoke 完成后清理。

使用已通过 Linux 测试的不可变源码快照 `bee29a4dd87fd9bb278ff2a7548d42b8dded90104d7f803715dc4cee160bd462`，路径 `.local/gateway-environment-linux-race/20260917T200319.786588Z/source`。入口核验快照文件哈希，并要求其 `database.go` 与共享当前源码一致。最新 000–003 SQL 先复制冻结，再通过 Go overlay 映射给 embed。共享源码在途变更不会被混入测试，也不会改写快照。

## 已验证和待绑定的边界

| 验收点 | 具体方法 |
| --- | --- |
| 003 事务失败无半迁移 | 创建同名索引占位关系，让实际迁移器在 CREATE TABLE 成功后、CREATE INDEX 时收到 SQLSTATE 42P07；验证目标表不存在、ledger 只有三个旧文件 |
| 幂等与串行化 | 解决合成冲突后运行实际迁移器，再并发四次 Migrate；比较 ledger checksum 与 applied_at 完全不变 |
| checksum 防漂移 | 仅篡改随机 schema 的 003 ledger checksum，确认实际迁移器拒绝；恢复后再次通过。生产不能用改 ledger 方式绕过检查 |
| 数据保留 | 000–002 schema 中先写合成账号/加密凭据/1 条 usage event 与日志，003 前后对比实际账号列表、日志和累计 17 tokens |
| 密文可解 | 使用合成 AES 密钥，实际 Store.Get 验证 access/refresh 凭据在迁移前后及新进程中仍可解密；凭据不写报告 |
| 表与索引 | 验证 owner 为测试迁移用户、过期索引条件正确、无额外 FK、复合主键拒绝重复、允许两个 preview 指向同一已不存在日志 ID |
| 运行权限 | 先只对旧表授予运行角色 CRUD，证明新表没有权限；补显式 GRANT 后实际执行新表 SELECT/INSERT/UPDATE/DELETE |
| 只读权限 | 临时 NOLOGIN 只读角色经 SET ROLE 执行 SELECT，INSERT/CREATE TABLE/DROP INDEX 必须 SQLSTATE 42501；运行角色也不能执行 DDL |
| 重启边界 | 关闭重建 PG pool，另启动真实编译测试子进程重新连库、Migrate 幂等重查、读取账号/日志/统计与两个清理目标；这不是 PG 容器重启 |
| 精确旧 binary 回退 | 由独立 rollback smoke 绑定现网旧 binary SHA，验证其启动、ready、管理读写、密文解密与重启；不能用源码快照 helper 冒充现网 binary |

首轮 Go 实际测试通过，耗时 55.40 秒。原始证据 `.local/migration-003/20260917T201115.006178Z/go-test.jsonl`；该轮 Python 报告解析器未处理 Go test2json 长输出分块，随后已修复。不能把该解析错误解释为 PostgreSQL 验证失败，也不能把缺少的结构化报告当成发布 gate 证据。最终结构化结果见后续执行记录。

最终结构化验收 **PASS**：`.local/migration-003/20260917T201629.573381Z/report.json`，8 个发布检查与 13 个详细检查均通过，000–003 SHA 完全匹配且执行前后 SQL 未变化。真实复核独立 PG 后，临时 NOLOGIN 角色数为 0；为后续精确旧 binary 验证保留 1 个禁用合成 schema。生产迁移执行标志明确为 `false`，隔离迁移执行为 `true`。该 schema 的清理结果与精确旧 binary 回退证据另行补充。

后续精确旧 binary 回退验证当前 **blocked**，证据 `.local/migration-003-rollback/network-blocked.json`。已完成旧 binary 只读复制与 SHA `a9bf68d1899ec1d58381c788b84cc63123c88cb8196d1acb0b4faafa1b65831d` 核验；首轮在启动前读取 SQL 多行 JSON 的工具解析失败，已本地修复，但网络恢复受阻，修复版未远端执行。旧 binary 隔离进程未启动，不能宣称 start/read/write/decrypt/restart 通过。随机 schema 与私密 fixture 按 root 最新要求保留，供后续准确旧 binary / 候选 / 旧前端兼容验证使用；本次不清理。

报告合同为 `schema=codex-agent-pg-additive-migration-v1`、`scope=migration-focused`，绑定真实源快照、实际迁移器、全部 SQL SHA 和实际检查结果。**它不假设当前共享源码已经冻结，不提供未经测试的候选 binary SHA，也不替代精确旧 binary 回退证据。**

## 发布清单与失败处理

旧 `deploy/upgrade_remote.py` 的 v1 路径只认可 000–002 且要求 schema 不变，003 发布不能绕过这个 gate。新增显式 v2 additive 路径由部署脚本 owner 实现，必须单独要求本迁移证据与精确旧 binary 回退证据；v1 的原限制保持有效。

发布前应完成：

1. 联合冻结候选前后端及 API，绑定候选 binary/source/全部 SQL SHA，确认 000–002 checksum 没有变化。
2. 只读核对实际数据库 owner、迁移角色、运行角色、schema、连接地址与密钥 ID；候选与旧 binary 使用相同真实数据库和凭据加密密钥。
3. 在独立环境验证准确旧 binary 能对 003 后 schema 启动、ready、读写和解密合成数据，发布报告引用独立 rollback 证据 SHA。
4. 预留短暂停写/排空窗口，保护同一套真实凭据不被新旧进程同时刷新；正常回退沿用最新数据库，不恢复旧 dump。
5. 显式执行真实迁移器，再核验四条 ledger checksum、目标表 owner/索引和运行权限。若没有覆盖迁移 owner 的 default privileges，需为真实运行角色显式授予新表 `SELECT, INSERT, UPDATE, DELETE`；索引不需要独立 GRANT。
6. 候选启动、ready、网页认证、管理读写、日志统计及重启恢复完成后，才能分别标记“生产已迁移”“候选已部署”。

| 失败时点 | 保留状态与处理 |
| --- | --- |
| checksum/前置校验失败 | 不迁移、不切换，旧进程维持可用；核对冻结 artifact |
| 003 SQL / ledger 事务提交前失败 | 003 表/索引/记录整体回滚，000–002 保留；旧 binary 可继续使用原 schema |
| 003 已成功但新表授权失败 | 暂不启用新清理功能，补齐明确的运行权限；不删除新表或篡改 ledger |
| 003 已成功但候选启动/健康失败 | 按已验证的 rollback 路径启动准确旧 binary，继续使用最新 DB 和加密密钥，保留 003 及切换后产生的数据 |
| 精确旧 binary 回退验证未通过 | 不发布；不能以表是“新增”作为未经验证的兼容结论 |

本文件只描述发布准备与验收，不执行生产迁移命令。独立测试没有启动模型推理、刷新真实凭据或发起真实并发压力。
