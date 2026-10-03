# 工具发现结果与当前声明的缓存前缀

核对时间：2026-09-29 04:14（UTC+8）。本页修复已随 **0.4.20 / PID 2479613 / binary f71fc1a2…5e5c4ee** 上线，release `6207cd8871eb5bba…`、source `5b760dc5…`。04:12:25 的正式现场回执验证两次生成、一次本地 top_p 拒绝及三次 CSV 对账，settings 27、账号配置、api_key 缓存域及 cooldown=false 保持，见[部署独立核验](../.local/collab-a-20260928/discovery-release-review/postdeploy-review.json)。current request SHA `8ea9a509…` 与私有 `02334388…` 仅解释注释不同，见[集成指纹](../.local/collab-a-20260928/discovery-policy-consumer-review/adopted-source.json)。首次实验及单请求诊断均在基线第一轮 HTTP400 停止，原因是 client tool_search 声明缺少 parameters；只补该字段后的独立四请求对照已全部通过，结果如下。

`Prepare` 原本用一个集合同时完成两件事：将历史 `additional_tools` / `tool_search_output.tools` 加入有效工具目录，以及判断是否省略网关生成的顶层工具声明。当末尾新出现的发现结果完整重复顶层 `tools` 时，后一规则会删除先前稳定的头部声明，改变长历史之前的输入前缀。这个问题也可通过正常存储的上游发现结果和下一轮 `previous_response_id` 触发，并不限于手工构造完整历史。

候选只分离去重视图：两种历史来源仍完整进入有效工具目录；仅显式 `additional_tools` 可抑制网关生成的同义顶层声明，`tool_search_output` 不再起这个作用。客户端自己的显式声明及 ID 保留，历史项内容和顺序不改，schema 不重新排序或截断。私有 `request.go` SHA 为 `02334388…`，相对已部署 `94093ef8…` 的改动见[候选说明](../.local/discovery-prefix-continuity-20260929/README.md)。

声明在 wire 中出现两次不意味着授权两份工具。`collectTools` 在出站去重之前按 type/name/namespace 和定义合并；策略、强制 namespace、并行限制、formatter 碰撞检查均使用这个有效目录。A 独立真实网关测试已确认：

| 消费边界 | baseline 与 candidate |
|---|---|
| 重复发现历史 + structured + required，仅内部 formatter 返回 | 均拒绝，不能用格式化动作满足客户工具要求；无可续接 history、未交付答案或 TTFT。 |
| 相同历史下返回已声明客户调用 | JSON/SSE 均成功，保存精确 namespace。 |
| ToolPolicy 计数 | wire 声明 1→2，有效 catalog、prepare/rejection count 均保持 1。 |
| 已收到的真实用量 | input100/output10/read80/create7 保持。 |

当前本地消费者 fixture `302c66d6…` 在两版各一顶层四子项通过，八个请求仅本机 HTTP，无外部模型/SQL。首版 fixture 曾遗漏 `tool_search_output.status`；其原日志保留，修正版明确设置 `status:"completed"`。它验证调用/结果配对与网关策略，仍不是完整上游请求模板：其 search 声明没有后续真实诊断所要求的 parameters。不能把本地 `Prepare` 容忍等同于完整官方协议或真实 BPS 接受，两个批次来源不混算。见[A 消费者报告](../.local/collab-a-20260928/discovery-policy-consumer-review/GATEWAY-REPORT.md)。

协议证据需要区别 client 与 server 发现：client 输出关联实际非空 call_id，status 为 completed，tools 为定义数组；server call/output 的 call_id 可以为 null。官方 CLI 中允许的形状、当前网关可通过的形状、真实 BPS 上游接受的形状是不同层次。主任务的[本地官方 writer 审查](../.local/discovery-prefix-continuity-20260929/history-review/REPORT.md)已保留这一区分，不能用通过 `Prepare` 替代上游实验。

私有字节对照的 input 共同前缀从 112 增至 99,227 字节，是前缀稳定性证据，不是实际缓存 token。真实对照需将主要长上下文放在生成声明之后的用户历史中，否则长 instructions 在声明之前仍可被命中，会掩盖本候选要测的失效位置。主任务独占真实请求预算；A 不另发对照或把此前 formatter-tail 的 99.365% 结果转用到本候选。

候选恢复声明也增加出站正文和相应编码工作，不能概括为零成本或普遍更快。当前未修改冷却、重试、owner、历史有效期或完整 done 对象选择策略。

实际首轮 run `20260928T194920.043117Z` 已终态 exit1：fixture `9227f728…`、test binary `7528dc8e…`、runtime source 无变化。保存的唯一 row 为 baseline/turn1、generation_count1、public HTTP400、candidate_inserted_first_turn_declaration=false，插入前后输入 SHA 相同。该行没有公开上游错误详情、完成终态或缓存计量；不能从它推断候选重复声明失败、真实发现历史不受支持、额度问题或缓存收益为零。最大预算四请求不等于已经发出四次。见[原失败回执](../.local/discovery-prefix-continuity-20260929/20260928T194920.043117Z/report.json)及[唯一原始行](../.local/discovery-prefix-continuity-20260929/20260928T194920.043117Z/live.log)。后续若调整探针，应保存这轮失败，并针对明确拒绝边界另建运行，而非把它改成已完成 A/B。

