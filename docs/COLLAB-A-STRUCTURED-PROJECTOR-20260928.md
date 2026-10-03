# Structured output 投影独立实验

2026-09-28。结论：**真实离线 JSON Schema validator → 未改动的 `bps.Stream/Transformer` → 公开 Commit → 公开事件链已在私有模块闭合；只改 NormalizeItem 会泄露内部工具事件。** 真实失败帧与 usage 已补齐基本路径，schema/Commit 失败也保留已耗上游 usage。本实验尚不能作为完整 structured-output 实现或产品支持声明；仍有三类已复现的失败回显泄漏边界。

本轮先阅读 `.local/structured-probe/adapter-design-review.md`，随后仅复制 `internal/bps`、`internal/publicerrors`、go.mod/go.sum 到 `.local/collab-a-20260928/structured-projector/`。冻结主树源码、测试、依赖和工具均未修改；不访问真实上游、不部署。

## 可运行结果

项目根下执行一条命令：

```powershell
go -C .local/collab-a-20260928/structured-projector test ./internal/bps -run '^TestProjectionHarness' -count=1 -v
```

最新结果：10 个顶层测试、38 个子用例通过，无跳过；其中 1 个顶层/3 个子例是**确认尚存泄漏的复现**，不能把它们解释为对应隐私契约已通过。原拷贝的 bps/publicerrors 全套亦 PASS。最新输出为 `real-validator-tests.txt`、`copied-package-tests-latest.jsonl`、`latest-test-summary.json`；初版输出保留作历史证据。

最新 `snapshot-verification-latest.json` 记录复制的每个生产/原测试文件与主树 SHA 一致。原 `stream.go` 仍为 `15C7879231E200AA61CCA3B37ECEBBA120F67E7B6153A14239E85B77362C5436`。私有文件 SHA：

| 文件 | SHA256 |
| --- | --- |
| `internal/bps/projection_harness_test.go` | `94BCED4AEF8FBF6A0299DF702E6040BE4B92C65D4B1CAEF0016F42CD4256824F` |
| `internal/bps/projection_validator_contract_test.go` | `CAFE9B162972EF0AD172C3F83791B98E45B4C9C8F54D095A9115C96B176E640B` |
| 私有 `go.mod` | `D2EAE4EB9643C2EBBA1DC2028E3B4C773E505C2593F2E66D92BEE3143548CD3A` |
| 私有 `go.sum` | `4924AC7787F35A88268C13DF32F86F590A4B34D0B74CF665BDBD3D2E92C5CE09` |

## 已证实的最小接口

```go
type projectorHarnessPlan struct {
    Name, Namespace string
    Validate func(string) error
}
type projectorHarnessHooks struct {
    Commit func(bps.StreamResult) error
    Emit func([]byte) error
}
```

`Validate` 接收真实完整 arguments 字符串，不先解码到 float64。现在连接本地 `../structured-validator` 的 Compile/Validate，底层锁定 `github.com/santhosh-tekuri/jsonschema/v6 v6.0.3`，使用离线 loader、本地 $ref、AssertFormat、json.Number、重复键/尾随内容/无效 Unicode 拒绝。只有私有 go.mod 通过 require+replace 指向该模块，主项目依赖未改。`Commit` 收到已投影公开 Output/Response；`Emit` 为同步零到多 JSON event 输出，HTTP SSE/WS 外层序列化应共享此边界。

原始 Stream 的 destination 为 io.Discard，原始 Transformer 完整处理并校验内部 function added/arguments/done/completed。OnComplete 只有在原始 done/terminal 一致后才运行，先调用 Validate，再构造公开结果，再调用 Commit。提交成功后，原始 OnEvent 收到 terminal 并输出公开 7 帧：

1. `response.output_item.added`，type=message、status=in_progress。
2. `response.content_part.added`，空 output_text。
3. `response.output_text.delta`，值来自原始完整 arguments。
4. `response.output_text.done`。
5. `response.content_part.done`。
6. `response.output_item.done`，type=message、status=completed。
7. `response.completed`，包含相同公开 message。

该最小实验暂扣所有前置事件，公开 sequence_number 从 0 连续编号，output_index/content_index=0，message ID 是稳定 `msg_` 摘要，保留真实 response ID。公开 7 帧再送第二个未改动的 Transformer，证明其 done/terminal 完整性也成立。

