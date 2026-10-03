# Codex Agent 迁移与部署手册

本手册仅覆盖本项目管理后台、账号/代理、PG/Redis、日志统计及发布。不得修改 Sub2API、accounts、28765 或其他现有项目。服务器操作前必须阅读当前 `sub2api-production` 技能，并使用技能当前主节点；README 中地址不能代替现场确认。

## 当前状态与证据边界

2026-09-17 18:42:53（Asia/Shanghai），PG/Redis 版已通过统一验收并完成生产迁移、部署；18:45 复核网页及 live/ready 均为 200，实际数据库写入和服务重启恢复通过。实际发布清单见 `docs/RELEASE-2026-09-17.json`。用户随后要求的前端企业风格重构、统计卡片与账号字段恢复已于 19:15:40 单独验收上线，未重复导入生产数据；当前前端发布与回退请使用 [UI-RELEASE.md](UI-RELEASE.md)，实际结果见 [UI-ACCEPTANCE-2026-09-17.md](UI-ACCEPTANCE-2026-09-17.md)。以下表格保留迁移前旧版基线。以下现网基线来自集成负责人完成的只读检查，时间为 `2026-09-17T09:14:43.265647+00:00`，原始去敏证据为 `.local/acceptance/production-baseline.json`。本手册作者未另行操作服务器。

| 项目 | 已核验的旧版基线 |
| --- | --- |
| 服务 | `codex-agent.service`，DynamicUser，`StateDirectory=codex-agent` |
| 主节点/访问 | 当前主节点 `43.225.196.170`，SSH `12865`；`http://43.225.196.170:18879`，仅记录地址，不公开秘密 |
| 监听 | `0.0.0.0:18879` |
| 程序/页面 | `/opt/codex-agent/codex-agent`、`/opt/codex-agent/frontend/dist` |
| 状态目录 | `/var/lib/codex-agent`；JSON/JSONL 文件存储 |
| 启动命令 | `--data /var/lib/codex-agent --listen 0.0.0.0:18879 --web /opt/codex-agent/frontend/dist` |
| systemd | `/etc/systemd/system/codex-agent.service`，`TimeoutStopSec=60` |
| 健康检查 | service active/running，`GET /health` 与 `/` 均 200；旧版不具备 PG/Redis readiness 证明 |
| 版本 | 后端 `0.2.0`；以现场二进制 SHA256 和候选 manifest 为准，侧栏字符串不作版本证据 |
| 运行中与磁盘二进制 SHA256 | `96e708aff1430d24a6efa0a584e6f4089a5e03868e6dfd8cc8e35149a8d07aed`，二者一致 |
| service SHA256 | `5421a6b3ff4e8d60c0e59800c738927d72de422c0f8fd6d292d4361042109495` |
| 前端 index SHA256 | `79d0dd2925aa8b11d5b693402b94e3b60d84b689afe9a37d8e228d92b0587d5e`；静态目录存在旧文件，不能把目录总字节当实际首屏资源 |
| 环境/数据 | 未配置数据库环境变量；存在 client-key/accounts.json/groups.json/stats.json/logs.jsonl，日志为 0 字节，不据此推断性能 |
| 版本控制 | 本次首次检查目录无 `.git`；不得编造提交号，必须记录源文件和产物 SHA256 |

`tools/deploy.py` 已替换为 prepare/stage/provision/preflight/cutover/rollback/status 流程，远端状态机位于 `deploy/release_remote.py`。脚本存在不代表生产部署完成；必须通过真实隔离演练和最终发布门禁。`tools/smoke.py --import-account ...` 读取其他项目账号，不用于本轮验收。统一验收命令为 `python tools/acceptance.py`；未满足门禁前禁止进入生产切换阶段。

## 发布目录与材料

每个 release ID 使用不可变产物哈希；发布记录应独立保存以下内容，权限禁止其他用户读取秘密文件：

