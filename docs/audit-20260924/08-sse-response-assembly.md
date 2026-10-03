# 08 — SSE、终止事件与非流式响应重建

审查日期：2026-09-24。以本地 `codex-source-0.155.0` 的实际 Rust 源码为基准，完成解析、重建、身份、内存边界、错误与透传边界的修复和回归。未访问生产环境。

本专项修改 `response_events.go`、`response_output.go`、`source_sse_lines.go`、`gateway_cache_diagnostics_sse.go`；仅窄改 `proxy.go` 的 `collectResponseObserved` 与 `writeStreamPipe`。新增 `response_assembly_audit_test.go`，修正旧测试中将不同 item ID 当作同一项更新的 fixture。未修改 `stats.go` 或 `gateway.go`。

## 核验基准

- CLI：`.local/codex-source-0.155.0/codex-rs/codex-api/src/sse/responses.rs`。
- `ResponseCompleted` 的必选 ID 与 usage 结构：118、128 行；item.done 独立消费：357 行；failed、incomplete、completed：417、472、483 行。
- CLI 使用 eventsource 解析：575 行；JSON 反序列化失败后继续下一事件：613 行；记录事件错误并继续轮询：679 行；收到 Completed 后结束：670 行附近。
- `protocol/src/models.rs:1061`、1121 行：function/custom tool 的 item.id 可缺失，call_id 用于调用关联。
- `Cargo.lock:6745` 锁定 `eventsource-stream 0.2.3`。从 crates.io 获取同版本源码到 `.local/sse-audit-20260924/eventsource-stream-0.2.3/`，归档 SHA256 为 `74fef4569247a5f429d9156b9d0a2599914385dd189c539334c625d8099d90ab`，与锁文件一致。
- 依赖 `parser.rs:9` 的语法包含可选 BOM，12 行定义 CR/LF/CRLF；`event_stream.rs:225` 按完整事件派发，270 行处理 BOM；`utf8_stream.rs:62,80` 使用严格 UTF-8 解码。

注意：依赖 `event_stream.rs:270` 检测 U+FEFF 后用 `&string[1..]` 截取，而 UTF-8 BOM 长 3 字节。按 Rust 字符串切片规则，这会在该分支触发非字符边界 panic；本次是源码确认，没有运行该依赖的 BOM fixture。网关正确移除 3 字节，不复制该缺陷。

## 已修复的行为

| 问题 | 修改与边界 | 证据位置 |
| --- | --- | --- |
| BOM 导致首个 data 字段不被识别 | 只去掉流开头的一次 BOM，支持逐字节分片；后续 BOM 保留为字段名内容 | `source_sse_lines.go:13`、`proxy.go:379`、`gateway_cache_diagnostics_sse.go:67` |
| 非法 UTF-8 被 Go JSON 自动替换后当成功 | collector 与诊断 observer 在完整行检查 UTF-8；共享 terminal 解析也拒绝非法字节，但不把本地解析失败称为协议 terminal | `source_sse_lines.go:8`、`response_events.go:47` |
| 相同 output_index 的不同 ID 静默覆盖已完成工具 | 显式索引中出现两个明确不同的 item ID 时返回错误，保留之前的项；同 ID 更新仍允许 | `response_output.go:120` |
| 可缺 item.id 的工具重复或覆盖 | function/custom/local_shell/tool_search 的同类型 call_id 参与去重与冲突检测；工具结果不与调用按 call_id 合并 | `response_output.go:89,117,199` |
| 终止 output 非空但只含 final message，丢失先前 reasoning/tool | 只有所有终止项均有唯一已知 ID 时，按已观察索引补回遗漏的 done 项；同 ID 内容以 terminal 为准 | `response_output.go:233` |
| 跨响应混入输出 | 检查 created/in_progress 的 response.id、item.done 的 response_id 与 terminal.id；已知身份不同时拒绝拼接 | `response_output.go:29,64,222` |
| 无 status 的 incomplete 被非流式记为成功 | `recordResponseOutcome` 使用事件 observer 已确认的 `TerminalStatus`；响应 JSON 保持原样 | `response_events.go:78` |
| 恰好 32 MiB 的合法重建被提前拒绝 | 修正已有 output 字段的大小估算，避免重复计入字段名；最终序列化结果仍再检查 | `response_output.go:293` |
| JSON 重建将 `<>&` 转义后放大六倍 | 使用关闭 HTML escaping 的 JSON encoder；避免正常代码/文本响应因无必要膨胀越界 | `response_output.go:304` |
| collector 丢失原始读取错误 | 使用 `%w` 保留取消/断流等错误链；超长行明确返回 32 MiB 上限错误 | `proxy.go:405` |
| 上游 body 持续返回 `(0,nil)` 形成忙循环 | 连续 100 次空读后返回 `io.ErrNoProgress`，有数据时清零 | `proxy.go:603` |

`proxy.go` 行号会随专项 07 的同文件修改移动，函数名为稳定定位点。

## 边界矩阵与一致性结论

