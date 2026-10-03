# 已保存 turn-state 证据：HTTP 状态与字符串长度必须分开

本次仅离线读取 v25、v26、v15 的公开报告和对应冻结采集器源码；逐份报告与历史 cleanup 清单的 SHA 校验通过，没有新发模型、usage、trace、OAuth 请求，没有注入或替换 state，也没有改网关。

| 既有实验 | 实际模型报告 | HTTP/升级状态 | `x-codex-turn-state` 响应头存在证据 | 长度能否判断 |
|---|---:|---|---|---|
| v25 models-v3 | 8 个 HTTP | 全部 200 | 8/8 哈希列表含该头名 | 不能，未保存原值字节数或字符数 |
| v26 models | 7 个 HTTP（原计划12，中途停止） | 全部 200 | 7/7 哈希列表含该头名 | 不能 |
| v15 HTTP 分支 | 4 个 HTTP | 全部 200 | 4/4 哈希列表含该头名 | 不能 |
| v15 WSS 分支 | 4 个升级及其预热→推理链 | 全部 101 | 101握手头列表没有该头；专用状态观察均为 false | 没有已观察 state，不能赋予长度值 |

19 份 HTTP 推理报告中不存在 HTTP 292 或 312。v26 最后一格虽然 HTTP200，但 SSE 终态失败，故不能把HTTP状态与模型/协议成功混为一谈。本次同样不能把外部文章中的字符串长度数字当成状态码去解释这些数据。

## HTTP 头存在，但长度没有被保存

冻结采集器 `quality_cookie_lifecycle_probe.safe_response_headers` 调用 `quality_exact_websocket_probe._pairs`。后者对收到的每个头只保存 `name_sha256` 与 `value_sha256`。本次计算 `SHA256(lowercase("x-codex-turn-state"))` 为 `32d2463cbff63dc265904a36bcfdafdfc999f6914640e0af9bcc15d663361d71`，19 个HTTP报告各有且仅有一项匹配；顶层和inference内副本一致。

这足以证明对应HTTP响应头被观察到，不能求出其292/312/332等字符长度。64位十六进制value_sha256本身的长度只是摘要表示长度，绝不是原state长度。报告里没有state_fields/current_turn_state也不是“状态不存在”的证据；HTTP采集器未保留对应原始SSE metadata状态内容，不能从缺字段反推出backend不存在该内容。

## v15 的 observed=false 有明确但有限的范围

固定 `quality_websocket_chain_probe.py` SHA为 `a41deb83fc7b9fead2898e176f40d610f383157aa0057fb353449507c153ce46`，与v15 model-tooling-manifest相同。`_exchange`在101后检查握手响应头，`_read_stage`再将每个文本事件交给`_TurnState.event`；后者仅接受`response.metadata.headers`中的同名头，首次值包括空字符串锁定。

四份WSS报告都保存了 `warmup.turn_state_at_send`、`inference.turn_state_at_send`、最终 `turn_state`，三处均 observed=false/source=null/sha256=null/bytes=0。两个阶段receipt_complete均为true，读取停止原因均为 inference_terminal_and_write_ack_observed；两阶段的response.metadata事件计数均为0。这支持“在该次101握手及截至推理终态+ACK的所检查metadata来源中未观察到state”，不是泛指账号/HTTP/后续会话没有state。

这里 bytes=0是未观察到值时的占位输出，不是观察到了零长度字符串。若真正观察到空字符串，源码会报告 observed=true 且有空串SHA。即使将来有observed=true，当前字段测量的是UTF-8字节数，不天然等于Unicode字符数。

## 对公开说法的使用边界

根任务报告的原作者说明把292/312描述为turn-state字符串字符数，并区分personal/team长度；本离线核对未重新抓取网页，也没有验证其启发式，更未将其视为官方质量指标。既有证据只能确认上述状态码、头存在性和有限观察窗口，无法用来确认“292/312意味着降智”的说法，也不能据state哈希/长度证明账号风控、隐藏算力或推理集群。

机器证据：`.local/research-20260926/public-status-v26-context/state-length-evidence-review.json`。复跑：`python .local/research-20260926/public-status-v26-context/review_state_lengths.py`。它只读本地已保存报告，核模型格数、HTTP状态、头名摘要、显式state字段、清理归档SHA及采集器冻结SHA。
