# 上游失败终态与诊断合同

核对时间：2026-09-29 02:58（UTC+8）。本页修复已随 **0.4.18 / PID 2186899 / binary 133f242c…1e424e** 上线，release `65a408448e06ae8d…`、source `36ec9457ddada16de…`。02:56:28 的正式现场回执已验证两次生成、一次本地 top_p 拒绝及三次 CSV 对账；settings 27、账号配置、api_key 缓存域及 cooldown=false 保持。见[实际部署核验](../.local/collab-a-20260928/terminal-release-review/postdeploy-review.json)、[关键包审](../.local/collab-a-20260928/terminal-release-review/package-critical-review.md)及[源与 Go 核验](../.local/collab-a-20260928/terminal-release-review/REPORT-go.md)。

`error → response.failed` 归一化本就在 .17 中，不能算本轮新增修复。它为客户端提供失败终态，沿用已观察 response id，不凭空生成 model、usage 或 output。普通 JSON 客户收到 502 和原错误对象；SSE 客户通常已收到 HTTP 200，以唯一失败事件结束。失败事件不触发成功 `OnComplete`，不会保存该结果为可续接 history。HTTP 200 与应用层成功是不同事实。

| 名称 | 来源与用途 |
|---|---|
| `TerminalError.Type` | 终态事件名，例如 `response.failed`；不是原生错误分类。 |
| `TerminalError.Code` | 选中错误对象中的有限机器码。本版补齐既有 `usage_limit_reached`，不从 message 或 type 猜 code。 |
| `TerminalError.ErrorType` | 从同一错误对象独立读取原生 `type`，仅允许 `server_error`、`invalid_request_error`、`rate_limit_error`、`authentication_error`；未知、空、null、大小写变体与非字符串均省略。 |
| `RequestRecord.Error` | `Error()` 生成的诊断摘要，可追加 `(code=...)` 和 `(error_type=...)`。不写上游 message、完整 response 或内部 formatter 名称。 |
| `FinalUpstreamErrorCode` / attempt code | 网关既有事件分类得到的规范机器码，例如 `rateLimitExceeded`；不能把它与原生 `usage_limit_reached` 或 ErrorType 当作同一字段。 |

选择优先级保持为第一个合法对象：`event.error`，然后 `response.error`。已选外层对象的 code/type 即使未知或缺失，也不会从低优先级内层补值。非对象错误沿原有选择规则处理。顶层事件 `type:"error"` 不能成为 ErrorType。`Error()` 会再次检查有限类型；公开 payload 保持原生 `error.type` 和 `code:null`，不添加内部 `error_type` 协议字段。

例如原生 `code:null, type:"server_error"` 可以得到日志摘要：

```text
bps response terminated with response.failed (status failed) (error_type=server_error)
```

该诊断能区分今后同类事件，不能据此补写过去没有保存的错误类型或推断历史失败根因。`gateway_bps_live_test.go` 的诊断输出过滤器只追加同四枚举的可选后缀，既有无 type / code-only 摘要仍接受。

失败 envelope 的 root/response `message`、`detail` 字符串沿用 `publicerrors.Message`。结构化输出另有本请求专属 formatter：本版修复失败终态两层 `code/message/param/detail` 对它的遗漏，复用既有 scrubber，只替换精确内部名称。成功正文和 metadata、普通机器字段、其它扩展及精确数值保持；不对任意未知字段遍历改写。

这项遗漏已在 .17 源码复现：nested error 已清理，但 flat error 保留的顶层 message/param 及 response 自由诊断仍可带出内部名称。私有核心与真实网关均保留原始红测；组合修复后 **10 顶层 / 11 子项核心测试**及 **JSON/SSE 两项完整网关测试**通过。用量 input 100/output 10/read 80/create 7、一实际 POST、失败无 history、无输出 TTFT 的合同同时成立。见 [结构化交接](../.local/collab-a-20260928/terminal-error-consumer-review/STRUCTURED-HANDOFF.md)、[真实网关红绿](../.local/collab-a-20260928/terminal-error-consumer-review/GATEWAY-HANDOFF.md)。

普通 HTTP 的独立六子项前后对照还覆盖无 usage、已知零、已知非零和先前观察到的 id：unknown 不补零，失败不丢已收到的用量。见 [HTTP 消费者报告](../.local/collab-a-20260928/terminal-error-consumer-review/HTTP-REPORT.md)。ErrorType 的选择规则、公开响应及 live 过滤回归由[主任务原始候选证据](../.local/remaining-errors-release-20260929/terminal-error-type/README.md)证明。

最终同源完整 Go/vet、17 项真实 PG、主 race 及 9 组 live 均通过。新增五文件 13 顶层 / 58 含子项实际通过；其中两项 Independent 前缀的服务端测试未被主 race 选中，另做同源精确 race 补验。补验执行本身 exit0、两顶层八子项全部 PASS，但原汇总器测试键解析错误，原 report.status=fail 保留；离线核原日志的纠正回执及原件分别封入包，不把原报告改写为成功。部署后的普通 smoke 没有故意触发上述失败分支。

现有重试、已输出/WS-created 屏障和默认关闭冷却不因增加日志字段而修改。监控与管理端可以有不同的诊断分类，分类变化不等于新增重试或改变计量。read/create/unknown 仍各自保留；自然效果需要部署后的真实流量，不能拿合成失败或部署验收请求推算。
