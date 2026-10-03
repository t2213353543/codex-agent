# 独立验收入口

在项目根目录执行：

```powershell
python tools/acceptance.py
```

结果写入 `.local/acceptance/<UTC-run-id>/report.json`、`report.md`，最新结果同步到 `.local/acceptance/latest.json`、`latest.md`。退出码 `0` 表示全部适用要求通过，`1` 表示有失败，`2` 表示缺少环境、交付或证据。`blocked` 不计为通过。命令不会迁移或部署生产。

默认执行 Go test/vet、环境允许时的 race、新浏览器 mock 脚本、前端构建、包体测量和合成数据生成。若 `test:browser` 仍指向 `tools/browser-test.cjs`、`tools/ui-smoke.cjs` 或 `tools/smoke.py`，入口明确阻止执行。

Windows 缺少 CGO/C 编译器时，在前后端交付完成后使用独立 Linux 容器执行完整 race：

```powershell
python tools/run_linux_checks.py
```

该工具使用当前主节点 SSH helper，将冻结的 Go 源码和 migration 上传到本项目集成目录；只在临时容器安装 `gcc musl-dev`，容器限额为 2 CPU / 2 GiB / 128 PID。输出位于 `.local/integration/linux-checks/<run-id>/report.json`。报告中的 `checks` 可合入统一入口的 `--evidence` 文件；只有源码 SHA256 和证据文件 SHA256 一致、完整 Linux 测试无 skip 时，才能替代本机受阻的 Go/race 结果。本机环境限制仍保存在 `local_attempt`。只准备源码而不连接服务器时使用 `--prepare-only`。

`python tools/run_linux_checks.py --prepare-runtime` 仅在独立容器安装编译器并缓存私有 runtime image，不执行 Go 测试。正式 race 运行复用该 image。

连接已经启动的隔离服务：

```powershell
python tools/acceptance.py --environment-file .local/integration/environment.json --admin-key-file .local/integration/admin-key
```

显式启动一个待验收二进制并在结束后停止：

```powershell
python tools/acceptance.py --environment-file .local/integration/environment.json --binary .local/integration/codex-agent-test.exe
```

默认参数为 `--data <本轮目录>/service-data --listen <隔离URL地址端口> --web <项目>/frontend/dist`，自动生成测试管理密钥。`--backend-args-file <JSON文件>` 可指定完整参数字符串数组。已有服务占用端口时拒绝冒认该服务为指定二进制。Windows 停止使用 CTRL_BREAK，退出本身不代表已经验证优雅退出或令牌持久化。

PG schema 须先由迁移流程建立，入口不会隐式执行 migration。已初始化的测试数据库须通过环境 `ADMIN_KEY` / `TEST_ADMIN_KEY` 或 `--admin-key-file` 提供其测试管理密钥；自动生成的密钥仅适用于首次初始化。

环境文件至少需要以下结构；环境变量支持平铺大写字段，也支持 `env` 对象。真实连接串只留在 `.local`，不要放入版本控制或报告：

```json
{
  "purpose": "codex-agent-integration",
  "synthetic_data": true,
  "base_url": "http://127.0.0.1:18890",
  "database": {"name": "codex_agent_integration", "user": "codex_agent_integration"},
  "redis": {"namespace": "codex-agent:integration:"},
  "DATABASE_URL": "<isolated database URL>",
  "REDIS_URL": "<isolated Redis URL>",
  "REDIS_PREFIX": "codex-agent:integration:"
}
```

环境声明防止误选目标，独立数据库/用户/权限的真实验证仍属于 `database.isolation` 验收证据。API 目标仅允许 loopback 地址，可通过 SSH 隧道访问独立远端服务。黑盒用例只创建合成账号和文档地址代理；结束只删除本轮获得的实体 ID，不访问网关推理或 OAuth。

API 黑盒覆盖缺失/无效管理认证、cookie 登录/注销/属性、敏感字段、导入预览不写入、JWT 前缀碰撞、完整多行 JSON/数组/JSONL/纯 token、错误行定位、同名不同身份、同身份显式替换、409 版本冲突、代理密码语义和游标分页。故障注入、Redis 停机、真实重启、SQL 性能、迁移/回退不因普通 API 通过而视为通过。

身份语义按交付契约验收：完整凭据 fingerprint 用于重复导入识别；同名换凭据须显式 `replace`，同名不同身份的无替换导入必须明确拒绝且保留原账号。**不同名称之间不会仅因未核实的上游用户/工作区声明而自动合并。** 测试不将这些行为描述为跨名称的稳定上游身份去重。

