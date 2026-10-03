# A 组：0.4.10 统一冻结后证据复核

复核对象是主会话 20:31 CST 宣布冻结、20:32:24 启动的新一轮验证。仅核对本地快照、已有执行结果和经过筛选的本地进程元数据；未编辑 backend/frontend/test/发布工具源码，未启动重复测试、远端连接或生产调用。

## 已确认纳入的源码

截至 **2026-09-28 12:40:56 UTC / 20:40:56 CST**，`cache-scope-release-20260928/final-source.json` 与当前 `source_manifest()` 完全一致，均为 1215 个文件：

`93193eda3e9321ed3ede224cfba39fbdbdc0778bb9f628f7a47185adf85190e8`

- A 的 `gateway_bps_terminal_error.go` 和 `gateway_bps.go` 单一调用、两个 cache creation 聚合修改均在冻结清单且与交接摘要一致；两个 collab 测试文件也在清单。
- B 的 WS 终态、历史 cache known 修复及其两份测试均在冻结清单。
- C 的 `reliability_acceptance.py`、`frontend_release_gates.py`、PG freeze runner 修复已纳入。C 最终交接中的 16 份源码摘要全部匹配本轮冻结清单。
- custom 工具图片修复和 `gateway_bps_tool_attachments_live_test.go` 已纳入。

新 PG、race、live 三份独立受测清单各含 650 个文件，按发布校验器的 `backend`、`runtime`、`powerpoint_runtime` 三个作用域与最终冻结清单比较均无新增、删除或修改差异。完整源码摘要与作用域摘要不同是清单范围不同，不能仅比较裸摘要判断漂移。

## 已形成的执行证据

| 验证 | 本轮目录/证据 | 独立核对结果 |
| --- | --- | --- |
| Go | `.local/cache-scope-release-20260928/final-go.jsonl` | 4065 个测试/子测试 pass、451 skip、0 fail，六个包均有终态；A/B 本地回归通过 |
| 前端 | 同目录 `frontend-gates.json` | 实际只读调用 `verify_report()` 通过：204 unit、build、三个 browser gate；七份 work 内证据、完整源码集合和当前 dist 摘要均匹配 |
| PG | `.local/cache-observation-pg-linux/20260928T123224.168735Z/` | `status=pass`、`exit_code=0`、10 项 selected tests；含子用例 18 pass、0 skip、0 fail；`source_changed_after_freeze=false` |
| BPS live | `.local/reliability-modes/live/20260928T123224.174866Z/` | `exit_code=0`，日志最终 PASS；六组所需测试及 20 个测试/子测试完成事件全部 PASS |
| race / Redis | `.local/powerpoint-migration/race/20260928T123224.174502Z/` | `status=pass`、`exit_code=0`、`race_warning=false`、`source_changed_after_freeze=false`；四项 Redis 全部 PASS |
| acceptance | `.local/cache-scope-release-20260928/acceptance.json` | `passed=true`；源码与冻结清单一致；21 个 evidence 引用摘要全部一致；纯本地 `validate_evidence(..., "0.4.10")` 通过 |

PG 的 `selected_tests` 明确包含：

- `TestPGCollabCacheCreationControlExclusion20260928`，`measured_zero` / `measured_positive` 均实际 PASS。
- `TestOfficialUsageRepairCacheKnownPGLegacyDryRunCommitAndConflict`，`matching_durable_usage` / `conflicting_durable_usage` 均实际 PASS。

最终本地 Go 结果中，这两项 PG 父测试也会显示 PASS，但其子测试因未配置 PG 而 SKIP。因此本报告以独立 PG runner 的 18 条真实 PASS 和零 SKIP 作为 PG 完成证据，不把父测试 PASS 单独当成数据库验证。

