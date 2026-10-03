# Codex 网关并发、连接与限流资料调查

检索时间：2026-09-18 03:37–03:43（Asia/Shanghai）。范围：官方 OpenAI/Codex、Go/RFC、GitHub 原始问题及 Linux.do 第一手经验。只进行公开资料检索及已下载官方源码核对；未读取真实凭据、修改 Go、访问 18876 数据或执行生产推理。账号同步与 30/50/70/100 实测由集成 root 负责。

**可落实的方向是稳定复用连接、按来源分类错误、账号级背压与冷却、失败刷新去重、分层观测。没有查到可以据以保证“换 IP、改 HTTP 头或仿 TLS 就能消除风控或恢复模型能力”的官方证据。** HTTP 可用性、速率和任务正确率必须分别验收。

## 直接证据

| 来源与时间 | 已核实内容 | 对本项目的适用边界 |
| --- | --- | --- |
| [OpenAI API Rate limits](https://developers.openai.com/api/docs/guides/rate-limits)，2026-09-18 读取 | 临时 429 与 503 应结合错误 code、Retry-After 处理；缺失提示时退避加抖动，限制次数及总时长，避免多层重试叠加。 | API 文档不等于 ChatGPT OAuth 后端的固定容量合同；不将 API RPM/TPM 套到 Pro 账号。 |
| [Codex/Work usage](https://learn.chatgpt.com/docs/pricing#what-are-the-usage-limits-for-my-plan)，2026-09-18 读取 | 用量依模型、任务和上下文变化；额度及重置时间需看当前 usage。 | 页面未给“每 Pro 固定支持 100 同时请求”的保证；同步成功也不证明目标并发可用。 |
| [Codex 0.154.0 provider 源码](https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/model-provider-info/src/lib.rs#L314)，本地与网页交叉核对 | HTTP provider 的 `retry_429` 为 false，5xx 和 transport 则可重试。 | CLI 显示 retry limit 不证明它已重试；不能用返回 503 伪装本地 busy 来诱导重试。 |
| [Codex 0.154.0 retry-after 测试](https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/core/tests/suite/retry_after.rs#L244)，2026-09-18 核对 | HTTP 503 测试明确记录仍用本地退避而非上游头；另有 WS 429 为终止错误的用例。 | 网关仅透传 Retry-After 不等于官方 CLI 会遵守；需要实际 attempt 时间线。 |
| [Go net/http Transport](https://pkg.go.dev/net/http#Transport)，2026-09-18 读取 | Client/Transport 可并发复用；空闲连接数、活动连接总数、响应头期限是不同参数。 | 池内 client 条目上限不能代替 socket/请求并发限制；HTTP/2 多路复用需实测。 |
| [Go HTTP tracing](https://go.dev/blog/http-tracing)，2016-10-04，2026-09-18 复核 | httptrace 能观测 DNS、连接建立/复用、TLS、写请求和首响应字节。 | 可定位 502 与延迟阶段；首响应字节不等于模型首 token。 |
| [RFC 9110 Retry-After](https://www.rfc-editor.org/rfc/rfc9110.html#name-retry-after)，2022-06；[RFC 6585 429](https://www.rfc-editor.org/rfc/rfc6585.html#section-4)，2012-04 | Retry-After 支持秒数与 HTTP-date；429 本身不规定限流主体如何识别。 | 不能把任何 429 自动解释为 IP、账号余额或模型拥塞；必须保留来源与有效载荷分类。 |
| [Codex rate-limit 头解析](https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/codex-api/src/rate_limits.rs#L61)，2026-09-18 核对 | 官方客户端识别 primary/secondary 窗口、reset-at、used-percent 及命名限额族。 | 保留这些字段，以实际出现的 reset 确定冷却；不要只识别一个固定头名称。 |

官方源码固定为 `rust-v0.154.0`，既有交付记录 commit `6b9826e3aa83b1a5947db50f4332cb9c65f1b340`。本轮读取本地 `model-provider-info/src/lib.rs:314`、`codex-client/src/retry.rs:22`、`core/tests/suite/retry_after.rs:247` / `:1463`；未重新编译 Rust 测试，不把源码断言写成此次动态实测。

## GitHub 与 Linux.do：原始经历不是通用根因

| 原始链接 / 日期 | 能支持的结论 | 尚未证实的部分 |
| --- | --- | --- |
| [openai/codex #30471](https://github.com/openai/codex/issues/30471)，2026-06-28 | 报告者追踪到 HTTP 429 不重试却使用 retry-limit 文案；与本次 0.154.0 源码相符。 | 不代表所有 CLI 版本、SSE/WS 路径都相同。 |
| [openai/codex #17068](https://github.com/openai/codex/issues/17068)，2026-04-08 | 原始 503 文本含上游连接拒绝和 WS URL，说明需检查完整错误链。 | 没有证明该用户 IP 被风控，也不能定位本项目的 503。 |
| [openai/codex #32337](https://github.com/openai/codex/issues/32337)，2026-07-11 | 用户报告多实例共享凭据下重复认证恢复，随后 device-auth 429。 | 这是认证端点经历，不是模型请求容量测试；不据此推断本项目发生了注销或 RT 撤销。 |
| [golang/go #34941](https://github.com/golang/go/issues/34941)，2019-10-16，已关闭 | 历史复现涉及 HTTP/2 + MaxConnsPerHost 的交互。 | Go 1.13 问题不能冒充 Go 1.26 当前缺陷；仅作为取消/并发耗尽回归场景。 |
| [Linux.do 1674489](https://linux.do/t/topic/1674489)，2026-03-01/02 | 楼主反馈将内网网关加入 NO_PROXY 后 502 消失，支持“先查是否额外绕行代理”的排查步骤。 | 其中关于所有 Rust 程序/系统代理的一般性说明并未得到该帖技术验证；不能照抄为定律。 |
| [Linux.do 2579180](https://linux.do/t/topic/2579180)，2026-07-13/14 | 楼主报告 50 并发、429 和低输出速率同时出现。 | 未给受控基线、出口对照、请求数/成功数与原始 headers；无法证明风控、降智或固定并发阈值。 |
| [Linux.do 2296657](https://linux.do/t/topic/2296657)，2026-06-03 | 多位用户同一时段反馈额度仍有、无并发也有 429。 | 仅为相关性，不据此宣布当时或现在官方故障。 |
| [Linux.do 2300870 第 7 页](https://linux.do/t/topic/2300870?page=7)，2026-06-04 | 中转使用者贴出 `concurrent_conversation_limit_exceeded`，说明聚合站自身并发限制也可能以 429 呈现。 | 无法仅凭这一段判断限制出自中转还是原始上游，更不能推广为所有 Pro 只支持一个会话。 |

检索未读取需要登录的帖子内容，也未采用转述文章代替以上原始链接。社区结论只用于产生实验假设。

## 建议的错误分类

以下为本项目可实现的数据结构建议，不是上游承诺字段。

| 分类 | 判定依据 | 调度处理 |
| --- | --- | --- |
| `local_admission_busy` | 本地未向上游发送、账号槽位满 | 同账号短有界队列或明确 429；不得标记代理坏/账号额度耗尽。 |
| `upstream_concurrency` | 上游 429 且明确并发错误 code | 调低该账号/模型目标并发；完成释放后逐步恢复，不立即轮询。 |
| `upstream_rate` | 临时 429、有效 Retry-After / reset 信息 | 保存冷却状态；未绑定的新请求选择其它可用账号，绑定会话留原账号等待/返回准确提示。 |
| `upstream_quota` | 明确额度、账单或长窗口耗尽错误 | 使用可见重置时间或人工恢复；不能用短周期重试当作恢复方案。 |
| `upstream_overload` | 503 + 明确 overload code | 记录模型/出口相关性，遵循时间提示；不直接永久禁用账号。 |
| `proxy_connect` | 在 CONNECT 阶段收到 403/407/429 或建隧道失败 | 分别记录代理策略/认证/容量错误；不算模型上游返回，也不显示代理检测通过。 |
| `transport` | DNS/TCP/TLS/响应头阶段错误 | 保存错误类别和阶段时间；失败并不证明请求未送达，不能盲重放可能已接受的 turn。 |
| `stream_terminal_error` | HTTP 已为 200，SSE/WS 终态失败 | 保留 HTTP 状态与模型终态双维度；已输出部分内容的请求不自动重放。 |
| `unknown` | 缺头、非 JSON、未知 code | 保留脱敏摘要与来源，维持未知，不硬归因于风控或额度。 |

## 可实施顺序

1. **先减少不必要的连接建立。** 按代理 ID/配置版本、传输模式及确有区别的超时配置复用 transport；禁用、密码轮换、删除时淘汰旧路由。池条目、空闲连接、活动连接与业务并发各有独立上限。自定义 header timeout 不能重新退回逐请求 clone 池。基于 Go 文档选择参数语义，具体数值由本项目 FD、协议与延迟实测决定。[Transport](https://pkg.go.dev/net/http#Transport)
2. **错误观测先于自动恢复。** 给每次业务请求分配本地 trace 标识，记录 `attempt`、匿名账号/出口 ID、阶段、HTTP/模型终态、重试提示与是否输出。增加 DNS/connect/TLS/header/TTFT/完成耗时及连接复用率。日志不存凭据、代理密码、完整正文。[httptrace](https://go.dev/blog/http-tracing)
3. **429 使用状态机。** 建议 `ready → cooling → probe → ready/cooling`；冷却 key 至少包含稳定账号身份和有证据支持的限额 scope。有效 Retry-After 到期前不发探测；超出请求等待预算则返回/延后，不能截短服务器要求的等待。无有效提示时采用有上限的指数退避加抖动；重新导入凭据与单纯改名分别处理，不能让改名清空状态。[API 指南](https://developers.openai.com/api/docs/guides/rate-limits)、[RFC](https://www.rfc-editor.org/rfc/rfc9110.html#name-retry-after)
4. **把准入与重试预算分开。** 可以评估每账号有界等待队列，但排队不得占 PG 连接/模型槽位，取消必须立即退出。队满立即反馈，本地队列长度/等待时间有指标。真正投递前再检查权限与账号版本，固定会话不跨身份迁移。只选一层控制重试，汇总 CLI + 网关 + HTTP 库的实际 attempts，避免乘法放大；本项目默认不新增模型 POST 盲重试。
5. **OAuth 失败去重独立于模型冷却。** 以稳定账号身份和凭据版本合并同时失败的刷新；临时故障短退避，明确 invalid_grant 等终止错误等待凭据更新。保存新 token 后再发布成功结果；不因为客户端断开而丢失已取得的新 RT。18876 与本 Agent 若使用同一份会旋转的 RT，需由同步流程明确唯一刷新所有者；公开资料不能替代这项本地一致性设计。[原始认证问题](https://github.com/openai/codex/issues/32337)
6. **代理路径做对照，不做“风控魔法”。** 区分 CLI→网关和网关→上游两段；NO_PROXY 仅排除确需直达的 loopback/网关地址，不能顺手让上游走错出口。同账号/会话保持可解释的代理选择；用协议、成功终态、TTFT 和连接阶段对比出口，不凭地区或 UA 名称宣称更聪明。[Linux.do 原始排查](https://linux.do/t/topic/1674489)
7. **缓存优化独立验证。** 固定模型、工具顺序、推理设置、静态前缀与请求历史；测 `cached_tokens / input_tokens`、TTFT 和任务结果。API 文档说明缓存命中依赖匹配前缀且不改变输出生成；新缓存字段对当前 Codex OAuth 端点是否受支持仍需协议验收，不能直接照搬。[官方缓存说明](https://developers.openai.com/api/docs/guides/prompt-caching)

以上状态机/队列/观测是工程建议，需要实现方评审和合成故障测试，并非已经完成的补丁。三个具体源码问题已**追加**到 `CODEX-SOURCE-COORDINATION.md`：Name 准入、失败 OAuth 串行重复、429 无冷却，附独立复现步骤；没有覆盖其他会话内容。

## 30/50/70/100 阶梯实测记录要求

每档同时记录“配置目标并发、实际同时在途峰值、账号数/每账号上限、启动速率”。30 个被准入、70 个立即拒绝不能写成成功支持 100 并发。同一时刻同步发起的 burst 与平滑补充的 steady-state 分开；先固定模型、推理档位、输入/输出规模和传输方式，避免用换模型掩盖容量变化。

每档输出：投递数、实际 upstream attempts、完成终态数、HTTP 状态分布、错误来源分布、取消/超时、P50/P95（排队/TTFT/全耗时分别）、每秒完成数、输出 token/s、缓存 token 比例、峰值 RSS/CPU/FD/TCP、PG 等待与 Redis 延迟。另存冷却触发/恢复、OAuth 次数及路由版本。客户端单次超时与整档总 deadline 分开，不凭 HTTP 200 算成功。

30/50/70/100 是待测负载档位，并非已证实安全容量。真实响应若给 Retry-After，后续同 scope 请求遵守该时间；所有未执行或被冷却延后的样本如实记录。任务质量采用同一合成题集和可验证答案评分，与网络吞吐分列；观察到一次错误或低 token/s 不称“降智”，找到一个快出口也不称“消除风控”。
