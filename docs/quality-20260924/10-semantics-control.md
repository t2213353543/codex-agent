# 糖果题语义对照与独立数学核验

本子任务只生成离线实验材料、验证数学和测试评分边界，没有发送账号请求，也没有改动网关生产代码。新增 `tools/quality_semantics_variants.py` 和对应测试；原题、原基准与 `quality_probe_report.py` 保持原样。

## 对照的意义和限制

原题要求的通过答案仍为 **21**。已有 29 答案中出现的“最大不满足样本 28，再加一”是完全不能选择形状时的正确界；这使“模型忽略或误解可选形状”成为可以实测的解释，但不能仅凭这点排除账号差异或传输差异。该对照只检验解释能力描述是否改变输出，不检验服务端隐藏集群。

同账号、同模型、同 effort、同传输路径、相同 instructions，每个 variant 使用全新会话，不自动重试或换号，遇到限流就停止该账号的剩余尝试。生成器初始预设每个 variant 最多一次；正式 Q4 `plan.json` 将原题分成 direct、transparent 两个传输臂，各一次，并为两个语义控制各登记一次，总上限四次。没有返回完整终态的记录为可用性结果，没有数学分数。历史基线只能作背景，不能替代近邻同配置原题对照。

| variant_id | 唯一替换的原括号内部文字 | 离线答案键 | 可记原题通过 |
| --- | --- | --- | --- |
| `baseline` | 圆形和五角星形，不同的形状靠手感可以分辨 | 21 | 仅此项 |
| `clarified_shape_choice` | 圆形和五角星形，取出前可以在袋内靠手感分辨并选择糖果的形状，但不能辨别或选择口味 | 21 | 否 |
| `blind_control` | 圆形和五角星形；糖果取出前，参赛者完全不能辨别或选择形状，也不能辨别或选择口味，只能不加选择地逐个取出 | 29 | 否 |

`baseline` 的 `input` 与 `mother_account_benchmark.CANDY_PROMPT` 逐字符相同。两种控制仅替换上述能力描述，保留原表、目标和“活动前决定数目”等文字。三份 instructions 均等于 `CANDY_INSTRUCTIONS`。模型可见材料没有答案、分配数量、算法、抽屉原理或其他解题提示；全部数字只来自原表。`blind_control` 是明确改变能力限制的不同任务，即使回答正确也绝不能补算原题通过。`clarified_shape_choice` 也单独记录，不与原题池合并。

若观察到“原题 29 / 澄清 21 / 盲抽 29”，证据支持模型会随形状选择能力描述切换数学解法，且原题依然未通过；不能写成网关恢复。如果“澄清 29 / 盲抽 29”，只能说澄清后仍失败，不足以推出集群降档。某个控制请求限流、缺终态或回答冲突时，应明确控制缺失。一次观察没有稳定准确率的含义。

## 文件隔离与评分

运行：

```powershell
python tools/quality_semantics_variants.py
```

默认生成 `.local/quality-20260924/semantics-control/`：

- 每个 variant 单独的 `prompt.txt` 和 `request-content.json`。后者只有 `input`、`instructions`，可据此构造请求。
- `manifest.json` 含 variant、离线答案键、任务关系、prompt SHA-256、执行预设和评分边界；它明确 `model_context=false`，不能加入模型上下文。
- `mathematical-proof.json` 含完整数学结果及每个失败固定取法的反例，同样不能加入模型上下文。

manifest 的 `prompt_sha256` 对 `{input,instructions}` 按 UTF-8、排序 key、紧凑 JSON 计算。它不是旧 Go harness 对纯 prompt 字节的 SHA，二者不可直接比较。scorer 另保留捕获 input 的 JSON 哈希，便于区分字符串输入与单消息数组封装。