1. 后端二进制、前端资源列表与哈希、服务配置、API 契约、前后端交付说明。
2. 完整验收报告、浏览器截图、性能数据、实际 SQL 的 EXPLAIN 输出、迁移/回退演练记录。
3. 脱敏发布清单、旧版二进制和页面备份、旧 service/config 备份、旧数据快照、目标数据库备份位置与哈希。
4. 单独的秘密备份：应用加密密钥、数据库/Redis 凭据、管理认证恢复材料。只记录秘密的引用路径和 key ID，不能把内容放进清单、仓库或日志。

发布清单使用 `deploy/release-manifest.example.json`，执行后复制为本次实际清单填写证据。`tests_passed`、`migrated`、`deployed` 分别填写，禁止用单一“完成”掩盖未做的阶段。

当前两份交付说明已经到位：`docs/BACKEND-HANDOFF.md` 和 `frontend/FRONTEND-HANDOFF.md`。前端交付中的浏览器结果使用合成 mock，不替代真实后端会话/数据库验收；后端交付中的 CLI、退出与保留限制纳入下文流程。最终版本由发布清单中的文件 SHA256 绑定。

本地统一发布入口按顺序执行如下；前两个命令可以准备未获最终发布门禁的候选，provision/preflight/cutover 必须绑定相同源码的通过报告：

```powershell
python tools/acceptance.py --profile release --environment-file .local/integration/environment.json --use-remote-checks --evidence .local/integration/evidence.json
python tools/deploy.py prepare --acceptance .local/acceptance/latest.json
python tools/deploy.py stage --release .local/releases/RELEASE_ID/release-manifest.json
python tools/deploy.py provision --release .local/releases/RELEASE_ID/release-manifest.json --acceptance .local/acceptance/latest.json
python tools/deploy.py preflight --release .local/releases/RELEASE_ID/release-manifest.json --acceptance .local/acceptance/latest.json
python tools/deploy.py cutover --release .local/releases/RELEASE_ID/release-manifest.json --acceptance .local/acceptance/latest.json
python tools/deploy.py status --release .local/releases/RELEASE_ID/release-manifest.json
```

上面 `RELEASE_ID` 为 prepare 实际返回的 hash。验收报告 `latest.json` 只是便捷入口，正式发布须复制并保留对应 run 目录的不可变报告，防止后续测试覆盖引用。验收入口退出码 0 为通过、1 为失败、2 为阻断；报告位于 `.local/acceptance/<UTC-run-id>`。不得因为部分子测试通过而改写 `release_ready`。

本次网络例外由用户于 2026-09-17 明确授权“那你走本地代理试试”。直连及系统当前美国线路超时，独立临时香港线路成功；所有项目 SSH/SFTP 命令显式设置 `$env:CODEX_AGENT_SSH_PROXY='http://127.0.0.1:17897'`，仍采用 SSH 加密认证。未修改共享 skill、系统代理或既有节点选择。这个端口只在临时本地进程运行时有效；默认不设置该变量时仍直连。重跑应先检查可用传输，不把本次临时端口视为永久基础设施。

## 0. 现场基线与发布门禁

现场只读核验必须记录 UTC 时间、目标 host、服务 active/substate、MainPID、实际命令行、监听端口、程序/静态资源哈希、数据路径、配置变量名称、现存备份、磁盘空间和资源余量。不要打印 `/proc/.../environ`、秘密文件或带密码的连接串。只读命令可以包括：

```bash
systemctl show codex-agent.service -p ActiveState -p SubState -p MainPID -p FragmentPath -p ExecStart -p TimeoutStopUSec
ss -ltnp '( sport = :18879 )'
sha256sum /opt/codex-agent/codex-agent
curl --fail --silent --show-error http://127.0.0.1:18879/health
```

若 systemd 的 ExecStart 后续改为包含秘密参数，须对输出脱敏。保存运行配置的备份不等于把内容附到报告。

满足以下全部条件才可安排停写窗口：