发布判定额外接收本轮源码的证据：

```powershell
python tools/acceptance.py --source-manifest
python tools/acceptance.py --profile release --environment-file .local/integration/environment.json --evidence .local/integration/evidence.json
```

真实 Go/race/API 已在同机远端完成时，加 `--use-remote-checks` 使用证据代替本地重复运行。每个 Go/race 和 API 用例都必须是同源码 SHA256 的 `pass`，且所有证据文件存在并匹配 SHA256；缺失、失效或不同版本证据会明确 `blocked`，不会作为跳过通过。Go vet 和前端测试仍本地运行。

合并多份报告时使用以下命令，`--report` 可重复，按旧到新排列：

```powershell
python tools/assemble_evidence.py --report .local/integration/linux-checks/latest.json --report .local/integration/remote-migration/report.json --report .local/acceptance/latest.json --report .local/integration/attestations.json
python tools/acceptance.py --profile release --use-remote-checks --environment-file .local/integration/environment.json --evidence .local/integration/evidence.json
```

独立性能、环境隔离、部署演练证据先通过 `python tools/assemble_evidence.py --init-attestations .local/integration/attestations.json` 生成全 `blocked` 模板，由实际执行者完成对应观测后填写状态、具体结果、原始证据路径和 SHA256。不能把旧版本性能数字换成新源码哈希。合并工具将不同/缺失源码哈希的报告列入 `historical_reports`，不用于通过判定；只从已验证的具体 API 用例推导身份/导入/冲突项，且仅在 race 日志真实出现 PG 并发、CAS、进程断开+SIGTERM 测试通过事件时推导刷新持久化。

源码指纹排除所有 `results`、`artifacts`、`test-results`、`__pycache__`、`node_modules`、`dist` 输出目录，验收生成结果不会反过来改变源码版本。部署/测试脚本和构建配置仍参与指纹，修改它们后应重新冻结。

证据格式如下。`source_sha256` 来自上述 `--source-manifest` 输出，防止使用旧版本报告给新版本放行。每个通过条目须有可读取的证据文件、其 SHA256 和具体观测。所有要求 ID 定义在 `tools/acceptance.py` 的 `EXTERNAL_REQUIREMENTS` 中；缺项保持 `blocked`。源码若在验收期间发生变化，最终 `source.stable` 会阻止发布，需要对交付完成后的固定快照重跑。

```json
{
  "source_sha256": "<current source hash>",
  "checks": [
    {
      "id": "database.isolation",
      "status": "pass",
      "detail": "独立数据库、角色授权与 Redis 命名空间已核实；证据含真实连接后的身份查询。",
      "artifacts": [
        {"path": ".local/integration/isolation-report.json", "sha256": "<artifact hash>"}
      ]
    }
  ]
}
```

合成旧格式迁移数据可单独生成，完全不读取当前 `data` 或生产账号：

```powershell
python integration/fixtures/synthetic.py --output .local/integration/fixtures --accounts 1000 --logs 100000
```

`manifest.json` 记录账号数、用户身份数、共享工作区数、关系数、到期时间、日志/令牌统计和每个文件的 SHA256。默认固定参考时间方便复现，今日/时区测试通过 `--anchor <Unix秒>` 指定。日志包含相同时间戳和超过 120 天的历史条目，用于区分排序、保留策略与数据丢失。所有 JWT 签名、刷新令牌和代理密码均明确不可用。

`bundle.json` 包含构建前后 JS/CSS 大小、gzip 大小及 HTML 静态入口依赖，不将动态 import 的页面资源计入入口。实际首屏资源、布局和操作必须另看浏览器证据。

同机远端合成迁移验收：

```powershell
python tools/run_remote_integration.py --binary .local/integration/codex-agent-candidate
```

必须显式提供新构建的 Linux 候选二进制，工具不使用旧 `dist` 二进制。测试针对独立 `codex_agent_migration` 数据库内每次新建的 schema，在 28881 端口运行；私密配置为 `.local/integration/migration-environment.json`。工具将冻结脚本、候选二进制和前端构建以哈希分块上传；在 1 CPU / 1 GiB / 128 PID 的临时容器运行。私有 Python runtime image 中安装 Python/psycopg，不改宿主机包。执行完整迁移/重入/事务故障/密钥轮换/统计/Redis 断连/代理 CONNECT/真实 API/重启导出后，仅下载脱敏报告和 stdout 到 `.local/integration/remote-migration/report.json`，不下载 legacy/export 凭据文件。
