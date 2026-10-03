# 四账号权限/额度对照与管理快照修复

上一轮同期六格已得到 Q0 两次 21、q5/q6 各两次 29，且内容与工具定义相同。本轮继续检查 JWT、工作区选择来源和上游额度响应，**没有发送模型请求**。

## 实际上游读数

使用与生产一致的固定原生 Rust helper，分别对 Q0、q4、q5、q6 做一次 `GET /backend-api/wham/usage`，均为 HTTP 200/HTTP 1.1，helper 正常退出并清除 socket。没有开启 Reserve 请求头、刷新 OAuth、购买额度或写入工作区配置。实际访问身份与之前样本 epoch 核对，AT 只在服务器内存中使用，没有写入凭据文件。

| 当前状态 | Q0 | q4/q5/q6 |
| --- | --- | --- |
| plan_type | self_serve_business_prolite | 相同 |
| 普通额度 allowed / limit_reached | true / false | false / true |
| 七天窗口 used_percent | 1 | 100 |
| rate_limit_reached_type.type | null | workspace_member_credits_depleted |
| spend_control.reached | false | false |
| spend_control.individual_limit | null | null |
| credits.has_credits / unlimited | false / false | 相同 |
| credits.overage_limit_reached | false | false |
| gpt-6-astra available / credits_would_enable | true / false | false / true |
| reset credits available/applicable | 0 / 0 | 相同 |

这是实验后的最新状态，明确了当前拒绝继续使用的上游原因，**不能倒推此前完成且答 29 时就是该状态，也不能证明低算力集群**。`credits_would_enable` 是服务端可用性提示，不等于授权自动花费额度；本轮没有进行任何额度消费操作。

原始脱敏字段摘要与派生对照为 `.local/research-20260925/workspace-metadata/{report,review}.json`。未知字符串仅保存摘要，身份字段不写入原值；不是把后台完整账户资料复制到报告。

## OAuth 与来源链

四个实际 AT 的字段集合相同，`client_id` 都是 Codex 官方 client，audience、issuer、四项 scope、AMR、plan、localhost/sl/email_verified 等相同；leaf 差异仅账号/用户/会话身份与签发、过期、密码认证时间等。source scope 是 `openid email profile offline_access`，而固定官方 CLI 默认还包含两个 connector scope。这是所有四账号共有的来源差异，不能解释本轮为什么一个账号答 21、另两个答 29。

只读检查 28765 checkout 的目标 workspace 选择、Team AT claim 校验、advanced/prolite seat 映射、在线 usage 检查，没有找到该路径把 personal token 仅改标签当成 Team 的证据。实际 seat/role、历史 OAuth 环境和更早的 usage 时间线没有被当前源码替代。详细来源和精确位置见 [工作区源码研究](quality-20260925/workspace-source.md)。没有修改 28765 源码或部署。

固定 CLI 的普通 Responses 构建没有找到按 plan 设置 512 reasoning budget 的路径。显式 effort 和目录默认的来源与实际请求已分开核对；命中 512 的 Guardian 或内部缓冲常量不能拿来解释四次 516 reasoning tokens。usage 会影响额度说明和恢复选项，不等于修改正常已发送请求的推理档位。[权限处理源码](quality-20260925/workspace-entitlements.md)记录正面证据与查找范围。

## 网关确有一个诊断信息丢失问题

网关原管理 usage parser 只保留普通窗口、部分 credits 和 additional limits，丢掉上游提供的 `rate_limit_reached_type`、`spend_control`、`additional_rate_limits.normal_model_slug`；本次实际响应里的 `model_usage`、`credits.overage_limit_reached`、`rate_limit_reset_credits.applicable_available_count` 也没有进入持久快照。因此即使上游明确区分了成员额度耗尽、工作区限额等，管理 API 仍看不到原因。

修复仅在管理 parser/cache 中保留这些类型明确的字段：已知限额原因按官方枚举，未知原因记 unknown，不透传任意 details/upsell/account/user/email；显式 false/0 与缺失/null 不混同。可选金额保留合法数值的原字符串，未知格式留空；无法理解的可选日期留空，不丢掉同响应中的已知可用性和额度值。缓存新增个人消费限额与模型恢复时间的到期失效判断。

这不改模型请求、账号调度、冷却、认证或 TLS，不自动切 Reserve、恢复信用或换模型。实际 GET→管理快照→缓存→API 红测试先复现字段丢失，边界红测试先复现新时间字段过期后缓存仍可用，随后修复。独立复核又复现新增可选字段的异常 JSON 类型会连带丢掉有效核心额度，随后按新字段隔离处理；旧核心字段仍严格校验。

## 已验证并上线

北京时间 **2026-09-25 04:22:21** 完成运行时验收，生产后端 SHA256 为 `161bc6157580b950a4847e9a492174866dadf2fbef8d7842e7773ea57f888126`。相对上一版 `f8edfc3e…b9a3db7`，生产代码只改 `admin_account_upstream.go` 与 `admin_account_usage_cache.go`，其余推理源文件逐文件 hash 不变。原生 helper、环境变量、HTTP/WS TLS 配置、前端、账户元数据保持不变，没有数据库迁移。

验证刻意区分两个 Linux 快照，避免以初版测试替代最后的兼容补丁：

| 验证范围 | 实际结果 |
| --- | --- |
| 初版管理修复的完整 Linux race / PG / Redis / native 回归 | 907 个顶级通过、0 失败、23 个 opt-in 跳过，2648 个含子项终态；331.816 秒，零 DATA RACE，vet 通过。 |
| 最终可选类型隔离补丁的 Linux 全管理链路回归 | 125 个顶级通过、0 失败、0 跳过，348 个含子项终态；24.868 秒，race / PG 管理持久化和缓存用例通过，vet 通过。相对上面快照仅解析器和新增测试变化。 |
| 最终代码 Windows 完整回归 | 786 个顶级通过、0 失败、146 个运行条件不满足的跳过，2517 个含子项终态；39.738 秒，vet 通过。 |
| 独立只读复核 | 原 3 个红例修复、8 类旧核心错误仍拒绝、金额词法精确保留；22 个隔离解析用例通过，实际管理 24 个顶级 / 50 个含子项通过。 |
| 生产 HTTP / WS | `/v1/models` 返回 200 / 9 项；WS 返回 101、accept 正确、正常关闭后 in_flight=0。仅握手，没有模型 turn。 |
| 生产管理额度往返 | 实际 Q0 upstream usage 返回 200，新 spend_control/model_usage/rate_limit_reached_type 字段经过 POST 检查、持久存储和 GET 快照后相同，health=ready。此次额度读取不产生模型推理。 |

最终构建及部署记录为 `.local/research-20260925/usage-entitlements-continuation/{build-manifest,activation-result,runtime-smoke}.json`；Linux 初版和最终快照分别为 `.local/gateway-audit-20260924/go-usage-entitlements-20260925-final/` 与 `go-usage-entitlements-review-20260925/`。补丁、源码 SHA 和三类先失败后通过的证据均保留。本次没有重跑此前已覆盖的官方 CLI 推理链路：相关生产源文件没有变化；也没有把握手验收算作质量恢复。

**修复已上线，先前答 29 的根因仍未证明。** 本轮确认并修复的是网关丢弃已知限额/可用性诊断及新状态过期缓存问题，不是已验证的账号降档恢复。