- API 契约和两份交付说明齐全，前后端实现与验收版本/哈希一致。
- `ACCEPTANCE-MATRIX.md` 的关键正确性、真实 PG/Redis、页面、迁移和回退演练无失败或未执行项。
- PG 导入工具实际支持 dry-run、导入及可重复执行；每个选项已用 `--help`/交付说明核对，不能依据本手册猜测 CLI。
- 导入目标与线上其他库、角色、缓存隔离；测试数据不来自生产或外部账号项目。
- 数据冻结/排空/最终快照和凭据刷新单写者机制已验证。
- 旧版产物与数据备份可恢复；新凭据出现后的回退方案已实测，或已明确为只允许修复前进的切换边界。
- 发布清单中的所有阻断项已消除，实际执行人与窗口已记录。

## 1. 隔离 PG 与 Redis

测试环境与生产分别使用本项目专属数据库、角色、秘密和 Redis namespace。不得使用其他项目连接串。数据库名、用户名和缓存前缀应含 `codex_agent` 及环境/运行标识；具体值写入本次清单。

隔离测试环境入口已经落地，必须在读取当前生产技能后运行：

```powershell
python tools/integration_env.py up
python tools/integration_env.py status
python tools/integration_env.py tunnel
```

`tunnel` 是持续运行的 SSH 隧道，在独立终端保留；测试结束后可终止该终端，并用 `python tools/integration_env.py stop` 停止测试容器。它只操作 Compose project `codex-agent-integration` 和 `/opt/codex-agent-integration`；PG/Redis 仅绑定服务器回环 `25432/26379`，本机同端口通过隧道访问。数据库 `codex_agent_integration`、角色/Redis ACL 用户 `codex_agent_test`、Redis 前缀 `codex-agent:integration:`；秘密仅保存在 `.local/integration/environment.json`，不要打印或提交该文件。

该入口配置测试 PG 512 MiB/1 CPU/30 连接、Redis 容器 128 MiB/0.5 CPU/maxmemory 96 MiB。PG 服务端 statement/lock timeout 为 15s/3s；仍需核对应用池限制。测试 Redis 使用 allkeys-lru，不能照搬为生产会话可靠性已验证。实际容器健康、ACL 和持久化行为由验收报告证明，脚本存在不等于环境已验收。

PG 推荐边界：schema owner/migrator 与 runtime 角色分离；runtime 为 `NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION`，只获得本项目 schema/table/sequence 所需权限；撤销不需要的 PUBLIC schema 创建权限。数据库/角色连接限制与应用连接池上限同时配置。所有迁移以事务执行或清楚标记非事务步骤；重复执行只推进未应用版本，checksum 不符立即失败。

Redis 使用独立实例最易隔离资源，或使用支持 ACL 的独立用户及严格 key 前缀。Redis logical DB 编号不是权限边界。只授权后端实际所需命令和 `~codex_agent:<environment>:*` keys；禁用不需要的管理/全库命令。若实例承载会话，需确认 maxmemory/淘汰策略不会导致不可解释的失联；必须验证 Redis 不可用时认证失败关闭。

在本次资源清单填写实际值：PG 最大连接数、应用 pool max/min/acquire timeout/连接寿命、statement_timeout、lock_timeout、Redis pool/读写超时/TTL、CPU/内存/磁盘上限、明细保留期、聚合保留期。值依据当前主节点余量选定，不能占用服务器总配额。

不得为运行新版未经评估地修改其他项目依赖的 PG、Redis、Docker、OpenSSL、SSH 或防火墙配置。仅监听本机/私网的数据库和缓存端口，凭据文件权限为 `0600`，目录为 `0700`。

## 2. 候选配置与静态部署

实际后端配置名以交付文档为准。发布清单必须列出并核验：

