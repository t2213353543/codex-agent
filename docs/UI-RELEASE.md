# 仅前端发布与回退

> 当前版本：2026-09-24 16:29 居中弹窗与操作流程优化已上线，发布工具绑定 c6cffc 后端与 43e1af 二进制。本文以下基线属于历史流程；现行命令、验证与回退见 [操作流程发布记录](UI-WORKFLOWS-DEPLOYED-2026-09-24.md)。

当前字体与色彩细化版本已于 2026-09-17 19:33:57 上线，实际结果及当前回退命令见 [UI-REFINEMENT-2026-09-17.md](UI-REFINEMENT-2026-09-17.md)。

本流程用于已完成 PostgreSQL 迁移后的 Codex Agent UI 更新。只上传静态前端与发布证据，仅替换现有 systemd `ExecStart` 中的 `--web` 路径；Go 可执行文件、数据库、Redis、环境文件、工作目录、权限和退出超时均沿用当前 PG 版本。不会运行 `migrate`、`import-legacy`、`export-legacy` 或还原旧 JSON 数据。

当前绑定的后端：

| 项目 | 值 |
| --- | --- |
| 主节点 | `43.225.196.170:12865` |
| PG 发布 ID | `07d3784ae9502b4260b6e9c0c6b413fca439ee80b13bf682f156595d4922bf06` |
| Go 可执行文件 SHA256 | `07f958e065f8473e5a21c037d8527943cf71652e8b089f245a2c8aa91f673894` |
| Go/SQL 源码集合 SHA256 | `0283e76c219e418b1c0d89f10562b705f28423fdf48d367ba0ebe2de403b45b7` |
| 后端验收基准 | `.local/acceptance/20260917T103822-15128/report.json` |
| systemd unit | `/etc/systemd/system/codex-agent.service` |
| 环境文件 | `/etc/codex-agent/production.env` |
| 正式地址 | `http://43.225.196.170:18879` |

SSH 复用 `tools/integration_env.py` 的当前主节点连接。用户本轮已授权必要时使用本机代理，项目级开关为 `$env:CODEX_AGENT_SSH_PROXY='http://127.0.0.1:17897'`。不修改共享技能、系统代理或其他项目。

## 验收与证据

统一命令：`python tools/ui_acceptance.py`。运行前保持独立合成 PG/Redis API 的本机 `28882` 隧道可用，并按当前网络设置项目 SSH 代理。命令实际执行单测、生产构建、浏览器测试和真实 API 页面检查，核对测试容器中正在运行的 Go binary。输出 `.local/ui/runs/<UTC时间>/report.json`，每轮独立保存，失败不会生成通过状态；无部署和迁移操作。

报告对应的浏览器日志、结果、四主题截图都保存在同一个目录。使用此报告执行 `prepare --evidence <本轮report.json>`，无需手工填写通过结果。

先由前端交付构建、单元测试、真实浏览器验收；再通过合成数据的独立 PG/Redis 环境完成真实 API 页面检查。构建通过不代表页面验收通过。四项都完成后才能准备可发布包。

```powershell
python tools/ui_release.py fingerprint
python tools/ui_release.py self-test
```

`fingerprint` 输出当前源码、`dist` 完整文件集合、后端源码和固定后端 binary 的 SHA256。前端指纹覆盖 `src`、`tests`、`scripts`、`public` 和前端根目录的配置、package、HTML；不包含 `dist`、`node_modules`、运行报告、图片截图和交付 Markdown。`dist` 另以完整文件集合独立绑定。

运行每项验收时，记录前后指纹并确认未变；把真实结果文件或日志作为 artifact。不得给旧前端报告重新贴上新指纹。`.local/ui/acceptance.json` 格式如下，`checks` 必须恰好包含 `build`、`unit`、`browser`、`real_api` 四项：

```json
{
  "schema": "codex-agent-ui-acceptance-v1",
  "status": "pass",
  "frontend_source_sha256": "<fingerprint 值>",
  "backend_source_sha256": "<fingerprint 值>",
  "dist_sha256": "<fingerprint 值>",
  "checks": [
    {
      "id": "real_api",
      "status": "pass",
      "frontend_source_sha256": "<同上>",
      "backend_source_sha256": "<同上>",
      "dist_sha256": "<同上>",
      "synthetic_data": true,
      "backend_binary_sha256": "07f958e065f8473e5a21c037d8527943cf71652e8b089f245a2c8aa91f673894",
      "artifacts": [
        {"path": ".local/ui/real/report.json", "sha256": "<实际文件 SHA256>"}
      ]
    }
  ]
}
```

上例仅展示 `real_api` 项；其他三项使用同样的指纹与 `artifacts` 字段，无需 `synthetic_data`、`backend_binary_sha256`。artifact 必须位于当前项目中，可以是结果 JSON、命令日志或脱敏截图，不能含凭据。多个 artifact 可以共同支持一项验收。

```powershell
python tools/ui_release.py prepare --evidence .local/ui/acceptance.json
```

