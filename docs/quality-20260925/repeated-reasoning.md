# 四次 reasoning=516：已保存证据的离线复核

2026-09-25。只读核对 `.local/research-20260925/multiaccount-abccba/`，**新增模型请求 0、生产访问 0**；原题标准答案仍为 **21**。不修改历史实验、采集器或网关核心。

## 直接结论

q5/q6 四次相同的是 **reasoning token 数 516 和最终答案数字 29**，不是完整最终文本：四份最终文本 SHA256 全不同，output tokens 也全不同。六格全部最终文本均不同。现有数据排除了这四格“同一份已保存 final 原样复用”；不能因此排除共享的服务端 reasoning、内部缓存或不同 final 前的其他复用。

| 格次 | 标准评分答案 | input | cached input | output | reasoning | output−reasoning¹ | elapsed 秒 | final SHA256 前 12 位 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 01-q0 | 21 | 6881 | 5760 | 2684 | 2406 | 278 | 67.808 | `5e438b9469d7` |
| 02-q5 | 29 | 6881 | 5888 | 675 | 516 | 159 | 17.202 | `2a42cdc3d557` |
| 03-q6 | 29 | 6881 | 0 | 728 | 516 | 212 | 21.566 | `87d813e7588d` |
| 04-q6 | 29 | 6881 | 0 | 745 | 516 | 229 | 38.621 | `f5043b4ca22d` |
| 05-q5 | 29 | 6881 | 5888 | 681 | 516 | 165 | 38.063 | `ba037eb84c2f` |
| 06-q0 | 21 | 6881 | 0 | 1838 | 1552 | 286 | 46.537 | `3ef0aabe12e80` |

¹仅两项已报告数字相减，不是重新 tokenizer 计数，不把它冒充可见文本的精确 token 数。完整哈希见附属 JSON。

### 已保存答案的解释内容

- 02-q5：以总数 41 减去 `min(7,6)+min(9,7)=13`，得到最多不满足条件 28，再加一为 29。
- 03-q6：组合 `7+9+(8+4)=28`，再加一为 29。
- 04-q6：列出全部西瓜味、圆形苹果味和圆形桃子味共 28，并作同类上界说明，再加一为 29。
- 05-q5：同样使用 `7+9+12=28`，但措辞与文本均不同。
- Q0 两次均显式利用可按形状摸取的 9 个圆形 + 12 个五角星形策略，得 21。

以上是**final 里公开解释的摘要**，不是隐藏 chain-of-thought。没有保留隐藏 reasoning 文本、reasoning summary 或 encrypted_content，因此不能判断四次隐藏推理是否相同。

## 缓存可证明到哪一层

1. **已报告 input-cache 计数**：01-q0/02-q5/05-q5 为正；四次 516 同时出现在 cached=5888 与 cached=0 情形，而正确 Q0 也既有正值又有零值。此小样本不能把 input cache 当作 516 或错误答案的充分原因。
2. **零值边界**：本批只保留 CLI `cached_input_tokens`，未保留上游原始 `usage.input_tokens_details.cached_tokens`。固定 CLI 0.155 的 `codex-api/src/sse/responses.rs:138–159` 在 details 缺失时默认 0。因此这三个“0”不能区分服务端明确报告零与上游字段缺失；`cache_write_input_tokens=0` 同理不是“没有任何缓存写入”的独立证明。
3. **不是输出缓存命中证明**：input cached-token 计数不是生成输出缓存标志。本批没有 response cache headers、Age/Cache-Status 等响应证据，也没有 response ID/hash 或完整响应摘要，不能证明服务器使用了 output cache，更不能定位其内部实现。
4. **六个 prompt_cache_key 的 canonical hash 各不相同**，并与各格 fresh session 的 canonical session/thread ID hash 对应。不同 key 不能证明服务端没有前缀/其他缓存；相同 input token 总数也不等于完整输入字节相同。
5. HTTP 下游 `Cache-Control: no-cache` 是网关 `gateway.go:862` 设置的流式传输头，不是上游模型 cache HIT/MISS 的证据；模型目录缓存也不是推理输出缓存。

## 复用与收集逻辑复核

