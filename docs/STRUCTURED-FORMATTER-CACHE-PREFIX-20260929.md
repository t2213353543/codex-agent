# 结构化格式声明与缓存前缀

核对时间：2026-09-29 03:35（UTC+8）。本页 formatter-tail 修复已随 **0.4.19 / PID 2338498 / binary 46119b03…a8521c4** 上线，release `4f84927039696ea2…`、source `77fed72b…`。03:34:07 的正式部署验收已通过，首页与 Logs 实际资源也匹配发布包，见[部署核验](../.local/collab-a-20260928/prefix-release-review/postdeploy-review.json)。发布 `request.go` 相对受测私有候选只增加两行解释注释，运行改动仍是一处声明位置移动；生产缓存域仍为 `api_key`。

网关将 `text.format` 转为内部格式化工具。原实现把它的 `additional_tools` 声明放在全部输入之前；切换 schema 时，该声明的内容或 ID 变化，长历史之前的共同前缀随之改变。候选只移动网关生成的内部声明，将其放到原有全部输入之后、最终格式/工具约束之前。客户端 instructions、工具声明、历史项目的内容与相对顺序保留；没有裁切历史、删除工具、放宽 schema 或修改缓存 key。

实际 `Prepare` 字节对照中，schema A→B 的输入共同前缀由 11 增至 98,720 字节，总输入长度仍为 100,411 字节。这是字节比较，不是 token 计量或缓存命中证明。客户端自行改变工具 schema 的序列化顺序，以及发现结果导致重复声明去重的既有行为，不属于本次修复。见[作者前缀对照](../.local/cache-prefix-continuity-20260929/README.md)。

主任务唯一执行的四请求实验 `20260928T190647.512560Z` 已完成。原上游和网关记录的用量一致，四次均 HTTP 200、输出通过各自 schema 校验：

| 声明位置 / schema | input tokens | cache read | cache creation |
|---|---:|---:|---:|
| 原位置 / A | 82,565 | 0 | 82,187 |
| 原位置 / B | 82,571 | 0 | 82,190 |
| 历史后 / A | 82,565 | 0 | 82,047 |
| 历史后 / B | 82,571 | 82,047 | 0 |

最后一条的读取比例为 99.3654%。试验采用 `client-cache-key` scope，生产保持 `api_key`；同一位置的 A/B 使用相同 cache key，两个位置分别使用独立 cache key、带不同位置标记的 instructions，且每个 schema 的 session 不同。传输拦截器本身只移动精确内部声明对象，但四个请求不能概括为 wire 只有位置一个变量。每种位置仅一次 schema 切换、顺序固定，结果支持本次受控场景；不能据此承诺自然流量缓存比例或长期首字耗时改善。见[原始实测日志](../.local/cache-prefix-continuity-20260929/20260928T190647.512560Z/live.log)、[执行回执](../.local/cache-prefix-continuity-20260929/20260928T190647.512560Z/report.json)及[C 独立来源审查](../.local/coordination-20260928/team-c/phase34/formatter-position-review/REPORT.md)。

续接方面，网关保存原始 `historyBody` 和经过结构化投影的公开输出，不保存带内部声明的 `prepared.Body`。`previous_response_id` 展开旧历史后，本轮 `text` 覆盖旧配置，再由 `Prepare` 生成本轮声明；因此不需要把旧 formatter 带入新请求。客户端工具目录、策略及名称碰撞检查仍先于新增内部 formatter。

A 已独立补齐真实网关两轮 A→B 续接：同一 fixture 在原位置、候选位置的 JSON/SSE 均 PASS。第二轮上游恰有一个当前 B formatter，旧 A 内部声明不再出现；客户多工具的顺序和定义保持，保存的 `text` 与公开输出均为 B，历史中原有 A 回答保留，存储及公开内容不含内部 formatter。每例仅两个本机 HTTP 请求，无外部模型。见[续接补证](../.local/collab-a-20260928/done-authority-history-review/formatter-continuation/REPORT.md)和[实际合入指纹](../.local/collab-a-20260928/done-authority-history-review/formatter-tail-adoption.json)。本补证验证语义保持，不替代真实缓存计量。

修订冻结后的正式第十组 `TestBPSLiveStructuredFormatCachePrefix` 已直接使用候选 `Prepare` 出站，没有传输层重新排列。原 run `20260928T192204.079273Z` 实际 exit0：A 的 input 82,560/read 0/create 82,042；切换 B 后 input 82,566/read 82,042/create 0，读取比例 **99.3653562%**。两次 schema、原客户端项保留和上游→响应→记录计量均通过，第二条满足原 ≥70% 断言。该测试仍使用 `client-cache-key` scope，不能据此报告生产 `api_key` 的自然命中率。原始结果见[正式 live 日志](../.local/reliability-modes/live/20260928T192204.079273Z/live.log)，不与首轮旧源或四请求位置探针对调。

完整 done 对象的来源选择是另一项设计评估，目前没有产品候选，未随本缓存调整放宽 reasoning ID/cipher 校验。
