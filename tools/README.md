# 工具索引

现有脚本暂时保留文件路径，使用索引区分用途。源码目录规则统一在 `project_layout.py`，新工具不要自行假设全部 Go 文件都在项目根目录。

| 用途 | 主要入口 |
| --- | --- |
| 本地全仓验收 | `acceptance.py`、`verify_gateway_audit.py` |
| 控制台 10 项调整与浏览器复验 | `verify_console_polish.py`（前端、Go、导航/记录/账号/运维） |
| 当前 Go PowerPoint 网关 + 真实 Codex | `bps_validate.py`，调用 `TestBPSLiveCodexIntegration` |
| 当前 PowerPoint 并发与Redis验收 | `bps_race.py --with-redis` |
| 三种模式合成 Linux race | `bps_race.py --with-redis --include-official`，增加官方运行时与模式隔离测试，明确排除 Live / Fixture 主入口 |
| 当前 PowerPoint HTTP/WS 性能对照 | `gateway_bps_performance.py`，冻结源码、校验完整输出、交替采样首字与完成耗时 |
| 管理接口性能对照 | `system_performance.py --baseline .local/system-perf-20260927/baseline-src --benchtime 30x --count 3`，冻结两版，在隔离合成 PostgreSQL 上验证与测量 |
| 千万级真实 HTTP 性能与 SLO | `performance_slo.py --rows 10000000`，隔离合成库、源码冻结、结果语义、首次请求与 1/4 并发 P50/P95/P99；失败保留，`--reuse <report.json>` 复用 fixture |
| 接口 SLO 与千万级日志 | `performance_slo.py --rows 10000000 --samples 20`，冻结当前源码、随机隔离 schema、真实 loopback HTTP、P50/P95/P99 与失败语义；`--reuse <report.json>` 复用 fixture，`--test-run <正则>` 运行真实 PG 回归 |
| 管理候选 Linux race | `system_performance_race.py --source <性能候选/source>`，绑定同一源码快照，执行管理 PG/Redis 回归 |
| 管理前端 CPU 与加载对照 | `node frontend/scripts/benchmark-system.mjs --browser`，合成数据输出等价、独立构建与图表按需加载验证 |
| 当前 PowerPoint 版本发布/回滚 | `powerpoint_release.py`，固定证据与产物，SQL不变 |
| 当前 PowerPoint 发布证据汇总 | `bps_acceptance.py`，校验live/race/Redis/PG/浏览器与源码差异 |
| 官方用量窗口 USD 估算价表 | `sync_account_window_prices.py --source ../sub2api/current-source`，固定 Sub2API 目录及 fallback 源码 hash |
| Sub2API 计价可执行对照 | `verify_account_window_parity.py --source ../sub2api/current-source`，执行上游原始计价函数并逐项比较当前网关 |
| 窗口费用与耗时的真实隔离 PG | `validate_usage_storage.py`，冻结源码，临时 SSH 端口连接隔离容器，在随机 schema 测试费用、采样、剪枝、重建及导出 |
| 源码布局与测试包选择 | `project_layout.py`、`test_project_layout.py` |
| 官方 CLI 的隔离对照 | `codex_cli_smoke.py`、`codex_cli_workload.py`、`quality_official_cli_probe.py` |
| App Server 推理与 OpenAI 兼容文本流对照 | `quality_app_server_probe.py --route bridge --effort max`；[题库与验证范围](../integration/app_server_quality/README.md) |
| 网关合成验收与 release gate | `gateway_acceptance.py`、`gateway_release_check.py`、`gateway_release_acceptance.py` |
| 冻结源码与 race | `gateway_source_race.py`、`gateway_local_race.py`、`management_local_race.py` |
| 产物准备和发布 | `deploy.py`、`upgrade.py`、`ui_release.py`、`full_release.py` |
| 原生传输构建与溯源 | `native_transport_local.py`、`native_transport_compare.py`、`verify_native_provenance.py` |
| 质量调查 | `quality_*`、`mother_*`：具体环境与输入按各脚本参数说明 |
| 历史候选封存 | `freeze_*`、`seal_*`、`closeout_*`：部分绑定日期与不可变源 hash |

