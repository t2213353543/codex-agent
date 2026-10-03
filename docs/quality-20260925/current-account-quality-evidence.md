# 当前 new1/new2 的质量证据：题义、账号代次与可控下一步

离线复核 v7、v8、v11、v12、v15、v18 中当前两个账号的 36 格记录，身份哈希和已有人工注释均能绑定原报告，未发现归档冲突。new2 共 22 个完整答案，均为 21；new1 共 13 个完整答案，均为 29，另有一格 terminal error 无答案，单独保留。这些是跨批次描述性计数，涉及不同捕获、配置、缓存键、运输和时间，不是 36 个独立同分布试验，也不应转换成账号的真实正确率。

| 批次与边界 | new2 | new1 |
|---|---|---|
| v7：原生 HTTP / 隔离网关，各两次 | 4×21 | 未纳入本汇总 |
| v8：其他账号刷新前后，new2 作为未刷新对照 | 4×21 | 无本账号 |
| v11：固定/新 cache key，各两次 | 4×21 | 3×29＋1 无答案错误 |
| v12：当前网关源码 / 原生直连，各两次 | 4×21 | 4×29 |
| v15：派生完整 HTTP / 真实 WSS 预热链，各两次 | 4×21 | 4×29 |
| v18：实际官方 CLI，new2/new1/new1/new2 | 2×21 | 2×29 |

这里的 21/29 包含已有、绑定 final 哈希的人工注释；原始自动 `null` 全部保留，未用工作区新评分器改写历史。v7 只取 new2、v8 也只取未刷新的 new2，没有把当时的 g7 结果归到 new1。

## 原题与数学合同

原题明确形状“靠手感可以分辨”，要求在活动前决定总取数，成功条件是圆形苹果＋星形桃子 **或** 圆形桃子＋星形苹果。原题没有额外明文写出“摸到形状后可以选择保留”，因此应如实说明解释前提：当前固定 benchmark 把可分辨形状理解为可按形状选择，口味不能选择；不能临时增加“必须不择形状随机摸取”条件。题目原文及 SHA `cd771ba38f8f75696ba703c7d3e91e5273588c91cb8c4145e3ce4b077c075c3f` 未更改，评分目标继续是 21。

在这一既定合同下，取 9 圆＋12 星保证成功：圆形西瓜只有8颗，因此9圆含苹果或桃子；星形非苹果最多10颗、非桃子最多11颗，因此12星同时含两种口味，能和圆形的对应口味配对。已有 `tools/candy_strategy_verify.py` 本次离线重新验证 201,600 个可行组成和 450 个固定形状分配：固定形状策略最优21、唯一21颗分配为9圆12星，所有总数不超过20的固定分配都有失败实例。工具还独立算出若完全禁止选择形状，最大失败集合为28，盲取保证阈值为29。该验证的策略类是预定形状数量，不宣称证明另一个未定义的动作规则。

多数29回答的主要问题在量词：它们对所有不控制形状的集合求“最多失败28，然后＋1”，没有先选择可执行的形状策略。这个28颗集合可由24圆＋4星组成，但不是9圆12星策略会遇到的集合，所以不能用它证明21颗形状策略失败。最大盲取失败集合计算本身通常成立，使用它替代本题的策略优化才是缺失步骤。代表：v11/04-fixed-new1，final `0336ec53…`（完整哈希见机器证据）；v18/02-new1 延续同类论证。

另外一格有更直接的成功条件错误：v12/02-g-new1 把“圆形桃子9＋五角星苹果7＋西瓜12”当成失败样例，但前两类本身就是题目明确允许的配对。这不只是对形状选择的不同解释。机器证据将它单列为 `invalid_failure_witness_already_satisfies_success`（final `b3bdd1b9…`）。其余两格另标文字细节问题，例如把需要同时成立的两项避配条件写成“之一”；没有把所有29都归为完全相同错误。

new2 的22个完整答案均明确使用9圆12星，并给出取星形保证两口味、取圆形避开全西瓜的上界策略。这里核对的是可见最终论证，而非隐藏推理过程，也不是对每段下界证明完整性的自动认证。代表可使用 v15/05-w-new2，final `2fe14878…`。

