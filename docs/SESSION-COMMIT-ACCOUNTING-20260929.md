# 会话提交与用量记录合同

核对时间：2026-09-29 02:25（UTC+8）。本页修复已随 **0.4.17 / PID 2058745 / binary 4cf31b8c…1f9ad3** 上线，release `49c77636fd630acd…`、source `0636f98e359a4a3d…`。02:24:32 的正式现场回执已验证两次生成、一次本地 top_p 拒绝及三次 CSV 对账；settings 27、账号配置、api_key 缓存域和 cooldown=false 均保持。见[部署后独立核验](../.local/collab-a-20260928/session-release-review/postdeploy-review.json)、[关键发布包核验](../.local/collab-a-20260928/session-release-review/package-critical-review.md)及[六文件指纹](../.local/collab-a-20260928/session-commit-contract-review/source.json)。普通现场验收没有故意触发 CAS 冲突或 TTL 到期，相应边界依据同源正式测试。

| 边界 | 错误与交付合同 |
|---|---|
| 内部 session commit 失败 | `commitOwner` 返回 typed `gatewaySessionCommitError`，保留原 `Error`/`Unwrap`；记录 `error`、`gateway/session_binding`。屏障拒绝交付时清除 `TTFTMs`/`FirstOutputKind`，上游已观察到 delta 不等于客户已收到回答。相同文字的普通 error 不能冒充该类型。 |
| 真正 client writer 失败 | 下游 writer 确实被调用并返回错误时，继续保留 `client/response_write` 合同；不因新增 typed 分类把所有写失败归到 session binding。 |
| 已收合法终态，本地完成失败 | 仍记录请求失败，并保留实际返回 model、input/output、cache read/create 及 known 标志；不能把本地失败伪造成上游 generation 错误，不能由成功上游状态把请求改成成功。已证 CAS 冲突/授权撤销路径不交付回答或 compact 密文、不保存可续接 history、不重放 generation。 |

原生 `/responses/compact` 先验证完整 JSON、compaction output 和成功终态，再调用 `OnComplete`。本次只修其 **错误分支**：`recordBPSTerminalUsage(raw)` → 明确 `Status=error`/保留原错误 → 尝试 typed commit 分类 → 返回 502。成功分支原位置不变。不能采用旧的“把 usage 提前到 OnComplete 前”候选，否则非 typed 错误可能残留成功状态。未收到有效 usage 的请求仍为 unknown，不由提交失败补零，也不推断额度原因。

当前正式 Go 日志 **02:08:46 +08** 已有核心 **8 子例 PASS**：普通 JSON、structured JSON、structured SSE、native compact，各成功/真实 CAS 冲突一对。冲突时每请求 1 POST、真实 observed HTTP 200；JSON/compact 公共 502，structured SSE 为 HTTP 200 内 `response.failed`、`output:[]`。真实用量保留，未交付 TTFT 清空，history/回答不泄漏、并发 winner 不被覆盖。另一个真实“终态前禁用账号”的 **non-typed authorization-revoked** 用例 PASS：502/error，input 100/output 10/read 80/create 7 与 returned model 保留，但不误标 `session_binding`。依据[当前补充审查](../.local/session-reliability-release-20260929/commit-review/REPORT.md)及源码/日志回执；这些局部事实不替代整轮 race/live/PG 终态。

**20 秒 lease 修复与旧六条自然错误分开。** 同 replacement 修复仅让续租接受原 owner 或本请求已选择的精确 replacement；不同 winner、读失败、父 context 与既有迁移守卫不放宽，也不意味着每 20 秒重新授权。另有真实 21 秒反例支撑该修复。旧 .15 六条完整请求耗时仅 **2,851–3,918 ms**，本请求新 ticker 首次在 20 秒后触发，所以不能用其自身的 20 秒误取消解释这些短请求；其他请求先前改绑定的可能性仍未知。见 [B 归因/计时](COLLAB-B-SESSION-CAS-ATTRIBUTION-20260929.md)、[A lease 资格审查](../.local/collab-a-20260928/cas-lease-boundary-review/REPORT.md)。

最终修订版全 Go/vet、PG 17 项、单轮 race 和 9 组完整 live 门禁均已通过，并绑定同一冻结源与发布包。首轮旧源的摘要 SSE 无可见摘要失败记录仍保留；修订轮 SSE/WS 各有及时 summary_done，不能把这次有摘要归因于 compact 错误计量修复。自然效果另看部署后窗口，不由验收通过推断错误已消失。

附件 TTL 清理是同期独立优化：[A expiry](../.local/collab-a-20260928/attachment-expiry-review/REPORT.md)已核目标 key 即时过期、命中不续 TTL、有效项不丢及并发一次上传；不能拿它解释 CAS 或承诺端到端首字改善。本页更新仅读取既有源码与回执，**没有新测试、SQL、模型调用或运行文件修改**。
