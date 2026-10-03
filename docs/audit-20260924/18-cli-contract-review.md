# 18 — 官方 CLI 契约独立复审

审查时间：2026-09-24。只读生产实现与 `.local/codex-source-0.155.0`，独占新增 `gateway_cli_contract_audit_test.go`。官方 Linux CLI 0.155.0 已有证据中的 SHA-256 为 `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`。本次不改生产代码。

## 结论与证据分层

现有测试确实启动官方 CLI，经过 Go 网关和实际 Rust UDS helper，再连接 loopback 上游。工具不是由测试伪造 output：CLI 实际运行 shell / apply_patch，后续请求包含实际结果，file-edit 还检查磁盘文件、失败退出码及 resume，parallel 还检查排他创建文件与进程运行时间重叠。合成上游事件负责决定工具调用和最终回答，不是实际模型推理。

本次补充原始 SSE / JSON 汇总边界、原生身份头、API-key 特征、无效终态及不重放的可重跑实例。普通 Go 用例本机已通过；root 已实际运行新增 Linux `TestGatewayNativeActualCLIContractAudit`，正常/无效终态两个场景均 PASS（2.28s），race 与 vet 亦通过。报告为该轮 `go-cli-contract/output/codex_native_contract_cli_report.json`，由 root 下载汇总。本机普通用例并非实际 CLI 结果的替代。

证据等级分别是：源码行为；普通网关协议 fixture；官方 CLI 的真实本地工具执行；合成 ChatGPT auth 触发的 CLI 运行时分支；真实 TLS 与 HTTP/WS 握手。真实账号鉴权、远程模型推理、生产缓存命中率、生产网络延迟均未在这些用例中验证。

## 契约矩阵

源码路径以下均相对于 `.local/codex-source-0.155.0/codex-rs/`。