随后诊断 run `20260928T195233.805390Z` 仅一次 generation，实际错误为 `Invalid Value: 'tools.tool_search.parameters'. Client-executed tool_search requires parameters.`。它定位基线声明不完整，仍未测试重复声明。主任务在 `20260928T195427.237854Z` 只给 searchTools 增加对象参数 schema（query/limit，required query，additionalProperties false），保持其它工具与执行设置；旧两次失败不算新 A/B 的成功部分。见[单请求诊断原日志](../.local/discovery-prefix-continuity-20260929/20260928T195233.805390Z/live.log)。

修订 run `20260928T195427.237854Z` 实际 exit0，fixture `00eb3233…`、binary `c045eb55…`，四条 row 均 HTTP200、实际发现 call/result 配对和答案通过，记录计量与上游一致：

| 变体 / 回合 | input | cache read | cache creation | 当前头部声明 |
|---|---:|---:|---:|---|
| baseline / 发现 | 82,599 | 0 | 82,284 | 存在 |
| baseline / 续接 | 82,737 | 0 | 82,423 | 被省略 |
| candidate / 发现 | 82,599 | 0 | 82,284 | 存在 |
| candidate / 续接 | 82,807 | 82,284 | 209 | 插回首轮原对象 |

候选续接读取比例为 **99.3684109%**，不把 read 与 creation 混成命中总数。实验仍编译 .19 runtime，通过 transport 插回第一轮实际出站声明；因此它先证明这个 wire 操作可接受并保留缓存，不能直接说新 `Prepare` 源码已完成正式验收。每个变体各自有真实模型生成的发现调用/历史、独立 marker/cache key，回合内容和输出数量有差异；两次续接输入量也不同。每臂只有一次续接、顺序固定，不能称四份请求只有声明一个变量或量化长期 TTFT 收益。该测试 scope 为 `client-cache-key`，生产仍 `api_key`。见[修订原日志](../.local/discovery-prefix-continuity-20260929/20260928T195427.237854Z/live.log)及[回执](../.local/discovery-prefix-continuity-20260929/20260928T195427.237854Z/report.json)。

三个运行实际 generation 数分别为 1、1、4，共六次；前两失败完整保留，不混进修订四请求的通过率或缓存比例。

本版新增正式 `TestBPSLiveDiscoveryCachePrefix` 直接使用新 `Prepare`，读取 `GetBody` 副本观察实际出站，不改写正文。测试用现有凭据解析器恢复完整 owner，采用生产同类 `api_key` 缓存域，要求两次真实 generation 的实际 cache key 不变、原前缀与完整首轮输出保留、真实 call_id/registry 结果配对及三方 read/create 计量一致，并保留第二轮 ≥70% 读取断言。凭据解析不等于 JWT 签名校验；同 owner/key/session 两轮也不代替跨会话隔离覆盖。该正式组的运行结果必须独立读取，不能用前述 transport A/B 的 client 域结果补齐。

正式 run `20260928T200048.756989Z` 的这一新增组已实际 PASS：第一轮 input82,595/read0/create82,280；第二轮 input82,849/read82,280/create255，读取比例 **99.3132084%**。两行 `bps_prompt_cache_scope=api_key`、`outbound_body_mutated=false`，原前缀、真实发现调用/结果、答案和三方计量检查均通过。见[本版原实测日志](../.local/reliability-modes/live/20260928T200048.756989Z/live.log)。这独立证明本次两轮合成场景，不能把它外推为所有自然请求的命中率。

本版全 Go/vet、19 项真实 PG、单轮 race 和十一组 live 均已终态通过，新四项核心与正常存储结果续接回归均实际执行。未变前端 237 单测/三项浏览器检查/55 产物、监控 30 项证据逐 SHA 复用。相同源、九件关键证据及实际发布 binary 的绑定见[关键包审](../.local/collab-a-20260928/discovery-release-review/package-critical-review.md)。部署后普通 smoke 未触发工具发现两轮路径，不能代替上述正式测试；自然缓存收益仍需新流量窗口。

后续首次有实质自然样本的固定窗口为 **2026-09-29 04:26:48–04:46:48（UTC+8）**，同 .20 PID2479613。20 分钟内 759 请求（747 成功、12 失败），已知输入 36,719,176 / cache read 30,873,206，读取比例 **84.0792%**；cache creation 5,642,249 单独计，747 条已知读/写样本、12 条读取未知。三条发布 smoke 在 04:12:25 前结束，已在该窗口之外，不能再减三；同起点进程累计 762 扣三条的各计量与窗口精确一致。这是实际自然表现，不是受控 99.31% 的复现；混合模型、上下文与请求类型的聚合没有对照组，不能单凭本窗把收益归因于此补丁或保证今后比例。见[固定自然窗口复核](../.local/collab-a-20260928/compact-release-review/predeploy-natural-window-review.md)。
