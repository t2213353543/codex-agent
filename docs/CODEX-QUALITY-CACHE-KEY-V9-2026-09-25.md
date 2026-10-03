# v9 缓存键对照：零模型中止与凭据失效证据

v9 没有执行模型对照：**实际模型请求 0 次、刷新 0 次、账号修改 0 次**。原计划比较 new2/g7 的固定缓存键与逐格新 key；执行前 g7 的 usage 已返回 401，违反账号可用前提，因此没有用无效凭据凑齐八格，也没有在同一批次替换账号。[最终状态](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cache-key-controls-v9/experiment.json)

本记录冻结 v9 结果，不包含随后另立的 v10 new2/new1 实验结果。

## 实际执行与清理

- 完成新的官方 CLI 零模型 capture，保存输入摘要及完整请求哈希；capture 中的目录 GET 属于真实非模型网络请求，不能称整个流程零网络。
- 2026-09-25 08:56:10 UTC 的 v9 inventory：new2 usage 200 allowed，g7 使用与 v8 刷新后相同 AT 摘要仍返回 401。
- 09:02:01 UTC 对 g7 做一次只读错误解析，明确错误码 `token_revoked`，前后 credential epoch 相同，没有 refresh/retry。
- 09:23:19 UTC 将 v9 登记为 `not_started_group_auth_precondition_failed`；原始 capture 已删除，capture 进程 MainPID=0、退出码 0。该批次不能统计为缓存实验答错、缓存修复失败或八格完成。
- 随后的替代账号盘点仅发送 **5 次 usage GET**：g2/g3/g4/new1/new2 各一次；q0 因已禁用排除，g7 因已观察 401 排除。没有模型、刷新、状态修改或自动选择。[替代盘点](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cache-key-controls-v9/replacement-inventory.json)

## 三个 401 与 g7 的错误响应完全匹配

离线逐项核对完整 response body 的长度与 SHA-256：g2、g3、g4 新盘点的响应均为 **182 bytes**，与 g7 已解析的 401 响应一致：

`ec94b559fd8f6b159922f3183f517600dd049dcaa800fc96ca5e21930a358a4b`

g7 解码参考错误码为 `token_revoked`；已脱敏错误文本为：

> Encountered invalidated oauth token for user, failing request

该文本 61 个 UTF-8 bytes，其 SHA-256 `a2b73a17b0bd2534f9dc5b273b1bf8785a1e4e2c2fe1d64793bddfe62c71faa9` 已从保存文本重新计算吻合。三账号 body 长度与完整摘要相同，支持它们得到相同错误文本；不是仅凭共同 HTTP 401 推断相同错误。[解码参考](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cache-key-controls-v9/auth-diagnostic.json) · [离线比较及来源摘要](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cache-key-controls-v9/auth-timeline-review.json)

所有时间均为 UTC，按**同一个 token SHA**寻找保存的最后 usage 200 与首次 401：

| 账号 | 同 AT 最后已观察 usage 200 | 同 AT 首次已观察 usage 401 | 两次观察间隔 | 本次 v9 复核 401 |
| --- | --- | --- | --- | --- |
| g2 | 06:46:41.955，v5 自然 CLI 随后 usage | 07:07:10.965，v6 inventory | 20 分 29.010 秒 | 09:23:19.348 |
| g3 | 07:46:32.121，v7 inventory | 09:23:19.519，v9 替代盘点 | 1 小时 36 分 47.398 秒 | 09:23:19.519 |
| g4 | 06:14:39.554，v5 inventory | 07:07:11.592，v6 inventory | 52 分 32.038 秒 | 09:23:19.699 |
| g7（刷新后 AT） | 08:40:17.183，v8 after/03-g7 usage-after | 08:56:10.873，v9 inventory | 15 分 53.690 秒 | 09:02:01.357 的诊断复核 |

表格是保存样本的时间间隔，**不是准确的令牌失效时刻**，也不能证明失效由期间某次模型调用、刷新、缓存或网关操作触发。g2/g4 在 v6 就已见相同 401，不是这次 v9 缓存计划新造成。g7 的 usage `token_revoked` 只说明该 AT 被上游拒绝，不能据此认定 RT 已失效、账号被路由到低算力集群或 quota 用尽。

## 替代账号盘点结果

09:23:19–09:23:20 UTC，new1/new2 均 usage HTTP 200、`allowed=true`、已观察配额使用比例 0、`in_flight=0`。同一只读 SQL 快照中，两者近十分钟已持久化生产 request_logs 均为 0。g2/g3/g4 为上述 401，g7 与 q0 保持排除。

近十分钟计数只覆盖**已完成并落库的网关请求**，不覆盖外部调用或尚未落库请求。usage allowance 与零流量也是采样事实，不保证后续 POST 接受或答案正确。

new1/new2 两个可用账号因此被交给主任务用于另立 **v10**。v9 不将新账号塞进原 new2/g7 的计划，也不将未来 v10 结果回填为 v9 成绩。

## 缓存假说的已有反例仍需保留

同一 v7 批次中，`01-d-new2` 答 **21**，cached input **0**、reasoning **1266**；`05-d-g7` 答 **29**，cached input **5888**、reasoning **516**。更早也有未命中答对和命中答错样本。因此“新 key/未命中必降智”或“命中必答对”均不符合已有结果。[v7 对照报告](C:/Users/Administrator/Desktop/项目/codex-agent/docs/CODEX-QUALITY-GATEWAY-DIRECT-2026-09-25.md)

本轮离线转换工具及发送前 key pin 已验收，但 v9 **没有模型结果**。更换 `prompt_cache_key` 只是协议字段干预，不等于关闭服务端缓存，也不保证 miss；未实施的 v9 无法证明或否定其质量因果。[事前设计与审查](C:/Users/Administrator/Desktop/项目/codex-agent/docs/quality-20260925/cache-key-control-review.md)
