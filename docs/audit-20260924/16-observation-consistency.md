# 专项 16：观测、SSE、usage、终止状态一致性

日期：2026-09-24。只读对照 `.local/codex-source-0.155.0`；未操作生产环境。

## 已修复

1. `stats.go` 的 SSE 观察器使用共享 `stripInitialSSEBOM` / `validSSELine`。支持跨读分片的首 BOM、UTF-8、CR/LF/CRLF、多行 data 和裸 data 字段；非首 BOM 不会被误剥离。CR 等待后续字节，事件分发时机与专项 13 的 idle 追踪器一致；EOF 处理保留已有 CR 行终结兼容语义。
2. 非法 UTF-8（包括注释/未知字段和 EOF 不完整字符）及观察器容量上限只终止计量观察：`finished=true`、`terminal=false`。`writeStreamPipe` 继续完整透传后续原始字节，避免本地观察失败截断上游流。未出现空行的 EOF 不记成功。
3. `response.failed`、`response.incomplete`、嵌套 failed/incomplete/cancelled 不再被矛盾的 `status:completed` 覆盖。统一失败优先规则供 HTTP SSE、HTTP JSON 聚合、WS 和直接响应计量使用；错误仍独立于 HTTP 200/101。
4. usage 优先依据原生字段的存在性，明确零不会回退为 prompt/completion 兼容字段；原生 `input_tokens_details.cached_tokens` 优先于顶层兼容 cached 字段。负数、非整数、字符串/null、字段或 input+output 溢出均不进入 token 累计。
5. `usage:null` 不再被当成有效零；非法/缺失 nested response 不回退顶层 usage，防止不同对象串入。兼容 schema 记录 `chat_completions_usage`，原生记录 `responses_usage`；有对象但核心计数非法时 `usage_known=false`。
6. Response ID 冲突不再覆盖已有 ID、model、headers、TTFT 或 usage；顶层 response_id 与嵌套 id 矛盾的观察事件被忽略，终止对象换 ID 则记录固定错误。WS 的现有 lane/FIFO/历史 ID 保护保持原有独立计量。
7. 受限字符串元数据写入前使用已有凭证脱敏规则，覆盖 User-Agent、request/response ID、模型、tier、reasoning、incomplete reason 和 attempt 模型，保持 UTF-8 边界及长度上限。正文、工具参数、输出和未知 headers 不进入日志。
8. CLI `response_model()` 支持的 `response.headers` 优先于事件顶层 `headers` 已补齐；可选 headers 类型损坏不会掩盖合法输出/TTFT。事件顶层 model 或非法 UTF-8 不再冒充 attempt 返回模型。
9. `gatewayResponseWriter` 捕获 Write、short write、FlushError 真实失败，记录 aborted/client/response_write；保留已有上游明确 error 及 terminal/usage，不重写 HTTPCode。`http.ErrNotSupported` 不作为连接故障。保留专项 14 的 101、informational status、invalid status 和 Unwrap 行为。
10. HTTP JSON/compact/models 下游写失败不再误记 ok；attempt 的实际上游 200 与最终客户端 aborted 保持独立。已 aborted 的投递状态不会被后到的成功终止观测重置。
11. `forwardModels` 在提交 200 前使用 8 MiB+1 有界读取；读取失败或超限返回固定诊断的 502，避免成功返回半份模型目录。304 和空目录响应保持合法。

## 对照源码及保留差异

| 事项 | CLI 0.155.0 证据 | 网关结果 |
| --- | --- | --- |
| incomplete 是失败 | `codex-rs/codex-api/src/sse/responses.rs:472` | HTTP/WS/计量一致，即便 nested status 为 completed |
| completed usage 原生字段 | 同文件 `ResponseCompletedUsage` / `From`，约 127–169 行 | 原生字段及明确零优先；兼容 prompt/completion 仍支持并标明来源 |
| 完成前 EOF 是流错误 | 同文件 `process_sse`，约 580–620 行 | 未终止事件时 aborted；JSON 语法不合法事件与 CLI 一样可跳过等待后续事件 |
| event idle 是完整事件 | 同文件 `timeout(idle_timeout, stream.next())`，约 583 行 | 与专项 13 O(1) tracker 逐字节联检，注释/只有 id 不续时，data 空值可分发 |
| nested model header 优先 | 同文件 `response_model()` 约 200 行及约 1937/1953 行测试 | 新增 nested headers 优先及坏 headers 不遮盖输出 |
| WS warmup | `codex-rs/core/src/client.rs:17` 描述 `response.create` + `generate=false` | 被动标记 warmup；返回的实际 usage 仍计量；不虚构 TTFT、不消费其他 lane |
| 网关兼容扩展 | CLI completed 结构要求 id，usage 要求 input/output/total；不将 response.done 作为官方完成分支 | 保留网关 response.done、无 id、缺部分 usage 字段兼容性；不宣称为严格 CLI schema 校验器 |
| 非法 token | CLI 使用 i64 反序列化，并未统一拒绝负值 | 网关计量层拒绝负数与单条总量溢出以保护统计；透传内容不修改 |
| incomplete JSON 输出 | CLI 交付错误；网关支持 stream=false 聚合扩展 | 保留既有返回原始 incomplete JSON 的 HTTP 行为，但最终计量为 error |

`usage_known=true` 对空 usage 对象的既有“上游明确提供对象”语义保留；它不代表所有计数均出现。optional details（reasoning/image/cache-write）继续逐字段区分未知与明确零。

## 验证

新增 `gateway_observation_consistency_audit_test.go`，15 个测试函数、超过 80 个子案例/逐字节组合，覆盖数字/schema、BOM/UTF-8/CRLF/EOF、invalid/limit 不截断透传、HTTP 与 WS 矛盾状态、idle dispatch、ID 污染、凭证脱敏、Write/Flush/short write、models 读错及超限、warmup/lane/attempt 独立性。

复跑命令：

```powershell
go test ./... -run 'Test(AuditObservation|SSEScanner|UsageFromResponse|GatewayObservation|GatewayWarmup|GatewayAttempt|GatewayResponseWriter|AuditNonStreamingIncomplete|GatewayWSResponseIdentity|GatewayAuditSSE|CLI)' -count=1
```

本机专项回归通过；根任务统一执行 Linux 全量/race。

## 范围边界

- 单条 token 字段和 input+output 的 int64 溢出已防护；跨大量请求的聚合桶或 PostgreSQL bigint 累计极限不在本次窄改范围，已向根任务交接。
- 元数据脱敏沿用明确凭证语法规则，不把任意未知标识符猜作秘密；WS 真实响应归属由 gatewayWSTurns 执行，本次只补观测防污染。
- models 改为最多 8 MiB 的有界缓冲，以在发送成功头之前识别截断/超限；SSE 正常路径仍边读边传。