| 配置类别 | 必需事实 |
| --- | --- |
| 持久存储 | 明确 PG 模式、目标库/角色/schema、TLS 与连接池，不允许配置失败时静默回退文件存储 |
| 凭据加密 | 独立提供稳定主密钥与 key ID，备份可用；重启不重新随机生成；列表/日志不泄漏明文 |
| 管理会话 | 有效期、撤销、密钥轮换恢复流程；HTTPS 下 HttpOnly/Secure/SameSite cookie；无长期管理 key localStorage |
| Redis | namespace、ACL 用户、pool、超时、缓存 TTL；认证与普通缓存的故障行为明确 |
| 时区与保留 | IANA 时区、“今日”和“累计”口径、日志/聚合保留与重建策略 |
| HTTP/退出 | 连接/读写/请求超时、排空期限、后台刷新和持久化等待、systemd 停止上限 |
| 健康 | liveness 不替代 readiness；readiness 真实检查 PG 及必要会话依赖，响应不包含秘密 |

公开地址当前历史记录为 HTTP，不能把 `Secure` cookie 配置完成写成会话传输保护已验收。实际候选必须验证部署入口与会话方案相容；若需要新 TLS 入口，记录实际方案和验证结果，不擅自更改其他项目的反向代理配置。

新产物先上传至本项目独立 release 目录，校验本地/远端 SHA256；此阶段不启动可刷新生产凭据的新进程。systemd 应保留最小权限、私有状态目录、只读系统保护及有界资源。凭据通过受权限保护的环境文件或 systemd credentials 注入，不能出现在 ExecStart、CLI 参数和 shell 历史。

## 3. JSON/JSONL dry-run 与隔离导入

此阶段使用合成/脱敏夹具；只有工具与流程通过后，才对本项目生产快照开展保密的迁移验证。不得从其他账号项目复制凭据。后端 `migration_cli.go` 已实现 `migrate`、`import-legacy --source DIR --dry-run`、`import-legacy --source DIR`、`export-legacy --target NEW_DIR`、`rebuild-usage` 和 `prune-logs`。本地可用 `go run .` 加上述子命令；远端状态机使用候选二进制绝对路径。启动只验证 schema，不能通过启动新版本隐式迁移生产。

状态机中 `migrate` 读取 root-only `/etc/codex-agent/migration.env`，此文件只允许包含 migrator 的 `DATABASE_URL`；其余 CLI 和服务读取 `/etc/codex-agent/production.env`。两角色必须不同，目标库均为独立回环 `127.0.0.1:25433/codex_agent`；Redis 为 `127.0.0.1:26380`，namespace 为 `codex-agent:production:`。生产 unit 的内存上限 512 MiB、CPUQuota 100%、TasksMax 128、停止上限 180 秒；状态机的 systemctl 子进程超时为 210 秒，不会在 unit 的正常排空期限结束前中断。

旧目录可能包含 `accounts.json`、`groups.json`、`keys.json`、`proxies.json`、`settings.json`、`stats.json`、`logs.jsonl` 和 `client-key`；以现场文件清单为准。需要同时保留原始文件、大小、mtime 与 SHA256，不能只导入账号后宣布迁移完成。不要在进程写入时把逐文件复制视为一致快照。

dry-run 必须不写入目标业务表、凭据、版本或聚合，并输出去敏清单：

- 总输入数、有效数、无效数、每条错误的文件/行号/字段；JSON 错误不得退化成 token。
- 源稳定 ID 与目标 ID 映射、身份摘要、同名不同身份、同身份重复与选定处理方式；身份摘要使用不可逆指纹，不输出凭据。
- 分组、代理、账号和 key 关系的引用数、未解析引用和孤立项；同名实体不能导致错误关联。
- 每账号 expiry 是否存在、UTC 规范化值、无时区/格式错误/过期数；不得把未知到期写成有效或自行延长。
- 日志数量/事件 ID、时间范围、成功/失败数、请求/token/cache 字段；统计按明确时区和保留范围对账。
- 原统计快照与日志可能重叠；工具必须定义历史聚合与明细的归并水位，避免导入两者后双计。缺少历史明细时明确可验证的范围，不能声称全量重建已通过。

