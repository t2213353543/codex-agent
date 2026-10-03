# HTTP/1.1 SSE 终态与连接复用

66 次 loopback 请求证实：当前终态及时返回行为与 HTTP/1.1 未消费完响应分帧共同导致部分 SSE 连接不能复用，并非连接池失效或 idle wrapper 独有问题。

| 上游行为 | 当前网关终态读取（每组 6 请求） | 读到 EOF 的诊断对照 |
| --- | --- | --- |
| Content-Length 完整响应并立即结束 | 1 新连接、5 复用；6 次已读到 EOF | 1 新连接、5 复用 |
| chunked 终态 flush 后立即结束 | 6 新连接、0 复用；未观察到 body EOF | 1 新连接、5 复用 |
| chunked 终态 flush 后延迟 100ms 结束 | 6 新连接、0 复用；P95 0.52ms；6 个上游 handler 及时取消 | 1 新连接、5 复用；P95 100.53ms |
| chunked 终态后保持打开 | 6 新连接、0 复用；P95 0.53ms；6 个 handler 及时取消 | 不执行无限等待对照 |

绕过 `idleStreamBody`、直接使用 Go response body 的终态读取对照得到相同复用结果。`httptrace.GotConn` 记录真实连接复用；服务端 ConnState 记录新连接和活跃连接，body wrapper 记录实际读到的 EOF。每组关闭后活跃连接与 handler 均为零，整个测试进程 goroutine 数为 2→2，未启动任何后台 drain。

Go `net/http` 的 HTTP/1.1 复用需要响应 body 消费到协议 EOF。完整终态 JSON 只是 SSE 应用层边界；它不等于 chunked 的末尾零长度块与 trailer 已被读取。尤其 `Flush` 会形成独立块，随后返回的 HTTP 分帧终结仍可能需要另一次 Read。

官方 CLI 0.154.0 的 `codex-rs/codex-api/src/sse/responses.rs:669-674` 在发出 `ResponseEvent::Completed` 后立即 return，离开并丢弃 SSE byte stream，没有等待 HTTP EOF。本实验只比对该源码语义，没有测量官方 Rust CLI 在这些场景下的连接池行为。

当前不改网关终态生命周期。为了复用而等待 EOF，会直接增加已完成请求延迟；无期限后台 drain 还可能占用连接与 goroutine。可行方向是保留现有终态取消语义，分别优化已完整读完的响应与配置一致的客户端池，并在实际支持 HTTP/2 时利用其流级取消能力。HTTP/2、本地 Rust helper 与生产出口需要各自独立证据，不能从本次 H1 对照推断。

复跑：

```powershell
$env:GATEWAY_H1_DIAGNOSTIC_REPORT = (Join-Path (Get-Location) '.local/h1-terminal-new.json')
go test -run '^TestGatewayH1TerminalConnectionDiagnostic$' -json -count=1 -timeout 40s .
```

结果：[脱敏 JSON](GATEWAY-H1-TERMINAL-DIAGNOSTIC-2026-09-18.json)。原始 Go JSON 输出为 `.local/h1-terminal-diagnostic.jsonl`。立即 chunked EOF 存在调度竞态，测试只报告实测复用率，不要求该场景永远为零；loopback 的低延迟数值也不代表真实网络收益。