live 六组为：`TestBPSLiveCodexIntegration`、`TestBPSLiveCodexWebSocket`、`TestBPSLiveSharedAPIKeyCache`、`TestBPSLiveCompatibleModelRoutes`、`TestBPSLiveInlineImageAttachment`、`TestBPSLiveToolInlineImageAttachment`。启动中的 Windows 进程参数与最终日志均包含本轮要求的新增共享缓存、custom 工具图片测试。

## 门禁修复核对

`source_differences()` 已比较同作用域完整键并集；`require_runtime_sources()` 和 `powerpoint_fixtures()` 已采用对称集合比较。原有明确批准的单文件纯测试豁免仍保留。

acceptance 已通过 `verify_frontend_report()` 校验 work 内冻结报告、七份固定证据、源文件和完整产物集合；读取的是 `work/settings-results.json`、`work/metrics-results.json`，并直接绑定该报告内已验收 artifact 清单。本轮实际校验通过，不再从共享 `frontend/test-results` 重新绑定结果。

本轮没有发现 A/B 新增 PG 被遗漏、C 修复未纳入或受测作用域源码不匹配。

## 活跃执行与完成状态

12:35 UTC 通过 Windows CIM 实际查到 20:32:24 创建的 PG Python PID 13276、race Python PID 38476、live Python PID 38272，以及 Go/server.test 父子进程；不是根据状态文件的 running 字样推断。

12:39:04 UTC 中间快照中只剩 race Python PID 38476，PG/live 已有对应成功回执。12:40:56 UTC 最终快照中相关测试进程全部退出，三组均已形成成功结果。活跃的本地 SSH 调度进程本身不能证明远端测试仍在运行；本报告等待正式结果才将 race 更新为通过。

`final-vet.log` 在 12:34:46 UTC 已被本轮重新写入且为空；主会话统一交接于 20:37 明确记录 Go/vet `exit0`、`source_changed=false`，最终 acceptance 的 vet 门禁为 true。仅凭空文件本身不能独立恢复进程退出码。

最终机器可读固定快照：`.local/collab-a-20260928/release-followup/review-20260928T124056Z.json`；12:39:04 的中间快照保留，可查看 race 完成前的真实状态。只读复核命令会生成新的带时间快照，不启动测试或远端访问：

```powershell
python -B .local/collab-a-20260928/release-followup/inspect_frozen_release.py
```

最终 acceptance 的 backend 门禁为 649 个文件、前端为 241 个文件、runtime 为 226 个文件、BPS powerpoint_runtime 为 206 个文件；成本 parity 限定三个价格源文件。受测 backend 清单另含一个已登记的非价格嵌入资源，按现有发布作用域筛选后完全一致。`official_live=false` 与明确退役 scope 一致，不是一个被掩盖的失败门禁。

**0.4.10 本轮冻结源码的完整验收与引用闭环已成立，未发现协作用例遗漏或源漂移。** 后续发布状态见以下单独记录。

## 20:44 发布包与激活回执复核

主发布包：`da708caa5ae5776c79bc604d3df4286ddced2010b8440609375201f9005d58dd`，目录 `.local/reliability-release/da708caa5ae5776c79bc604d3df4286ddced2010b8440609375201f9005d58dd/`。

12:44:10 UTC 只读复核整个 package 和 `release.tar.gz`：manifest 所列 82 个产物全部 SHA-256 一致；包内无缺项或额外文件；压缩包成员与 package 完全一致；21 个 evidence 副本摘要全部正确。source-manifest、acceptance、manifest 的源码摘要仍为同一 `93193eda…`，纯本地 evidence validator 通过。

- 二进制 SHA-256：`b3c8acf1694ffe1c055f85b677a537a028435a8491874c8d07db36f0deb32dca`。
- manifest SHA-256：`5cf405a3b8cc3640061ce6eac119331823e1a4e9a41305ca2f2b2b0acacffe58`。
- `preflight.json` 为 `preflight=true`，候选 binary 和 manifest 摘要均与包一致。
- `activate.json` 于 12:44:05 UTC 写入 `deployed=true`，runtime PID 为 775841，可执行路径为该 release 下的 `codex-agent`，运行二进制摘要与 manifest 完全一致。
- 激活回执版本 `0.4.10`、模式 `powerpoint_bps`、settings_version 为 26、WebSocket 开启；实际 schema 与预检预期一致。

