# 工具策略观察与终态用量合同

**0.4.14已于2026-09-29 00:17:55北京时间完成现场验收。** release `3b80aef4b4cefa029acc6d3c3ae02b599f8dd82589aef04493faaf45bc8c5c2a`，运行PID1562231、binary `68c5a18cbe7eecb555e45a946ce454200c403f49d1968ce1a0a93fac7c64722f`。两条生成、一次本地参数负例、三条精确CSV导出均通过，ToolPolicy的已知0/false与未Prepare省略均在现场核验；账号和设置保持。[上线核对](../.local/collab-a-20260928/tool-policy-release-review/postdeploy-review.json)与[包内关键证据](../.local/collab-a-20260928/tool-policy-release-review/critical-package-review.json)绑定本轮源码，未据此宣称所有自然错误已解决。

## `tool_policy` 字段

`RequestRecord.ToolPolicy` 是可选观察对象；成功Prepare后才创建。旧记录及Prepare前拒绝省略该字段，已观察的数量0、布尔false照常保留，不当作unknown。

该可选字段不改变BPS现有`observation_schema_version=2`或数据库迁移版本。compact路径也可记录Prepare得到的选择策略，但不执行普通生成的整响应工具校验；仅有`effective_choice=required`不代表已经发出工具调用。

| 字段 | 合同 |
| --- | --- |
| `effective_choice` | 已验证策略的 `auto` / `none` / `required` / `forced`，不从错误文本推断。 |
| `catalog_at_prepare_count`、`named_namespaces_at_prepare_count` | Prepare时有效目录及非空namespace数量，包含已纳入的历史/发现声明；空namespace是合法全局声明，不计入“具名”数量。 |
| `tool_search_at_prepare`、`structured` | 本请求是否声明工具发现、是否使用结构化输出。 |
| `catalog_at_rejection_count`、`named_namespaces_at_rejection_count` | 被拒绝操作之前的目录快照；未拒绝时省略。 |
| `rejection_phase`、`rejected_family`、`rejected_identity`、`rejected_matches_exact_declaration` | 第一处实际拒绝的位置、有限类别和精确声明匹配结果；无法确定具体item时不编造身份。精确声明不等于策略允许执行。 |

拒绝位置限定为 `normalize_item`、`parallel_policy`、`validate_response`。身份只记录 `exact`、`undeclared`、`ambiguous_namespace`、`unique_namespace`、`internal_formatter`、`not_client_action`、`unobserved` 等类别；**持久化对象不保存工具名、namespace原文、参数、schema、提示词、密文或完整错误正文**。

逐项检查先临时借用原始type/name/namespace三个标量，失败时立即求值，保留normalizer补全namespace之前的身份。例如原请求缺namespace，补全后因forced策略拒绝，仍记原 `unique_namespace / exact=false`。整个响应验证可能先改变发现目录，仍在调用前取快照；第一拒绝不被后续清理覆盖。

## 来源与存储

每次Prepare产生独立对象。实际fixture确认：第一账号429、后续OAuth失败而恢复`priorRecord`时，元数据随最终采用的第一条生成记录；换号成功则随第二条记录。下一请求不继承旧拒绝，Prepare前取消保持省略。[A来源验证](../.local/collab-a-20260928/tool-policy-failover-review/REPORT.md)

typed JSON、脱敏JSON及本地日志关闭重开已验证保留0/false/省略。PG沿用整条记录JSONB存储，专用`TestPGBPSToolPolicyObservationRoundTrip`已在本轮14项隔离PG门禁实际通过，并封入发布证据。**CSV继续 `codex-usage-v4` / 77列，不追加ToolPolicy列**，不把JSON与CSV字段集合差异误报为丢失。[B存储边界](../.local/cross-session-20260928/01a0e7ed/tool-policy-record-review/REPORT.md)

## 精确身份提示与样本边界

普通及structured约束追加完整、稳定排序的客户端声明身份JSON；内部格式函数单列处理。提示不扩大真实允许集，`tool_choice`、normalizer及验证规则仍决定是否接受，提示本身不是执行权限。

有效轮次 `155456.709891Z` 的6对合成请求均为同模型/effort/owner：基线4次协议通过、2次未声明工具拒绝；精确清单6次通过、0次拒绝。差异发生在skills/connectors场景。**工具实际执行为0，协议通过不代表任务完成。** 各臂session/turn/cache身份不同，比较时剥除了metadata/cachekey；不能称完整wire只差提示、缓存条件相同或生产长期错误率已下降。前两次声明不合法的400单独保留。[有效样本审计](../.local/collab-a-20260928/tool-policy-probe-review/VALID-PAIRS.md)

## 非法终态输出仍保留真实用量

当前已接入 `OnTerminalMetadata`：收到真实终态但工具输出验证失败时，在身份及终态信封有效的前提下，仅提取实际ID/model/usage供计量，不保存或放行非法output、不写成功历史、不伪造completed。身份冲突不回调；早期item已拒绝而尚未收到终态时，不继续读流来寻找用量。

缺失/非法计数保持未知和既有别名优先级，已知零仍是零。失败仍计失败；真实usage可进入对应统计，cache creation不加入读取分子。独立HTTP交付测试确认合法delta公开后仍保留原abort、单次派发及真实用量，不保存成功历史；早期拒绝后不主动读取下一段终态。该边界与race/PG均按本轮封存源码核验。[交付边界](../.local/collab-a-20260928/terminal-usage-delivery-review/REPORT.md)与[B计量语义](../.local/cross-session-20260928/01a0e7ed/terminal-usage-accounting-review/REPORT.md)

## 性能范围

新增成功逐项观察只捕获三个标量，额外工作为O(1)；失败求值、Prepare目录计数、整响应预快照仍会扫描目录，精确清单还需排序/编码。B同轮微基准以**每op 16次回调**计，1600工具正常路径baseline约713ns、原逐项扫描约712116ns、延迟求值约1036ns，后者与baseline均224B/24alloc；未包含Prepare、整响应或网络。不能换算为端到端延迟/吞吐承诺。[同条件性能证据](../.local/cross-session-20260928/01a0e7ed/tool-policy-perf-review/lazy-comparison/REPORT.md)
