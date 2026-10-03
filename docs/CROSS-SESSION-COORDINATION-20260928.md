# 错误修复与缓存优化跨会话协作（2026-09-28）

统一协作入口已由较早加入的会话建立：[ERROR-CACHE-COLLABORATION-20260928.md](ERROR-CACHE-COLLABORATION-20260928.md)。请以该文件的分工与认领为准。本文件只保留本会话索引，避免双重认领。现有主会话继续负责在途功能与统一发布。

## 协作会话 01a0e7ed-44f0-7111-8bf2-30bb322fd170

- 工作：独立检查错误终态与缓存统计边界，运行本地合成回归，向统一发布提供可重跑证据。
- 当前文件认领：`internal/server/gateway_bps_websocket.go` 的终态枚举最小修复、新增 `gateway_bps_terminal_boundary_test.go`；`internal/server/official_usage_repair.go` 的缓存已知标识最小修复、新增 `official_usage_repair_cache_known_test.go`；`.local/cross-session-20260928/01a0e7ed/`。
- 并行只读审查：错误传递/HTTP/WS、缓存 key/统计/续聊。
- 当前候选问题（尚待复现）：`gateway_bps_websocket.go` 的终态枚举少于 `internal/bps/stream.go`；`cache_rate_report.py` 与服务端的控制请求过滤口径可能不一致。
- 边界：本会话的本地结果不代表生产发布完成；共享目录不是 Git 仓库，结果须注明源码哈希及运行期间的外部改动。

缓存报告的控制请求过滤差异已交接给统一入口中的会话 C，本会话不修改其认领脚本。

## 协作会话 01a0e7eb-3f11-7882-85b7-91e04060776b

- 20:15 +08 已看到以上候选发现，主动避开重复：WS 终态枚举与 `cache_rate_report.py` 控制请求过滤交由 01a0e7ed-44f0 处理。
- 本会话认领两个局部复现：① BPS `stream:false` 遇 `response.failed` 丢失客户端错误 code/message/param；② `managedUsageTotals.add` 与对应 PG 聚合对 cache creation 缺少控制请求过滤，与总览口径不一致。
- 测试文件独占：`internal/server/collab_error_boundary_20260928_test.go`、`internal/server/collab_cache_boundary_20260928_test.go`。运行文件暂不写；先复现，再在此更新具体修改范围。
- 三个子任务分别完成错误边界、缓存创建口径、发布证据复核。报告分别为 `docs/COLLAB-ERROR-BOUNDARY-20260928.md`、`docs/COLLAB-CACHE-BOUNDARY-20260928.md`、`docs/COLLAB-RELEASE-EVIDENCE-20260928.md`。
- 本会话不占真实模型探测或生产发布窗口。发布仍由现有主会话统一；详细任务状态见 `docs/ERROR-CACHE-COLLABORATION-20260928.md`。

### 20:17 +08 实际复现与最小修复认领

- `TestCollabErrorBoundary20260928JSONFailureDetails`：SSE 对照 PASS，JSON FAIL，HTTP 502 的 generic proxy_error 丢失上游结构化 code/message/param。认领新增 `internal/server/gateway_bps_terminal_error.go`，以及 `gateway_bps.go` 中 `!prepared.ClientStream` 错误分支的单一调用接入；不碰其他正在进行的 schema/路由/缓存改动。
- 缓存创建控制请求过滤认领 `internal/server/admin_usage_management.go` 的 CacheWrite 分支和 `internal/server/admin_usage_statistics_pg.go` 的对应 SQL 聚合；测试先行，其他逻辑保持。
- 新复现测试会改变全树测试源码哈希。正在冻结的 cache-scope 候选请继续绑定其已冻结源码；不要把旧全树指纹描述为已覆盖此处新修复。统一主会话可在本轮候选之后接续合入并验证。
