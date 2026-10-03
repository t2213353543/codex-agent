# 错误请求与缓存优化：发布证据独立复核

本报告固定于 **2026-09-28 12:18:41 UTC / 北京时间 20:18:41** 的本地证据快照。共享工作区仍在修改；后续产物不得自动套用本报告结论。本次没有连接生产、执行发布或重跑完整测试，没有读取凭据或客户请求正文。

机器可读结果：`.local/collab-a-20260928/release/evidence-review-20260928T121841Z.json`。其中保留源文件差异、旧/新摘要、门禁引用核验、历史部署回执与审查边界。

## 当前可证实的完成范围

| 工作目录 | 本地证据 | live / 部署证据 | 可以作出的结论 |
| --- | --- | --- | --- |
| `.local/model-routing-release-20260928` | `acceptance.json` 通过；Go 4002 个测试/子测试 pass、444 skip、无 fail；前端 197 pass | 不可变包验证通过；BPS live 通过；`activate.json` 为 deployed；生产四条兼容路由均 HTTP 200 completed | **0.4.9 已有历史部署闭环**，仅适用于该包的源码和二进制 |
| `.local/attachment-release-20260928` | Go 3986 个测试/子测试 pass、443 skip、无 fail；前端复用记录为 196 pass；没有独立 `acceptance.json` | 图片 live 测试由随后 0.4.9 的完整 BPS 验收覆盖 | 这是先期验收工作目录，不能称为独立完成发布；图片修复已进入随后 0.4.9 的已部署包 |
| `.local/cache-scope-release-20260928` | Go 4019 个测试/子测试 pass、446 skip、无 fail；前端 198 pass、build 与三个 browser gate 通过 | 目录内尚无 `acceptance.json`、`postdeploy.json` 或发布回执；`postdeploy.py` 只是脚本 | **0.4.10 仍处于整合验收阶段**，不能据此声称已部署或自然流量改善 |

Go 数字统计 JSONL 中带 `Test` 的完成事件，包含子测试，不包含包完成事件。六个包均有 pass/skip 终态。三个目录的 `final-vet.log` 均为零字节；空日志本身不记录进程退出码，完整 vet 成功结论应引用封好的验收记录。

`verification_scope=powerpoint_bps`，0.4.9 的 `official_live=false` 且明确记录退役功能 `official_app_server`、`hosted_codex_tasks`。这不是所有历史运行模式都通过 live 的证明。

## 0.4.9 历史部署闭环

- 发布 ID：`2bfb81eef21c5d945dc5d6fc7aa5669c5f078cbcd64bf3889ba649be28aa64eb`。
- 源码摘要：`70ab5d2762249c034feae447b1be4bb8c3504737a1c2cb9eb271fa11bb47850f`。
- 二进制摘要：`3d00cac386b7104d20f2adfccde5f583984b5206c60dfd92bb41c9e1c9b1607f`。
- 包目录：`.local/reliability-release/2bfb81eef21c5d945dc5d6fc7aa5669c5f078cbcd64bf3889ba649be28aa64eb/package/`。

本地纯校验调用 `validate_evidence(acceptance, source, "0.4.9")` 通过。包内 acceptance、source-manifest、manifest 的源码身份一致；20 个封装 evidence 副本的 SHA-256 全部匹配；acceptance、source-manifest 和两份发布执行脚本均与 manifest 摘要匹配。本次不重新执行二进制或重连生产。

同级 `preflight.json` 记录候选二进制摘要，`activate.json` 记录 `deployed=true`。`natural-observer-summary.json` 保存 11:45:21 UTC 启动进程的可执行路径和相同二进制摘要，`postdeploy-routes.json` 于 11:46:00 UTC 记录四条生产路由成功、真实返回模型及 usage 已知。由此可证明当时确已运行该包，不能用这一历史回执断言当前生产仍未变化。

完整 BPS live 目录 `.local/reliability-modes/live/20260928T113519.538090Z/` 的结果为 `exit_code=0`，包内引用摘要正确；日志明确包含 `TestBPSLiveInlineImageAttachment`、HTTP 集成、WebSocket 集成和兼容模型路由的 PASS。图片修复的上线依据应指向这一包及其回执。

工作目录有两个容易误读的差异：

1. `model-routing-release-20260928/final-source.json` 的摘要为 `5b7750a930d6dd2c15200d1d474a18c165b7adc9fc14e6a04a9874eed069f370`，与最终 acceptance 不同。包内清单相对它仅新增 `tools/bps_routing_postdeploy.py`，原清单文件均一致，属于最终封包时纳入发布后工具；不能把两个摘要当成同一个快照。
2. 工作目录 acceptance 的 20 个原始引用中，`frontend/test-results/settings/results.json` 已被后来运行覆盖，摘要不再匹配。不可变包内对应 evidence 副本仍匹配，因此历史发布闭环未被破坏；不要再从共享原路径重新取这份历史证据。