真实隔离导入后检查约束、身份、关系、expiry、统计和凭据密文读取；再次执行相同导入不得新增重复账号/日志或重复聚合。对失败批次验证事务边界与恢复行为。导入结果只有计数相同不足以通过；必须核对 ID 映射和关系集合。

## 4. 短暂停写窗口与最终导入

1. 保持新进程未启动。记录旧版/候选产物哈希、备份位置、目标库标识和窗口开始时间；通知/展示本项目维护状态，阻止新管理写入和网关请求触发凭据刷新。
2. 对本项目执行 `systemctl stop codex-agent.service`，等待优雅排空结束。确认 MainPID 为 0、无旧版残留进程、18879 不再由旧服务监听、没有本项目后台刷新任务。不能只依赖“stop 命令已发出”。
3. 对停止后的整个状态目录建立最终一致快照并计算哈希；捕获最后凭据版本/指纹和统计水位。备份应保留权限/归属。秘密快照不得提交仓库。
4. 对最终快照再次 dry-run。与预演差异须仅是停写前的正常写入，身份冲突、未知引用、错误行或凭据缺失一律中止。
5. 在独立目标库执行已验证的版本迁移和最终导入，记录 migration IDs、导入 run ID、source snapshot hash 与目标事务结果；只允许当前发布运行持有迁移锁。
6. 在不开启刷新/公网写入的条件下核对账号数、身份/ID、关系、expiry、日志、聚合、凭据密文可解密、管理认证恢复路径。所有对账通过后才填写 `migrated: true`。
7. 记录旧版最后凭据水位与候选启动前水位，切换候选 service/config，再启动一个实例。不得同时启动指向同一套凭据的旧/新服务，不得未经设计开启双写。

任何步骤失败：新服务保持停止。若新系统尚未执行凭据刷新或其他外部不可逆修改，可按下一节恢复旧产物/配置及冻结快照。若不能证明没有新凭据产生，则转入凭据协调流程，不能直接复活旧快照。

## 5. 切换、线上验收与完成判定

候选启动后先检查 systemd、版本/构建 ID、端口、liveness、readiness、PG 连接和 Redis 会话。逐项完成：

1. 公网网页与静态资源哈希/版本正确，无缓存残留旧前端；桌面和手机可登录。
2. 匿名管理访问被拒绝，合法会话可用，过期/退出会话失效，cookie 和秘密脱敏正确。
3. 仅创建带 release ID 的合成测试实体；完成账号/分组/代理 metadata CRUD、版本冲突和关系解绑，之后清理；不得使用真实 token 做故障测试。
4. 确认写入目标 PG，不能只根据 API 成功推断落库。核对日志倒序、时区、统计值和已声明保留范围。
5. 进行一次有界正常重启，检查账号状态、日志、聚合和会话约定；确认后台任务没有重复启动。
6. 记录公网/本机地址、UTC 时间、HTTP 状态、service 状态、候选实际 SHA256、数据库 schema 版本、已知限制和回退命令。

上述线上验收全部通过，才写 `deployed: true`。服务 active 或 `/health` 返回 200 单独都不足以宣布部署完成。如失败，维持维护窗口并执行符合凭据边界的回退，不把半成品暴露为完成版本。

## 6. 回退：先确认凭据边界

| 失败位置 | 可执行回退 |
| --- | --- |
| dry-run/导入失败，候选未启动且旧凭据未变化 | 目标库保持隔离，停止候选；恢复旧产物、service/config 与停写一致快照；启动旧服务并核验 |
| 候选启动失败，能证明未发生刷新或其他外部变更 | 同上；保留候选 PG 失败现场用于排查，不边恢复边写入 |
| 候选已成功刷新任何凭据 | 先停候选并确认排空；旧冻结快照里的 refresh token 可能已失效。必须用经过测试的 PG→旧格式凭据导出/协调步骤回填最新 AT/RT、expiry、身份、credential_version，再恢复旧版；缺少此工具时不得声称“一键回退”，保持单实例修复前进 |
| 候选产生新日志/设置/关联数据 | 导出或保存切换后的增量和审计水位；明确恢复旧快照的数据差异，不能把已提交写入静默丢弃 |
| 加密密钥缺失或错误 | 不删除/重建库或重新生成密钥；从已验证秘密备份恢复同一 key ID，再验证解密 |