| 契约 | 源码依据 | 当前证据及判断 |
| --- | --- | --- |
| 1. Function 工具调用及输出 | `protocol/src/models.rs:1061`、`:1101` | native HTTP / file-edit / parallel actual CLI 校验 call_id、工具名、输出类型及退出码；不是仅检查 HTTP 200。 |
| 2. Custom 工具原始输入 | `protocol/src/models.rs:1121`、`:1142` | file-edit 报告 `apply_patch_wire_kind=custom`，2 次 custom 调用实际创建/修改文件，5 次工具结果；真实 namespace 为 absent，不能宣称已真实执行 namespaced custom。 |
| 3. Parallel 工具及完成顺序 | `core/src/client.rs:877`、`core/src/tools/router.rs:233` | 重跑 HTTP / WS 均为 2 工具、退出码 `[0,7]`、完成顺序 `b,a`；运行区间分别重叠 122288092 / 120300708 ns，排他创建文件验证没有二次执行。 |
| 4. SSE 原始 done 与 JSON 聚合不同 | `codex-api/src/sse/responses.rs:356` | CLI 将 done.item 转为 ResponseItem，不依赖 output_index。新普通 fixture 共用索引的 function/custom 被流式逐字节转发，JSON collector 拒绝冲突；新 actual-CLI fixture 两 function 共用索引并真实运行，单独检查原始字节 SHA-256。 |
| 5. previous_response_id 增量 | `core/src/client.rs:1255`、`:1297` | WS fixture 要求同一连接内最后 completed ID，按 prior.input + prior.output + delta 重建逻辑输入，工具 follow-up 恰为一个新结果；不是只检查 previous 字段非空。 |
| 6. Warmup 与空 delta | `core/src/client.rs:1804`、`:1934` | actual WS 验证 generate=false 和零 token，不发真实工具。已有实际 CLI warmup 的 empty-delta 计数为 0；允许空 delta 的源码及 native synthetic fixture 证明边界，但不能把该值说成已有真实 CLI 空 delta 证据。 |
| 7. 426 回退责任与无重放 | `core/src/client.rs:1731` | 实际 CLI 2 次 WS 426、3 次 HTTP、1 次实际工具，拒绝握手 body/post-header data 均 0。加入高优先级竞争账号后仍保留原账号，说明 binding 的断言并非空转。未证明所有 HTTP 错误均可回退。 |
| 8. 网络/终态错误不可网关自动重发 | `gateway_upstream.go` 的 `doAccountRequest` | 生产仅显式 401 能 auth recovery。新增无效 completed fixture 只准 1 次上游请求，CLI 必须非零退出且网关记录 error；不存在为了得到成功而把已接收 SSE turn 重放的逻辑。 |
| 9. User-Agent / Originator / Version | `model-provider-info/src/lib.rs:454`、`core/src/client.rs:1223` | builtin provider 注入 Version，custom API-key provider 不一定有。新 actual fixture 比较 CLI 入站与 native 出站 UA/Originator 相等、Version 缺省保留，不能将“总要 version”视为一致性要求。 |
| 10. Authorization / Account / Attestation | `core/src/client.rs:1231`、gateway header/auth 路径 | native helper 成功且 Go fallback guard 为 0；新 actual fixture 要求出站 managed token/account，保留原生身份对应的 synthetic opaque attestation。此处只证实透明转发，未证明 attestation 真伪或远端认可。 |
| 11. HTTP 与 WS turn-state | `core/src/client.rs:275`、`:1225` | 既有 WS 测试区分握手 header 与 response.metadata；CLI 0.155 WS core 不将握手提示直接视为当前 streamed state。新 HTTP fixture 验证第二次工具请求回显响应 header 的 state。 |
| 12. Compaction V2 / resume | `core/src/compact_remote_v2_attempt.rs:76` | 实际 CLI 经 `/responses`、一次最后位置 compaction_trigger、5 请求/2 工具/1 压缩、3 次后续 opaque 透传，2 进程恢复；不是旧 `/responses/compact` API 结果假扮 CLI V2。 |
| 13. Compaction retained-context 差异 | `core/src/compact_remote_v2.rs:542`、`:606` | CLI 使用内部用户/开发者/agent 元数据、token 与 image budget、边界截断；gateway legacy adapter 为最多 256000 字节完整 user/developer/system message 后缀。这是明确兼容适配差异，不能宣称完全一致；不影响原生 CLI 直通 V2 的本地 retained 逻辑。 |
| 14. Guardian 独立路由 | `codex-api/src/endpoint/responses.rs:29` | `/guardian`、`/guardian-classifier` 通过普通/native compression fixture，保持专用 instructions，不注入常规 prompt/cache 策略。未有 actual CLI Guardian 审批/分类流程端到端执行，按协议 fixture 级别标注。 |
| 15. API-key / OAuth 压缩上下文 | `core/src/client.rs:1450`、`model-provider-info/src/lib.rs:526` | network-none 实际 CLI 四组：ChatGPT+OpenAI+enabled 才 zstd；disabled / API-key / 自定义 provider 名称均不压缩。合成 OAuth → gateway → native helper 还比较 input/instructions/tools/cache 等完整 JSON 语义。不是只看 Content-Encoding 字符串。 |
| 16. 终态与无效 response 差异 | `codex-api/src/sse/responses.rs:118`、`:480` | CLI completed 必须反序列化含字符串 id 的 ResponseCompleted；gateway generic observer 只要求 response 为 object，因此 `{}` 或错误 usage 结构的诊断可能比 CLI 宽松。已反馈 root；应区分 CLI 专用诊断与兼容调用方，不能改变流原文以“修正”上游。 |

## 新增 fixture 的可复现边界

`TestGatewayCLIContractAuditRawStreamAndCollectorBoundary` 使用 CRLF、未知事件、Unicode、`<>&`、超出 IEEE-754 精确范围的整数和冲突 output_index，要求网关流字节完全相同且仅一次请求、admission 归还；完成对象缺失时记录 error。相同内容经过 JSON collector 必须拒绝两个可执行项的索引冲突。

`TestGatewayNativeActualCLIContractAudit` 使用明确 opt-in 的官方 CLI 与 Rust helper。正常场景发出两个不同 call_id、相同 output_index 的实际 shell 调用，各自 append 一个文件并回传 Unicode/符号 marker；测试读取文件精确值以排除重复执行。第二次请求返回终态 output 省略的完整消息，验证 CLI 使用 done.item。异常场景发送缺少 response 的 completed，CLI 非零退出、仅一次请求且网关 error。两场景均比较网关写出的 SSE 原字节、TLS1.3/H2、native header 与 managed auth，禁止 Go 出站降级。独立临时 HOME / CODEX_HOME，所有凭据为合成值，无 Python 依赖。