## 缓存新版的源码边界

`cache-scope-release-20260928/final-source.json` 摘要为 `4dad81002936e81ed14037a4e79a76e85907180b4e5eb8b8a1bc0dbd65f44c4b`，包含 1201 个文件。审查时当前清单为 1205 个文件，摘要 `eb79b92cf861a82458b63c5c0fd414df239257f4273a1e4c14c06cfdecc16a21`。

相对已测快照，审查时已有五个文件发生变化：

- `internal/server/admin_usage_management.go`
- `internal/server/admin_usage_statistics_pg.go`
- `internal/server/gateway_bps_cache_live_test.go`
- `internal/server/gateway_bps_websocket.go`
- `tools/cache_observation_pg_linux_check.py`

另新增四个测试文件：`collab_cache_boundary_20260928_test.go`、`collab_error_boundary_20260928_test.go`、`gateway_bps_terminal_boundary_test.go`、`official_usage_repair_cache_known_test.go`，均位于 `internal/server/`。这些是正在整合的协作修改，不是历史测试失败，但旧 Go/race/live/PG 证据不能直接升级为当前代码已通过。

缓存新版的前端源清单 240 个文件仍与当前一致，`frontend-gates.json` 的七份引用均匹配，产物集合与当前 `frontend/dist` 也完全一致。可在该作用域不再变化的前提下复用这一前端门禁。价格 parity 是三个精确源文件未变时复用的 588 例历史验证，并非本次重新跑了 588 例；复用不能证明其他后端修改通过。

## 缓存效果与文档口径

`natural-traffic.json` 和 `natural-traffic-followup.json` 以 11:46:00 UTC 为起点，在 11:48:17 / 11:50:10 UTC 都只有 **0 个完成请求**，命中率为 null。它们既不证明无错误，也不证明缓存改进。

90 分钟汇总于 11:50:47 UTC 显示 60.3236% 读取率；11:56:04 UTC 的后续汇总为 59.5646%。这些窗口跨越 0.4.9 的启用时点，不能归因为这次发布，更不能归因为仍在验收的 0.4.10。读取率应保持同一批已知样本的 read/input，cache creation 单列；unknown 不能补零，creation 不能加入命中分子。

`docs/CACHE-AND-STREAM-RELIABILITY-20260928.md` 的发布回执明确属于 0.4.7；它记录发布前已有超过 70% 的读比例，这不是新版优化收益。`docs/BPS-MODEL-COMPATIBILITY-20260928.md` 所列早期三路由 live 也只是中间证据；最终四路由结果应引用 0.4.9 的 release-summary 与 postdeploy-routes。已有文档没有为 0.4.10 提供部署证明。

## 封版工具发现与交接

以下问题已经报告协调者，并交由会话 C 负责门禁修复；本审查没有修改发布工具。

1. `tools/reliability_acceptance.py:37` 的 `matched()` 只遍历当前 backend 清单，遗漏“旧快照中的文件后来被删除”。只读 AST 提取真实函数并用内存 fixture 验证：当前只有未变的 `main.go`，旧清单额外含已删 `_test.go` 时仍被接受。`runtime_matched()` 和 BPS live 的测试文件比较也存在同类单向比较。应按作用域比较双方完整集合，保留有依据的明确豁免；新增测试当前会被正确拒绝。
2. 同文件 `114`、`116`、`122` 继续读取共享 `frontend/test-results`；`135` 对当前 dist 重新计算摘要，却没有对照 `work/frontend-gates.json` 中已验证的 artifact/source/evidence 清单。并发覆盖会造成证据串用。应读取 work 内固定副本并核验已有门禁的完整源清单、产物清单和引用摘要。本次旧 settings 引用被覆盖已经证明共享路径确实在变化；没有据此断言历史不可变包存在错误。

发布负责人接下来应固定整合后的源码，纳入这轮 helper/cache/terminal 修改的针对性结果，为受影响作用域补齐最终 Go、race/Redis、PG 和 BPS live 证据，再生成 acceptance。只有不可变 package、激活回执、运行二进制身份及部署后验证对应同一版本，才能将 0.4.10 状态更新为已部署。自然流量效果需要部署之后另开时间窗和足够样本。

复查这份固定审查快照，不触发测试或生产操作：

```powershell
python -m json.tool .local/collab-a-20260928/release/evidence-review-20260928T121841Z.json
```