测试入口使用 `python -m unittest discover -s tools -p 'test_<name>.py'`。`integration/` 下包含另行运行的合成 HTTP、数据库和 overlay 验证。

`source_manifest` 必须覆盖 `internal/`、`cmd/`、`migrations/embed.go`。聚焦 Go 测试与 `go test -c` 使用 `server_package(snapshot)`，兼容旧冻结源码和当前 `internal/server` 布局。历史 `gateway_freeze_overlay.py` 不接受把当前 server 包拼接到旧 package-main 基线；应重新冻结完整当前源码。

目录实际搬迁前先处理 Python 同目录导入、`__file__` 根路径、Go helper 引用和发布上传列表，详见[结构迁移清单](../docs/architecture/STRUCTURE-MIGRATION.md)。

## 当前 PowerPoint 验收

从项目根目录运行：

```powershell
go test ./internal/bps ./internal/server
python tools/bps_validate.py
```

`bps_validate.py` 编译**当前** `internal/server` 的 Linux amd64 Go 测试程序，经现有 `integration_env.connect` 连接授权测试主机，校验上传产物 SHA256。远端只读取当前两条启用的测试账号，将 AT 与 account_id 放入权限 0600 的私有临时文件，不导出 RT，不把凭据交给 CLI，也不部署网关。测试结束清理该轮私有目录；脱敏日志和构建摘要写入 `.local/powerpoint-migration/<UTC时间>/`。

默认验收包含文件 17+26 与随机标记写回、恢复会话、实际加密压缩后的记忆恢复、apply_patch、原生网页搜索、MCP 调用与实际并行、两账号糖果题。每项最多 10 次 POST，CLI 执行期限约 600 秒。模型使用 `gpt-6-astra` / `xhigh`，没有答案提示。缺少成功终态、工具执行证据或压缩结果时不会按 HTTP 200 判成功。

测试函数也支持在已有隔离主机上直接运行：

| 环境变量 | 用途 |
| --- | --- |
| `BPS_LIVE_ACCOUNTS_FILE` | 私有 JSON 文件，形状 `{"accounts":[{"alias":"test1","access_token":"…","account_id":"…"},{"alias":"test2","access_token":"…","account_id":"…"}]}` |
| `BPS_LIVE_CODEX_BINARY` | 实际官方 Codex 0.155 可执行文件的绝对路径 |
| `BPS_LIVE_WORK_ROOT` | 已存在的隔离工作根目录，CLI 临时目录创建于此；不要使用被 CLI 拒绝的 `/tmp` helper 位置 |
| `BPS_LIVE_EVIDENCE_FILE` | 可选，写入脱敏 JSON 证据文件 |

```powershell
go test ./internal/server -run '^TestBPSLiveCodexIntegration$' -count=1 -v -timeout 35m
```

同一套显式 live 环境还可单独验证真实 CLI WebSocket，避免重复 HTTP 八项：

```powershell
go test ./internal/server -run '^TestBPSLiveCodexWebSocket$' -count=1 -v -timeout 25m
```

该项验证文件任务、会话恢复和低阈值压缩；要求真实 101 Upgrade 和网关 WebSocket 请求记录。隔离测试入口拒绝 HTTP 生成回退，不能把回退成功当成 WS 成功。结果仍以对应运行证据为准。

未同时设置凭据路径和二进制路径时，测试明确 SKIP。账号文件只读取调用者指定路径，不扫描其他凭据，不自动刷新账号。`--skip-build` 仅供明确复跑已有候选；默认应重新构建，避免用旧产物代替当前源码验收。

HTTP/WS 单测、独立 PG/Redis 集成、真实 BPS 模型调用、前端浏览器检查和生产发布属于不同证据。真实测试产生模型用量；当前执行结果以该轮 `result.json` / `live.log` 为准，本索引不预先宣布通过或上线。

## 历史工具边界

`quality_*`、`mother_*`、旧 native Codex 对照和早期 Python BPS 原型用于复现历史调查，不是新的生产回退通道。不要把旧 `chatgpt.com/backend-api/codex` 对照脚本用作 PowerPoint 模型路由的验收。当前协议代码在 `internal/bps`，服务集成在 `internal/server/gateway_bps*`。归档规则见[历史材料索引](../docs/history/README.md)。
