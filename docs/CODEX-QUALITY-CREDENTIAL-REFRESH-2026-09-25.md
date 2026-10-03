# 正常凭据刷新前后原题对照（2026-09-25）

**g7 在刷新前、仍持有 v7 的原 AT 时已经两次答 21；刷新后同一新 AT、相同实际请求又分别答 21 和 29。** 因此本轮不能称刷新使模型恢复，也不能称刷新导致降级。未刷新的 new2 四次均答 21。全部 8 次模型 POST 正常完成，冻结自动评分为 **7 正确、1 错误、0 无法判分**，没有用错误后重试补格。[独立完整核验](../.local/research-20260925/credential-controls-v8/review/results-final.json)

## 预登记与逐格结果

执行前固定两期相同 ABBA 顺序 `new2,g7,g7,new2`，中间只对 g7 发一次现有管理 API 的正常 CAS refresh。每期每账号两次，合计最多 8 个模型 POST、一次管理 refresh action。原题、模型 `gpt-5.6-sol`、effort `medium` 不变；验收值 21 不进入请求。全部模型请求由每格全新 Rust helper `9110b869…ccce8` 直连官方 HTTPS，cold、无目录预热 GET，推理不经过 Go 网关。[计划](../.local/research-20260925/credential-controls-v8/plan.json)、[独立设计审阅](quality-20260925/credential-refresh-independent-review.md)

| 阶段 | 顺序 / 账号 | 答案 | reasoning tokens | input tokens | cached input tokens |
|---|---|---:|---:|---:|---:|
| 刷新前 | 01 new2 | 21 | 2049 | 6879 | 5760 |
| 刷新前 | 02 g7 | 21 | 2474 | 6879 | 6656 |
| 刷新前 | 03 g7 | 21 | 1908 | 6879 | 6656 |
| 刷新前 | 04 new2 | 21 | 1881 | 6879 | 6656 |
| 刷新后 | 01 new2 | 21 | 1552 | 6879 | 6656 |
| 刷新后 | 02 g7 | 21 | 2388 | 6879 | 6656 |
| 刷新后 | 03 g7 | 29 | 425 | 6879 | 0 |
| 刷新后 | 04 new2 | 21 | 1034 | 6879 | 6656 |

八格均 HTTP 200、`response.completed`、`status=completed`；实际返回的 model/effort 与计划相同，每格恰一次 native POST，response ID 摘要各不相同。自动结果已用同一冻结 scorer 重算，并核最终文本 hash，没有事后修改 scorer 或人工改分。北京时间约 **16:35:15–16:40:42**。[刷新前执行](../.local/research-20260925/credential-controls-v8/before/execution.json)、[刷新后执行](../.local/research-20260925/credential-controls-v8/after/execution.json)

## 相同输入下观察到的变化

本批 body 为一次新官方 CLI 0.155.0 首轮 capture，SHA-256 为 `e2ddc5e83f2ed83f2429fd006824b3135a17e32e46b8b8c2b9e3cd0fc9e282ae`。八格实际 native IPC body 全部匹配，同一原题、完整 input、session/cache metadata、非认证头在本批保持。账号间只按绑定的凭据与 workspace 发请求，刷新后 g7 使用经持久化核验的新 AT；实际 Authorization/workspace 头各恰一项并与该格 epoch 对应。八格 helper scope 与 IPC connection ID 均独立，实际可执行文件摘要一致。

更直接的同凭据对照是刷新后的 `02-g7` 与 `03-g7`：AT、workspace、helper、body、实际 IPC body、捕获 head 的摘要全部相同；**全部出站 IPC header 的名称和值摘要，包括 Authorization，逐项相同**；服务端返回 controls 除 usage 外也相同，两个 response ID 不同，但答案为 21 与 29。两次 reasoning 为 2388 与 425，cached input 为 6656 与 0。这是同时出现的响应差异，不能把 cache miss 或某个 token 数直接判为原因。[成对证据](../.local/research-20260925/credential-controls-v8/same-credential-variation.json)

“相同”限定在已捕获的应用/native IPC 边界，没有抓取完整 TLS wire bytes 或隐藏推理。两次 fresh helper、连接与调用时间不同，未知后端状态和采样过程仍可能变化；不能由此指定某个私有集群、风险标记或缓存因果。

v7 中同 g7 曾四次答 29，本轮刷新前已两次答 21；旧 AT 摘要仍为 `19107bed…88116`，不能归因到尚未发生的 refresh。**v7 与 v8 不是同一份完整请求**：新 capture 的 `client_metadata`、`input`、`prompt_cache_key` 三个顶层字段摘要改变，model/reasoning 摘要保持。逐 input item 核对还发现 **索引 5 的 message content 变化，涉及不同 cwd 的上下文**，不只是 ID 或 metadata 变化。原题未变，但完整上下文跨批不同，不能宣称所有 instruction/content 相同，也不把两批合并成同字节试验或随机正确率。[跨批 capture 对比](../.local/research-20260925/credential-controls-v8/review/capture-cross-batch.json)、[逐项差异](../.local/research-20260925/credential-controls-v8/v7-v8-input-diff.json)、[v7 报告](CODEX-QUALITY-GATEWAY-DIRECT-2026-09-25.md)