实际回退命令（本地清单路径替换为本次生成的真实路径）：

```powershell
python tools/deploy.py rollback --release .local/releases/RELEASE_ID/release-manifest.json
python tools/deploy.py status --release .local/releases/RELEASE_ID/release-manifest.json
```

远端等效命令：

```bash
python3 /opt/codex-agent/releases/RELEASE_ID/deploy/release_remote.py rollback --release /opt/codex-agent/releases/RELEASE_ID
```

`RELEASE_ID` 必须为实际 64 位十六进制 release ID。状态机对整个操作持有 `/opt/codex-agent/.deployment.lock` 的排他 flock，不接受外部 root/data/service/端口覆盖。不可逆操作前追加并 fsync `private/journal.jsonl`，状态原子写入 `private/state.json`；秘密日志/数据/导出只留在权限 0700 的 private 目录，stdout 仅输出去敏状态。

停止旧服务后，状态机检查 MainPID、服务状态、相关进程及端口；完整备份 unit 和本项目数据，记录哈希。生产 StateDirectory 只允许顶层 `/var/lib/codex-agent` 映射到确切的 `/var/lib/private/codex-agent`：链接与私有父目录须由 root 管理，目标权限私有且 UID 对应服务身份、systemd 动态 UID 或其 id-mapped nobody；内部 symlink/特殊文件仍被拒绝。备份与恢复使用通过验证的实际目录，状态记录 realpath/UID/GID。演练不接受这一生产映射。

候选尚未请求启动时，回退保留未被修改的旧数据并恢复旧 unit。只要候选 start 曾请求过，即使启动失败，也先停候选并执行 PG 最新 `export-legacy`，校验八个旧格式文件完整，再逐文件原子替换并恢复原 UID/GID。导出拒绝重名账号、文件缺失、校验失败或异常强停时保持停止，不盲启旧凭据。

迁移后的统计读回按冻结 logs 重新构造分钟和账号 UTC 日桶，再与 stats 快照合并。日志在所有指标上覆盖旧快照时，以较新日志为准；快照在所有指标上覆盖日志时，只补快照缺失基线，不重复计数；不同指标互相领先的交叉情况拒绝继续。对比合并结果的全部保留期桶及 PG 额外桶，避免把“日志已更新、stats 尚未定时保存”误报成迁移丢失。保留期外桶单独计数，不声称从旧格式导出验证无限历史。

回退先验证保留的旧二进制与 index 哈希，再恢复 unit/daemon-reload/start 和健康检查。若恢复旧进程后控制脚本中断，再次 rollback 会保留旧进程可能刚写入的新令牌，不把此时较旧的 PG 凭据倒灌。导出失败时重试使用全新目标目录。旧版产物一直位于 `/opt/codex-agent/codex-agent` 和 `frontend/dist`，候选从独立 release 目录启动，不覆盖旧产物。

同一 release 成功后再次 cutover 只复核运行哈希、页面、健康和认证。已回退后再次迁移不等于可向原目标库覆盖导入：后端会拒绝不同 source manifest，需先准备新的独立目标库与发布计划。任何脚本异常返回非零，失败不标记完成；回退成功后 `deployed=false`，曾成功提交的 PG 迁移事实仍单独保留。

Windows 本地模拟覆盖六项状态机流程、五项统计合并场景及 StateDirectory 元数据边界。模拟替换了 systemd/PG/网络及 Linux fsync/chown，不能计为真实 D06/D09 通过。

