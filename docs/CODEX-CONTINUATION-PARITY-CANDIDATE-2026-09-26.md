# HTTP/WS上下文与续接修复：源码已修，尚未部署

后续工作区已在本候选上叠加[请求生命周期修复](CODEX-REQUEST-LIFECYCLE-CANDIDATE-2026-09-26.md)，本页保留397文件版本的历史证据，不代表最新工作区哈希。

本轮候选基于实际线上Go `6ecdf93f…` 对应392文件冻结源，合并后397文件；只修改7个生产文件并新增4个测试文件。完整本机Windows测试2994个通过事件、382项明确跳过，vet通过。组件独审及合并HTTP独审通过。远端Linux合并构建启动在进程创建前被平台拒绝，没有换路径重试或让子agent代发；只读核对没有对应容器、目录，线上仍为`6ecdf93f…`。本地Linux交叉编译成功不等于Linux运行验证。

## 1. JSON聚合的精确字段身份

`response_output.go` 对非流式响应收集事件时使用大小写不敏感结构体，导致未知`Type/Item`扩展改变事件类别或替换真实item，`ID/Call_ID`扩展可将两份不同加密推理或工具调用误并为一份，created对象扩展还能制造错误响应ID冲突。真实TCP原版红例已保留。

修复只按正式字段精确读取事件、响应及item身份；未知扩展继续原样保存在opaque item中。稀疏终态仍补回已收到的加密字段，显式终态空值/null/对象保持权威，不从delta/added-only凭空制造工具调用。item中某个可选身份字段类型错误，不会使其它合法身份的识别依赖map遍历顺序。真实ID冲突仍拒绝，SSE透传字节不改。

固定官方CLI独立消费`output_item.done`，Completed不读取output；JSON聚合是本网关的适配合同，不宣称与CLI事件流逐字相同。官方源9文件哈希、8个source-backed序列以及独立13顶层＋6子用例均已核验。

## 2. WebSocket续接配置连续性

官方CLI在非input属性相同且历史前缀匹配时才发送previous_response_id增量。然而网关两轮间改提示词文本、mode、enable或兼容设置后，原先仍保留previous_response_id却应用新改写配置，客户端不知情。

现在连接内按响应ID保存有效改写策略摘要，不存提示词明文。已知链的策略改变后拒绝增量请求，提示发送无previous_response_id的完整input；完整请求可在原连接继续。客户端主动改变instructions不作为网关策略漂移拒绝；无关设置及禁用/空提示词的等效配置保持可用。此门禁对if_empty已有指令等情况采取保守连续性策略，不能说每次配置改变一定已改变出站字节。未知响应ID与连接内256条历史窗口等原兼容边界保持。

## 3. 活动WS lane归属

已绑定唯一活动队首response ID的事件若缺stream_id，原先可能不释放正确槽位；显式B lane夹带已归A的response ID则可能把A计量与429归给B。现在只按唯一活动队首精确关联，未知/歧义ID不猜，显式错误lane不能抢占。已退休ID不释放后续队首。

独立审查又复现默认lane空字符串与“无归属”混淆：已知默认lane的顶层error会关闭另一named lane。最终v2用独立matched位区分，错误仅结束目标队首，其他lane继续。未知/无ID的连接级错误保持旧行为。WS v2组件单独完成Linux断网race87通过、0skip，含真实bf920 helper；该组件结果不能替代合并源Linux全量回归。

## 4. HTTP hop-only头的内部视图

Connection提名的Retry-After等字段早已不会转发到客户端，但内部冷却/记录仍读取原值：3600可能造成异常长冷却，0可能绕过实际429的默认冷却；请求ID与模型标签也被误记。

现在在实际HTTP响应边界产生不修改原对象的净化视图，再供内部观察/调度与下游使用；OAuth refresh错误响应也使用该视图。正文、状态码、Body/Request引用与底层framing不改，真实429默认冷却和SSE正文限流保持，非hop opaque头及重复未来扩展头保留。此函数不用于WS upgrade。实际本机HTTP/2、多轮401最终凭据/头、404两模式原正文与三组件合并场景均通过；8个新增HTTP native分支仍需在合并Linux中运行。

## 验证状态与交付

- 冻结源码SHA：`907aafc79ee1680379b2ee5856530612f3779b0865e731f57807a6bd8c78b5b3`。
- Windows全套2994通过、382明确跳过、0失败；vet通过。
- 本地Windows产物：`a6e7f5ce…`；Linux交叉编译产物：`393b07c3…`。均不是当前线上二进制。
- 生产七文件与四测试已核对旧SHA后同步工作区，未覆盖其它修改。测试生成的pyc明确排除，发布归档只含声明397文件。
- 远端合并race/native/PG/Redis运行尚未完成，部署尚未执行。发布脚本7＋23项检查通过，只说明发布流程，不说明候选已满足上线门槛。
- 本轮没有真实模型、账号刷新、重新授权或设备/指纹更改，也未重试此前被拒绝的v27在线状态观测。

这些是可复现的网关内容保真和调度问题，没有证据将它们认定为已有糖果题29/23错误的根因；原题质量目标仍未完成。

[候选清单](../.local/research-20260926/continuation-combined/candidate-manifest.json) · [本地完整验证](../.local/research-20260926/continuation-combined/summary.json) · [远端未启动核验](../.local/research-20260926/continuation-combined/remote-unstarted-runtime.json) · [工作区同步](../.local/research-20260926/continuation-combined/shared-source-sync.json)

仅在冻结目录本地复查：`go test -count=1 -timeout=12m ./...`。该命令不启用真实账号测试或绕过远端执行边界。
