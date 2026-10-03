# 错误终态与请求校验独立检查 — 2026-09-28

检查与修复验证时间：2026-09-28 12:16–12:23 UTC。环境：Windows amd64，Go 1.26.7。

本次仅检查共享工作区源码和本地合成测试；未访问真实上游、未部署，不能由本报告推断线上状态。阅读了 `CACHE-AND-STREAM-RELIABILITY-20260928.md`、`BPS-INLINE-IMAGE-422-20260928.md`、`BPS-MODEL-COMPATIBILITY-20260928.md`。

## 已本地修复：非流式 JSON 丢失失败详情

优先级：P2。位置：`internal/server/gateway_bps.go` 中 `bps.Stream` 返回错误后的 `!prepared.ClientStream` 分支。

上游以 HTTP 200 SSE 返回合法失败终态：

```json
{"type":"response.failed","response":{"id":"collab_failed","status":"failed","error":{"code":"invalid_encrypted_content","message":"synthetic ciphertext fixture rejected","param":"input[0].encrypted_content"}}}
```

同一错误在 `stream:true` 下保留原始 `code`、`message`、`param`，但 `stream:false` 返回：

```json
{"error":{"message":"bps response terminated with response.failed (status failed) (code=invalid_encrypted_content)","type":"proxy_error"}}
```

HTTP 状态为 502。结构化 `error.code`、`error.param` 和实际错误消息丢失；识别出的 code 仅出现在摘要字符串中。这会让 JSON 客户端无法按机器码处理错误或显示上游给出的具体字段。本地请求日志可继续使用不包含任意上游文本的摘要，但客户端 JSON 应保留经现有公共错误处理后的错误对象。该问题与上游 HTTP 422 直接返回的路径不同；`bpsWriteUpstreamError` 对非 2xx 响应采用另一套转发流程。

复现文件：`internal/server/collab_error_boundary_20260928_test.go`。经协调，该文件只覆盖此 JSON/SSE 对照，未加入其他会话已认领的 WebSocket 终态枚举测试。每个子测试只调用一次 loopback 上游，凭据来自合成 fixture。

```powershell
go test ./internal/server -run '^TestCollabErrorBoundary20260928JSONFailureDetails$' -count=1 -v
```

12:16 UTC 及修复前留档结果：退出码 1；`sse` PASS，`json` FAIL。失败断言同时检查原始详情和结构化 code/message/param，避免 code 只出现在摘要字符串时误判为保留。

经主会话认领后，新增 `internal/server/gateway_bps_terminal_error.go`，并仅替换 `gateway_bps.go` 非流式错误分支的一行调用。辅助函数按合法 `event.error`、`response.error` 对象的顺序选择，采用 `json.RawMessage` 和现有 `publicerrors.JSON`，JSON 响应保留实际 code/message/param 与未知错误扩展字段。HTTP 状态仍为 502，原记录逻辑继续写入 `TerminalError.Error()` 摘要。没有合法错误对象的 failed/incomplete/cancelled，以及非法流，仍返回 generic 502。未添加 HTTP 状态映射或重试逻辑。

当前复现命令及扩展用例全部通过：

```powershell
go test ./internal/server -run '^TestCollabErrorBoundary20260928' -count=1 -v
```

结果：PASS，12 个子测试。覆盖原 SSE/JSON 对比、flat error 转换、event.error 优先级、非对象回退、字段缺省/空对象、`9007199254740993` 与 `1.2500e+02` 精确保留、既有品牌错误处理、失败/不完整/取消/非法流回退。端到端断言一次上游调用、JSON Content-Type、无遗留 SSE Content-Length，以及请求日志不含上游任意 message/param。

原成功路径与错误回归：

```powershell
go test ./internal/server -run '^(TestCollabErrorBoundary20260928JSONFailureDetails|TestCollabErrorBoundary20260928JSONTerminalCases|TestBPSGatewayHTTPProtocolAndNamespacePreserveRawNumbers|TestBPSGatewayJSONHistoryIsKeyAndOwnerBound|TestBPSGatewayLargeJSONResponseRetainsExactContent|TestBPSTerminalUsagePersistsForStreamAndJSON|TestBPSGatewayStreamOptionsKeepsTerminalUsageWithoutForwarding|TestBPSGatewayInterruptedStreamNeverCompletesOrRetries|TestBPSReviewLegacyCompactFailureIsRejected)$' -count=1 -v
```

结果：PASS，`internal/server` 0.771s。包括成功 JSON 历史隔离、大响应原文、SSE 工具命名空间、usage 记录，以及断流不伪装成功。

