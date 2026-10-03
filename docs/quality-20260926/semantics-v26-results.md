# v26 语义控制结果：new1 原题29，澄清后23，原题未恢复

预登记12格镜像实际执行7次模型POST，前6次完整回答；第7次HTTP200流内错误后依约停止，剩余5格未发、没有补样。独立离线核验 **221项通过**，回执为有效停止前缀，`controlled_twelve_completed=false`。原题验收仍为21；澄清题21、完全盲抽29仅是各自诊断任务答案。

| 格 | 账号 | 题面 | 自动答案 | 人工附注 | reasoning tokens |
|---|---|---|---:|---:|---:|
| 01 | new2 | 原题 | 21 | — | 3505 |
| 02 | new1 | 明确可选形状 | 23 | — | 1552 |
| 03 | new2 | 完全盲抽 | 29 | — | 256 |
| 04 | new1 | 原题 | null | 29 | 389 |
| 05 | new2 | 明确可选形状 | 21 | — | 2710 |
| 06 | new1 | 完全盲抽 | 29 | — | 516 |
| 07 | new1 | 完全盲抽 | 无答案，流内错误 | 不评分 | 未返回 |

04原文末句明确“最少取出29个”，正文也是28＋1。人工附注绑定final UTF-8 SHA `82ff357891f8f6d75ac2271f1ba0d1d76896c9a49464de1431ea748b69c64b38`，原自动null、报告与文本均保留。07的code/type摘要分别精确匹配已有协议字符串 `server_is_overloaded` / `service_unavailable_error`；没有恢复其未知message，也不由该错误推断算力或内部路由。

**23是有效方案，但不是最小值。** 02已识别可选形状，并正确列出四个失败矩形，选择18圆＋5星保证成功；错误发生在“所有22颗分配都失败”的下界断言。独立枚举201,600种组成、450个形状分配，确认唯一最优为9圆＋12星＝21；9圆＋13星及10圆＋12星两种22颗方案也能保证成功。原文SHA `4de0f8c9a7d6741481c9aa491ea8c80d01cb4b8bb851092cc1da16cdec19960e` 与数学报告绑定，不能把这次错误描述为“仍完全没理解可选形状”或“23颗方案不保证”。

本批在一份实际CLI首轮捕获上仅替换 `/input/6/content/0/text` 的题目字符串。原题body SHA `5f90a8c18eab09e0fc9585187b26fb10c478d741238d6c05988faafb620f675c`；投影工具冻结 `f46398b4…`，三个变体的其余字节、11个顶层字段中的非input部分、13个非认证头、开发者上下文和控制字段均保持。逐格实际IPC、当前token/user/workspace、helper bf920、直接出口与清理记录一致。请求使用medium，六个完整响应也回显medium；没有从reasoning用量推断隐藏预算。

原题只有new2一次21、new1一次29。澄清题中两账号分别21与23，盲抽各有一次29；这支持对new1错误步骤的进一步定位，没有证明原题恢复或稳定账号正确率。与旧q4“原题29/澄清21/盲抽29”相比，本轮new1澄清后仍未达到该任务最小值；旧q4不是当前账号或当前AT，不能合并成随机样本率。

最终16次usage GET＝全组2次＋7格前后14次；初始库存2次usage另计。48份远端公开文件SHA全部匹配，3个worker均MainPID0/inactive/exit0、4份原始捕获删除、helper回收、运行时和身份代次保持。7个实验窗口的已持久化生产网关请求重叠均为0；这不覆盖外部或未完成流量。证据位于 `.local/research-20260926/semantics-controls-v26/` 的 `review/results-final.json`、`review/math-review-02.json`、`review/manual-answer-04.json`、`cleanup-verified.json`。复核：`python .local/research-20260926/semantics-controls-v26/review_results.py --final`。
