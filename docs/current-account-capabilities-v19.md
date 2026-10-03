# 当前两账号的目录与凭据声明对照（v19）

2026-09-25。new2/new1 当前模型目录和主要 AT 应用声明一致；这不能证明两个账号的隐藏状态或每次模型输出必然一致。该批没有模型推理、OAuth refresh或账号配置修改。

目录侧每账号一次models GET和一次usage GET，均200，usage均allowed。9个模型全字段比较相同，`gpt-5.6-sol`52个字段的精确数值词法canonicalhash逐项相同。两者目录都声明`low, medium, high, xhigh, max, ultra`，默认medium。该结果由目录工具记录，不能用usage allowed替代特定模型每次推理的服务端授权。[目录证据与清理](../.local/research-20260925/account-capabilities-v19/README.md)、[后续effort能力凭证](../.local/research-20260925/account-capabilities-v19/effort-support.json)

独立凭据脚本只从版本绑定的管理export在内存读取AT及可选ID token，不输出RT，前后重读管理状态。48项离线结果核验通过：两者record/token/user/workspace/identity摘要均匹配v18，管理Version分别8、10且读取期间未变；管理identity与export内identity的user/workspace分别匹配实际AT的本地claims，两者ID token均未提供。[独立48项回执](../.local/research-20260925/account-capabilities-v19/review/claims-results.json)

| 可见AT字段 | 两账号观察 |
|---|---|
| iss / aud / client_id | 均存在，类型和原canonical值摘要一致 |
| scp | 均存在且相同：openid、email、profile、offline_access |
| chatgpt_plan_type | 均为self_serve_business_prolite |
| azp / scope / auth_time | 两者均缺失；不是观察到两个同值 |
| iat / exp / nbf / pwd_auth_time | 数值摘要不同 |
| user / workspace / membership / session / jti / profile | 有相应账号差异，完整差异以安全树为准 |

通用比较器有26个不同路径，其中包括随子值变化的父对象摘要，不能当成26项独立权限差异。叶路径另行列出，包含一个AT auth namespace未知字段，以及两个管理identity未知字段，名称与值只保留hash；本审阅不将其命名为风险、算力或账号档位。两个存储client_id的比较都是`not_comparable`，因为管理凭据未提供可比较值；这不等于AT client_id彼此不同或与官方应用不匹配。

以上全部是**未验签的JWT本地声明与当前管理元数据一致性**，不做issuer key、audience授权、历史注册或服务端风险验证。`iat`是token签发时间，不是账号注册时间；`pwd_auth_time`保持私有数值、单位未确认，不按位数猜毫秒或换算日期。已观察的AT寿命差`exp-iat`两者同为864000秒，也不表示账号年龄相同。[原始安全claims报告](../.local/research-20260925/account-capabilities-v19/claims-summary.json)

真实执行脚本是SHA`b4225146e060a276d2060b98e42fa79a7ecfbca63406073caf8a1cf12ca96e0f`，复用已修时间单位的纯工具SHA`9bfd49ab22ae852648fc737240bbfa7363eb8df3d71cf6f184a2dd86e294074f`。之后仅本地把loader改成直接执行已验hash的同一份bytes，消除二次打开文件的TOCTOU；未来脚本SHA为`ec4f3f577097c241e0deea274eaf0291b4b9de875a25665075d0f2b21881d1e9`，**没有为这个实现改进重跑账号读取，也不把它记为已执行版本**。11项离线合成测试通过，覆盖epoch变化安全回执、读期间变更、元数据版本与凭据版本区分、身份冲突、RT不输出、只读路径、redirect拒绝和trickle绝对期限。export可能写正常管理审计项，非账号状态写入。

## 后续单字段 effort 控制的源码边界

固定CLI`core/src/client.rs:765`在明确effort缺省时使用模型默认，随后调用`ModelInfo.resolve_reasoning_effort()`；Lite继续发reasoning.context=all_turns。`medium/high/xhigh/max`原值通过，`ultra`是模型相关alias，会优先解析为目录multi_agent_reasoning_effort、否则max/最后支持档位；`persistent`映射成disabled。故后续比较medium与xhigh可以直接用明确wire值，不以ultra别名替代高档位。[固定实现](../.local/codex-source-0.155.0/codex-rs/protocol/src/openai_models/reasoning_effort.rs:10)

请求选择、真实发出的reasoning.effort、返回response.reasoning.effort是三层证据。CLI的model_requested/config只能证明选择，未捕获wire时不能冒充实际发送；原生IPC可证明出站；response回显缺失时应记未观察，不用请求值补齐。`reasoning_tokens`只是用量，不能反推出服务端确实执行某个努力档位或固定预算。

v20若采用单字段控制，应在同一原始完整body中只替换reasoning.effort原value span，保持model、context、summary、input、cache/session、headers、tier、账号epoch与helper；变体在答案产生前预登记，额外模型参数或提示修改均不属于同一干预。目录支持是前置事实，不预先保证输出改善。此前旧q1 xhigh已实际两次29，但旧q别名和本批new1并非同一账号，不混成当前账号的结论。
