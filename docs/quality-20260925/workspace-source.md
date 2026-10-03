# 28765 工作区、席位与 OAuth 来源核对

2026-09-25。已先读取 28765 checkout 的 `AGENTS.md` 与 `docs/production-stability.md`。只读源码目录为 `C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920`，Git HEAD=`92f39d7fd591d0a1e1934193169fe1e0e0866309`；未读取本地 secrets、数据库或运行配置值，未修改 28765 文件，未运行其业务代码或模型请求。服务器行为只引用根任务另行采集的脱敏真实材料。

**当前证据不支持“Q0 与错误账号的 OAuth scope 或 token client 不同”。** 四账号实际 claim 的 client/scp/aud/issuer/认证方法/plan 均相同；最新真正不同的是 usage 接口的可用额度。28765 源码也有显式 Team workspace 与席位检查，未发现它仅靠改 Account-ID 就把个人 token 当 Team token 推送的正常路径。相同 plan 仍不等于相同席位、workspace 配额或完整权益。

## 已证明的源码机制

| 边界 | 源码位置与实际逻辑 | 能证明/不能证明 |
| --- | --- | --- |
| Codex OAuth 来源 | [auth_flow.py:931](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/auth_flow.py:931) 默认 client=`app_EMoamEEZ73f0CkXaXp7hrann`、localhost:1455 callback、PKCE S256、scope=`openid email profile offline_access`；client/scope/redirect 可被进程环境覆盖 | 是独立 Codex OAuth，不能仅凭源码默认值认定所有账号历史都用同一配置 |
| 工作区选择 | [auth_flow.py:3027](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/auth_flow.py:3027) 优先 `OAUTH_TARGET_WORKSPACE_ID`，否则才按 cookie/HTML/query 找 workspace；[3043](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/auth_flow.py:3043) 将该 ID POST 到 `api/accounts/workspace/select` | 显式目标优先，不依赖“第一个 workspace”顺序；成功与否还要看返回凭据及后续验证 |
| Team 重授权 | [oauth_repair.py:97](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/oauth_repair.py:97) 强制独立 Codex PKCE、RT-only，禁用普通 Web callback token fallback；158 行给 flow 设置目标 workspace | 加入前的 browser/personal token 与入组后 Codex Team token 是两条不同用途路径 |
| 重授权身份 | [oauth_repair.py:74](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/oauth_repair.py:74) 拒绝明确 email/user/workspace 改变；[team_rotation.py:177](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:177) 必须 AT claim workspace 等于 owner.workspace 且 plan 属 Team allowlist | 是本地 claim 一致性检查，不是 JWT 签名或隐藏权益证明 |
| 在线工作区确认 | [team_rotation.py:4145](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:4145) claim 通过后用同 AT 与 workspace 请求 `/backend-api/wham/usage` | usage 接受该身份，仍不等于模型推理成功、未限流或质量恢复；该函数只要求 dict，不要求 `allowed=true` |
| 高级席位 | [team_rotation.py:765](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:765) 把 `advanced`/`prolite`/相关别名映射到高级；[867](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:867) 映射上游 `prolite`；[876](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:876) 不因另一 typed pool 有剩余而改席位类型 | 配置选定的席位不会自动退到普通 `default` 池；不能替代当前 live 成员 seat_type |
| 成员及席位实查 | [team_rotation.py:5660](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:5660) 以稳定 user_id/email 找成员、冲突拒绝；[6885](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:6885) 若成员席位不符则更新并最多回查 8 次，只认可目标类型 | 首次激活有真实成员/seat gate。当前保存的 `seat_type` 仍是历史快照，不能代替实时权益 |
| 付费容量 | [team_rotation.py:811](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:811) 分 `default`/`prolite` 读取 paid/assigned/available/held；[mother_checkout.py:190](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/mother_checkout.py:190) 核验新 owner workspace、匹配 user 和购买的两种 paid 数量后才准入 | plan 名称或 checkout URL 不是 paid entitlement，typed paid 座位另查 |

轮转在子号加入后重新取目标 workspace OAuth，明确不上传入组前 repaired personal token（[team_rotation.py:7806](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:7806)）。子号 OAuth 有数据库租约、owner/assigned_at 校验，成功后持久化 token 再推送（[3838](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:3838)、[3955](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:3955)、[4676](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:4676)）。补推可复用首次保存的 Team token，但缺 token 或 claim workspace 不符即拒绝，未把远端管理端故障当作重新 OAuth 的理由（[4244](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:4244)）。

## OAuth 与官方 CLI 的明确差异

官方 CLI 0.155.0 的 [login/src/server.rs:589](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/login/src/server.rs:589) authorize scope 还包括 `api.connectors.read api.connectors.invoke`；28765 默认没有这两项。官方若配置强制 workspace，会在 authorize URL 添加 `allowed_workspace_id`（603 行）；28765 在后续 workspace/select 中显式选择目标。官方 authorize 还发送 originator；28765 的此函数使用可选 `prompt`。这些是流程差异，不能从名字推断会影响数学推理。

RT 刷新也有明确格式差异：28765 [exporter.py:266](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/exporter.py:266) 使用 form-urlencoded，显式发送固定四项 scope；官方 [login/src/auth/manager.rs:1601](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/login/src/auth/manager.rs:1601) 用 JSON 发送 client_id/grant_type/refresh_token，不在此请求重发 scope。只有观察刷新前后 claim 才能判断实际授权集合是否变化，本研究不发刷新来试探。

