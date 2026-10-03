# 网关环境优化联合发布清单

## 当前状态：已功能发布

2026-09-18 12:00:54 +08 已完成003生产迁移与固定956/a66后端发布，前端47资源未变；健康、认证、数据、实际重启和生产网页验收通过。当前事实、具体release及回退命令以 [发布记录](GATEWAY-DEPLOYED-2026-09-18.md) 为准。以下段落为不同时点的历史准备/阻塞记录，不能把此前“未迁移/未部署”当当前状态。高并发容量仍未验证，不把功能部署标成完整环境优化目标已完成。

## 11:16 阻塞复核

本轮继续只读检查了最新源码交付、正式测试和网络。最新在途 native HTTP 快照属于源码会话后续功能，不替换当前已验956候选；正式 `accounts_shutdown_test.go` 仍未加入已交接的PG密钥/默认设置初始化修正，没有可用于复验的正式交付。

11:16 直接 TCP 探测 `43.225.196.170:12865` 在8秒内超时，127.0.0.1:17897仍没有监听。此前恢复项目代理的启动动作被平台拒绝，本轮没有重启它、换通道绕过或代跑已拒绝的真实四档。该网络阻塞已跨多轮重复核验；本机可执行的迁移、回退、浏览器、管理性能和源码补充检查已完成，不再用重复绿测代替实际推进。

继续生产工作所需外部变化：可直连当前主节点的网络恢复；源码owner交付正式shutdown夹具修正；真实四档测试的执行限制解除后才能取得其数据。当前保持不可发布，生产003未迁移、956未部署、上游错误率改善未验证。停止自动空转不等于目标完成，所有固定产物和证据保留用于恢复后重查。

## 11:14 本轮结果

当前固定版本仍为956/a66（不是其它会话继续开发的共享目录）。本机新后端与原47前端的13项真实API浏览器、准确旧binary对003的6项回退兼容、新版本159网关+73管理race均通过。补充PG17中唯一shutdown fixture初始化失败，定向初始化对照已证明完整生命周期可通过，正式测试仍待owner修正与复验，不能把原失败报告置绿。

代表规模性能：1000禁用合成账号、50000日志及events、6250000tokens，600次串行管理HTTP全部200，7份实际EXPLAIN。数据见 `.local/performance-local-joint/20260918T030229-6a9ed7ff53/`。

| 管理端点 | P50 / P95 ms |
| --- | --- |
| 账号列表，50行 | 6.55 / 30.67 |
| 新管理账号列表，50行 | 6.73 / 31.31 |
| 日志列表，50行 | 17.98 / 42.49 |
| 概览，缓存命中 | 14.57 / 27.01 |
| 概览，指定范围不缓存 | 70.68 / 89.11 |
| 概览，每次移除独立缓存键 | 65.51 / 82.83 |

日志排序实际全范围扫描+top-N，OFFSET950账号关联子查询各1000loops，已交管理owner优化评估。这里只证明该数据规模下的串行管理结果，不能当生产50k等同、也不是上游并发容量。固定冷/热口径、原始100个样本和SQL身份均在报告中；文件系统/PG buffers冷缓存未测。

发布审计新增源码到构建产物交叉绑定、原角色测试集合下限、浏览器全部结果/JS errors/HTTP失败一致性校验。24项审计单测通过，三个实际反例均拒绝（`.local/release-gate-review/20260918T031033.214009Z/report.json`）。最新版统一执行 `.local/gateway-acceptance/20260918T031236.944080Z/report.json`：六组本地范围通过，审计10 pass / 3 blocked / 1 fail，整体非零。通过率不是发布授权。

本轮PG/Redis/browser/performance临时容器、卷和网络全部清理；旧binary本地证据保留，remote旧schema未操作。清理点查 `cleanup-report-20260918T030418.json` 与性能报告cleanup=true；结束时 `docker ps` 无本轮遗留。生产003迁移和新版本部署均未执行，已部署的工作台同步仍需网络恢复后验证CAS更新状态。

## 11:03 当前候选

后续固定候选已更新为 `956fc90be91a922ac1593b283102bc5149674dac337564e6023479ec8e60a6f9`，Linux binary `a66c7be3de344221d6433a6bc7140523d95a6a068fd2312757c80f2dc50c073d`。独立封装 `.local/joint-candidate/20260918T024841.878837Z/joint-candidate.json`；管理 5a 基线 166 个文件未变，新增 3、修改 5 个网关文件，包含 compact 超时修复及实际 header/error 观察接入。旧 5a 的缺陷/结果仍是历史证据，不套到新二进制。

