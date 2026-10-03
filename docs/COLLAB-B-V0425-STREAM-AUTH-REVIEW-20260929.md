# B 组 0.4.25 流式认证修复复核

本页记录不同修订的证据归属，不代表发布放行。线上状态以主会话真实部署回执为准。

| 修订 | 已核实 | 待完成 |
| --- | --- | --- |
| 首冻结 `e060fa307a89`，1361 文件 | 全 Go/vet 通过；race 1416 PASS / 21 SKIP；常规真实模型 13 top / 20 sub 通过 | 真 PG 首轮失败；WS 503 文案被下游判为认证失败 |
| PG 断言修订 `0b484ec2e9ff`，1361 文件 | 全源逐项比较仅 PG fixture 变化：`64f2e593` → `85ab603b`；新24 top / 39含子 PG全通过，775执行源与二进制匹配 | 后续两个验收工具文件变化另审；尚未包含 WS 文案补丁 |
| A 私有 WS 补丁 `ff7fc16c` | A 的真实本机 WS 红绿及实际下游分类器通过；新公开错误归通用 502 | 主会话合入后的最终冻结和验收 |

首轮 PG 的唯一失败叶为 `http200_typed_auth_sse`：实际规范化错误码是 `unauthorized`，fixture 仍期待 `invalid_api_key`，且随后原始上游文案断言也与安全摘要合同不符。修订只改期待，保留内部实际 HTTP、错误来源与计量；首轮失败原件保留。

race 已核原始 JSONL 的唯一 RUN 与终态，23 项 required 全部通过。两个 `TestIndependentTerminalErrorType…` 不匹配本轮 selector，不能声称它们也已跑 race；普通 Go 覆盖另有 A 组证据。

初冻结索引位于 `.local/cross-session-20260928/01a0e7ed/v0425-stream-auth-release-audit/initial-gate-index-e060fa307a89.json`。该索引固定 A 的 Go、B 的 PG/race、C 的真实模型复核哈希，整体状态为未放行。

新 PG `20260929T011455.219094Z` 已完整执行认证失败和本地完成权限变化两分支，实际 freshPG、JSONB、统计、JSONL、CSV 断言全部通过；新报告位于 `revised/pg-review.final.json`，SHA256 `98499366e87c7cc658ade4e7b8e0dc55e4bf5ba8660eb1e176f483e3ceff0dff`。旧 race/live 的选择范围不包含这一 PG 测试，主会话另提供精确单测试文件复用证据；复用不适用于尚未合入的 WS 运行补丁。

源码、缓存迁移候选、终态诊断候选各自独立。B 不新增生产请求、不重跑其他会话持有的数据库或模型测试；发布包出现后按最终冻结源码与实际回执继续逐项核验。
