# A 组 structured stream 私有快照审查 — 2026-09-28

范围：`StreamStructured` / `projectStructuredTurn` 的事件一致性、回调顺序、内存、取消和性能。只复制 `internal/bps`、`internal/publicerrors`、go.mod/go.sum 到 `.local/collab-a-20260928/structured-stream-review/` 后加本地测试；没有修改主工作树源码、真实模型调用或部署。正在开发的主源码不一定与本快照相同。

## 三项明确红测

这些测试断言“应拒绝不一致或保证已读终态得到内部观察”，在旧快照上实际 **FAIL**，并非绿色测试断言 bug 存在。日志 `regression-before.log`；主任务已接收并转交实现 owner。

1. **普通文本的 item_id / output_index 不一致仍成功输出。** 终态 output 只有 index 0 的 msg_a，前置 text.delta 使用同一 msg_a 但 output_index=8。`structuredValidateTextEvents` 优先以 item_id 选最终文本，未交叉核验索引；成功投影原样输出 index 8 和最终单项 output，Completed=true。fixture：`TestCollabStructuredTextIndexIdentityMismatch`。
2. **纯 refusal 终态允许不属于最终输出的普通 text.delta。** 前置 msg_ghost/index 7 发 `UNVALIDATED PLAIN TEXT`，终态仅 msg_refusal/refusal；`publicCalls == 0 && !refusal` 分支因 refusal 为真而跳过所有普通文本一致性校验，未验证 ghost 文本仍被发出且 Completed=true。fixture：`TestCollabStructuredRefusalDoesNotAuthorizeUnmatchedTextDelta`。与 C 的 `phase=final + client tool` 复现不同，但建议合并处理同一验证分支，避免重复改动。
3. **读到真实失败终态后，首帧下游写失败会遗漏终态 OnEvent。** 上游已经完整读完 response.created + response.failed(rate_limit_exceeded, usage.input_tokens=37)；首帧 writer 返回 io.ErrClosedPipe，外部 OnEvent 只观察到 response.created。`StreamResult.TerminalType` 与 `Usage` 仍保留，但返回的 err 是写错误而非 TerminalError，依赖 OnEvent/TerminalError 的限流观察需要独立兜底。fixture：`TestCollabStructuredFailedTerminalObservationBeforeDelivery`。这是回调层事实；生产 cooldown 的最终影响需结合 C 的运行实现，不在本任务直接断言。

从仓库根一条复跑（三条强红测；此旧快照应返回非零）：

```powershell
go -C .local/collab-a-20260928/structured-stream-review test ./internal/bps -run '^TestCollabStructured(TextIndexIdentityMismatch|RefusalDoesNotAuthorizeUnmatchedTextDelta|FailedTerminalObservationBeforeDelivery)$' -count=1 -v
```

Owner 修复后可复制更新的 `structured_stream.go` 和必要依赖到同一私有目录，以完全相同测试验收，另存 after hash/log；不要覆盖当前 before 证据。若 owner 新增了独立 upstream observer API，第 3 条 fixture 可按新 API 验证终态内部观察，但应保留原始失败证据及明确的契约变化记录。

## 契约待确认：多个普通 message 拼接后才满足 schema

`TestCollabStructuredMultiMessageSplitSchema` 构造两个 message 文本分别为 `{"value":` 和 `"OK"}`。当前把所有 message text 拼接后校验，通过后保留两个原始 message，Completed=true；逐 item 消费者得到两个非法 JSON。私有测试按“每个公开 structured 文本 item 均应可解析”断言，因此是 FAIL。

这个结果不直接列为确定产品 bug：如果公开契约明确允许跨 message 的 response.output_text 聚合，它可能合法；如果 SDK/客户端逐 output_text/message 解析就不满足预期。需主任务明确选择：逐消息合法、归并为单个结果并保持索引事件一致，或明确文档化聚合消费。不要仅把两个半段都验证成功。

## 内存、取消与复杂度实测

环境：Go 1.26.7，windows/amd64，AMD Ryzen 7 9700X；固定本地合成数据，非模型响应性能。普通投影微基准每档 3 次：