`prepare` 会验证源码与已上线 PG 后端验收文件完全一致，冻结 `dist`、验收报告、artifact 和远端状态机脚本，写入 `.local/ui-releases/<64位发布ID>/ui-manifest.json`。发布 ID 覆盖全部源码映射、静态文件 hash、证据 hash 和脚本 hash。文件变化或证据缺失立即失败。此时 `tests_passed=true`，`deployed=false`，`database_migrated_by_ui_release=false`。

## 独立真实演练

固定演练边界：

- 根目录 `/opt/codex-agent-integration/ui-rehearsal`，需提前创建真实目录。
- 服务 `codex-agent-integration-ui-rehearsal.service`，`Type=exec`，绑定 `127.0.0.1:28884`。
- 使用现有专用 PG 演练环境 `/opt/codex-agent-integration/rehearsal/config/production.env`，数据库 `codex_agent_rehearsal`、PG 端口 `25432`。
- 原始 `ExecStart` 为固定 PG binary 加 `--listen 127.0.0.1:28884 --web /opt/codex-agent/releases/<PG发布ID>/frontend/dist`。演练可用 `User=root` 穿过测试根目录 `0700`；这不替代正式 `DynamicUser` 的可读性验证。
- 原演练 unit 必须已运行并通过 live、ready 和首页检查。工具不创建数据库、不迁移、不启动其他项目。

将下列 `$uiManifest` 设置为 prepare 输出的本机绝对路径：

```powershell
$uiManifest = '<项目绝对路径>/.local/ui-releases/<发布ID>/ui-manifest.json'
python tools/ui_release.py stage --release $uiManifest --rehearsal
python tools/ui_release.py activate --release $uiManifest --rehearsal
python tools/ui_release.py activate --release $uiManifest --rehearsal
python tools/ui_release.py rollback --release $uiManifest --rehearsal
python tools/ui_release.py activate --release $uiManifest --rehearsal --fault startup
python tools/ui_release.py status --release $uiManifest --rehearsal
```

第二次 activate 必须复用成功状态而不再次重启。`--fault startup` 会在新 unit 启动后停止测试服务，模拟启动后健康故障；命令必须失败退出，同时自动恢复原 PG unit 并通过健康检查。故障注入只允许在演练。演练成功只写 `rehearsal_deployed=true`，正式 `deployed` 保持 false。

## 正式切换

完成真实演练与正式服务权限验证后，按顺序执行：

```powershell
python tools/ui_release.py stage --release $uiManifest
python tools/ui_release.py activate --release $uiManifest
python tools/ui_release.py status --release $uiManifest
```

stage 先分成 SHA 命名的 4 MiB 块上传并校验，冻结至 `/opt/codex-agent/ui-releases/<发布ID>`。再次 stage 校验同一不可变目录，不覆盖现用目录。为兼容尚未刷新的浏览器，stage 从当前 UI 复制不冲突的旧 hash 静态资源；这些额外文件记录在独立 `compatibility.json` 中，不改变新构建的 `index.html` 或原 `frontend` 文件清单。同一 hash 文件名但内容冲突会阻止发布。

activate 与 rollback 共用 `/opt/codex-agent/.deployment.lock` 文件锁。备份当前 PG unit 到私有 `/opt/codex-agent/.ui-state/<发布ID>/previous.service`；状态文件和 journal 持久化后才替换 unit。只改 `--web`，然后 `daemon-reload` 与 graceful `restart`。工具会拒绝旧 JSON binary、异常环境文件、额外服务生命周期命令、drop-in，以及非本项目的 web 路径。

只有新进程的 binary SHA、`--web` 路径、`/health/live`、`/health/ready`、首页内容 SHA 均通过，才写 `deployed=true`。随后仍需实际浏览器登录、账号页面、日志统计、主题与布局抽验，结果另附发布记录。一次普通 restart 会短暂中断管理页面请求，不会回退数据库或重新导入账号凭据。

如果 SSH 断开导致操作结果不明，先执行 status；如果中断时 phase 停在切换过程中，重跑 activate 会先保守恢复原 PG unit，失败退出并提示再次 activate。不会同时启动两份服务。

## 回退

```powershell
python tools/ui_release.py rollback --release $uiManifest
python tools/ui_release.py status --release $uiManifest
```

回退只恢复本次发布之前的 PG unit，因此也只回退前端。它先验证私有备份 hash，并确认当前 unit 仍为本次切换内容或其原始版本；发现他人后续发布则拒绝覆盖。恢复后重新检查原 UI 首页、live、ready 和同一 Go binary。失败状态不会标记部署完成，也不会把数据库恢复成 JSON。

本机发布包丢失时，可通过已授权 SSH 在服务器执行等价命令：

```bash
python3 /opt/codex-agent/ui-releases/<发布ID>/deploy/ui_release_remote.py rollback --release /opt/codex-agent/ui-releases/<发布ID>
```

本地 `self-test` 覆盖旧 unit 拒绝、过期证据拒绝、unit 字节保留、重复 activate、后续修改保护、幂等 rollback、启动故障自动回退、中断恢复与备份篡改拒绝，共 9 项。它只证明状态机模拟通过，不能代替服务器真实演练或正式上线结果。
