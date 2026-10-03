# 官方 Responses Lite 字段缺失语义修复（2026-09-25）

修复已包含在当前上线的组合版本 `a47774284a6be8416b9d0ed2a20b2fb8505f9064fbb9de38258ec534ae20136a` 中。北京时间 15:10 的组合发布通过 models 200、WS 101 和 health ready；本任务随后直接核对运行 SHA、源码归档中 HTTP 调用及 `proxy.go` 精确摘要，并确认组合 Linux 日志中的 35 个 Lite 测试终态全部通过。[组合上线核验证据](../.local/research-20260925/lite-instructions-validation/combined-release-proof.json)。

本任务的独立候选 `9e8272e5…e856359` 没有另行部署。旧等待任务 `codex-lite-release-idle-20260925` 在运行版本变化时正常停止，activation_attempts=0；没有覆盖并行发布。旧独立候选的测试与构建记录继续保留，不能与组合版本的全套计数混写。

真实 CLI 0.155.0 的 Responses Lite 正文没有顶层 `instructions`；旧网关会补入空字符串。修复仅在实际转发 Lite 标志精确为 `true` 时保留缺失状态，显式值、传统 API 默认、compact 和 WS 的既有行为保持。头判断与正式转发使用相同 scalar／canonical／Connection 规则。

最终快照 `2360108c9f3d94cf994193c2a4f1c1a424b6bad49c2712fe66eca25614698f4d` 仅在当前生产基底加入 `gateway.go` 一行调用修改与 `proxy.go` 的 Lite 准备函数，未混入共享工作区其他并行缓存功能。

- Windows：827 顶级通过、0 失败、148 跳过，vet 通过。
- Linux race／PG／Redis／native：951 顶级通过、0 失败、23 opt-in 跳过，无 DATA RACE，vet 通过；identity／zstd 两项真实 native Lite 用例已执行。
- 官方完整 CLI：7 项通过、0 跳过。

这修复了已观察到的协议差异，尚未证明空字段是答 29 的原因。[HTTP／WS 和自然会话对照](CODEX-QUALITY-HTTP-WS-2026-09-25.md)的十四个完整答案均为 29，另外两次 HTTP 429 没有计为错答。

[源码契约与红绿回归](quality-20260925/transport-pair-contract.md) · [最终构建清单](../.local/research-20260925/lite-instructions-validation/build-manifest.json) · [实际发布等待状态](../.local/research-20260925/lite-instructions-validation/deferred-activation.json)

```powershell
go -C .local/research-20260925/lite-instructions-validation/verified-build-source test -count=1 -run '^TestResearchLiteBody' .
```