| 普通 delta 数量 | 时间/op | 分配 bytes/op | allocations/op |
| --- | ---: | ---: | ---: |
| 100 | 0.888 ms | 581,405 | 15,051 |
| 1,000 | 8.036 ms | 5,623,514 | 146,333 |
| 10,000 | 91.837 ms | 56,647,037 | 1,460,450 |

普通多 delta 路径与事件数近似线性，没有测得普遍 O(n²)。多轮 JSON 解码/编码和保留 normalized/projected 事件带来很高常数项；10,003 个事件完整 Stream 合成输入仅 1,150,480 bytes、最终 text 10,012 bytes，至 OnComplete 已累计分配 **70,052,400 bytes**，采样 live HeapAlloc 相对基线增加 **4,052,056 bytes**。后者不是整个过程严格 peak，GC 时点会变化；不能把累计分配当常驻内存或 OOM 证据。日志 `projection-benchmark.log`、`cancellation-memory.log`。

20,000 delta 的 CPU projection 期间取消，约 **146.9 ms** 后返回 context.Canceled，OnComplete 未调用。快照已包含 projection 后的 ctx 检查；projection 内本身不响应取消，所以延迟受当次 CPU 投影长度影响。此项检查 PASS，未声称存在取消后写历史。阻塞 reader 取消由既有 Stream 的 io.ReadCloser cancellation 支持；无私加后台 reader。

**失败索引回退有 O(E×I) 候选。** 每个缺少 indexMap 的普通失败事件都遍历全部 `internalIndices` 计算前面的删除数；I 个 internal 加 I 个普通事件时该部分 O(I²)。`BenchmarkCollabStructuredFailedIndexRemapScale` 的 helper 级构造（不是完整线上 stream）结果：

| internal / 普通事件各自数量 | 时间/op | 分配 bytes/op |
| --- | ---: | ---: |
| 100 / 100 | 1.343 ms | 788,192 |
| 1,000 / 1,000 | 17.146 ms | 7,959,397 |
| 5,000 / 5,000 | 182.272 ms | 39,924,496 |

这是异常/失败时多 internal call 的路径，不推广成每次结构化响应都是 O(n²)。低风险候选是在排序后的 internal index 上求前缀删除数或二分计数，避免每事件全扫；本任务未修改实现。日志 `failure-index-benchmark.log`。

性能复跑：

```powershell
go -C .local/collab-a-20260928/structured-stream-review test ./internal/bps -run 'TestCollabStructured(CancellationDuringProjection|BufferedPeakMeasurement)$' -bench 'BenchmarkCollabStructured(ProjectionScale|FailedIndexRemapScale)$' -benchtime=3x -count=1 -benchmem -v
```

## 首字观察语义

当前 OnEvent 全部在完整上游读取、校验、OnComplete 之后重放；主 gateway 的 observation.event 会在重放时取 time.Now。对可见 structured text 的 TTFT 而言，这是客户端实际等到可发输出的时刻，不能把延迟简单称为统计 bug；但它不再表示上游最早产出 delta 的时刻，二者不应用同一标签作性能对照。失败终态即使无需 schema 校验，也要先完成之前所有公共事件的 replay，导致上面的 writer failure 漏观察边界。成功 OnComplete 前不发 model 输出是当前明确策略，本次没有要求取消该策略。

## 私有快照 SHA-256

完整列表：私有目录 `review-final-hashes.json`。本表绑定已经执行的旧源码，不声称覆盖 owner 后续修改。

| 文件 | SHA-256 |
| --- | --- |
| structured_stream.go | 22542e977faf12336ca48c41094229db2d6710bb9883992aec1653356fd99069 |
| stream.go | 15c7879231e200aa61cca3b37ecebba120f67e7b6153a14239e85b77362c5436 |
| structured_format.go | 2088e21a77becf1e54b55d9ccc9bcfab9e6472224661e1e2fc3505fdf1342d21 |
| structured_validation.go | 3735c58645fdf23b2e6077c16f2d7a5326e082bd1a89ccd8e78e38a18d9aba00 |
| collab_stream_review_test.go | e6e61b817c75c78c8f2a3a0985f199358087ab3b15d1304641a28207970e121f |
| go.mod | cba0c22505780cea9d61d1042cf921ca443e78c6403a4c46ef028b35c61fdad4 |
| go.sum | 4924ac7787f35a88268c13df32f86f590a4b34d0b74cf665bdbd3d2e92c5ce09 |

