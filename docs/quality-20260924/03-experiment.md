# 糖果题核验、评分与对照实验

2026-09-24。此子任务只读已有请求结果并实现离线评分，没有发送生产请求、移动账号或更改推送地点。

## 历史基线证据

重新读取 `.local/mother-benchmark/20260924T063432Z-candy/results.jsonl`、`20260924T064009Z-load/results.jsonl`、对应请求和已保存 SSE 事件后，38 次糖果请求中 23 次回答 21、15 次 HTTP 错误，无可确认的错答。按模型分层，Sol 为 16/16 已完成正确、3 次 HTTP 错误；Astra 为 7/7 已完成正确、12 次 HTTP 错误。此次本地重评没有证明任何账号被标记或路由到特定计算集群，也不能排除其他账号存在尚未捕获的问题。

历史初轮还存在 1 次 `IncompleteRead` 和 1 次连接拒绝；后续轮次有 8 次 `401`。这些都是协议/认证/可用性样本，不是数学错答。原 80 并发诊断将 503 定位为传输 route 容量拒绝，详见 `docs/MOTHER-BENCHMARK-2026-09-24.md` 及对应服务器日志。

产物 `.local/quality-20260924/historical-candy-rescored.json` 保留可见最终回答、判分、来源与分母。23 条成功答案中，17 条从独立 SSE 项重构，6 条旧记录只能使用 `output_text`；这 6 条没有可用的独立事件文件，不能声称重新核对过完整 SSE。历史数据没有 AB/BA 分组元数据，因此报告给出 0 个配对，不事后编造对照组。

## 精确题面和数学边界

权威题面定义位于 `tools/mother_account_benchmark.py` 的 `CANDY_PROMPT`，格式要求位于 `CANDY_INSTRUCTIONS`。各次运行还保存 `candy-prompt.txt` 和 `requests/<request_id>.json`。题面原文：

```text
在一个黑色的袋子里放有三种口味的糖果，每种糖果有两种不同的形状（圆形和五角星形，不同的形状靠手感可以分辨）。现已知不同口味的糖和不同形状的数量统计如下表。参赛者需要在活动前决定摸出的糖果数目，那么，最少取出多少个糖果才能保证手中同时拥有不同形状的苹果味和桃子味的糖？（同时手中有圆形苹果味匹配五角星桃子味糖果，或者有圆形桃子味匹配五角星苹果味糖果都满足要求）
苹果味 桃子味 西瓜味
圆形 7 9 8
五角星形 7 6 4
```

指令原文为 `请独立解答用户给出的题目，解释推理过程，并在最后单独一行写“最终答案：N个”，其中N替换为你的整数答案。`，没有发送标准答案。离线工具对 `input` 和 `instructions` 的 canonical JSON 哈希为 `c4159abf32cb3e7573cc28cd9bdbaf3280fee9f3e5cda3b1458f52c5a75ff788`。

答案 21 的上界是 9 圆形 + 12 五角星。12 星形必含苹果与桃子，因为没有苹果最多 10、没有桃子最多 11；9 圆形必含苹果或桃子，因为西瓜只有 8。两者必能满足跨形状、跨苹果桃子的配对。

`tools/candy_strategy_verify.py` 穷举 201,600 个组成与 450 个固定形状配额，20 及以下均有失败见证，最优配额唯一为 9+12。本轮增加独立 minimax 检查：即使每抽一颗后能看见口味、据此自适应决定下一颗的形状，最坏情况仍需 21；共检查 29,340 个可达状态。测试包含此断言。

若不能在抽出前按触感选择形状，最坏坏样本可达 28，保证数为 29。原题“靠手感可以分辨”与“活动前决定数目”应理解为可选形状，但回答 29 也可能来自不同题意理解。评分仍按用户指定的 21 标为 `wrong_answer`，原始可见解释留给人工归因，不能直接把 29 判成集群证据。

若加“可在袋内按形状选择，每次抽出后不放回”等消歧义文字，必须另设 prompt variant、独立统计，不与原题池合并。中文与任何英文翻译同样分开：英文 must guarantee / choose by touch / at least 与 randomly draw 的变动会改变任务。重复中的所有 A、B 请求必须使用同一语言和完全相同题面。

## 离线评分工具

`tools/quality_probe_report.py` 复用旧 `results.jsonl`、`requests/` 和 `events/` 格式，不增网络发送系统，也不改写旧证据。

