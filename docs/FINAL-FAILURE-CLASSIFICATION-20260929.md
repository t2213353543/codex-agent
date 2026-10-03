# 最终失败来源与管理分类

核对时间：2026-09-29 03:35（UTC+8）。本页修复已随 **0.4.19 / PID 2338498 / binary 46119b03…a8521c4** 上线，release `4f84927039696ea2…`、source `77fed72b7c1c1df1…`。03:34:07 的正式现场回执验证两次生成、一次本地 top_p 拒绝及三次 CSV 对账，原 settings 27、账号配置、api_key 缓存域及 cooldown=false 保持；首页与实际 Logs 脚本的 HTTP 字节也与发布包一致。见[部署与实际前端独立核验](../.local/collab-a-20260928/prefix-release-review/postdeploy-review.json)。

一个请求可以同时包含真实的上游失败信息和后续本地失败。例如上游返回 `response.failed`、原生 `rate_limit_error`，网关随后在交付前提交会话 owner 时遇到真实 CAS 冲突。此时 `final_upstream_error_code=rateLimitExceeded` 是已经观察到的上游事实，而请求的最终归因仍是 `error_source=gateway, error_phase=session_binding`。保留上游诊断不表示应把这个请求归为限流。

管理 Go 函数与 SQL CASE 现在采用同一有限优先规则：仅当请求状态为 `error`、最终 ErrorSource 为空或 `upstream`、FinalUpstreamErrorSource 为 `upstream` 时，已识别的最终 generation 机器码优先于旧错误文字。限流/额度、认证、过载分别得到 `rate_limit`、`authentication`、`overloaded`。成功行保持无失败类别；显式本地来源、未知机器码、没有该来源字段的其它记录继续原有规则。这里是抑制不适用的机器码优先，**不是删除旧 fallback，也不是保证任意本地错误文字都得到同一个类别**。

| 实际边界 | 管理结果与保留信息 |
|---|---|
| 原生 rate type 与 `model_not_found` 诊断冲突，最终来源确为上游 | 有效最终机器码决定 `rate_limit`，不再由旧文字先归为模型不可用。 |
| 原生 auth type 与 overloaded 文字冲突，最终来源确为上游 | 归为 `authentication`，实际机器诊断保持。 |
| 上游限流终态后交付前 CAS 冲突 | 保持 gateway/session_binding、原 CAS 原因；本用例管理分类为 `unknown`，上游 final code 仍保留。 |
| 旧记录或不认识的分类/码 | 沿原有 HTTP、状态及文字规则，不猜新的机器事实。 |

CAS 组合用例由真实本机上游和真实 session CAS 产生：一次 POST、未交付正文、TTFT/first-output 清空，原管理候选确实误归限流。修订版同 producer 的本地测试和 PG 用例均实际通过：PG 将该记录持久化后按 `unknown` 查到，同时 `rate_limit` 筛选不计入，保留 `FinalUpstreamErrorCode` 和本地来源。本轮原 19 项真实 PG 共 32 个含子项通过；与本机环境 SKIP 分开计证据。

常驻监控使用自身类别，例如 `upstream_rate_limit`、`upstream_busy`、`account_selection`，不要求与管理 API 的字符串逐字相同。修订后显式本地来源或 aborted 不再使用此前的 final/attempt/code 摘要推断上游限流或过载；原 CAS 文字继续得到 `account_selection`。监控允许投影层的 `unreported` 作为旧未知来源，管理记录的空来源则保留其旧数据兼容入口。原字段、attempt 列表、read/create/unknown 用量均不删除。

前端 Logs 和错误处理弹窗共用 `logFailure.js`。只有 `status=error` 且服务端已给出上述三类之一时，前端优先显示该类别；其它类别、成功/中断/未知状态仍沿旧显示路径。它不读取保留的 final_upstream 字段自行重判分类。A 将已保存真实 CAS 观测与固定错误文案组成有限 UI 视图，实际显示“请求失败”；其它旧文字/HTTP 规则保持，17 个纯 JS 样例验证了该范围。见[消费者审查](../.local/collab-a-20260928/prefix-release-review/local-source-ui-review.md)。

新前端的 **237 项单测、构建及三项浏览器检查**已经实际通过，246 源文件、55 产物及七份证据已与修订最终冻结源 `77fed72b…` 绑定，见[A 最终前端绑定](../.local/collab-a-20260928/prefix-release-review/frontend-final-binding.md)。修订全 Go/vet、真实 PG、单轮 race 和十组 live 均已闭合；[关键包审](../.local/collab-a-20260928/prefix-release-review/package-critical-review.md)绑定相同源及实际 binary。普通现场 smoke 没有故意触发 CAS/错误类型冲突，异常路径依上述同源 fixture 与正式门禁证明。

本次修改管理统计和说明，不改变重试、owner 选择、冷却开关、上游 HTTP 或 token 计量。首轮 `ad529779…` 的 Go/PG/race/live 结果与修订轮分开保留；新守卫不能借首轮通过结果宣称已验证。实际边界源码见[本页指纹](../.local/collab-a-20260928/prefix-release-review/classification-contract-source.json)，原失败和候选证据通过协作记录保留。