## 后续有界优化：失败索引回退（私有候选）

父任务要求后，在独立 `.local/collab-a-20260928/structured-stream-remap/{before,after}/` 创建对照，不改前述旧 review 副本。先检查 canonical：当时已更新至 SHA `643bc88fc1c406bbeb8bc9edf955f929a2072c716b568a856d13019b2928ee1c`，但目标失败索引循环仍未变，因此两侧都重新复制这一最新源码；仅 after 加排序索引优化。

最小 patch：`.local/collab-a-20260928/structured-stream-remap/failed-index-remap.patch`。失败时将 `internalIndices` 的唯一索引排序一次；每个缺少 `indexMap` 的公共事件用 `sort.SearchInts` 获得严格小于当前 index 的内部索引数。该数量与原 map 全扫描完全相同，保留 `indexMap` 优先、internal 事件排除、错误事件和 sequence 重编号。目标计算复杂度从 O(E×I) 降为 O(I log I + E log I)，额外 O(I) 个 int；成功路径不创建或排序此数组。

**语义验证实际 PASS**：300 个失败投影 corpus（稀疏/乱序/重复索引、索引缺失、最终 output 有无、indexMap 优先、超出 output 范围、MaxInt）+ 2 个成功内部工具投影，总计 302 例。两側 `StreamResult`、完整事件字节、sequence、usage 和 error 序列化摘要逐例完全一致，equivalence.json 文件 SHA 均为 `b5d36e8a5fd9150d90b273914b38e780cca21b1100b375c8a858ca2483c85d5d`。另外两侧原有 11 项 `TestStreamStructured*` 全部 PASS。此一致性结论只证明优化保持该快照行为，不代表上面三项独立红测已修复。

同机串行运行 before 后再 after，`-benchtime=5x -count=3`，下列为三轮中位数：

| 失败路径 internal / 普通事件各自数量 | before | after | 时间变化 | 分配 bytes/op before → after |
| --- | ---: | ---: | ---: | ---: |
| 100 / 100 | 1.244 ms | 1.106 ms | -11.09% | 788,984 → 789,880 |
| 1,000 / 1,000 | 18.493 ms | 16.288 ms | -11.92% | 7,957,908 → 7,966,504 |
| 5,000 / 5,000 | 180.600 ms | 69.199 ms | **-61.68%** | 39,917,843 → 39,958,249 |

普通成功投影对照：100 delta 为 0.881→1.010 ms，1,000 为 8.682→8.383 ms，10,000 为 92.613→87.900 ms，分配量基本相同。次数较少且同机存在其他协作工作，不能将这种小幅波动解释为成功路径性能变化；本 patch 未改变普通成功路径算法。**61.68% 仅属于这个 5k+5k 异常失败微基准，不表示全网关/常规请求提速。** 原始日志分别在两侧 benchmark.log，所有采样在 summary.json。

Owner 可审阅最小 patch 后在自己源码上合入，不应整体覆盖最新 `structured_stream.go`；本任务没有修改 canonical。完整证据一条复跑（会更新私有日志）：

```powershell
python .local/collab-a-20260928/structured-stream-remap/verify.py
```

该脚本只运行两份本地模块、固定合成测试与串行基准，再核对全部输出摘要；不访问生产/真实模型。已有日志由同样的 go 命令实际完成，不是预测值。

| 优化证据 | SHA-256 |
| --- | --- |
| 最新 before structured_stream.go | 643bc88fc1c406bbeb8bc9edf955f929a2072c716b568a856d13019b2928ee1c |
| 私有 after structured_stream.go | f5485fc9eecd7d659bd22e7e5f70f4aacbce5ac6072b8115d3f36caa5301e9ea |
| failed-index-remap.patch | d7a9674bc80d2191ab72babd0784a078d2e0fbc9e78ad6fe1d7458f639bcc252 |
| collab_remap_equivalence_test.go | f0f50d4071e1f1d123154bba159df59087e0808bd6ee90742bbbc9f661700e3e |