| 同一 956 候选的实际检查 | 当前结果 |
| --- | --- |
| 网关 Linux PG18/Redis8-race | 159 顶层 / 379 通过事件，0 fail/skip |
| 管理 Linux PG18/Redis8-race | 73 顶层 / 124 通过事件，0 fail/skip；不是只继承旧管理报告 |
| 其余数据库边界 Linux-race | 17 项中 16 pass / 1 fail，唯一失败是 shutdown 测试旧初始化导致刷新前 401，不是运行时令牌丢失结论 |
| shutdown 定向诊断 | 只修独立测试夹具的 PG 管理密钥及默认设置后，完整断连/SIGTERM/AT与RT/日志统计恢复通过；正式 fixture 待 owner 修复，历史失败不抹除 |
| 精确旧 binary 对 003 回退兼容 | 6 项实际通过，`.local/migration-003-local/20260918T024916-72594cd8d7/report.json` |
| 新 binary + 生产旧 47 资源实际 API 浏览器 | 13 项通过，桌面/手机 11 张截图，`.local/legacy-candidate-browser/20260918T025248-5f0e36/report.json` |
| 全库普通检查 | 344 顶层 pass / 66 环境 skip；vet 通过，不能把 opt-in 的跳过算实网通过 |
| 生产操作 | 仍未迁移 003、未发布新 binary/UI。已恢复的是本机 Docker，不是服务器连接 |

当前统一命令：

```powershell
python tools/gateway_acceptance.py --release-config docs/GATEWAY-ACCEPTANCE-INPUTS-2026-09-18.json
```

配置只包含固定候选和公开证据路径，无凭据；统一入口核对 snapshot 身份、运行合成测试和只读发布审计。最新执行六组本地测试通过，但发布审计仍非零，`local_tests_passed=true` 不等于 `passed=true`。报告 `.local/gateway-acceptance/20260918T030000.191024Z/report.json`。独立总审计 `.local/gateway-release-check/20260918T025645.421197Z/report.json` 为 9 pass / 3 blocked / 1 fail，保留上述 fixture 失败和上游实测/生产操作缺口。

本机 Docker 环境已实际恢复，旧 binary 和候选验证均使用全新合成 PG/Redis。此前关于“本机不能执行 Linux/PG”的段落只描述当时状态；生产 SSH 与真实四档压力仍未恢复执行。

## 10:40 新增复现结论

`5a818844...` 候选存在已复现的客户端工厂合同缺陷：当直连客户端注入非 `*http.Transport` 的自定义 RoundTripper 时，显式 compact 总超时未应用，客户端仍保留默认 20 分钟。原生 Transport 的 direct/proxy 实测通过；实际 compact 主请求另有 `doCompactRequestWithTimeout` 总期限保护，因此**不能称所有生产 compact 超时失控，也不是已证实的生产 502/503/429 根因**。

冻结候选上的真实断言失败，只有覆盖 owner 的 `gateway_client_pool.go` 后同测试通过。基线文件 SHA `27b924c5a46e18945a6c1b6a8b4927d66c372e39f8f5eb39259d737343b18bc1`，修复文件 SHA `234a6bebee0550a68d053f1efa0da145805fb47fff210769fe5f46d54e42e6ba`。证据 `.local/compact-timeout-regression/20260918T023719.775475Z/report.json`；已发布前候选 ELF 未被修改，必须由 owner 重新冻结包含修复的完整候选，不能把 overlay 通过写成原二进制已修复。

最新发布门禁为 **5 pass / 6 blocked / 1 fail**，报告 `.local/gateway-release-check/20260918T023849.294847Z/report.json`，新增 4 项审计回归后共 15 项工具单测通过。下面 10:32 的历史结果仍保留作为当时执行记录，不再代表当前候选没有已知缺陷。

10:40 已实际执行带 `--release-audit` 的统一入口，六组本地范围通过、发布审计 blocked，整体按预期非零：`.local/gateway-acceptance/20260918T024024.147451Z/report.json`。`local_tests_passed=true` 与 `passed=false` 分别记录，缺失发布验证和新发现的候选缺陷不会被合并成通过。没有重新启动任何真实压力、代理、迁移或发布进程。

记录日期：2026-09-18。本清单区分已验代码、隔离迁移和生产部署，不作为已发布凭证。

## 已固定的版本

