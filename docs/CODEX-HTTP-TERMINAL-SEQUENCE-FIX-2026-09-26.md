# HTTP 响应终态顺序与固定 CLI 对齐

已部署 Go `54615b6e859cd674680747b24a6dd77e19631bed79ba137f18b9bf29570901d2`。北京时间2026-09-26 02:43:04线上验收通过：readiness正常，models返回200和9个模型，WS握手101且正常关闭、in-flight归零；没有发送模型turn。

## 固定官方客户端的实际行为

未修改的官方 CLI 0.155.0（SHA `660e159a…`）在仅有loopback的网络命名空间中执行19个合成序列，每个只发送一次本地POST，全部符合预期：

- HTTP收到 `response.failed` 或 `response.incomplete` 后保存错误并继续读；后续合法 `response.completed` 可以成功结束。
- 多个已解析错误之后EOF，返回最后一个错误。底层读错、UTF-8解码错误或idle超时仍立即结束。
- 错误结构的completed可以被后续合法completed替代。普通 `type:error`、`response.done` 在固定CLI中没有完成语义。
- `response.completed` 的完成判断不依赖嵌套 `response.status`；其typed结构合法时，即使该可选字段写failed，也按完成事件结束。
- WS循环不同：收到对应错误立即返回。本修复不能通过修改共享函数改变WS错误顺序。

这些结论有实际CLI进程结果和本地服务接收计数，不是仅由测试名或注释推断。没有使用真实账号、模型或原糖果题。

## 候选修复

网关旧HTTP路径在首个failed/incomplete即截断，导致后面的正常完成事件永远到不了客户端。候选让HTTP保留有界的待定错误，继续转发原始SSE，等待合法完成、EOF或原有截止条件。成功完成后只记录该完成的用量和状态，不留下这条已恢复响应的临时429冷却，也不清除别的请求产生的冷却或账号已有永久OAuth错误。

待定事件仍建立精确的响应身份，防止随后外来ID抢占本请求记录；坏完成事件、顶层/嵌套冲突和未知大小写字段均独立验证。显式错误类型不会被当作成功终态。EOF只有失败时保留相应诊断；客户端取消仍是取消，不把尚未完成的流误记成功。

实现局限于HTTP的读循环、聚合和计量挂点。WS与共享即时终态函数保持原字节，独立测试覆盖FIFO、迟到事件、账户历史错误和永久refresh拒绝没有被清除。

## 仍保留的接口差异

这不是整个网关与CLI的完全等价证明。兼容上游省略completed ID或usage中的某些字段仍可接受，不新增严格serde缺字段拒绝；但显式错误类型会延后并在EOF时报错。`response.done` 不再提前截断HTTP，非流式EOF时保留既有兼容对象。顶层error仍可用于网关EOF诊断，但不会覆盖最后一个CLI可识别typed error。非流式incomplete可以返回partial JSON并记错误；SSE透传字节保持不变。

这些兼容扩展、JSON聚合接口和资源限额与CLI不同，应继续在后续一致性检查中单独记录，不能因本轮测试通过就宣称完全对齐。

## 验证

候选含5个生产文件与7个测试文件变更，基于线上 `e069de52…` 的完整冻结源码。真实TCP分块测试覆盖暂时失败后完成、EOF最后错误、取消/超时、响应身份、无遗留冷却、错误字段类型及大小写干扰。既有首terminal测试按新HTTP合同更新，并保留原失败日志。

Windows完整2779项通过、342项明确跳过，vet通过；独立7个顶层/23个叶子检查通过，官方CLI参照19项通过。Linux race、隔离PG/Redis与当前native helper完整3229项通过、26项跳过、0失败，新增46个真实native序列用例均执行通过；Linux vet通过。测试跳过项未计为通过。

发布仅切换后端。helper `bf920a0b…`、固定CLI身份、原环境变量、EnvironmentFile列表与哈希、前端、设置及账号元数据均核对一致。没有数据库迁移。临时smoke key/group及key文件已清理，账号原有组保持。实际 `/proc/2872551/exe` 的哈希与构建产物一致；5个生产及7个测试文件已在校验旧哈希后同步共享工作区，未覆盖其它修改。

[固定CLI证据](../.local/research-20260926/terminal-sequence/cli-reference/summary.json) · [候选清单](../.local/research-20260926/terminal-sequence/candidate-manifest.json) · [HTTP/WS边界审核](../.local/research-20260926/terminal-sequence/review/independent-review.json)

[完整构建与测试](../.local/research-20260926/terminal-sequence/build/build-manifest.json) · [上线回执](../.local/research-20260926/terminal-sequence/release/activation-result.json) · [HTTP/WS验收](../.local/research-20260926/terminal-sequence/release/runtime-smoke.json) · [临时资源清理](../.local/research-20260926/terminal-sequence/release/smoke-cleanup.json)

本修复解决响应处理顺序的明确不一致，未证明此前账号错答是由这些事件序列造成。