证据目录：`.local/collab-a-20260928/error/`，包含 `before-test.txt`、`after-test.txt`、`regression-test.txt` 和 `before-hashes.json`、`after-hashes.json`、`verified-hashes.json`。回归测试前后受检文件哈希一致。

## 已通过的限定检查

```powershell
go test ./internal/bps -run '^(TestStreamTerminalErrorKeepsMachineCodeWithoutLoggingPayload|TestStreamErrorBecomesFailedTerminalWithoutInventingResponseMetadata|TestStreamTruncationAndMalformedTerminalsNeverSucceed|TestStreamForwardsActualFailedAndCancelledTerminals|TestPrepareTypedUnsupportedParameters|TestCompactTriggerRetainsAllContext)$' -count=1 -v
```

结果：PASS，`internal/bps` 0.024s。覆盖缺少终态、损坏 JSON、错误成功状态、缺少/无效 output、失败与取消事件、statusless error 转 failed、日志机器码白名单、明确拒绝不支持的参数及 compaction 保留上下文。

```powershell
go test ./internal/server -run '^(TestBPSGatewayUnsupportedStreamOptionsAreTypedAndNeverSent|TestBPSGatewayStreamOptionsKeepsTerminalUsageWithoutForwarding|TestBPSCompactionDoesNotAcquireGenerationOnlyCacheFields|TestBPSModelRoutingAuthorizationChangedDuringCatalogStopsGeneration|TestBPSAttachmentsFailureNoRetryOrPartialRewrite|TestBPSAttachmentsOwnerChangeAndMalformedImage|TestBPSReviewLegacyCompactFailureIsRejected)$' -count=1 -v
```

结果：PASS，`internal/server` 0.241s。覆盖：

- 不支持的 stream_options 在本地以字段级错误拒绝，合法 include_usage 不转发且终态 usage 保留。
- 普通触发及显式 compact 路径不重新添加 generation-only cache 字段。
- catalog 查询期间撤权后禁止生成。
- 附件上传 401/403/422/502 不重试、不部分改写请求；账户归属及损坏图片检查。
- 失败 compact 响应不被接受为成功。

## 协作与边界

- 只读检查发现 transformer 接受 `response.done`、`basispoints.response.cancelled`，而 WebSocket writer 的 terminal 枚举不一致；该问题由独立会话 `01a0e7ed-44f0` 认领，本检查未重复实现或测试。其修复与验收以该会话证据为准。
- 本次拥有的变更：本报告、唯一复现测试、新增 `gateway_bps_terminal_error.go`，以及 `gateway_bps.go` 非流式错误分支一行调用；其他会话变更未覆盖。
- 未运行完整回归、真实 Codex CLI、Redis/PostgreSQL、race、线上接口或部署检查。通过这些限定测试不能推出所有历史 422 已消除。
- 工作区允许多个会话同时修改，下面哈希是 12:16 UTC 读取到的源码快照，不是发布制品哈希，也不等同于一次完整构建的来源证明。

## SHA256 修复前源码快照

| 文件 | SHA256 |
| --- | --- |
| `internal/server/gateway_bps.go` | `E3F7872B813D4D71B169B2BF0663CECFD2C584270C1DEAF4D0D7BD353684C294` |
| `internal/bps/stream.go` | `15C7879231E200AA61CCA3B37ECEBBA120F67E7B6153A14239E85B77362C5436` |
| `internal/bps/request.go` | `0DC20648396C75C52A600755806C80F0DFAD7BAEE28FFFC380E9D38206CF1FF5` |
| `internal/server/collab_error_boundary_20260928_test.go` | `49CC5B0A9F47E1E7AA0D33D90D69495987CC7BD951FD784D2C511B455506C8F1` |

## SHA256 修复后源码快照

12:21 UTC 留档；回归测试结束后再次校验，以下文件及 `internal/bps/stream.go`、`internal/bps/request.go` 均未变化。

| 文件 | SHA256 |
| --- | --- |
| `internal/server/gateway_bps.go` | `63EAEF8C4B3FA1DDA2A4E6045CDCCE20F76F5C608E4B0434997E5A0E782EA498` |
| `internal/server/gateway_bps_terminal_error.go` | `61C0401A46A7D4062D195E50288612182EE08A980A4FD201E5C9F1AE5EAEC436` |
| `internal/server/collab_error_boundary_20260928_test.go` | `BCEAEEC1318E27ACE710495D18169A50E2715E95D7FA4BF9EBA7788281215E1D` |