- 重新读取 60 个保留文件并哈希；逐格核对 `events.safe.jsonl` 唯一 `turn.completed.usage` = `cli/report.json.usage` = `comparison.json.usage`；唯一 agent_message 文本 = report final，重算 SHA256 与 comparison 一致。不是比较器把前一格 final 或 output 数字复制给后一格。
- 当前比较器与答案解析器 hash 匹配最终 comparison 中声明；离线 `build_report(index)` **完整 JSON 精确复现**原 comparison。`safe_usage` 只是按字段过滤数值，未截断成 516；同一采集/比较链还保留 Q0 的 2406 与 1552，不存在“报告端一律封顶 516”的迹象。
- 固定 CLI `exec/src/event_processor_with_jsonl_output.rs:117–127` 将累计 usage 字段直接写 JSONL；API SSE mapper 将 `output_tokens_details.reasoning_tokens` 映射为 reasoning_output_tokens（`sse/responses.rs:146–149`）。本批 fresh home、每格一个 native 推理 POST、一条成功 inference outcome、attempts=1、HTTP 200/completed，无拒绝额外请求、CLI错误、重路由或可见传输错误。
- `gateway_quality_cli_server_test.go`、`gateway.go`、`proxy.go`、`gateway_native_http.go` 当前文件 hash 与实验 build-manifest 一致。fixture 每格创建独立 proxy；SSE 路径由 `gateway.go:858–871` → `proxy.go:600–615` 直接转发读取到的 bytes，scanner 旁路采集指标；IPC capture 的 `relay_pair`（`tools/quality_native_ipc_capture.py:532–557`，源码 hash 与当时 runner pin 一致）也只转发响应，没有跨格输出缓存逻辑。
- **独立性限制**：safe events 与 report 来自同一 CLI stdout，不是第二份上游响应；server-report 只保留状态/model/tier/attempt/error 摘要，没有 usage。现有摘要因此没有独立验证服务器实际 terminal usage，不能绝对排除任何未捕获层的错误。
- 比较器目前只消费 report，不读取 safe events。这里额外完成了交叉核对，但建议未来把它变成完整性检查；这是验证增强点，**不是已发现本批错误**。此次不修改共享工具。

## 上下文与 ID 边界

离线重新执行 15 对、105 个 input-item 字段对照：input 的 content/tools/role/type 相同；item 0、1 只差 id，item 2–6 只差 id 及 internal metadata 的 create_time/turn_id。顶层仅 `input`、`client_metadata`、`prompt_cache_key` 摘要不同；client_metadata 七键随 fresh session 改变，嵌入式 `x-codex-turn-metadata` 只有整体 hash，不能穷尽其内部差异。没有 previous_response_id，也没有顶层显式 max_output_tokens；这不是服务端内部预算不存在的证明。

**本批 response ID/hash 未保存。** `server.ingress.request_id_sha256` 是请求的 `X-Client-Request-Id` hash，不可冒充响应 ID；IPC request/body hash 也不可冒充响应 hash。原始 response.completed、reasoning item、SSE body hash 都不在本批材料内。

## 后续 exact probe 已有字段与剩余缺口

只读检查当时 `tools/quality_exact_request_probe.py`：

- `response_id_sha256` 可验证已捕获响应身份是否重复；不同 ID 仍不能证明服务器未复用内部计算。
- terminal `response_usage`/`response_controls.usage` 的 safe_usage 保留上游 nested 数字，区分字段缺失与零，补本批扁平 CLI 计数缺口。
- `response_controls` 保留可见 reasoning 配置、max_output_tokens、model/tier/status；显式预算是证据，字段缺失不能证明不存在服务端内部预算。
- `response_header_hashes` 保留原生响应头名/值 hash，可确认已知名称、比较相等性或校验预知 HIT/MISS 常量；不能还原任意未知 header 值，也不能直接证明模型内部 cache。
- 已有 `response_signals` 只保留 response-level 控制值；当时不含 reasoning output-item/summary/encrypted-content 的内容摘要与完整响应/SSE hash。并行专项正在增强 output 摘要，**不能追溯补齐本批历史缺失数据**。即使未来 encrypted_content hash 不同，也不能据此断言隐藏 reasoning 不同（密文可能具有随机性）。

## 产物与复核命令

- `.local/research-20260925/repeated-reasoning/repeated-reasoning.json`：简明结论、各格完整 final/cache-key hash。
- `.local/research-20260925/repeated-reasoning/artifact-sha256.json`：60 个证据文件 hash。
- `.local/research-20260925/repeated-reasoning/audit.py`：无网络导入、无 probe/runner 调用的离线核查脚本。

```powershell
python .local/research-20260925/repeated-reasoning/audit.py
```

实际 exit 0，六格交叉检查全部通过、原 comparison 完整复现。**516 仅为观察到的重复计数，不能推成 512 硬预算、隐藏集群、账号降档或已证明的缓存原因。**
