# 凭据刷新对照：独立设计与执行边界复核

2026-09-25。本审阅只读既有证据和当前源码，独立检查脱敏工具与计划；不执行真实刷新、登录或模型请求。拟检验的边界是同一 g7 在一次正常 RT grant 前后是否改变当前原题结果，并用不刷新的 new2 观察同时间段变化。它不能证明某个 JWT 字段决定算力，也不能证明刷新消除了历史状态。

## 已有实验与避免重复

| 已核实记录 | 已执行的干预与结果 | 与本轮的关系 |
|---|---|---|
| [2026-09-24 调查](../CODEX-QUALITY-INVESTIGATION-2026-09-24.md) | 旧 q1 的 sol / xhigh，direct 与 routing 各一次，均 29，服务端 effort 回显 xhigh；同账号 medium 多路也为 29 | 已实际提高 effort；不必把再测 xhigh 当作未经检验的新解释。旧 q 别名不能等同当前 g 别名 |
| [题意对照](../CODEX-QUALITY-SEMANTICS-2026-09-24.md) | 同 q4、同凭据，原题 direct/gateway 为 29；明确可选形状的改题为 21，明确盲抽为 29 | 改题结果不算原题恢复；该批未刷新 RT |
| [八账号与 CLI 复测](../CODEX-QUALITY-EIGHT-ACCOUNTS-2026-09-25.md) | 固定请求 16 次，随后自然 CLI 8 次；g7 固定请求曾两次 21，之后 CLI 为 29 | 当前标签不是永久账号属性；这些不是刷新前后干预 |
| [v5 HTTP/WS](../CODEX-QUALITY-HTTP-WS-2026-09-25.md) | 12 格中的 10 个完整答案为 29，另两次 q0 HTTP 429；自然 CLI 追加 4 次为 29 | transport 与自然会话差异已测；没有 refresh |
| [v6 GET/helper 状态](../CODEX-QUALITY-COOKIE-CONTROL-2026-09-25.md) | 计划 12 格，因 q0 被外部停用仅执行 7 POST；5 个答案为 29，2 个 429；q0 有线上并发 | 不能当完整三臂镜像或纯 cookie 因果；没有 refresh |
| [v7 gateway/direct](../CODEX-QUALITY-GATEWAY-DIRECT-2026-09-25.md) | new2 的 direct/gateway 各两次均 21；g7 各两次均 29，8 个窗口已持久化生产请求重叠为 0 | 本轮最近的选择依据，仍只是账号关联；未执行普通 refresh |
| [OAuth header 修复](oauth-identity.md)、[来源环境修复](source-environment.md) | 合成/loopback grant 验证与源码修复；来源重授权流程核对 | 不能记成真实账号质量在 refresh/reauth 后已恢复 |
| [外部令牌 app-server 审计](native-auth-provider-audit.md) | `chatgptAuthTokens` 登录与 `account/read` 的零 turn 本地状态核验 | 没有 RT grant，不证明远端令牌有效或质量改善 |

上述已检查材料中，未找到当前 new2/g7 的“一次普通刷新 + 同题同完整请求 + 未刷新同期对照”的已完成因果试验。来源库的重授权状态或代码修复记录不补成该项证据；当前库查无记录也不等于历史从未注册。

## 本轮事前观测与固定计划

[credential-inventory.json](../../.local/research-20260925/credential-controls-v8/credential-inventory.json) 在 `2026-09-25T08:20:59Z` 记录：g7 enabled=true、has_refresh_token=true、Version=1、inflight=0；new2 enabled=true、has_refresh_token=false、Version=2、inflight=0。两者 user/workspace/identity/AT 四个摘要均与 v7 相同，proxy binding 均为 0；runtime gateway 为 `a4777428…20136a`，helper 为 `9110b869…ccce8`，fingerprint_enabled=false。这是当时状态，不能替代执行前再读。

已选计划为 **8 个模型 POST + 最多一次 g7 管理 refresh**：

| 阶段 | 顺序 | 凭据规则 |
|---|---|---|
| before | new2、g7、g7、new2 | 两个原 epoch 均固定 |
| 干预 | g7 一次带当前版本的管理 refresh | 不改 client、scope、账号、设备 profile、OS、代理或 cookie |
| after | new2、g7、g7、new2 | new2 仍原 epoch；g7 只接纳经本次持久化证据确认的新 epoch |

每期 ABBA 使账号位置对称，并保留每账号两次重复。只在两个时期之间改变 g7 凭据；不能按答案选择是否补跑、切换 effort 或更换账号。4 POST 版本虽更省调用，却只有每期每账号一次；root 已选择上述 8 次方案，本审阅不另加探索调用。