| 对象 | 精确标识 |
| --- | --- |
| 当前待验 Go 归档 | `5a818844f613ad35602ac466c80d8949a2e7f7a180e96ae21c4cbd1573ce6d22`，owner 使用 compact JSON 的文件映射摘要 |
| 归档路径 | `.local/management-parity/linux-race-20260917T214152-2d29dc/source.tar.gz` |
| 归档 SHA256 | `52b8e1be2c67a85a787387fef156267d9e43e58b145a5c884f53137dad45cd11` |
| 候选 Linux 二进制 | 同目录 `codex-agent-linux-amd64`，`6ea3efc086d510399fe149def6b3e56941462f5531f8875967a7ed0c43e28918` |
| 最近成功读取的生产后端 | `a9bf68d1899ec1d58381c788b84cc63123c88cb8196d1acb0b4faafa1b65831d`，04:13 +08 |
| 生产 release 路径 | `/opt/codex-agent/releases/4d4c4dfd91de306b6d1f79bfba232df49c85d3a1dfdda06bbf199538a7322e38` |
| 此次后端发布拟保留前端 | 47 文件、1,896,250 bytes，映射 SHA256 `d87032e071e7c72343ccb178857d4e56a0200ea7af87aa3b262083e73d15d1c6` |
| 前端取证目录 | `.local/runtime-frontend/d87032e071e7c72343ccb178857d4e56a0200ea7af87aa3b262083e73d15d1c6/` |
| 管理新 UI | 54 文件包仅作为单独交付，不混入本次保留前端的后端候选 |

发布工具 v2 的 source digest 使用默认 JSON 分隔符，与 owner compact digest 可能不同。只能通过同一完整 `files` 映射核对别名，不能把两个不同编码摘要误当作源码不同，也不能只比较显示用的短 hash。

## 发布门禁

| 验收范围 | 目前证据 | 状态 |
| --- | --- | --- |
| 最新归档普通全库/vet/双平台构建 | 管理冻结目录 `local-verification.json`，333 顶层 pass / 65 依赖环境 skip | 普通检查通过，跳过项不算通过 |
| 同归档管理 PG/Redis-race | `report.json`，73 顶层 / 124 通过事件，0 skip/fail | 通过 |
| 同归档网关本地专项 | `.local/frozen-gateway-race/20260918T022116-6f87b7/report.json`，154 pass / 18 skip，vet 通过 | 本地通过，整体 blocked |
| 同归档网关 PG/Redis-race | 服务器连接不可用；本机没有现成 C 编译器 | 未执行 |
| 003 隔离迁移 | `.local/migration-003/20260917T201629.573381Z/report.json`，13 详细检查，8 发布检查 | 通过，仅隔离库 |
| 精确旧 binary 对 003 的回退兼容 | `.local/migration-003-rollback/network-blocked.json`，仅复制/hash | 未完成，不发布 |
| 候选后端 + 保留前端实际 API 浏览器 | 需要独立合成 PG/Redis，与旧 47 资源一起运行 | 未执行，不发布 |
| 发布脚本失败恢复/封装 | `python -m unittest discover -s tools -p 'test_upgrade*.py' -v`，26 项替身测试 | 本地通过，不替代现场演练 |
| 30/50/70/100 上游真实压力 | 启动命令被平台拒绝，无压力进程 | 未执行，无性能数据 |

历史 `dc955...` 141 项网关 Linux-race 与 `bee29...` 五组环境 Linux-race 各自有效，但不为 `5a818...` 自动补齐证据。

统一合成验收命令：

```powershell
python tools/gateway_acceptance.py --snapshot .local/management-parity/linux-race-20260917T214152-2d29dc --release-audit
```

仅运行合成 source/token、loopback upstream 和只读发布审计；不会隐式开启真实压力、部署或迁移。当前发布审计会让统一命令保持非零，不把六组本地测试通过当作可以发布。省略 `--release-audit` 仅运行本地合成范围。

同一入口包含 Python→真实 Go 管理 HTTP 的六个桥接场景、18 项桥接语义测试、21 项压力/采样/编排替身测试、环境五组、26 项 upgrade 与 11 项发布审计单测。六个 HTTP 场景仅临时文件存储，不冒充 PG 事务通过；其中并发加入 RT 后提交旧版本返回 409、保留 RT 及 bridge checkpoint。显式 reauthorize 按已有契约替换整套凭据，未提供 RT 时不继承旧 RT，不能把它当作 OAuth refresh 的缺字段合并接口。

10:32 最终统一本地执行六组入口均 PASS，源/harness 前后摘要一致：`.local/gateway-acceptance/20260918T023156.140258Z/report.json`。真实 Go HTTP 六场景独立证据为 `.local/bridge-contract/20260918T022939.142921Z/report.json`。压力编排七项新替身测试证明采样失败/超时/运行版本变化不会被标记成功，子进程按期限终止并保留失败报告；未启动该编排器的实际压力进程。

