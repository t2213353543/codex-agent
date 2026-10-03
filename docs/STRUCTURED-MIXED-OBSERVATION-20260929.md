# 结构化混合输出观察合同

**0.4.15已于2026-09-29 01:19:12北京时间完成现场验收。** release `5d75f656986291a8648372032fa5c58f629719e95663a599fc79918f6bb3cce7`，PID1801914、binary `aacfb7aac3fc48522fee2648fb2bdcb0ae6a97c21bce532974b61986d204440d`。本次标记验收确认正常请求与本地参数拒绝不误生成mixed字段；真正mixed分类由同源码本地、race及PG回归覆盖，未故意制造生产混合错误。[上线回执核对](../.local/collab-a-20260928/compat-release-review/postdeploy-review.json)

线上0.4.14的5条混合错误只有聚合文案，无法恢复各条具体组合；新增字段只记录以后实际发生的拒绝。

`structured_mixed`是请求记录的可选对象。只有结构化投影真正命中原mixed拒绝条件，且网关进入既有结构化验证失败路径时才记录；相同文字的普通错误不能生成该观察。元数据不改变公开错误文字、响应、工具允许集或两阶段调用规则。

| 字段 | 含义 |
| --- | --- |
| `formatter_count` | 当前最终输出中精确匹配本请求内部格式化工具的数量。 |
| `client_call_count` | 按既有投影规则计数的客户调用，包含function/custom、local_shell/shell和client execution的tool_search。 |
| `other_call_count` | 既有全部公开调用数量减客户调用数量。它不表示这些调用已经执行，也不等于全部由服务器执行。 |
| `has_text` | 最终输出含非空assistant output_text；包括现有判据收集的commentary文本。 |
| `has_refusal` | 最终输出中出现真实refusal形状。 |

三个计数和两个布尔均为已观察值，0和false明确保留。未发生mixed、普通schema失败、Prepare前拒绝及旧记录省略整个字段，不能补成全零。对象不保存工具名、namespace、参数、文本、schema、密文或错误正文。

拒绝条件保持原式：多个formatter，或一个formatter同时伴随公开调用、非空文本或refusal。这里记录已有统计值，没有再遍历目录。`ObserveStructuredMixed`通过typed错误获取独立副本，不从自由错误文本猜测，也不共享可变观察指针。

JSON与JSONL沿请求记录携带该对象；CSV继续v4/77列，不新增mixed列，数据库继续使用JSONB，无需额外DDL。`TestPGBPSRequestCompatibilityObservationRoundTrip`已在本轮15项隔离PG验收实际通过，包含真实双formatter错误生成、与ToolPolicy共存、0/false和旧字段缺失等场景；该实际结果与本地Go选择后SKIP分开记录。[包内关键证据](../.local/collab-a-20260928/compat-release-review/final-source-package-review.json)

本组已完成[5顶层16子例核心验证](../.local/collab-a-20260928/structured-mixed-candidate/README.md)及[18个JSON/SSE独立网关对照](../.local/collab-a-20260928/structured-mixed-independent/REPORT.md)：5类mixed各在两种传输准确记录字段，4类非mixed控制均不产生字段；新增字段之外，公开wire逐字、错误、用量、历史保存与派发次数保持一致。随后当前集成工作集的11项相邻本地回归也已通过；这些局部证据不替代最终冻结版本的全量验收。

监控应保留原错误分类，并仅读取这五个已验证数值/布尔字段。旧记录缺失仍是未知；不能用该字段反推客户请求内容，也不能把分类更详细解释为错误已经减少。