`quality_probe_report.py` 的 `EXPECTED=21` 不变，不能用其 `score_row` 对盲抽控制评分。新工具的 `score_variant_record(row, variant_id, captured_request, events=())` 复用旧工具的终态分类、最终可见 assistant 文本提取、答案解析和脱敏，只单独实现按 variant 的答案判断。它要求传入实际请求中的 input 与 instructions，并逐字符匹配已注册 variant；仅靠更改标签不能把原题 29 改判成盲抽成功。

评分支持原字符串 input，以及现有 Go harness 的恰好一个 user message、一个 `input_text` part 的数组封装。额外消息、额外 content part、assistant 角色和未知上下文字段均拒绝，避免有答案历史的请求被记成无提示实验。其检查证明的是调用者传入的请求材料匹配；请求真正发出的证据仍须由采集器的请求文件/正文摘要提供，评分器不会声称独立抓取了 wire。

所有控制结果均输出 `original_benchmark_eligible=false`、`original_benchmark_pass=false`。`rate_limit`、`network`、`incomplete`、`protocol_error` 不变成错答或正确。冲突来源、多答案、条件答案均为 `ambiguous`。只评分最终可见回答，不解密或猜测隐藏 reasoning。

## 独立穷举证明

新 verifier 不调用旧 `candy_strategy_verify.verify`。它从六种糖果的计数上限 `(7,9,8,7,6,4)` 独立枚举所有 `8×10×9×8×7×5 = 201,600` 个可行取出组成。成功条件为“圆苹果与星桃均非零，或圆桃与星苹果均非零”。结果：190,890 个成功组成，10,710 个失败组成。

固定形状策略共 `25×18=450` 种，每种策略按圆糖与星糖的数量匹配全部可能口味组成。最小保证数为 21，唯一最小固定分配为圆 9、星 12。所有不超过 20 个的固定形状分配都有保存的真实失败反例。

该上界可直接验证：星形取 12 个必有苹果（非苹果星形最多 10）且必有桃子（非桃星形最多 11）；圆形取 9 个必有苹果或桃子（圆西瓜只有 8）。任意非西瓜圆糖都能与星形中另一种口味配对。

固定策略下界没有自动排除自适应策略，因此新 verifier 还对全部 201,600 个状态作逆序动态规划。即使允许每次取出后观察已取糖果的口味，再决定下次选择哪个形状，仍有最坏情况下至少 21 次的下界。成功状态距离为零；其他状态取 `1 + min(可选形状中的 max(该形状所有剩余口味的下一状态距离))`。下一状态总比当前多一颗，逆序枚举保证已计算，没有抽样或截断。

完全不能选择形状时用 `1 + max(所有剩余糖果类别的下一状态距离)`，起始最坏距离为 29。穷举也独立得到最大的失败组成是 28，例如全部 24 个圆糖与 4 个星形西瓜，完全没有星形苹果或桃子。所有总数至少 29 的可行组成均成功。这同时给出负控制 29 的下界与上界。

## 本地验证

```powershell
python -m unittest discover -s tools -p test_quality_semantics_variants.py -v
```

22 项测试通过，覆盖原题逐字符保留、只改能力描述、数值与解法提示防混入、答案键隔离、全状态与自适应数学边界、全部固定失败证据、三种任务独立判分、标签与实际请求不匹配、单消息数组封装、附加上下文拒绝、冲突终态/最终文本、SSE 重建、仅最终回答评分和限流不评分。此结果为离线核验，不是任何账号已恢复的证据。

## Q4 实际对照：原题仍错，语义控制分别通过

父任务于 2026-09-24 12:08–12:11 UTC（北京时间 20:08–20:11）执行预登记的四次请求。此子任务重新读取已下载的 19 个文件，生成 [独立离线报告](../../.local/quality-20260924/semantics-live-q4-report.json)。四次均为 `gpt-5.6-sol`、`medium`、HTTP 200 和完整终态，原始材料位于 `.local/quality-20260924/semantics-live-q4/`。

