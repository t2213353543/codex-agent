# HTTP 取消归因及下游截断处理

修复已部署 Go `4190b7ad…`，native helper 保持 `bf920a0b…`。北京时间2026-09-26 03:32:09线上验收通过：HTTP模型目录200、9个模型，WS握手101并正常释放连接；未发送模型turn。03:34再次读取实际进程，二进制哈希与构建一致。

旧网关在上游发送待定失败事件之后遇到调用者取消，会把 JSON 请求误记为上游响应错误，SSE 请求也残留错误归因。本次修复只在读取错误实际匹配调用者 context、响应尚未完成且此前没有明确解析错误时，将记录归为 `aborted/client/canceled` 或 `aborted/client/timeout`。真正的上游截断、idle/native总超时以及已经提交的结果不会被随后取消覆盖。

未终结的失败事件原先没有提交用量或429冷却，这不是本次发现的计费错误。回归测试继续保证这一点，并验证同账号、同会话后续请求使用自己的响应ID、用量和缓存元数据，凭据状态不被取消污染。

独立真实 TCP 审查曾拦住中间候选：只修正记录后直接 return，会使半关闭发送方向但仍读取响应的客户端收到空200，或在半截SSE后看到正常EOF。最终候选在此分支使用 `http.ErrAbortHandler` 中断连接，避免把取消变成成功响应。原红例、旧基线对照和修复后12个真实TCP叶子测试均保留；该中间候选没有上线。

另一个红例证明解析错误之后的取消会覆盖已知上游错误。最终候选对已有 error 状态保留原诊断，不重新归因为客户端取消。

变更范围为 HTTP 三个错误挂点及一个取消归因函数；WS、TLS、身份、数据库结构和默认请求参数均不改。完整源码从已部署版本的388文件冻结包起步，最终390文件。最终 Linux race 3265项通过、27项明确跳过，Windows 2809项通过、349项明确跳过，两端vet通过。六个指定native取消/期限/截断场景、隔离PG/Redis实际运行通过；一个没有确定读屏障的native JSON手动取消用例明确跳过，同路径caller deadline另有实际覆盖。独立12个真实TCP叶子通过。全部构建证据绑定最终390文件源码 `8ca073c4…`；中间候选证据按superseded保留，没有用于发布。

[红例与修改范围](../.local/research-20260926/http-cancel-boundary/review.md) · [候选清单](../.local/research-20260926/http-cancel-boundary/candidate-manifest.json) · [独立发布脚本审核](../.local/research-20260926/http-cancel-boundary/review/release-script-review.json)

这项修复提高诊断准确性与异常连接处理，没有证明账号错答的原因。

发布只切换Go后端，环境文件顺序/内容、helper、官方CLI身份、前端、设置与账号元数据全部核对保持。临时验证key/group及文件已移除，账号原组保留，无数据库迁移。工作区三个文件在核对旧哈希后同步自最终测试源，没有覆盖其他修改。

[构建与测试](../.local/research-20260926/http-cancel-boundary/build-final/build-manifest.json) · [独立TCP审核](../.local/research-20260926/http-cancel-boundary/review/independent-tcp-review.json) · [发布回执](../.local/research-20260926/http-cancel-boundary/release/activation-result.json) · [HTTP/WS验收](../.local/research-20260926/http-cancel-boundary/release/runtime-smoke.json) · [清理回执](../.local/research-20260926/http-cancel-boundary/release/smoke-cleanup.json)
