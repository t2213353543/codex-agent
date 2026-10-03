# B 组：WebSocket 终态与缓存已知标记修复

任务 `01a0e7ed-44f0-7111-8bf2-30bb322fd170`。本组修改四个文件，供主会话统一冻结与发布；没有发布服务或修复生产历史数据。协作入口为 [ERROR-CACHE-COLLABORATION-20260928.md](ERROR-CACHE-COLLABORATION-20260928.md)。

## 已复现并修复

1. BPS 已接受 `response.done` 和 `basispoints.response.cancelled`，但 WebSocket writer 没有标记终态。真实本地 HTTP→WS 连续两轮回归分别复现 `stream_busy`、追加 `upstream_stream_interrupted`。现在两类事件也会释放 lane；上游事件正文保持原样。
2. 官方 rollout 用量修复遗漏 `cached_tokens_known`，明确零读或原标记为 false 的读数仍可能被统计为 unknown。现在新修复写入已知标记，旧修复在完整证据和 durable usage 一致时只补标记。dry-run 不写；已知完整记录重复运行不变；冲突仍拒绝；仅补标记不会重写 tokens、冻结费用、时延或触发 token 聚合重建。

## 文件与 SHA256

| 文件 | SHA256 |
| --- | --- |
| `internal/server/gateway_bps_websocket.go` | `dd4670cefbe669b36cc50f3852c0b949c4b70e24d990da48a693c72820617990` |
| `internal/server/gateway_bps_terminal_boundary_test.go` | `f3986634f88d6e140eded6b3232f14179c67255914fe5273de52170e2f1c9f4b` |
| `internal/server/official_usage_repair.go` | `f0b2d5da39555f9528fac70d5aac503b84e7c9cb945147f8872ce36ce13f7969` |
| `internal/server/official_usage_repair_cache_known_test.go` | `fcbb1a99b96e96d7504030d7865f6e2744f42e6964bca51ab5fa32629a085391` |

## 验证

- 保存真实修复前红测：WS 两类终态失败、cache known 七个子用例失败。
- WS 两轮回归重复 20 次通过（40 子用例）；13 项相邻 WS 回归通过。
- 缓存修复、读写统计、导出相邻本地测试通过。独立审查未发现阻断问题。
- 隔离 PostgreSQL 三项 repair 事务测试全部通过、零跳过，包含新增 legacy dry-run/commit/conflict 两个子用例。确认只回填已知标记，不改 durable usage，重复执行幂等。源码未漂移，临时远端测试二进制已删除。
- 冻结源码全量 `go test ./...` 与 `go vet ./...` 均通过：1418 个顶层测试通过、214 个环境性跳过；含子用例 4063 通过、451 跳过，零失败。测试后工作区源码与受测快照一致。普通本地测试的 live/PG/Redis 跳过如实保存在报告中；本组 repair PG 已另外实测通过。
- 首轮冻结器漏复制 Python fixture，两个 bridge 测试曾失败，其余 1416 个顶层测试通过。已修正本组冻结器复用项目既有 `BRIDGE_FIXTURES`，原始失败报告保留，以上最终通过结果来自修正后的完整快照。

最终本地证据：`.local/cross-session-20260928/01a0e7ed/local-20260928T122554.715243Z/report.json`；含 fixtures 的快照 SHA `62427f7cd9010916c1b6bea96a38fe30d7cb09734cc7f01527716090a448d1e8`。综合交接回执 `.local/cross-session-20260928/01a0e7ed/handoff.json` 绑定两份报告哈希及四个修改文件哈希。本组代码冻结、验证完成，后续联合发布由主会话负责。

PG 证据：`.local/cache-observation-pg-linux/20260928T122144.191588Z/report.json`；冻结后端 SHA `220ebbfcade5a24af1f1f1f219b8571ecb073fa2d4d6f968e25c8cc916e32a46`，实际执行 binary SHA `49cfb23c4517d04d81c69f26ff59b409a77392c87bd59938d11069c834d0e79b`。

证据目录：`.local/cross-session-20260928/01a0e7ed/`。包括原实现副本、红绿日志、`terminal-minimal.patch`、`cache-known.patch`、SHA 回执。

本地重跑（自动冻结源码，隔离 live 配置）：

```powershell
python -B .local/cross-session-20260928/01a0e7ed/verify_local.py
```

三项 repair PostgreSQL 事务验证：

```powershell
python -B .local/cross-session-20260928/01a0e7ed/run_repair_pg.py
```

PG wrapper 复用既有隔离验证器，只覆盖选择的测试集合，不改验证器源码。新增 `TestOfficialUsageRepairCacheKnownPGLegacyDryRunCommitAndConflict` 不在该验证器默认八项列表，最终发布门禁需显式纳入或使用上述 wrapper。

## 已交接的其他发现

- `cache_rate_report.py` 对控制请求/WS warmup 的过滤口径差异已交 C 组。
- 非流式错误详情丢失由 A 组独立复现并认领。
- metadata-only 客户端在共享缓存 hint 模式下的观察可能归入弱 scope；跨线程 `history_changed` 不能直接解释为续聊异常。此为观察边界，不是串话证据。