真实演练候选 `54c24c2d225cf4f8cbabff33e1e345198c9ee00516cfbdcf5f50273aa2fc0de7` 于 `2026-09-17T10:27:44Z` 完成停写导入及导出对账，但在启动后校验失败；随后从 PG 导出最新凭据并恢复旧测试服务成功，生产未迁移或部署。只读日志表明默认 Type=simple 的 start 返回后约 19 毫秒即触发 PID/hash 检查，服务当时 active/running，脚本主动停止前没有候选应用异常记录；证据符合 exec 前竞态，未捕获当时 /proc/exe 链接，不能宣称完全回溯证明。

修订后的候选与演练 unit 显式使用 `Type=exec`。状态机另用最长 45 秒启动窗口同时验证候选运行哈希、三个健康/页面端点以及检查前后同一 PID；应用初始化失败或 auto-restart 仍失败，永久哈希不符到期失败并保留观测。五项本地时序模拟（exec 前过渡、初始化 503、真实失败、校验中 PID 改变、永久错误二进制）通过。

修订后已完成真实演练，候选为 `a1c8a9407c38ffa767a579d9567bc6b99b77818550e26f3653eecaa95e8b2f99`，源码 SHA256 为 `f54e9150b48d3e4503b16921d23955aca16ba7cbacdfabaf8eec6a8f5bf50369`，后端 SHA256 为 `07f958e065f8473e5a21c037d8527943cf71652e8b089f245a2c8aa91f673894`：

- `2026-09-17T10:34:47Z`：真实迁移、PG 导出对账、Type=exec 启动、稳定 PID/hash、liveness/readiness/页面及登录/注销检查通过；再次 cutover 只复核，通过。
- `10:34:49Z`：停候选后导出 PG 当前旧格式数据、恢复旧 unit/程序、健康检查通过；再次 rollback 不重新覆盖数据，通过。
- `10:35:30Z`：重建同一专用测试库后注入真实 ExecStart 缺失故障，cutover 按预期返回 1，自动导出 PG 最新数据并恢复旧测试服务，通过故障恢复验收。
- 取证后测试 service 已停止。只读检查生产仍 active，运行二进制与原 service SHA256 均未改变；生产 `migrated/deployed` 始终为 false。

真实报告为 `.local/integration/rehearsal/final-report.json`（SHA256 `3ac13a63f754dc2132ba8758d0a7f41a1352c5ee9e443d363fda4d5687f09221`），包含绑定该源码的 `database.rollback` 与 `deployment.rehearsal` 两项通过记录，以及本地下载的 state/journal/导入导出证据哈希。演练不触发真实 OAuth；刷新持久性由独立集成测试证明。旧候选失败证据仍保留，不标成新源码通过。

## 6.1 真实隔离部署演练

`--rehearsal` 固定以下边界：`/opt/codex-agent-integration/rehearsal`、`codex-agent-integration-rehearsal.service`、root/data、root/config、`127.0.0.1:28883`、测试 PG `25432/codex_agent_rehearsal`、Redis `26379` 与 `codex-agent:integration:rehearsal:`。不能从 CLI 改写这些路径、端口或库名。允许尚未通过最终门禁的候选参与演练，但不伪造 tests_passed；报告标记 `mode=rehearsal,synthetic_only=true`，生产 `migrated/deployed` 永远为 false，结果放在 `rehearsal_migrated/rehearsal_deployed`。

负责人先用现有隔离 PG 管理工具创建专用 `codex_agent_rehearsal` 库和不同 migrator/runtime 角色，配置权限及默认授权；不复用普通验收库或生产角色。建立 0700 演练根目录、`.synthetic-rehearsal` 标识文件，root-only 0600 `config/production.env` 与 `config/migration.env`。生产配置中的 DSN/端口/namespace 不可直接复制；加密密钥也应为合成演练独立生成。migration.env 只含不同角色的 DATABASE_URL，两角色连接同一 rehearsal 库。

下面命令中的 `RELEASE_ID` 替换为已 stage 的实际 hash。准备器只读复制旧生产程序与静态文件，不读取生产数据；它自行建立禁用的合成 JWT 账号、空关系/日志/统计和随机合成管理 key，启动旧版测试服务。重复 prepare 拒绝覆盖已有测试数据。

