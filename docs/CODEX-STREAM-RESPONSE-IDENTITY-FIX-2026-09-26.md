# 流式响应身份与计量修复

已部署 Go `e069de52e3da65dbce186780273af177cf157cf82ab7ab1668faba1403576a52`。线上readiness正常，HTTP models返回200、WS握手101且关闭后in-flight归零，临时测试key/group已清理。本修复针对网关自身的用量及请求状态归属，不把它当作模型错答根因。

原网关将用于展示的响应ID同时用于身份比较。展示会截断到1024字节、去除首尾空白并脱敏，因此两个不同原始ID可能变成同一展示值。真实HTTP/SSE回归复现：先创建A，再收到原始ID不同但展示值相同的B完成事件，旧网关把B的13/7 tokens记给A，并标记成功。

另有同类路径：事件顶层 `response_id` 与嵌套 `response.id` 矛盾时，前层观察器虽拒绝，后面的SSE终态和JSON collector丢弃外壳后又接受了嵌套对象。JSON模式因此可能错误返回200，SSE模式可能记错用量。

候选改为在任何截断/脱敏之前，用原始ID的SHA256维护本次请求的精确身份；摘要是未导出的临时字段，不进入日志JSON、管理API或数据库schema。终态 envelope 复用已有原始ID一致性规则，包括顶层有ID而嵌套ID为空的情况。原始SSE仍逐字传递并维持既有首terminal关闭行为；外来完成不获得本请求用量归属，JSON矛盾返回既有502错误。

该变更没有把CLI的终态策略重新定义成“忽略任意外来completed继续等”。固定CLI本身遇有效Completed即结束，先前已记录的 failed/incomplete 处理差异也没有在本次改动中隐藏。展示ID仍有界且脱敏，缺失ID的历史兼容保持；持久化旧记录不能恢复其已丢失的原始ID，报告不作此承诺。

本版基于此前 Go `5687aa1c…` 对应的385文件清单，仅改 `stats.go`、`gateway_observation.go`、`response_output.go` 并新增一个测试。真实TCP覆盖10种身份输入×SSE/JSON，含长ID、空白、脱敏、顶层/嵌套矛盾及匹配控制。最终Windows完整2727项通过、296项跳过、0失败；Linux race、隔离PG/Redis及当前helper完整3131项通过、26项跳过、0失败，新增20个真实native用例均已执行。两平台vet通过；跳过项不计为通过。

发布仅切换后端，helper仍为 `bf920a0b…`，固定CLI身份、其它环境变量、原EnvironmentFile列表/哈希、设置、前端与账号元数据核验一致。没有数据库迁移、模型turn或授权操作。四份部署文件的7项主流程及23项临时scope测试通过。发布后 `/proc/2724135/exe` 哈希与构建产物相同，三份生产源码和新增测试已逐文件核对旧哈希后同步共享工作区。

[候选清单](../.local/research-20260926/stream-lifecycle/candidate-manifest.json) · [红绿复现与CLI语义](../.local/research-20260926/stream-lifecycle/response-identity-review.md) · [独立复核](../.local/research-20260926/stream-lifecycle/peer-review.json)

[完整构建与测试](../.local/research-20260926/stream-lifecycle/build/build-manifest.json) · [上线回执](../.local/research-20260926/stream-lifecycle/release/activation-result.json) · [HTTP/WS验收](../.local/research-20260926/stream-lifecycle/release/runtime-smoke.json) · [临时资源清理](../.local/research-20260926/stream-lifecycle/release/smoke-cleanup.json)