## Owner 合并后三项最终独立复核

使用全新私有目录 `.local/collab-a-20260928/structured-stream-after-review/` 复制最新 canonical bps/publicerrors/go.mod/go.sum，带入同一原始 harness；没有覆盖先前 before/remap 证据，没有改主源码，没有扩展测试或重跑 bench/模型。

实际执行 **3/3 PASS，0 skip/fail，包用时 0.050s**：

- `TestCollabStructuredTextIndexIdentityMismatch`：原 msg_a / index 8 / 单项终态输入不变，现在返回 `bps response identity changed during the stream: event item identity and output index differ`。
- `TestCollabStructuredRefusalDoesNotAuthorizeUnmatchedTextDelta`：原 msg_ghost 普通文本 + 纯 refusal 终态输入不变，原输出漏洞断言已通过。
- `TestCollabStructuredFailedTerminalObservationBeforeDelivery`：上游 created+failed(rate_limit_exceeded, input_tokens=37) 与下游首帧 io.ErrClosedPipe 均保持原样；按新增正式契约把内部终态观察转到 `OnUpstreamTerminal`。实际 hook **恰好 1 次**，type/id/code/usage 原值均符合，result.Usage 仍为真实 `{"input_tokens":37}`，Completed=false、返回 io.ErrClosedPipe；公开 OnEvent 仍只见 response.created，符合其重放/写入语义，不要求失败后继续公开写事件。

只读查看 gateway 接线：structured 分支将 cooldown 接到 OnUpstreamTerminal；普通 OnEvent 的 cooldown 分支仅在 prepared.Structured==nil 时执行，因此这条结构化路径没有公开重放重复提交同一 cooldown 的明显接线。这里是源码检查，未执行 server 集成或生产 cooldown 测试。

复跑：

```powershell
go -C .local/collab-a-20260928/structured-stream-after-review test ./internal/bps -run '^TestCollabStructured(TextIndexIdentityMismatch|RefusalDoesNotAuthorizeUnmatchedTextDelta|FailedTerminalObservationBeforeDelivery)$' -count=1 -v
```

证据：`three-boundaries-after.log`、`canonical-before.json`、`canonical-after.json`、`tested-source-hashes.json`。测试前后 canonical 七文件快照一致；受测私有实现与复制时 canonical 一致。三项原明确问题在此源码快照无剩余失败；多 message 契约、完整 schema 回归和生产发布不在这次三项复核结论范围。

| 文件 | 原始红测快照 | 当前受测快照 |
| --- | --- | --- |
| structured_stream.go | 22542e977faf12336ca48c41094229db2d6710bb9883992aec1653356fd99069 | 5f745695c2a20f2821d1b13dfd6510d02b9cd2012e9ccb5e44639f023d361b1d |
| stream.go | 15c7879231e200aa61cca3b37ecebba120f67e7b6153a14239e85b77362c5436 | c40b124764612c989174aea7ea04766f4a2e6171b1d579f886815b780e824ba8 |
| structured_format.go | 2088e21a77becf1e54b55d9ccc9bcfab9e6472224661e1e2fc3505fdf1342d21 | 780017a2dc86aa9ca04360f06af9d417b416187dac7c197a17fa6de5dcdebfa3 |
| structured_validation.go | 3735c58645fdf23b2e6077c16f2d7a5326e082bd1a89ccd8e78e38a18d9aba00 | edafae10663eb8e0d4df50449b13fe4cfdc0928c97068bb84d7e38de804305bc |
| collab_stream_review_test.go | e6e61b817c75c78c8f2a3a0985f199358087ab3b15d1304641a28207970e121f | 87b17ed4a4fb7712b659f694a493a3f5b4b31125155a50f8a38809947746c33e |

本轮只读 gateway_bps.go SHA 为 `2ece082d4b1e2d78e167f9582dc6184b97ffc3f189e5c074b87683fcc2bea580`；go.mod/go.sum 与上表先前相同。