普通用例：

```powershell
go test . -run '^TestGatewayCLIContractAuditRawStreamAndCollectorBoundary$' -count=1
```

Linux 官方 CLI 用例（环境变量指向当前已构建二进制）：

```sh
CODEX_NATIVE_CAPTURE_BIN=/path/to/codex CODEX_NATIVE_HTTP_TEST_BIN=/path/to/codex-native-transport CODEX_NATIVE_CONTRACT_CLI_REPORT=/tmp/cli-contract-audit.json go test . -run '^TestGatewayNativeActualCLIContractAudit$' -count=1 -v
```

## 审查日志与解释边界

- `.local/gateway-audit-20260924/go-cli-wave/tests.jsonl`：初轮含 2 个 top-level 失败及 1 跳过；不能把整轮说成全部通过。
- `.local/gateway-audit-20260924/go-cli-python/tests.jsonl`：补 Python 后 file-edit / HTTP parallel / WS parallel 全部通过；以 `cli-python-*` 报告为修正证据，保留 `cli-wave-codex_native_parallel_cli_report.json` 的 `passed:false` 历史，不覆盖抹除。
- `.local/gateway-audit-20260924/go-cli-compression/tests.jsonl`：network-none 真实 CLI 四组对照与 gateway chain 通过；不是桌面真实 OAuth 账户。
- 本次 `go-cli-contract` 运行：actual CLI 两场景 PASS（2.28s）、race / vet 通过；正常场景两次请求/两个真实工具，异常场景一次请求/CLI 失败。使用 `CODEX_NATIVE_CONTRACT_CLI_REPORT` 输出逐场景断言与 raw SSE SHA-256。
- `.local/gateway-audit-20260924/cli-reports/cli-python-codex_native_edit_cli_report.json`：7 请求、5 真实工具、shell 退出 `[7,0,0]`、磁盘最终 SHA-256 与 resume 检查通过。
- `cli-wave-codex_native_fallback_cli_report.json`：严格请求顺序 WS rejected → HTTP → HTTP → WS rejected → HTTP，拒绝 socket EOF 2 次、竞争账号控制和绑定保持均通过。
- `cli-wave-codex_native_compact_cli_report.json`：真实 CLI V2 路由，opaque 为 fixture 字符串，未解密/验证真实 compaction ciphertext。
- `cli-compression-codex_native_compression_chain_report.json`：经过 native helper，比较语义 digest，并明确该场景外部两跳为 loopback HTTP，不能另算 TLS 证据。
- `cli-http-hello-codex_native_helper_report.json`、`cli-ws-tls-final-codex_native_ws_tls_report.json`：本次报告读取到 passed=true；TLS 指纹字段和连接复用细节由 17 专项独立复核。本报告不将握手捕获升级为生产端点可用性证明。

测试启动输出中的 `WebSocket Go (native WebSocket not enabled)` 来自 helper 初始化阶段；部分实际 WS fixture 随后设置 `p.NativeWebSocket=true`。判断最终链路应检查该赋值、fallback guard、专属 native adapter断言及最终报告，而非只看启动字符串。

## 尚未证明的范围与具体后续建议

1. 对真正无效 `response.completed.response` 的“CLI拒绝、网关诊断仍ok”分歧，可在原生 CLI 观测分支增加 ResponseCompleted schema 诊断；保持兼容 JSON 使用方策略明确、SSE 字节透明。当前未擅改生产处理。
2. 若要声明 legacy compact adapter 等同 CLI，必须实现或明确无法从 wire 获得的 envelope provenance、agent message 规则及 image/token budget；增加“超长最新消息 + 少量旧消息”的具体对照，不能只用短文本 fixture。
3. Guardian 实际执行、namespaced custom tool、CLI 自身网络错误重试/401 后跨身份恢复、真实账号模型、生产 proxy/cert-store 全矩阵，尚无本专项的实际 CLI 证据。

本专项新增用例针对已改终态/聚合边界产生独立反例与实际执行证据，不把已有测试名称或通过计数作为一致性的替代。
