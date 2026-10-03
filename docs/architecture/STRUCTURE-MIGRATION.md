# 目录与构建结构迁移

更新日期：2026-09-27。此文记录已执行的源码布局迁移，以及后续文档、工具归档的约束。此次目录迁移未运行部署，也未移动运行数据、凭据或既有 `.local` 证据。

## 当前布局

```text
main.go                         # 兼容 go run . / go build . 的极简入口
internal/
  server/                       # 原服务实现和同包 Go 测试
    main.go                     # Run(args) -> run(args)
    projectroot_test.go          # 测试显式解析项目根目录
  bps/                          # PowerPoint/BPS 协议适配
migrations/
  embed.go                      # 独立 migrations 包，嵌入 *.sql
  *.sql                         # 已发布迁移内容与文件名保持原位
frontend/                       # 前端源码与既有构建入口
native-transport/               # Rust/native 组件及独立测试
tools/
  project_layout.py             # 新旧源码布局、manifest、测试包和 overlay 共用规则
  README.md                     # 按用途查找现有工具
integration/                    # 合成验收、隔离环境与跨语言测试
deploy/                         # 现有发布执行与服务定义
docs/
  README.md
  architecture/
```

原根目录 397 个 Go 文件整体迁入 `internal/server/`，同包声明由 `main` 改为 `server`。根 `main.go` 调用 `server.Run(os.Args[1:])`；服务启动、flag、storage 子命令和错误处理保持既有入口契约。原同包测试仍能访问未导出的实现，不为目录整理扩大公共 API。

## 已处理的耦合

| 边界 | 原问题 | 迁移后的处理 |
| --- | --- | --- |
| SQL 嵌入 | `database.go` 的 `//go:embed migrations/*.sql` 依赖根目录；不能从 `internal/server` 嵌入 `../../migrations` | `migrations/embed.go` 嵌入 `*.sql`，服务通过导入读取。数据库中已存在的 `schema_migrations.name` 仍使用 `migrations/xxx.sql`，不改变校验和或迁移身份 |
| 启动目录 | `frontend/dist` 与部署单元按项目/发布根目录工作 | 保留根入口，默认路径不跟随 Go 源码目录移动；不使用全局 `chdir` |
| Go 调用 Python | CLI smoke/workload、Loophub 合同测试使用 `tools/...` 相对当前测试包 | 五个测试改用 `projectRoot(t)` 找到 `go.mod` 所在目录，再构造 helper 的绝对路径 |
| 源码指纹 | `ROOT.glob('*.go')` 会静默遗漏 `internal/server` 和 `internal/bps` | `tools/project_layout.py` 明确收集根 Go、`internal/`、`cmd/`、`migrations/`。acceptance、audit、performance、打包与发布核验接入相同规则 |
| 发布源路径白名单 | 门禁要求顶层 `database.go`，拒绝带子目录的 Go 文件 | 同时支持旧 `database.go` 与新 `internal/server/database.go`，要求恰好一个迁移实现。接受明确的源码路径，继续拒绝路径逃逸 |
| 迁移证据 | `migration_runner_sha256` 固定查顶层 database.go；迁移目录新增 embed.go 可能被误当 SQL | 按实际布局选择迁移实现；SQL checksum 集合只包含 `*.sql` |
| 聚焦 Go 测试 | `go test ... .` 迁移后只运行无测试的入口包，可能误报成功 | 通过 `server_package(source_root)` 选择 `./internal/server` 或旧快照的 `.`；全仓测试仍使用 `./...` |
| 测试选择器 | `gateway*.go` 等模式只匹配根相对路径 | 按文件 basename 选择既有用例，保留 required-tests 门禁。旧 `ContextCapacityAndRetirement` 与已重命名的 `ContextElasticRoutesAndRetirement` 有显式对应 |
| Go overlay | 历史模板是 `package main`，注入根目录 | `package_overlay` 为指定快照生成副本，选择 `main/server`，注入实际 server 包；不覆盖模板、不修改冻结快照里的原始源码 |
| 历史混合 overlay | 已固定哈希的 package-main 管理基线不能直接与新 server 文件拼接 | `gateway_freeze_overlay.py` 与 `gateway_freeze_delta.py` 明确拒绝跨布局混合，要求重新冻结完整当前源码；不伪造旧验收结果 |

主要更新入口包括：`tools/acceptance.py`、`verify_gateway_audit.py`、`gateway_source_race.py`、`gateway_acceptance.py`、`gateway_release_check.py`、`upgrade.py`、`ui_release.py`、`deploy/upgrade_remote.py`，以及 `integration/gateway_environment/` 的动态 overlay 运行器。

源码收集器只枚举显式源码目录，不扫描 `.local/`、`data/`、依赖缓存或构建产物。通用发布脚本仍输出既有 `codex-agent` 二进制及 `frontend/dist` 路径。

## 验证记录

- 结构 helper 与相关发布/验收 Python 回归通过，覆盖旧布局、子包清单、路径拒绝、冻结源和打包绑定。
- 三套实际源码 manifest 均已核对包含 `main.go`、`internal/server/main.go`、`internal/server/database.go`、`migrations/embed.go` 和 SQL；均未包含 `.local/` 或 `data/`。
- 网关聚焦选择器识别 400 个测试，管理选择器识别 142 个测试；这只是选择器计数，不冒充测试执行结果。
- `go test ./... -run '^(TestStore|TestLoophub|TestCodexCLIWorkloadFixture)' -count=1` 通过，实际服务测试包为 `codex-agent/internal/server`。
- `integration/gateway_environment/run.py` 的五个 loopback 合成测试全部通过，测试期间源码未改变。报告保存在本次系统临时目录。
- 后续完整 Go/BPS、Linux race 和生产验收由总任务记录；此清单不把目录整理等同于上线。

## 后续归档清单

| 范围 | 目标 | 搬迁前必须更新 |
| --- | --- | --- |
| 活跃文档 | `docs/architecture/`、后续 `docs/guides/` | 根 README、docs 索引、工具中 `HANDOFFS` 和 release manifest 引用 |
| 日期报告 | `docs/history/YYYY-MM/` | 所有 Markdown 相对链接、正文中的可执行路径、JSON 证据路径；不可变 manifest 指向的原件继续保留 |
| 工具 | 后续按 `validation/`、`release/`、`research/` 分组 | `Path(__file__).parents[1]`、同目录 imports、测试 importlib 路径、Go helper 路径、SSH 上传脚本和复跑命令 |
| 历史冻结/一次性发布脚本 | 后续 `tools/history/`，必要时保留旧入口包装 | 绑定的历史 source hash、目录常量、已有文档与测试；不将历史固定快照误标为当前发布流程 |
| 用户数据与证据 | 保持原位 | `data/`、`.local/`、账户导出和凭据均不属于源码清理对象 |

先更新枚举、白名单和测试路径，再整体移动实现与测试，最后生成全新源码指纹和新验收记录。旧快照按其自身布局验证；目录迁移后不要重用旧报告的“当前源码已通过”状态。

## 常用检查

从项目根目录执行：

```powershell
go test ./...
go vet ./...
go build -o dist/codex-agent .
python -m unittest discover -s tools -p 'test_project_layout.py'
python tools/acceptance.py --source-manifest
```

聚焦测试使用 `go test ./internal/server -run '<TestName>'`。需要复查旧不可变快照时，由 `server_package` 按快照目录选择包，不硬编码当前布局。