28765 `decode_jwt_identity` 只抽 email、account、user、plan、expiry（[team_rotation.py:370](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:370)），不会拿 scope/aud/azp/delegated claims 做席位/quality 分组；相关 OAuth/rotation 文件也没有通过 delegation 字段选择计算等级的逻辑。不存在此本地分支不等于提供方内部完全相同。

另一个可检查的来源边界是导出：SUB2API payload 显式 account_id 优先，organization 则先读 id_token 再读 AT（[exporter.py:591](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/exporter.py:591)）。CPA 缺 id_token 时会构造明确标注为本地兼容解析用途的 token，包含合成的缺省 organization/project/role 等字段（[exporter.py:331](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/exporter.py:331)、[412](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/exporter.py:412)）。因此历史管理标签或 ID-token 元数据不能替代真实 AT claim；本轮官方 CLI 对照是 AT-only，没有使用这个兼容 ID-token 合成路径。

## 根任务真实四账号材料的独立复核

已读取 `.local/research-20260925/workspace-metadata/report.json`，采集时间 `2026-09-24T19:59:00Z`。其中是 **4 次真实 Rust helper GET usage、0 次模型调用**，四次均 200、helper exit=0、无强制终止、socket 已清理。各 usage 返回的 account/user 摘要与该 AT 的 workspace/user claim 一致；没有出现“请求头选错 workspace，而 usage 返回另一身份”的证据。

四账号实际相同的字段：client_id=`app_EMoamEEZ73f0CkXaXp7hrann`；scp 四项完全相同；aud=`https://api.openai.com/v1`；iss=`https://auth.openai.com`；amr 同为 password/OTP/MFA/TOTP 序列；plan=`self_serve_business_prolite`；localhost、sl、email_verified 同为 true；compute residency 与 MFA-required 字段的摘要也相同。不同叶子字段只有账号/user/profile/poid/sub 身份、jti/session 以及 iat/nbf/exp/pwd_auth_time。

因此缺少 connector scopes 是 Q0/q4/q5/q6 的**共有来源差异**，不能解释本轮 Q0=21 而 q5/q6=29 的同期分组。

| 账号 | usage allowed | 周期 used_percent | rate_limit_reached_type | spend_control.reached | `gpt-6-astra` available |
| --- | --- | ---: | --- | --- | --- |
| Q0 | true | 1 | null | false | true |
| q4 | false | 100 | `workspace_member_credits_depleted` | false | false |
| q5 | false | 100 | `workspace_member_credits_depleted` | false | false |
| q6 | false | 100 | `workspace_member_credits_depleted` | false | false |

四者 primary window 都是 604800 秒且 secondary=null。28765 [team_rotation.py:953](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:953) 会把 advanced 的周 primary 规范为本地 secondary 周额度，还可能采用 child/owner 配置填补 seat_type（[4091](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:4091)）；**本地 primary/secondary/seat 标签不是完整上游 entitlement**。根任务直接保存的 usage 字段更适合此次比较。

这个稍后 usage 快照证明当前错误账号已耗尽 workspace member credits，解释为何不应继续用它们追加模型试验；它发生在六格回答实验之后，不能反推当时答 29 的原因，也不是低算力集群证明。

## 仍需服务器只读验证的字段

下列字段可以进一步区分 workspace/席位/来源，但本地源码不能提供当前值。只保存允许的字段或哈希，不导出 token：

1. owner 当前 `workspace_id` 摘要、`preferred_seat_type`、`join_mode`，child 的当前 status/user_id 摘要、assigned_at、seat_type、oauth_reauth_status/updated_at，与永久 join binding/history 的关联。Q0 若并非这个 checkout 的 rotation child，必须注明来源未知，不能补造一条同源链。
2. 两个 workspace 的实时成员记录：匹配 user/account-user ID 的 role、seat_type、is_deactivated、membership 状态；typed subscription 的 plan、paid/assigned/available/held、seats_entitled/in_use。比较原始字段存在性，不能用 plan 补全缺失权益。
3. OAuth 当时来源记录：注册/重授权/RT refresh 时间，client/scope/redirect override 的有效值或哈希，fingerprint profile 与 OAuth/Team route 的哈希。源码支持 owner→global→Team 的 OAuth proxy 优先级（[team_rotation.py:3277](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:3277)），实际四账号路径是否相同尚不能由 JWT证明。
4. AT、真实 id_token、远端导出 metadata 的 workspace/user/organization 是否一致，兼容 id_token 是否曾生成。`update_registered_oauth_tokens` 只在新 token 字段非空时更新对应列（[db.py:5783](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/db.py:5783)），故历史 id_token 来源须查实际元数据，不能假设每次 AT 轮换都同步所有声明。
5. 原始 usage 的 allowed、limit reason、model_usage、credits、spend_control 及 reset 时间，附准确采集时间和 token epoch。当前已覆盖四账号；未来有新的可用异常账号时可复用这个只读比较，不需先消耗模型额度。

本次只新增本文，没有改 28765 源码、配置、席位或 token。真实 usage 报告 SHA256：`ccabb8c6560408e3681670489bc3b0ded5049992ea52b8efe6475f35e34f79ca`。
