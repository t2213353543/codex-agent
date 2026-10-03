# Workspace 权限与额度：官方客户端分支及管理快照缺口

2026-09-25。本专项检查固定 Codex CLI 0.155.0 源码，并读取主任务保存的四账号真实 native GET usage 摘要。**没有发现普通 Responses 按账号套餐把 reasoning 预算设为 512 的本地源码分支；确认网关管理 usage 解析遗漏 workspace 限额原因、spend control 和模型可用性字段，已通过真实管理链路复现并修复。** 管理快照不参与模型选择、effort、调度或启用 credits；这项修复完善状态诊断，不能作为四次 29 的因果证明。

## 可见的新账号差异

直接读取 `.local/research-20260925/workspace-metadata/report.json`：Q0/q4/q5/q6 分别一次实际 Rust helper GET `/backend-api/wham/usage`，均 HTTP 200，合计模型调用 0。四账号 plan 声明均为 `self_serve_business_prolite`，其中：

| 可见字段 | Q0 | q4/q5/q6 |
| --- | --- | --- |
| rate_limit.allowed / limit_reached | true / false | false / true |
| primary used_percent | 1 | 100 |
| rate_limit_reached_type.type | null | workspace_member_credits_depleted |
| spend_control.reached / individual_limit | false / null | false / null |
| credits.has_credits / overage_limit_reached | false / false | false / false |
| model_usage.gpt-6-astra.available | true | false |
| model_usage.gpt-6-astra.credits_would_enable | false | true |

这些是采样时真实服务端可见状态，不再只有套餐名称的间接比较。**usage 采样发生在已执行的数学对照之后**，不能把此时 100% 倒推成每条先前回答当时的状态；也不能拿 gpt-6-astra 的 availability 断言本次 gpt-5.6-sol 的内部执行预算。`workspace_member_credits_depleted` 区分于普通百分比耗尽和 workspace owner 限额，但仍不是风险标记或后台集群名称。

Q0 两次 21、q5/q6 四次 29 与四个 516 reasoning tokens 是原始观察事实。不同账号、共享异常 workspace 及完全内容字段比较使账号/工作区差异值得继续控制实验，但仍没有单独证明 516 是硬上限。历史 q4 显式 xhigh、1189 reasoning tokens 也答 29，不能把所有非 21 都归为统一的 512 阈值。

## 官方 0.155.0 中哪些层实际控制请求

以下均为本地固定版本实现证据，非对服务端私有 router 的推断：

| 边界 | 源码 | 已确认行为 |
| --- | --- | --- |
| effort/summary/context | [client.rs:763](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:763) | 显式 effort 优先，否则取 ModelInfo 默认；summary 受模型能力和用户配置控制；Lite 设置 all_turns。此构造不读取 wham 剩余额度。 |
| effort 别名 | [reasoning_effort.rs:10](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/protocol/src/openai_models/reasoning_effort.rs:10) | Ultra 解析到目录支持的 multi-agent/max 等值；Persistent 映射 disabled；普通 medium/xhigh 原值返回。没有 plan→512 分支。 |
| 改模型时 effort | [turn_context.rs:534](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/session/turn_context.rs:534) | 当前 effort 不在新模型支持列表时，选支持列表中间项或目录默认。这是显式模型变化的兼容分支，不能解释请求 model/effort 已证一致的样本。 |
| tool mode | [tools/mod.rs:68](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/tools/mod.rs:68) | 优先 ModelInfo.tool_mode，之后才 feature fallback。 |
| Responses Lite | [client.rs:795](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:795) | ModelInfo.use_responses_lite 控制 instructions/tools 是否移入 input。真实对照 schema 4 已直接观察该请求形态。 |
| 目录身份 | [models_identity.rs:40](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/model-provider/src/models_identity.rs:40) | cache identity 包括账号/user/email/plan/FedRAMP；相同 plan 不等于所有权相同。远端可见模型目录权威性在 [manager.rs:518](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/models-manager/src/manager.rs:518) 判定。 |
| plan 的工具分支 | [spec_plan.rs:710](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/tools/spec_plan.rs:710) | Free 禁用 image generation；没有把所有推理请求降低为某数字预算。 |
| plan 的实验上下文 | [token_budget.rs:13](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/session/token_budget.rs:13) | ChatGPT 的 Plus/Pro/ProLite 在 feature/model 支持等条件齐备时启用实验 context/history-notes；这是上下文管理分支，不是普通输出 reasoning 硬上限。self_serve_business_prolite 是单独的 workspace plan 枚举，不与 ProLite 自动混同。 |
| reasoning token 计数 | [responses.rs:137](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/codex-api/src/sse/responses.rs:137) | 从返回 output_tokens_details.reasoning_tokens 直接映射；未加 4、对齐 512 或按套餐改计数。 |

针对 ordinary client/models/provider/protocol 中 512、516、reasoning budget/max budget 的检索，512 命中包括 Guardian approval 解释长度、trusted-tool context、提交队列容量、MCP 描述上限、日志片段；不是这次数学普通 Responses 构造中的预算。检索与局部数据流检查只支持“未找到该本地实现”，不支持“服务端一定没有预算分档”。

JWT `IdTokenInfo` 的普通 auth claims 在 [token_data.rs:29](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/login/src/token_data.rs:29) 解析 plan、user/workspace、FedRAMP；opaque access token 原样发往服务端。单独 membership claim 有 workspace 匹配验证。此次看到的 amr 或 compute_residency claim 差异，不存在本次已定位的本地 reasoning512 解码分支，不能据字段名推断其服务端后果。

## wham usage 的显示与 Reserve 边界