1. 先检查 HTTP、终态和错误，分类为 `network`、`auth`、`rate_limit`、`http_error`、`incomplete`、`upstream_error` 或 `protocol_error`。有一部分文本但终态不完整的请求不参与答题正确率。
2. 只取 assistant message 的 final answer，优先 `phase=final_answer`，不把 commentary 或 reasoning 项当成答案。优先读终态 message，再读 `output_item.done`，支持 `output_text.done`、`content_part.done` 和按项/片段索引聚合的 delta。非空但只含 reasoning 的 terminal output 也不能阻断最终文本重构。
3. 重建文本、终态文本、effective response 如冲突则标 `ambiguous`；没有明确最终答案、条件化结论、多答案冲突也标 `ambiguous`。既不会看到任意 `21` 就放行，也不会把输出缺失判为答错。
4. `correct` 为明确的最终 21，`wrong_answer` 为明确的其他整数。保留 `raw_final` 和 `rationale`，两者只含模型实际返回的可见最终解释，不解密、不推测隐藏 reasoning。输出 usage 的 token 计数作为诊断信息；token 数不能证明实际算力或集群。
5. 报告只复制白名单字段；不导出请求头、Cookie、认证对象、完整 response、加密 reasoning 或账户邮箱。账号做稳定匿名标签，可见文本再遮盖 JWT/Bearer/API-key/邮箱等模式。此工具面向纯合成糖果请求，不用于导出任意敏感会话。

复跑历史评分：

```powershell
python tools/quality_probe_report.py .local/mother-benchmark/20260924T063432Z-candy .local/mother-benchmark/20260924T064009Z-load --output .local/quality-20260924/historical-candy-rescored.json
```

专属回归：

```powershell
python -m unittest discover -s tools -p test_quality_probe_report.py -v
```

当前 45 个测试通过，覆盖数学两个语义/自适应选择、终态空/仅 reasoning/文本 done/片段 done/delta、来源冲突、错误分类、重试/模型/effort/账户/题面/输出上限混杂、AB/BA 顺序、真实 live 记录扁平格式和敏感字段不复制。

## 多账号对照设计

先把 arm 定义写入实验记录，例如 A=现网网关、B=候选网关，或者 A=官方 CLI 直连、B=网关。单个比较只改变待验证因素。若 CLI 与网关指令、tools、include、reasoning 设置、text verbosity 不同，该对照只能说明请求配置组合差异，不能归因 TLS。

每个账号、model、effort 为一个独立分层。确认网关实际命中指定账号，而非只看意图配置；记录 attempt 数，禁止自动失败换号把两个账号混进一对。采集 requested 与 response 的 model/effort，不把“配置了 medium”等同于实际发出了 medium。并行限制为每账号 1；短突发压测另开实验，避免容量拒绝和排队与题目质量混杂。

同一分层预先排定平衡的 AB、BA 顺序，例如 4 个近邻配对按 AB、BA、BA、AB 执行；每请求使用新的会话，没有前次解题上下文或 `previous_response_id`。每个账号至少先执行 4 对用于排查，出现稳定差异再扩充预先固定的重复数量。至少包含当前已观察异常账号与对照账号，不能把所有账号合并后用平均正确率掩盖某个账号的问题。

保留请求开始/结束时间、传输协议、session/cache 策略、两端二进制摘要、请求正文 canonical SHA、期望与实际账号匿名标签、输出/终态/事件、usage/effort/返回 model、响应状态与 attempt。两次近邻请求间如果发生 AT 刷新、账号更换、出口/IP/proxy 变更、配置变更或服务重启，应单列新 block，不把它当稳定配对。共享缓存状态可能影响后续请求，AB/BA 可以减少顺序偏差，但不能证明完全消除缓存效应。

Manifest 按真实 `request_id` 关联，不通过目录顺序猜账号或 arm。例如：

```json
{
  "req_a": {"account_label": "account-1", "arm": "A", "pair_id": "account-1-medium-01", "experiment_id": "gateway-vs-cli-01", "sequence": 0},
  "req_b": {"account_label": "account-1", "arm": "B", "pair_id": "account-1-medium-01", "experiment_id": "gateway-vs-cli-01", "sequence": 1}
}
```

`pair_id` 在一个 experiment 内必须唯一。工具要求恰好一 A 一 B、同账号/model/effort/prompt/settings、两端 `attempts=1`，返回 model/effort 如显式不一致就排除。没有请求文件时，采集器必须从实际请求生成并提供 `prompt_sha256` 和 `settings_sha256`；缺失不会默认视为相同。settings 对比排除 `input`、`instructions`（已有独立哈希）、`stream`、`store`、`prompt_cache_key`；后面三项的有意差异仍应在实验说明中披露。剩余请求字段，包括输出上限、temperature、tools、include、reasoning 附加设置，任何差异都排除配对。

```powershell
python tools/quality_probe_report.py RUN_A RUN_B --manifest MANIFEST.json --output REPORT.json
```

同时输出全尝试完成正确率、已完成可评分正确率、AB/BA 分层、账号/model/effort 分层、wrong→correct 与 correct→wrong 配对数。网络失败→正确保留为可用性转移，不计为答题改善。