| 顺序 | 题面 / 传输 | 实际最终答案 | 本任务答案键 | 独立判分 | 计为原题通过 |
| --- | --- | --- | --- | --- | --- |
| 1 | 原题 / native direct | 29 | 21 | wrong_answer | 否 |
| 2 | 原题 / transparent gateway | 29 | 21 | wrong_answer | 否 |
| 3 | 明确可选形状 / transparent gateway | 21 | 21 | correct，仅诊断 | 否 |
| 4 | 明确完全不能选形状 / transparent gateway | 29 | 29 | correct，仅诊断 | 否 |

两条原题的可见解释均先求不满足条件的最大集合 28，再加一得到 29；没有利用取出前可选形状降低保证数。澄清控制的可见最终解释主动选取圆 9、星 12，并给出上下界；负控制则恢复 28 加一的盲抽边界。这组结果支持“当前错答对形状选择能力的题意表述敏感”这一解释；原题两次仍未通过。它没有证明异常账号已恢复，没有证明网关导致或消除了账号标记，也没有识别服务端集群。

四次请求的前后 `identity_sha256`、`token_sha256`、`assigned_at`、`oauth_updated_at` 均一致；每次进程记录的 `source_before` 和结果行的 `credential_sha256` 也与初始快照匹配。每行客户端 attempts 为 1，三个网关请求的关联 gateway attempt count 均为 1。进程证据只包含预设环境项，没有继承 `SSL_CERT_FILE`、`SSL_CERT_DIR` 或代理环境变量；helper 与 probe 哈希在四次间一致。结束报告确认凭据快照已删除，本地证据目录也不存在该凭据文件。

输入证据从实际配置构造，并与 Go harness 序列化请求的 SHA 核对。离线汇总器再次以排序 key、紧凑 UTF-8 JSON、Go HTML/分隔符转义重新计算，四份均匹配 evidence 与结果行的请求哈希。原题两臂的请求 SHA 和 `predicted_upstream_body_sha256` 都等于 `763f3380b4807815c1dab11c2baf7586e4466e3bcc4a4e668c658f0be69df7f5`，因此报告标注该层面的正文可比较；这仍是本地序列化及上游正文预测证据，并非独立上游抓包，不能据此声称 TLS、请求头、时序与路由完全相同。

当前 raw 的 metadata collector 为 schema 1，可见 model/effort 与请求一致；`service_tier` 信号依次包含 `auto`、`default`。旧 collector 将两个不同值汇总为 `conflicts.service_tier=true`。报告保留该原始字段，但明确这与正常生命周期中选定执行等级相容，不能描述成服务端矛盾或降档。usage、reasoning token 数与耗时仅作观测字段，不用于推断实际算力、集群或能力恢复。未来 collector 的修正不会改写这四份 raw。

本轮仅一组固定顺序请求，澄清题与原题是不同题面，且没有随机化或跨账号复现。因此不能报告为稳定准确率改善。控制题通过不会抵消原题 0/2；离线报告分别列出 `original_task` 与 `diagnostic_controls`，两种控制始终 `original_benchmark_pass=false`。

离线重评，一条命令：

```powershell
python tools/quality_semantics_report.py .local/quality-20260924/semantics-live-q4 --output .local/quality-20260924/semantics-live-q4-report.json
```

`tools/quality_semantics_report.py` 除题面和答案外，还核验四次计划、唯一顺序、结果和 sidecar 文件集合、请求哈希、账号 epoch、model/effort、attempt、进程环境和二进制摘要。无法证明的记录标 `evidence_invalid`，不会静默进入质量分母。提前出现 403、429、503 或未完成响应时，支持仅有一条记录的停止报告；后续缺失控制明确列出。未报告额外请求、重复行、额外 case、继续执行于失败之后均使整体证据不合格。读取 JSON 拒绝重复 key 和非有限数，凭据文件只检查存在与否，不读内容。

汇总器 23 项专属测试通过：

```powershell
python -m unittest discover -s tools -p test_quality_semantics_report.py -v
```