同一份新官方 CLI 完整首轮 body 在 before 之前捕获并冻结，原题和 medium 均不改，21 判据不进入请求。v7 raw 已清除，不能以旧 hash 代替旧字节或声称新 capture 与 v7 wire 相同。本批 8 格各用 fresh 固定 helper 直连、cold、无目录预热 GET；usage/管理 GET 单列，不能复用其 helper 状态进模型 POST。body、非认证 header、session/cache/input/control 在本批固定；仅账号 bearer/workspace 和干预后的 g7 bearer按计划变化。POST 无自动 refresh、重试或 fallback。

## 管理动作与持久化门槛

源码入口为 [admin_accounts_management.go](../../admin_accounts_management.go:470)，采用 `POST /api/accounts/actions`，正文严格只有 `action:"refresh"` 与唯一 `{id,version}` item。不发送 RT、AT、scope 或 client_id；版本来自刚刚读取的权威管理记录。旧 `/api/accounts/refresh` 不带调用者版本条件，不用于本轮。

管理入口在 metadata、完整 account、Store 回调三处核 Version；若并发换 AT 导致 callback 未执行，也返回 version_conflict。接口本身不阻止 disabled 账号刷新，因此 runner 必须额外核 enabled、唯一 record、has_refresh_token、相同 identity/epoch、当前无在途和当前运行版本。缺 RT、账号移除/停用、source 推送或任何未登记 epoch 变化均停止，不借另一个来源导入凭据继续。

HTTP 200 仅是批量容器。进入 after 前必须验证 `results` 恰一项、ID 摘要等于本次 g7、`ok is true`、`succeeded` 为整数 1、`failed` 为整数 0、无 item error/code，返回 Version 为正整数。随后从权威持久化存储冷读并核返回版本、当前 AT/RT 摘要与身份，不只相信管理返回值或缓存；new2 epoch 仍完全相同。单次管理调用不自动证明底层恰一次 OAuth POST，需要实际 grant attempt/audit 证据或明确标为未直接观测。

[proxy.go](../../proxy.go:453) 采用当前 stored ClientID，空时才回退官方常量；JSON grant 仅含 client_id/grant_type/refresh_token，不重发 scope。调用开始后独立 45 秒预算用于保存旋转 RT。断连/timeout/不明确响应后禁止再发一次或恢复旧备份，先只读确认已经开始的操作和持久化结果；不能让旧 RT 覆盖可能已旋转的新 RT。

预期变化必须是一次有证据的状态转换，不能写成“刷新后接受任何新 token”。同 record、enabled、user、显式 workspace、owner revision、stored ClientID、路由和非凭据配置应保持；允许 g7 AT/RT、有效期、相关 token claim 与版本按本次结果变化。若新 AT opaque、AT/ID 身份冲突、owner unknown/changed 或 workspace 改变，即使 RT 已正确保存也停止 after，不通过旧 Identity 回填掩盖变化。new2 不得接纳任何 credential epoch 更新。

RT-only、ID-only、相同 AT 都是实现支持的 optional response。正常 grant 成功但 AT 摘要未变时，应记录“未观察到 AT 更新”，不能称已检验新 AT；计划可按预登记 no-change 分支停止 after，而不重试到拿到新 AT。凭据保存成功与原题答案变化分开报告。

## 凭据比较工具的合同

AT、真实 ID token、stored metadata 分来源，报告只保留摘要/存在性/类型/受控枚举。JWT 解码不验证签名、远端有效性、权益或隐藏风险。比较需要覆盖：

- AT 与 ID 的 iss/aud/client_id/azp，stored ClientID 单独来源；不使用未经验证 claim 改写发行 client。
- scope 与 scp 分开，missing/null/空集合区分；精确原值 hash 与规范化集合投影并存。
- iat/exp/nbf/auth_time/pwd_auth_time，有限非 bool 数值；不把原始认证时间误称 refresh 时间。
- user 按 `auth.chatgpt_user_id`，仅 missing/null 才回退 `auth.user_id`；非空类型无效不能 fallback，`sub` 不作为用户替代。
- workspace 与显式选择 account_id 独立，membership ID 不混为 stable user；session/jti/nonce/device 与未知字段仅 hash。
- JSON 重复 key、非有限数字、过深/超大输入明确失败；精确数字词法摘要不能先转 float 丢精度。