```bash
python3 /opt/codex-agent/releases/RELEASE_ID/deploy/release_remote.py prepare-rehearsal --rehearsal --source-release /opt/codex-agent/releases/RELEASE_ID --release /opt/codex-agent-integration/rehearsal/releases/RELEASE_ID
python3 /opt/codex-agent/releases/RELEASE_ID/deploy/release_remote.py preflight --rehearsal --release /opt/codex-agent-integration/rehearsal/releases/RELEASE_ID
python3 /opt/codex-agent/releases/RELEASE_ID/deploy/release_remote.py cutover --rehearsal --release /opt/codex-agent-integration/rehearsal/releases/RELEASE_ID
python3 /opt/codex-agent/releases/RELEASE_ID/deploy/release_remote.py cutover --rehearsal --release /opt/codex-agent-integration/rehearsal/releases/RELEASE_ID
python3 /opt/codex-agent/releases/RELEASE_ID/deploy/release_remote.py rollback --rehearsal --release /opt/codex-agent-integration/rehearsal/releases/RELEASE_ID
```

启动失败场景使用相同流程但 cutover 加 `--fault startup`。该选项只在演练 cutover 接受，生成不存在的候选 ExecStart，引发真实 systemd 启动失败。预期命令非零且自动回退恢复旧测试服务，private state 记录 `phase=rolled_back`、`rehearsal_deployed=false`，不以预期故障非零误报为成功部署。

两场景之间若旧文件被导出规范化，source manifest 会改变；负责人需重建这个专用 rehearsal 数据库并重新授予角色权限，再执行 preflight。仅重建这一明确命名的测试库，保留状态/journal/evidence，不清理生产库或常规验收库。数据库管理步骤不在状态机中自动执行。

演练 service 使用 `User=root` 以穿过测试根目录既有 0700 权限，不放宽集成环境秘密权限；生产继续使用 DynamicUser。因此还必须单独完成生产候选路径的 DynamicUser 可遍历/环境文件加载验证：用独立临时 systemd unit、测试数据库/env、仅回环 28884，验证后停止它。该检查列为独立发布门禁，不能用 root 演练通过替代。

## 7. 发布后维护与留存

保留旧版、最终快照、迁移审计和密钥备份直至明确留存期届满。日志按声明的保留策略小批清理并记录删除数量；聚合重建从持久事件与已声明水位完成，重复执行幂等。历史明细按策略删除不能记成“事故丢失”，超出策略的缺口不能解释成正常保留。

后续发布重复执行同一 release ID 应校验产物/迁移记录并得到无变更结果；内容不同不得复用 ID。运行目录、数据库/schema 版本、配置 key ID 和候选前后端 hash 都应可查询。秘密轮换与密钥轮换分别有备份、恢复和验证流程，不能和普通发布顺手混做。

## 待交付绑定

| 项目 | 当前状态 |
| --- | --- |
| API 契约与前后端交付说明 | `docs/API-CONTRACT.md`、`docs/BACKEND-HANDOFF.md`、`frontend/FRONTEND-HANDOFF.md` 已读取；最终哈希随发布清单保存 |
| 统一验收命令和结果目录 | `python tools/acceptance.py` 已实现；结果以当次报告为准 |
| 迁移 dry-run、apply、幂等恢复命令 | 已观察到 `migration_cli.go` 实现；真实环境结果待验收报告 |
| 停写/发布/完整回退命令 | 真实候选 `a1c8a940...` 的正常切换/幂等/回退/启动失败自动恢复已通过，见 `.local/integration/rehearsal/final-report.json`；生产尚未由本演练迁移或部署 |
| 当前主节点现场基线 | 已由负责人只读核验，见 `.local/acceptance/production-baseline.json` |
| 测试结果、EXPLAIN、性能/包体数据 | 待实际运行 |