## 正常刷新与凭据连续性

事前只读库存确认 g7 enabled、持有 RT、inflight=0、Version=1；new2 enabled、无 RT、Version=2。两者 user/workspace/identity/AT 摘要均与 v7 一致。实际调用为一次 `POST /api/accounts/actions`，只有 g7 的当前 ID 和 Version，不登录、不修改设备 profile、OS、代理、client_id 或 scope，也不刷新 new2。[事前库存](../.local/research-20260925/credential-controls-v8/credential-inventory.json)

管理回执 HTTP 200，唯一 item 成功、`succeeded=1`、`failed=0`，没有嵌套错误；g7 Version 从 1 到 2。随后重新读回的新 AT 与新 RT 摘要均改变，RT 仍保存；user/workspace、存储身份、client_id、管理配置保持。new2 的 AT、RT 状态、版本、client_id 与管理配置均保持。只有完成四个 before 格且上述约束通过才进入 after，没有重试 refresh。[刷新回执](../.local/research-20260925/credential-controls-v8/refresh-action.json)、[持久化约束核验](../.local/research-20260925/credential-controls-v8/refresh-invariants.json)

本轮可证明的是**一次管理 refresh action**及其成功、后续存储与模型使用新 AT 的证据；没有独立捕获此次 OAuth grant 的完整 wire 请求，不能把管理计数改写为已直接观测一个 OAuth wire POST。OwnerRevision/CredentialVersion 也未由此管理 JSON 直接暴露，记录 `owner_revision_observed=false`；存储 user/workspace 通过不扩大成 owner revision 已独立读取。

## claim 比较与时间字段纠正

刷新前跨账号 AT 的 issuer、audience、client_id、scp、plan 摘要相同；g7 存储 client_id 与 AT claim 匹配，new2 存储 client_id 为空，因此该关系为不可比较，不补成匹配。刷新后的 g7 claim 叶子中仅 **exp、iat、nbf、jti** 摘要改变；user/workspace、scope/scp、plan、session 与其余可见字段保持。new2 的 claim 树无变化。JWT header 摘要也保持。[跨账号比较](../.local/research-20260925/credential-controls-v8/claims-cross-account-review.json)、[独立前后逐字段比较](../.local/research-20260925/credential-controls-v8/review/results-final.json)

这些是**未验签的本地 JWT 解码观察**，不是服务端权益、账号风险或算力档位证明。`iat` 是令牌签发时间，不是注册时间；`exp/nbf` 也不能补出注册历史。运行中的冻结 claims 工具曾把私有 `pwd_auth_time` 数字标成 `seconds`，没有该字段单位合同支持。原报告保留，独立解读将它标为 **单位未确认的私有数值**，不按 13 位数猜毫秒、不换算日期、不用于身份 gate 或质量推论。后续离线工具已修正，未替换本批运行副本。[原报告 hash 锚定及纠正注释](../.local/research-20260925/credential-controls-v8/timestamp-unit-fix/historical-report-annotation.json)

## 并发、清理与复核

只读数据库查询覆盖八格全部窗口，new2/g7 对应的**已完成并持久化生产网关请求重叠数均为 0**。窗口由请求开始时间加 latency 推导，加入 ±1 秒余量。本批 direct 模型请求不进这些生产日志；该统计不排除外部调用、未完成请求或查询时尚未持久化的记录，不写成账号绝对无并发。[并发证据](../.local/research-20260925/credential-controls-v8/concurrent-traffic.json)

最终 unit 为 MainPID=0、inactive/dead、退出 0；所有模型 helper 已回收，socket 删除，raw capture 和临时凭据已不存在。服务器与本地 **8 份原始 report、2 份 phase execution、1 份 experiment** 的 byte SHA-256 全部一致。收集完成前一次校验因本地尚未更新而失败，收集结束后仍按原严格 hash 门槛重验通过，没有放宽 identity 或文件校验条件。[最终清理与服务器摘要](../.local/research-20260925/credential-controls-v8/cleanup-verified.json)

独立结果脚本核对了计划、每格真实发送与 epoch、usage、全部终态、评分、刷新前后 claim/持久化、原始文件摘要与清理，**8 格完整设计通过、0 项证据矛盾**。独立审阅未发模型或 refresh。复跑：

```powershell
python .local/research-20260925/credential-controls-v8/review/review_results.py --final --output .local/research-20260925/credential-controls-v8/review/results-final.json
```

本轮将可见凭据状态和固定请求的边界查实，仍未确定单次答案变化的原因；它不是生产网关端口质量测试，也不证明历史状态已消除。后续若继续研究，应保留这项“同新凭据、相同已观察请求、不同答案”的事实，而不是继续按账号永久分为正常或异常。