工具自身还不能证明旋转 RT 持久化，runner 需要独立 authoritative RT hash/presence、Version/CredentialVersion/OwnerRevision 与管理回执链。比较 schema 的“相同”须按字段集合、类型与值完整比较，不能只比较几个相同布尔值便接纳新 epoch。

## 结果解释与收尾要求

每格保留真实发送 body/header/auth 摘要、HTTP status、completed/usage/model/effort/final、冻结自动评分及单独人工判据；没有完整答案的失败不计错答。before/after 分期分账号呈现，不合并旧批次估计随机正确率。

新2稳定而 g7 改变，可支持“本次刷新前后结果发生变化、同期控制未变”，仍不能把变化归因到某个 claim 或断言特定后端集群。两者同时变化提示时间/共享服务状态混杂；g7仍29也不能证明 refresh 从来无用。RT grant 同时改变 token 签发时间、session/授权元数据等潜在变量，不能称纯某字段因果。

每格记录前后 epoch、usage、生产已持久化请求重叠与在途读数；重叠窗口可能重复计数，不相加成独立请求。零已记录重叠不排除外部调用或未完成请求。最终保留 8 格实际执行数、唯一 refresh 数及依据、全部 helper/unit 回收、私密 raw/credential 清理和 server/local report byte hash 核验。停止则如实报告 partial，不绕过禁用账号补齐。

## 独立离线验收结果

实际执行 runner 的 `main` AST，仅把管理 API、模型阶段和持久化适配为合成数据；没有 import 远程 runner 的真实凭据读取，也没有网络、模型或刷新。最终 [check_runner_refresh.py](../../.local/research-20260925/credential-controls-v8/review/check_runner_refresh.py) **15/15 通过**，见 [runner-final.log](../../.local/research-20260925/credential-controls-v8/review/runner-final.log)。覆盖：唯一精确 CAS payload；多余 results、bool 计数、成功项含 error/code、非 200 拒绝；new2 client/RT/version 变化拒绝；before 不完整不刷新；丢失响应仅一次调用且不 after；回执持久化失败不 after；g7 的 RT/client 与对照版本在 before 期间被外部改动时不发 refresh。

独立复现过一项真实逻辑缺口：同用户新 AT 已保存，但管理 record.identity 变为 unknown 时仍可进入 after。原因是只比较 AT 派生身份。runner 已增加存储身份与当前 AT 的 user/workspace 对照，并通过相应故障注入。**OwnerRevision/CredentialVersion 在该管理 JSON 中不直接暴露**，当前实现明确记录 `owner_revision_observed=false`；不得把 stored identity 通过写成 owner revision 已独立观测。对当前 direct 模型计划，owner revision 不参与发往官方的 bearer/body；若后续要检验网关续接，必须另取实际 owner revision 证据。

[check_claims_boundaries.py](../../.local/research-20260925/credential-controls-v8/review/check_claims_boundaries.py) **8/8 通过**，见 [claims-final.log](../../.local/research-20260925/credential-controls-v8/review/claims-final.log)。验证 AT/ID/metadata 与未知键值脱敏、精确数字词法、重复 key/非有限数/过深 JSON 拒绝、primary 非法或空不 fallback、bool 时间拒绝、missing/null/空集合区分、client 来源冲突以及 token 改变与 owner 不变分开。曾发现 scp 数组缺少额外的规范化集合摘要；作者已补 ordered/unique hash 与数量，非法元素数组不投射伪集合，独立检查转绿。原有原始结构摘要一直保留。

本次通过对应的冻结摘要：

| 文件 | SHA-256 |
|---|---|
| `run_remote.py` | `340a5becc41a6c08693cd54f9a6b62348326261ba1a5bbca4902d903a7624330` |
| `tools/quality_credential_claims.py` | `d97d2e341f4e771cf9815ef889c2d571b002554caffecdf57c5cf7fc56f8aa7b` |
| `tools/test_quality_credential_claims.py` | `72bfbdfa7cb97b9035f87d23004b6d24638cc24ad655636a0843cc893844cc4f` |

管理 action 响应现按绝对剩余时间 `read1`，超过 1 MiB 拒绝；成功计数要求真实整数。进入 after 明确要求 AT 已变化且已读回，RT 只要求仍保存，单列 `refresh_token_changed`，不要求 RT 必然变化。之前不明确的控制账号比较现在同时冻结 client/RT/version/management。独立验收未发现剩余阻断项；它证明离线故障路径，不替代 root 后续真实运行、实际 OAuth 尝试数、服务端令牌有效性或质量结果。

复跑两个独立脚本的单命令：

```powershell
python -m unittest discover -s .local/research-20260925/credential-controls-v8/review -p "check_*.py" -v
```