官方 [backend-client/client.rs:550](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/backend-client/src/client.rs:550) 将主 rate_limit、credits、spend_control、rate_limit_reached_type 映射到 snapshot，附加窗口保留 metered_feature/limit_name。五种已知 reached reason 在 `:626` 区分普通 limit、workspace owner/member credits depleted、workspace owner/member usage limit reached。

这些字段在 [tui/chatwidget/rate_limits.rs:287](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/tui/src/chatwidget/rate_limits.rs:287) 影响额度警告和 workspace 提示。普通“切换较少 credits 的模型”由 [rate_limits.rs:441](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/tui/src/chatwidget/rate_limits.rs:441) 的选择动作显式改变 model/default effort；不是在仍发送同一 model/effort 时悄悄改预算。

Reserve 支持必须区分： [rate_limit_resets.rs:27](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/backend-client/src/client/rate_limit_resets.rs:27) 只有能实际应用 Reserve 的调用者才用 `x-openai-codex-luna-reserve: 1` opt-in，普通只读 usage 默认不加。[luna_reserve_model.rs:13](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/tui/src/chatwidget/luna_reserve_model.rs:13) 借 normal_model_slug 的展示/effort metadata，但真正路由 model 仍为 Reserve。网关被动管理读取没有开启该 opt-in，本修复也不添加。

## 管理解析真实缺陷与修复

旧 `managedUsage` 已保留 ordinary `allowed/limit_reached`、primary/secondary、credits 和 additional_rate_limits，但遗漏以下字段：

- 官方 0.155 schema：spend_control.reached/individual_limit、rate_limit_reached_type.type、additional_rate_limits.normal_model_slug。
- 本次服务器真实观测的新扩展：model_usage 的 available/credits_would_enable/available_at，credits.overage_limit_reached，rate_limit_reset_credits.applicable_available_count。固定 0.155 OpenAPI 未定义这些新扩展，不能误称它们由旧源码保证；类型依据本次返回的 bool/string/null/number 观察，并只作管理诊断。

`TestResearchEntitlementsUsageSnapshotRetainsOfficialWorkspaceLimits` 走真实 HTTP GET usage → 管理 parser → management store 持久化 → 管理 GET snapshot，旧代码丢失上述官方三项，而 existing credits/allowed 与私有字段过滤保持。red 原日志 `.local/quality-20260925/workspace-entitlements/fail-before.jsonl` 为 1 项失败。快照独立于 inference metrics/cooldown/admission，测试同时确认没有影响推理选择。

修复只涉及 `admin_account_upstream.go` typed allowlist 与 `admin_account_usage_cache.go` 时间边界：布尔用指针保留 false 与未知区别；amount 保留数字字符串精确形式（负数、指数和大整数），未知未来/可能私有文本只置 nil，不让整个合法 usage 失败；未知 optional 日期同样保持 availability bool、时间未知并沿用原 TTL。unknown reason 归 `unknown`，不输出任意 details；account/user/email/upsell/private extra 不全透。

缓存原先只看 rate window reset，新增状态需要在 spend-control 已知 reset_at/reset_after_seconds 和 model availability 的可解析 RFC3339 时间跨过时失效。单独先复现旧缓存错误：3 个 expired 分支失败，4 个 future/unknown 控制通过；red 原日志 `cache-fail-before.jsonl`。未知时间不当 Unix 0、不自动变 available；没有因为状态显示修复去消费 credits、切账号或改模型。

## 验证与冻结

五个新增顶级测试覆盖实际管理持久链路、真实新扩展/PII过滤、8 个缓存时间边界、optional amount 格式及新增 optional 字段类型演进。关联管理 usage/auto-reset/OAuth/controls 测试合计 **30 个顶级、59 个含子项通过，0 fail、0 skip，0.943 秒**，日志 `green-associated.jsonl`。这些本地管理测试不等于 native/PG 全回归，也不等于模型质量恢复。

独立 Q12 复核发现初版仍将新 optional 字段与核心 quota 一起强类型解码：如 remaining=123、available_at={}、model_usage=[] 会在未知格式清理前使整份 usage 失败。新增 9 类真实 parser 回归先保存 `optional-type-fail-before.jsonl`，再将新增 optional 字段分离为 RawMessage 后按字段宽容读取；无关字段类型变化置未知，不丢已知核心 quota。原有 plan、rate_limit、credits 三字段、available_count 和 additional.rate_limit 的类型校验仍严格，未将整个管理 parser 改成自由 map。

```powershell
go test . -run '^(TestResearchEntitlements|TestManagedUsage|TestManagedUpstream|TestManagedOAuth|TestManagedAutoReset|TestManagedAccountControls|TestManagedControls|TestManagementReview)' -count=1 -timeout=90s
```

最终冻结：`admin_account_upstream.go` SHA `c42ab6d8d847af510e4727603c650078d5e1413a2ee95b40ae1b3ed3fdd6c304`；`admin_account_usage_cache.go` SHA `982587e8bfcb1eef068ba3d1d83d5c623ccc05cecdd39c1db5d1d24ca86dd5fe`；`gateway_research_entitlements_test.go` SHA `022804e2a63eb39c4593d7b0e74221674aedc0456041f9a0d4cc25dbab163e97`。主任务在初版全套运行期间应用上述一文件解析保护补丁，最终针对性与完整验证范围必须按其快照分别记录，不将旧快照全套冒称新补丁全套；发布及运行时验证由主任务记录。

这次新增证据明确了当前异常 workspace 的额度/成员状态，并修复网关把这些信息丢弃的问题。它仍不证明早先 29 的后台原因；固定完整 body、固定 session 的跨账号结果，以及对应请求时的真实 usage/执行元数据，才能继续缩小账号与会话随机效应之间的解释空间。