| 边界 | CLI 0.155.0 | 网关核验/处理 |
| --- | --- | --- |
| CR / LF / CRLF 与跨块 CRLF | eventsource 识别三种换行 | collector、cache observer 都有逐字节回归；原 streaming observer 既有测试覆盖 |
| 流开头 BOM | 语法允许，但依赖代码有上述切片缺陷 | 安全去掉一个 BOM；后续 BOM 不特殊处理 |
| 注释、event/id 字段 | 事件正文由 data 组成 | 注释及未知字段不进入 JSON；不以 SSE event 字段代替 JSON type |
| 多条 data 行 | 用 LF 合并 | 保留一个可选前导空格规则与空 data 字段；聚合大小有界 |
| UTF-8 跨字节分片 | 依赖缓存未完整字节 | 网关在完整行验证；合法中文/emoji 分片通过 |
| 非法 UTF-8 | 不能成为正常可解析事件 | collector 报错；cache observer 只记录失败，不干预传输 |
| 错误 JSON / 非字符串 type | 跳过解析失败事件 | 网关继续等待合法 terminal；测试包含随后合法事件 |
| EOF 没有空行 | CLI 不派发未完成事件 | streaming/cache observer 保持严格；collector 保留既有无空行 EOF 兼容，明确不宣称完全一致 |
| `[DONE]` | 不是 Responses completed | 不能单独完成请求；可以忽略后等待合法 terminal |
| completed 缺 ID / ID 类型异常 | CLI 的 ResponseCompleted.id 为必选 string | 网关为已有兼容源保留宽松响应对象，不新增断流；测试固定该差异 |
| status 缺失 / null / 数值 / vendor string | CLI completed 结构不反序列化 status | 网关保留原对象；已有明确失败状态独立处理 |
| usage 字段不完整 | CLI usage 结构要求 input/output/total 等字段 | 网关计量有独立宽松校验，保留兼容，不按 CLI serde 类型拒绝整个响应 |
| response.done | CLI 无此分支，作为未知事件 | 网关保留兼容 terminal 扩展 |
| 顶层 error | 此 SSE Rust match 无对应处理分支 | 网关保留顶层 code/message/param 与嵌套 error 诊断 |
| failed / incomplete 后出现 completed | CLI 保存前者错误并继续，后续 Completed 可结束 | 网关保留首 terminal 决定结果的既有行为，防止后续帧覆盖故障；不称为逐字节语义复制 |
| incomplete 响应返回形式 | CLI 作为 Stream 错误 | 网关 nonstream 返回有效 partial JSON，并将记录标为 error；适配层的明确差异 |
| 稀疏/极大 output_index | CLI 不依赖该字段消费 item.done | 网关 map + 排序，不按最大索引分配数组；既有 MaxInt64 fixture 通过 |
| 同 ID / 同 call_id 的完成重发 | CLI 独立产生每个 item event | 网关 JSON 适配去重以免重复工具调用；索引冲突明确失败，不能宣称 CLI 原生重建规则 |
| terminal 仅含已知 ID 子集 | CLI 已独立交付 earlier done items | JSON 适配补齐；仅 added/delta 从不补回 |
| terminal 含匿名/未知 ID | CLI 不把 terminal.output 当逐项输出源 | 网关保留 terminal 原始字节作为权威，避免猜测关联 |
| terminal 含重复已知 ID | 无 JSON 聚合步骤 | 网关在可以确定身份时拒绝；不修改旧累计项 |
| opaque reasoning 与未知字段 | typed CLI 模型有自己的字段接纳规则 | 网关 RawMessage 保留 opaque 内容及未知整数，不经 float64 丢精度 |
| 单事件、聚合输出与条目数 | 上游 SSE 路径无同等明确界限 | 网关保留单事件 32 MiB、聚合结果 32 MiB、8192 项保护；这些是网关资源限制 |
| terminal 后 HTTP 不关闭 | CLI Completed 后退出 | writeStreamPipe 在交付 terminal 后释放；已有测试覆盖 account/admission 释放 |
| 读取错误、短写、Flush 失败 | 传输层产生错误 | 维持短写/flush 错误传播；新增错误链和无进展读取防护 |

非流式聚合假设每个 HTTP 请求对应一个逻辑响应。对显式出现的不同 response ID 已加检查；若某源完全省略响应关联字段，无法仅靠单个匿名项证明归属，继续按该 HTTP 请求的事件序列处理。完整 terminal 中未知 ID/匿名项仍保持原有权威规则，不冒险把旧项加入不明响应。

## 测试与复跑

新增 12 个命名回归测试和 2 个 fuzz target，覆盖 BOM 分片、UTF-8、JSON 宽松字段、索引/ID/call_id、响应 ID、terminal 子集与状态、精确限额、HTML 膨胀、读取错误、空读忙循环。既有 SSE/collector/cache/opaque/usage/首 terminal/大事件用例一并通过。

```powershell
go test ./... -run 'Test(Audit(SSE|Output|NonStreaming)|ResponseEvents|CollectedResponse|CollectResponse|GatewayCacheResponseObserver|SourceSSE|CLISSE|GatewayNonStreaming)' -count=1
```

最终结果：`ok codex-agent 4.035s`；native transport test package 无匹配测试。

```powershell
go test . -run '^$' -fuzz '^FuzzAuditSSEFragmentedOpaqueResponse$' -fuzztime=10s -parallel=2
go test . -run '^$' -fuzz '^FuzzAuditOutputIdentityState$' -fuzztime=10s -parallel=2
```

两轮 fuzz 均通过：SSE 分片 29,209 次执行，output identity 状态 33,139 次执行。输出状态 fuzz 验证每一步更新/拒绝后的索引、ID/call_id 映射和累计字节计数不变量。

未在此专项运行生产测试或重复全仓 race；主代理统一进行 Linux race / 整体验证。

## 交接专项 16

生产 `sseScanner` 在 `stats.go`，按分工未修改。已交付 `stripInitialSSEBOM`、`validSSELine` 供其复用：需在完整行进入字段解析前调用，并保证本地 UTF-8/观测失败只结束 observer，不能把 `terminal` 置 true 而提前截断透传。

另外已上报 `response.incomplete` 与嵌套 `status:"completed"` 冲突时的非流式计量问题。当前修改覆盖 status 缺失；专项 16 将统一 `observeGatewayTerminal` 与 `recordResponseOutcome` 的冲突优先级。需要区分事件状态与嵌套状态，不能为计量而重写返回字节。