观察期间实际查到本地 `reliability_release.py activate` 进程 PID 1388，参数中的 release ID 与本包相同；随后进程退出并产生上述 deployed 回执。本审查没有运行发布命令。

包完整性机器证据：`.local/collab-a-20260928/release-followup/package-review-20260928T1244Z.json`。12:44 的包审查时尚无 postdeploy，以下以随后真正完成的回执更新状态。

## 20:47 部署后 scope 与八请求回执复核

已读取同一工作目录的 `postdeploy.json`，回执 `checked_at=2026-09-28T12:45:13Z`：版本 `0.4.10`、模式 `powerpoint_bps`、`cache_scope=api_key`、`settings_version=27`、`passed=true`。对照激活时 settings version 26，仅递增一次，符合这次 scope 设置动作。当前没有还在运行的 postdeploy handle，该步骤已经完成。

八个请求均 HTTP 200 / completed / `shared_api_key`，八个 `usage_matches` 全部 true。两账号分别保持单一出站缓存键；账号 enabled/concurrency/priority/weight 配置一致检查通过。读取率 74.7102% 和创建 75,802 tokens 来自明确标注的 `synthetic_release_validation_not_natural_traffic`，不能据此宣称自然业务缓存命中率达到该数值。

preflight → activate → postdeploy 的版本、模式、settings 增量、schema、请求 scope 与用量核对全部通过。新机器证据 `.local/collab-a-20260928/release-followup/deployment-review-20260928T124708Z.json` 保存三份回执摘要与 15 项跨字段校验，`all_cross_field_checks_pass=true`。

精确边界：postdeploy 回执自身没有 runtime SHA/PID，八次生成期间的二进制身份依据相邻激活记录间接关联。随后运行状态快照已补足持续运行身份，见下一段；本审查没有为此新增生产调用。

## 20:49 后续运行身份与自然流量核对

`natural-errors-postdeploy.json` 的 runtime/settings 快照时间为 **12:46:03 UTC**：PID 775841、binary SHA `b3c8acf…`，进程启动于 12:44:04 UTC，active/running，`NRestarts=0`；同一份快照明确 `bps_prompt_cache_scope=api_key`、`settings_version=27`、模式 `powerpoint_bps`。这些字段分别与激活及 postdeploy 回执匹配。

`natural-postdeploy.json` 于 **12:47:00 UTC** 再次记录同一 binary SHA，统计起点为 12:45:15 UTC，workload 明确为自然流量：41 请求、32 成功、9 失败，32 个已知缓存读取样本、9 个读取未知请求，读取率 71.1902%、创建 691,488 tokens。已观察到 `shared_api_key`，但这段很短的发布后窗口不能单独证明相对于发布前的因果提升。

自然错误仍有 8 个 `rate_limit_exceeded` 失败终态和 1 个不支持 structured text.format 的 400。它们说明仍有真实错误需要后续专项处理，不是 preflight/activate/postdeploy 步骤失败。

时间口径提醒：`natural-errors-postdeploy.json` 的 `snapshot_at` 及 `since_process_start` 汇总属于 12:46:03；其中后来补入的 `error_groups_since_release` 最晚事件到 12:46:51。因此分组 9 个错误与较早汇总的 5 个错误不能按同一原子快照比较。

机器证据：`.local/collab-a-20260928/release-followup/runtime-scope-review-20260928T124854Z.json`，release binary、runtime binary、自然报表 binary、scope、settings version 和模式交叉核验全部通过。**本轮已形成冻结验收 → 不可变发布包 → 预检 → 激活 → api_key scope / 八请求验证 → 后续运行身份的完整证据链。**