## 凭据与返回字段

纳入的 new2 始终是同一 access-token SHA `84ba24ba…`，稳定 user/workspace/record SHA 也不变；new1 从 v11 起始终为 `0e28971d…`，对应稳定身份不变。账号元数据 Version 的变化（new2 2→3→8，new1 8→10）单独记录，不当作 token 已变化。不同账号的正常 user/workspace 值不同，不能据此推断错答原因。

31 个完整的原生/网关后端响应（new2 20个、new1 11个）均公开返回 `model=gpt-5.6-sol`，reasoning controls 均为 `effort=medium`、`context=all_turns`、`mode=standard`、`summary=null`，`max_output_tokens` 字段存在且值为 null。它们支持请求与返回控制字段一致，但服务器公开 model 字段不证明内部执行路由或容量策略。v11 无答案错误没有这些返回控制字段，不补作 medium 成功记录。

v18 四格官方 CLI 的 `model_returned` 与 `transport_observed` 为 null，返回 reasoning controls 没有直接观测；CLI仅报告请求模型/强度、目录观察和累计 Usage。其 reasoning_output_tokens 来自官方 thread/turn 用量，不与单条后端 response usage 混作相同来源。

| 账号 | 后端 response reasoning_tokens | 官方 CLI reasoning_output_tokens |
|---|---|---|
| new2 | 20次，范围1034–3624 | 1552、1865 |
| new1 | 11次，范围371–900 | 516、516 |

“new1 总在516 token停住”与现有记录不符：同一 token 代次还出现430、887、900、763、371、481、510、731。516是多个观察中的重复值，不构成硬上限证据。较短的输出和不同策略选择在这些样本中同时出现，但没有仅改变预算后得到的因果对照，不能先下“推理被封顶”的结论。

缓存也不能单独解释：既有同批记录包含 cached=0 的21和29，cached>0的21和29；如 v15/05-w-new2 在真实 WSS 中 cached=0仍答21，v15/07-w-new1 cached=5888仍答29。跨批输入 token 数及正文 capture SHA 有变化，机器记录保持每批差异而不合并为一个固定请求。

## 下一有限对照

下一步可在当前两账号的真实目录都明确支持 medium 和 xhigh 后，仅改变 `reasoning.effort` 的现有字符串值。早期八账号目录曾列出 xhigh，但这不是当前 new1/new2 的直接授权能力证据；应使用 v19 当前目录结果做门禁。

v19 当前目录证据现已到位：2026-09-25 13:09:03/04 UTC，两账号各一次 native GET models 均为200，公开目录 body SHA 相同，目标模型均声明 low、medium、high、xhigh、max、ultra。`effort-support.json` 绑定 catalog-comparison 与 capabilities 原始文件 SHA 及当前身份哈希，v20 的 catalog-authorization 是该安全证据副本。独立审查只读取这些文件，没有补发目录请求；能力声明只说明可以提交这些档位，不保证具体算力或答案变化。

建议八格固定顺序：new2-medium、new1-xhigh、new1-medium、new2-xhigh，然后完全反向镜像。每个账号每个档位两次。使用同一批完整 HTTP 原文、相同题目/工具/指令/session/cache/application headers/helper、同一账号凭据代次；只替换 effort 值区间，保存未修改字节段证据。仍做整组 usage/epoch 和逐格前后门禁，不刷新、不重试失败格，不修改设备或指纹值。观察返回 effort 是否按请求回显、完成状态、正确答案和独立人工注释、reasoning/output 用量；不把 xhigh 增加 token 而答案不变预先解释成内部限制。

本轮离线产物是 `.local/research-20260925/account-capabilities-v19/review/account-quality-observations.json`、`mathematical-contract.json`，每格保存原 report/final 哈希、epoch、请求/返回字段存在性及用量来源。没有读取原凭据、模型调用或网络请求。

复核命令：`python .local/research-20260925/account-capabilities-v19/review/build_observations.py`