## 统计和归因边界

历史 23/23 只说明这 23 次已完成请求正确。其 95% Wilson 区间约为 85.7%–100%；重复同一个题目/账号存在相关性，这个区间仅是描述性参考。4 对或 8 对适合快速定位显著异常，不足以证明低频错误已经消失。即便有 6 个独立、完全同方向的 discordant pairs，exact McNemar 双侧值才到 0.03125；如果只有 1 个 discordant pair，不应声称统计性修复。

工具提供 exact McNemar/binomial 描述值，不把重放同一题目视为多个独立题目。持续测试直到“全对”为止属于选择性停止；应事先规定样本数，并报告包括网络/认证错误在内的全部尝试。单题质量改善可以支持继续调查某个请求差异，但“被标记”“满血集群”“算力不足”等服务端因果需要服务端可验证标识或更强的实验支撑，不能由回答 21、延迟、reasoning token 数或默认 service tier 单独推出。

## 新 live harness 兼容与 Q0 首轮结果

随后 root 提供 `.local/quality-20260924/baseline-q0.jsonl`，这是 `gateway_quality_live_test.go` 的新扁平格式，与历史 mother harness 不同。记录包含 `account_alias`、`arm`、`round`、`sequence`、`response_status`、`response_reasoning`、`response_usage` 和 `event_counts`，不含完整 response 对象或独立终态字段。工具现在兼容该格式，但必须同时满足 `protocol_success=true`、`response_status=completed`、`event_counts["response.completed"]>=1`，且没有相反终态/错误记录，才允许数学评分。

Q0 首轮 6 条原始记录已在本地重新核验：production/candidate/no_prompt/transparent/routing_hint 均为 21，direct 为 29。direct 的可见解释计算最大坏样本 `12+7+9=28`，然后加一得到 29；这正是忽略可选择形状时的保证数。工具保留完整可见解释，并加描述信号 `29_matches_blind_draw_bound_28_plus_1`，没有把它升级成集群诊断。

其中 direct 与 transparent 的 `request_sha256` 和 `predicted_upstream_body_sha256` 完全相同；routing_hint 的两项正文哈希也相同。candidate 的 client request 哈希虽相同，但 predicted upstream body 不同，必须显示为上游正文变更对照。预测 hash 是请求准备代码的结果，不等于独立 wire capture；即使两项 hash 相同，也不能因此声称 TLS、请求头、时序、会话、出口完全相同。

新报告的 `arm_comparisons` 按同 source run/account/model/effort/round 汇集命名 arm，仅每 arm 恰好一条时形成描述性对照。逐字段输出 `same`、`changed` 或 `missing`，并显示 `prompt_changed`、`predicted_upstream_body_changed`、`same_request_and_predicted_body`、`body_comparability_unproven`。正文/提示改变的样本保留在报告里，不混入同正文质量比较，也不一律丢弃。重复 arm/round 无法唯一配对时记录 excluded，不任意选一条。

`--controlled-field FIELD` 可重复，用于记录实验者声明的控制变量；它是声明，不是工具验证字段差异的证据。对于严格 AB/BA manifest，除原有 settings hash 规则外，也允许用两端相同的 client request hash 和 predicted upstream body hash 证明该层面的可比性；只有 client request 相同而上游预测正文不同仍排除严格配对。

已将 candidate/transparent/direct 三条真实记录的白名单内容存成脱敏 fixture `tools/testdata/quality_live_q0.jsonl`，保留真实输出和哈希关联，移除完整 attribution/headers，并替换凭据摘要。新增测试覆盖三项终态证据缺一、矛盾终态、四次 429 + 一次显式 cooldown、29/28 解释信号、变更正文/提示、跨 run 隔离、重试/凭据更换及严格 AB/BA 的正文哈希规则。

429、网络错、未完成和 `local_cooldown` 均无数学分数。`gateway_attempt_count` 默认标记为 `unverified_record_correlation`，因为旧 harness 用 `Metrics.LastFor` 可能取得上一请求；不会仅凭 stale 1 将本地 cooldown 算成一次上游执行。显式 cooldown 标记优先分类为 `local_cooldown`。原始记录没有此标记时保留 HTTP 错误，不臆造限流来自上游还是本地。新的请求 ID 关联 harness 应同时标记可靠的 evidence 来源。

复跑该首轮：

```powershell
python tools/quality_probe_report.py .local/quality-20260924/baseline-q0.jsonl --output .local/quality-20260924/baseline-q0-report.json
```

此处的 Q0 是首轮单次采样，并不覆盖后续 ABBA、其他账号或后续补丁；这些应分别导入对应原始 JSONL 后再汇总，不能把任务消息中的进展描述当作已经审计过的最终证据。