查看当前完整发布门禁，不触发部署：`python tools/gateway_release_check.py`。当前返回 exit 2 / blocked，5 项通过、6 项待补；输出完整候选、证据 hash、两种源摘要别名和明确发布范围。该工具不会生成可发布的全 true acceptance。

最新总门禁证据：`.local/gateway-release-check/20260918T023229.694222Z/report.json`。owner compact 源摘要 `5a818844...` 与 v2 默认 JSON 摘要 `266990c592d1a28599f7ac7957d707c2b8d1e89bd472be7141cf3f27fd774153` 对应同一 171 文件映射，均已独立重算。

## 迁移与切换顺序

1. 恢复可用连接后，先重新只读核对运行 binary/PID/unit、47 个 HTTP 静态资源、数据库 ledger、加密 key ID 和源账号资格。不凭数小时前的状态直接部署。
2. 补齐同一归档的网关 PG/Redis-race、旧 binary 对 003 的实际读写/解密/重启、候选与旧前端真实 API 浏览器。所有账号均合成且禁用，不与生产共享刷新凭据。
3. v2 acceptance 必须明确绑定 source、binary、前端映射、迁移报告、旧 binary 回退报告及现场前端证据的完整 SHA256。缺项状态必须 blocked/fail；不可将本文件转换成手填全 true 的发布凭证。
4. `tools/upgrade.py prepare --schema v2` 只接受显式冻结源码、候选 ELF、保留前端与全部证据。v1 仍只支持原 000–002 unchanged schema，不能用 v1 发布包含 003 的二进制。
5. stage 仅上传不可变 artifact；preflight 验证独立 PG `127.0.0.1:25433/codex_agent`、迁移角色 `codex_agent_migrator`、运行角色 `codex_agent_runtime`、Redis `127.0.0.1:26380` 和 `codex-agent:production:`。迁移配置路径固定，秘密不能出现在命令参数/报告里。
6. activate 先校验并备份旧 unit/环境/当前 PG 快照，再短暂停写并完全排空旧 service；只有确认旧进程/cgroup 清空后才调用候选 `migrate`，随后检查 000–003 精确 ledger 和运行权限。新旧进程不能同时刷新。
7. 003 已提交则明确记录 schema_migrated=true；新服务启动或健康失败时恢复旧 unit/binary，但沿用最新 PG 数据。不得恢复旧 dump 丢弃新令牌/新日志，也不得重跑旧 JSON 导入覆盖数据库。
8. 上线后分别验网页/认证/账号 CAS/日志统计/数据库写入/重启与 bridge AT 更新及幂等。最后才把迁移、部署、业务验收三个状态分别更新。

`003_management_cleanup.sql` SHA256 为 `31ca0e1e58bb846c9691bd505718a1c13ef3aba03f9abf157034317e85cde56e`；仅新增清理/使用快照目标表及索引，旧三迁移不变。实际迁移器逐 SQL 文件事务，003 失败不撤销此前已提交的 000–002。具体故障注入、权限和幂等证据见 `MIGRATION-003-ACCEPTANCE-2026-09-18.md`。

## 回退与运行信息

实际网页地址：`http://43.225.196.170:18879`。最近成功 health/live 与 health/ready 均 200；记录时间 04:13 +08，当前连接阻断，不能声称实时健康已查。

新候选尚未部署，因此没有可填的“本次 release ID”或已生效的 binary 回退命令。将来 v2 实际发布后，使用该次不可变 manifest 执行 `python tools/upgrade.py rollback --release <that-upgrade-manifest.json>`，不能对别的 release 运行。该路径只回退应用，保留当前数据库。

已部署的独立账号同步可单独停止：

```powershell
python tools/workbench_bridge.py stop
```

它只停用 `codex-agent-workbench-bridge.timer/service`，保留已导入账号、凭据、日志，不重启主服务、不修改 18876 支付流程或其他项目。此命令也需要可用 SSH；本文没有声称当前已经执行停用。

## 明确限制

- 本次没有证据证明 IP/UA/TLS 造成模型能力下降；502/503/429 按本地准入、代理、传输、上游及流内终态分别观察。
- 18876 持有唯一 RT 刷新职责，Agent 接收 AT-only；旧 runtime 没有 ID+version CAS 端点，持续 AT 更新仍未通过。账户首次同步不是持续更新成功。
- 源退款/失效会改变账号可用数，初次导入 5 个不等于此刻仍可用 5 个。04:18 已按明确源状态停用一个，当前状态需重查。
- 临时代理 17897 已退出；直连及 7897 不可用；恢复项目代理进程被平台 `blocked by policy` 拒绝。没有换执行通道或委派绕过，生产未迁移 003、未切换候选。
