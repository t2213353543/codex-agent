# v20：medium / xhigh 对照及凭据变化后的停止

v20 独立核验通过：445 项检查、0 findings。预登记八格，实际发送七次 POST，其中六个完整答案、一次流内过载错误；第八格在发送前因 new2 的凭据 epoch 变化停止。八格镜像设计未完成，停止是既定身份门禁生效，不补发、不把新凭据结果拼入旧批次。

在本批已完成的 new1 样本中，medium 返回29、reasoning_tokens=796；两个 xhigh 也都返回29、reasoning_tokens=1552。提高 effort 后，这一题没有由错答变为21。xhigh 的公开返回 effort 与请求相符，并观察到更多 reasoning tokens；这并不等于测量到服务器分配的实际计算预算，也不能把两次1552当作新的硬上限。

| 格 | 账号 | effort | 状态 | 自动答案 | 独立人工注释 | input | cached | reasoning | output |
|---|---|---|---|---:|---|---:|---:|---:|---:|
| 01 | new2 | medium | completed | 21 | — | 6881 | 0 | 1237 | 1552 |
| 02 | new1 | xhigh | completed | 29 | — | 6881 | 0 | 1552 | 1791 |
| 03 | new1 | medium | 流内错误，无答案 | null | — | 未观察 | 未观察 | 未观察 | 未观察 |
| 04 | new2 | xhigh | completed | null | 21 | 6881 | 0 | 2895 | 3166 |
| 05 | new2 | xhigh | completed | 21 | — | 6881 | 6656 | 1936 | 2250 |
| 06 | new1 | medium | completed | 29 | — | 6881 | 0 | 796 | 997 |
| 07 | new1 | xhigh | completed | 29 | — | 6881 | 0 | 1552 | 1881 |
| 08 | new2 | medium | 凭据变化，未发送 | — | — | — | — | — | — |

自动分类保留为2个正确、3个错答、1个未评分完整答案、1个无答案错误、1个未发送。第四格正文首句、9圆12星方案和末尾方框明确为21，人工注释绑定 final SHA `bc41bfcc85d3076ec32f925c4aea19aaca5af9c32320acffd856a649a3bd193b` 与原 report SHA `917c1d65d4b9cb2fa744b4ed37ccfe57a7b7df248315cb40eb1df94bc21f901c`。原自动 `null` 不改，计入单列人工注释后，六个完整答案是3个21、3个29。评分器固定为 manifest 的 `0441c9c5…`，没有为本轮格式继续改评分代码。

本批只将现有 `reasoning.effort` 字符串值从 medium 改为 xhigh。medium 正文为36,330字节、SHA `e550de8ef54b27349eb1363ebb675b06e8fbe29ea1dcacbaf76c5ce07ece1d4a`；xhigh 为36,329字节、SHA `f3c3abbe6b160ac38cf74301240f97723891f3e88f51f3886cc3edef03735f76`。原值字节区间 `[35065,35073)`，替换后为 `[35065,35072)`；区间前后 SHA 一致，其余正文原始字节保留。原题、input、工具、指令、cache/session/安装字段和非认证头值没有同步改变。实际七次 native IPC 正文与各自计划 SHA 一致，使用独立 helper/IPC、zstd、冷 HTTP、无目录预热、无自动刷新或推理重试。

模型仍为 `gpt-5.6-sol`，原题 SHA `cd771ba38f8f75696ba703c7d3e91e5273588c91cb8c4145e3ce4b077c075c3f`，评分目标21未改变。medium/xhigh 两档由 v19 当前两账号实际目录声明支持，catalog 原报告 SHA、账号 epoch 和一次 GET 成功证据均被绑定。六个完整响应的公开返回 model 均为请求模型，reasoning effort 均匹配各自 medium/xhigh；其他返回控制保留记录，不能从这些公开字段推断私有路由。

第三格 HTTP 状态是200，其后出现 response.created、response.in_progress 和 terminal `error`，没有 completed 或最终文本。公开报告因白名单将 code/type 写为 other，但保留的 code SHA 精确等于官方源码已知字面量 `server_is_overloaded` 的 UTF-8 SHA；独立已知源码字典对照证明这一点。不能把它改写为 HTTP503或429，不能计为29错答。type 未恢复，61字节错误消息只有哈希、未重建；错误码也不确定是哪项容量、调度或内部状态造成。详情保留在 `review/known-error-hash-matches.json` 和 `known-error-interpretation.md`。

第八格还没有执行 usage-before 或模型请求，就检测到 frozen token epoch 不同。批后只读证据显示 new2 的 token SHA 从 `84ba24ba…` 变成 `889c9b1d…`，元数据 Version及 token iat 也变化，但稳定 user、workspace、record 和身份哈希未变；new1 的凭据哈希不变。这里能确定“同一稳定账号的新凭据已出现”，不能仅从该读取确定由哪个外部操作产生或把它当作错答原因。已经发送的七格都绑定本批旧 epoch，后续新凭据必须另起实验，不与本批混合。

这是一个被门禁提前结束的小规模镜像对照。new1 只留下一个完成的 medium 样本与两个完成的 xhigh 样本；new2 只留下一个 medium和两个xhigh。因此只能报告本批可观察差异，不给独立样本正确率、不称永久修复，也不以无改观判定增加 effort 在其他题目无效。

清理证据显示 unit MainPID=0、inactive/dead、退出码0，原始请求捕获和临时凭据全部清理，29份远端公开 artifact SHA及 execution SHA 与本地一致。七个已发送格期间，生产持久化日志中已完成重叠请求均为0；该证据不覆盖未完成或外部使用。

主要证据：`.local/research-20260925/effort-controls-v20/review/results-final.json`、`summary.json`、`order-statistics.json`、`human-annotations.json`，以及批次 `after-epoch-change.json`、`cleanup-verified.json`。独立审查没有联网、刷新、重发或调用模型。

只读复核命令：`python .local/research-20260925/effort-controls-v20/review/review_results.py`
