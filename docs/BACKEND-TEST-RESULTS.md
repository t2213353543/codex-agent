# 后端最终交付与验证状态 · 2026-09-17

实现已交付；**最新源码的完整 Linux PG/Redis/race 验收仍受测试环境阻断，不具备生产发布结论**。本后端任务未迁移或部署生产。

## 最终文件清单

| 分类 | 文件 |
| --- | --- |
| 账号与凭据 | accounts.go、accounts_import.go、accounts_pg.go |
| 管理与设置 | admin.go、admin_extra.go、store.go、store_pg.go、sessions.go |
| 数据库与迁移 | database.go、migration_cli.go、migrations/000_core.sql、migrations/001_accounts.sql、migrations/002_metrics.sql |
| 日志与统计 | stats.go、stats_pg.go、stats_legacy.go |
| 生命周期与依赖 | main.go、go.mod、go.sum |
| 凭据入口补丁 | gateway.go：仅 ensureReady 的 PG 账号级刷新锁和稳定 ID，未改变转发协议或调度行为 |
| 测试 | accounts_import_test.go、accounts_pg_test.go、accounts_review_test.go、accounts_shutdown_test.go、admin_extra_test.go、admin_pg_test.go、stats_pg_test.go、storage_runtime_test.go；web_test.go 的管理接口兼容断言 |
| 文档 | docs/API-CONTRACT.md、docs/BACKEND-HANDOFF.md、本文件 |

没有修改 frontend、tools、deploy。接口契约和两次兼容扩展已经通知前端与集成任务；前端已确认接入 ID/version、settings_version、Keys.version、会话、导入预览、游标及轮换。

## 验证证据

| 验证 | 结果 | 适用边界 |
| --- | --- | --- |
| 最终 `go test ./... -count=1` | PASS，169 个测试项/子用例，包耗时 3.076 秒 | 未注入不可用数据库；20 个 PG/Redis/POSIX 专项明确 Skip，详见 `.local/backend-final-unit.jsonl` |
| 最终 `go vet ./...` | PASS | 当前 Go 源码 |
| 最终 Windows `go build` | PASS | `.local/codex-agent-candidate.exe` |
| 最终 Linux 测试交叉编译 | PASS | `.local/codex-agent-linux.test`；交叉编译不等于运行通过 |
| 独立 PG 账号测试 | 5 组整轮 PASS，201.277 秒 | 加密/元数据隔离、版本/FK、账号刷新串行、CAS/重启、导入冲突；最后入口锁增量在此之后 |
| 独立 PG 日志/统计测试 | 相关组实际 PASS；并发/故障恢复复跑 PASS，87.082 秒 | 事件去重、事务回滚、重启、游标、重建、清理、历史基线；最后改名导出增量待完整复测 |
| 同机隔离黑盒迁移/API | 22 项 PASS | `.local/integration/remote-migration/20260917T094252/report.json`；该报告是较早源码快照，不冒充最终版本证据 |
| 最终完整 Linux/race | BLOCKED | SSH `WinError 10051`；本地测试隧道 25432 关闭；本机 CGO 禁用 |
| 本机独立容器替代路径 | BLOCKED | Docker Desktop 启动失败：`sailor-ingest.sock: The file cannot be accessed by the system.`；未重置或删除 Docker 数据 |

最终后端源码 SHA256（Go、go.mod/go.sum、SQL 文件哈希映射）：`6e14c27efe65debeac49ff2cb8ba4e45eef303fc3925a6e692b94ad7dfbfc467`。文件级映射保存在 `.local/backend-final-manifest.json`。

最终 Linux 测试二进制 SHA256：`14a4c9b8afd281a083a1e322488be8b3787de2f065e0c8f184683bbc8f92f9b9`。

## 集成任务继续事项

恢复独立测试环境后，在项目根目录运行既有统一 Linux runner：

```powershell
python tools/run_linux_checks.py
```

该命令冻结当前源码并在独立 Linux 容器运行完整 PG/Redis/race；普通 helper 不再产生无意义 Skip。重点核实以下新增测试已经实际 PASS：

- `TestPGShutdownPersistsRefreshAfterClientDisconnectAndSIGTERM`：真实子进程、真实 HTTP 请求、OAuth 阻塞、客户端断开、SIGTERM、停止接收、回传新 AT/RT、退出后 PG 新凭据及请求事件均存在。
- `TestPGEnsureReadyRefreshesDifferentAccountsConcurrently`：通过实际凭据入口验证不同账号可以同时进入 OAuth，同账号由 Store 串行，改名后仍按稳定 ID 刷新。
- `TestPGAccountRefreshFailureSurvivesRestartAndDoesNotMarkReplacement`：失败状态重启保留，较旧刷新失败不得污染已替换的新凭据。
- `TestPGLegacyImportExportRoundTrip` 及统计导出测试：改名后回退文件使用当前名称连接历史用量，导出最新 AT/RT，保留名称快照和同秒日志顺序。
- `TestPGAdminRotationFailureAndLostResponseReplay`：失败回滚、旧 key 限定重放、已被后续轮换取代的结果返回冲突。

随后按统一集成入口对**同一冻结源码**重跑 API/迁移验收；较早 22 项 PASS 不能直接作为新源码的发布放行依据。用户请求中不属于本后端的真实浏览器/部署验收仍由相应任务负责。

账号统计 SQL 已按实际 EXPLAIN 消除 `account_ref OR account_name` 造成的逐账号日期桶扫描，改为固定次数的 ID 批量解析、索引筛选与 GROUP 聚合。较早报告的 500 账号约 2029 ms 是修复前数据；修复后性能须由集成复测，未声称已测得改善数值。

生产切换必须先停止旧写入/刷新、执行 dry-run 和迁移数量/关系验证。新版本刷新过凭据后，回退必须停新服务并 `export-legacy --target 全新目录`，不能恢复旧 token 快照。命令、密钥配置、TTL、保留和回退细节见 [BACKEND-HANDOFF.md](BACKEND-HANDOFF.md)。