已逐项断言：无内部函数名/call_id/fc_ ID/function_call 参数事件泄露；terminal tools 回显移除精确登记的内部函数；公开 SSE 终态、非流 JSON 返回值、Commit 保存 Output/Response 相同；真实 model/id/usage/cache_read/cache_write 未改写；arguments 内 `9007199254740993`、`1.2500e+02` 与转义字符保持原始字符串，未知终态数字也保留。

## 负例与已明确失败的实施方法

- schema/JSON fixture 拒绝、done/terminal mismatch、无 terminal EOF、持久化失败、未知内部事件、同名不同 namespace、真实 failed 均不发公开成功答案。
- done/terminal mismatch 由原 Transformer 返回 `ErrInvalidEvent`，没有削弱/跳过原完整性检查。
- Emit 第三帧失败后，返回 Completed=false，未尝试发送 completed。Commit 已成功的公开历史仍保留，与现有“先持久化、后终态交付”顺序一致；这不是 delivered=true 的保证。
- 单点 NormalizeItem 将内部 function_call 改为 message 的对照仍泄露 `response.function_call_arguments.delta` 与内部名字，实测确认此方案不够。
- 混合客户工具、多个内部结果、响应回显 forced tool_choice 均被明确拒绝，未删掉它们来制造成功。

## 真实 validator 与失败观测闭环

14 个 schema 子用例覆盖 nested object、enum、根和嵌套 required、$defs/local $ref、pattern、additionalProperties、数组、数字边界、重复键和尾随 JSON。两种合法 JSON（含不同字段顺序及 `125.00` / `1.2500e+02` 数字拼写）交付文本等于原 arguments 字符串。`9007199254740992` / `9007199254740994` 均不能冒充 schema 的 `9007199254740993`，无 Commit/Emit；所有 schema 不合格反例都保留真实上游 Response/Usage 作为内部观测，Completed/HistoryComplete=false。

7 个失败终态子用例从原 Transformer 的 `TerminalError.Payload` 出发：failed/incomplete/cancelled/provider cancelled、statusless flat error、没有 response 对象的 failed、同名不同 namespace 客户工具。保留原错误、真实 id/model/usage/未知字段，不补不存在的元数据、不运行 schema validator/Commit、不发 completed；只过滤直接 output/tools 中精确登记的内部函数。返回的 TerminalError.Payload 与实际公开失败帧一致，日志仍用机器码摘要。失败 Emit 出错只尝试一次，依然保留真实 usage。

另有 validation、Commit、done/terminal mismatch 三个子例证明本地拒绝不会把已经消耗的 token 变为 0。内部返回 Response 可能保留上游真实 completed 状态，但 Completed=false 且 err 非空，必须仅用于观察；不能将该内部结果当成功响应发送。

已明确复现的三个未满足契约：失败 `response.tool_choice` 引用内部函数、`response.tools` namespace 内嵌内部声明、未知 `future_trace` 引用内部 ID/名字，目前仍会进入失败公开帧。`TestProjectionHarnessKnownFailureEchoLeakBoundaries` 以确认泄漏为成功条件并清楚打印 KNOWN UNSATISFIED，供后续实现者把断言改成不泄漏；它们不是合格验收。独立未知事件类型引用内部项目前会 fail closed，但未知字段/嵌套声明还没有完整策略。

## 仍缺少的接口/实现和发布边界

本原型只支持“一个内部格式函数输出，无客户工具/拒绝/部分普通文本”的窄路径。完整产品至少还需：

- 对客户函数/custom/search 的保真实时转发、未知身份 stub 暂存、public output_index/sequence 映射；内部格式函数不可计入 public required/forced/parallel 策略。
- 恢复公开 text.format、tool_choice 与工具回显；真实 refusal 作为 refusal；修复上述三类失败回显泄漏。直接失败帧/usage 基本路径已通过，但完整投影仍不能视为合格。
- 真实 validator 本地链已接入，仍须验证上游实际可接受的 schema dialect/关键字范围、严格度和复杂结构；本地 JSON Schema 支持不等于上游 admission/模型输出能力。
- 公共历史与上游 reasoning 密文的真实续接、换 schema/撤销格式、previous_response_id、HTTP↔WS、compact；原型 Commit 使用同一公开结果仅证明本地顺序与数据一致，不能证明上游愿意接受该历史。
- 内部输出与普通响应混合时的延迟公开、流失败帧、取消后资源释放、超大输出 spill/performance；原型为最低界面证明，未实现这些状态机。

这些是新增 structured-output 功能的实施/验收缺口；不构成既有缓存与错误修复冻结版本的新增回归证据。当前实验不应合入运行源码直接启用 schema 支持。
