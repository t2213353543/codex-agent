# 官方 CLI 原生 WS 回退

本轮仅新增本地测试和证据说明，不修改生产 Go/Rust 实现。使用官方 CLI 0.155.0、
封存原生 helper 和 `network none` 容器中的合成 TLS 上游；没有真实账号、模型调用、
桌面认证读取或生产部署。

## 源码行为

官方 `codex-rs/core/src/client.rs:1730` 对目标 WS 握手的明确 HTTP 426 返回
`FallbackToHttp`。prewarm 分支在 `:1944` 调用 `try_switch_fallback_transport`；
普通流分支在 `:1993` 切换后调用 HTTP Responses。`:562` 的 fallback 状态属于
当前 ModelClient 会话，并清除缓存 WS 状态，`:926` 随后禁用该会话的 WS 尝试。

这是客户端对协议选择的明确处理，不是“任意握手失败都重试 HTTP”。401 有单独
认证恢复分支；其它错误和耗尽重试预算的行为不能从此 426 用例推导。新进程 resume
建立新的客户端状态，本次实际再次尝试 WS 后收到 426，再继续 HTTP。

## 实际路径

网关同时启用 native HTTP 和 native WS。上游对 TLS HTTP/1.1 Upgrade GET 返回
426，附带合成 X-Codex-Turn-State 和 X-Request-Id；测试在网关回客户端的响应边界
核对状态和这两个头。没有创建上游 WS 会话，也没有伪造成功的 101。

实际顺序为：

1. 首 CLI 进程的 WS 握手被拒绝。
2. CLI 主动发 HTTP Responses，经同一网关的 Rust UDS/native HTTP 路径和 TLS/H2。
3. 合成响应请求实际 shell 工具，CLI 执行并按原 call_id 回传，第二次 HTTP 完成。
4. 新进程 resume 同一线程，再收到一次 WS 426。
5. 第三次 HTTP 带完整历史完成续接，不重新请求工具执行。

两次拒绝 GET 的请求正文均为 0 字节；服务端在拒绝响应后观察到 EOF，期间没有
任何额外数据字节。这里证明的是这两条受控连接没有 WS 数据发送，不是系统级抓包
或所有失败条件下的 exactly-once 执行保证。

## 账号与状态控制

首次 WS 的账号绑定完成后，夹具通过实际 Store API 加入优先级更高的第二个合成
账号。无绑定的选择控制确实会选它，而后续 HTTP 和 resume 仍使用首次账号的
凭据及 workspace，session/thread 和 cache key 均保持稳定。这比单账号环境中的
“没有换账号”断言更强，但不扩展为所有代际变更或生产路由状态的保证。

仅发出 1 个工具调用，观察到其正确退出码、call_id 和输出；后续完整历史没有重复
或改写该结果。native 两种模式始终启用，Go 上游 fallback trap 为 0。因此回退是
CLI 从 WS 切到 HTTP，不是网关从 Rust 切到 Go。3 条 HTTP usage 记录与准入释放
符合预期，合成 token 总量为 input=90/output=3；不是模型成本或缓存收益测量。

## 最终证据

- Go 候选 SHA256：`834800a86730e17c663763f9ffb3a489d4254d4a5d6ea6a6d73b1624652378b6`。
- 测试文件 SHA256：`fb00baac88c6a5539df2f1a176fed06f8da4aad1fb21e0c3f1eab3b7a4f30db3`。
- CLI SHA256：`660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`。
- helper SHA256：`6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`。
- 回退报告：`.local/native-transport-build/gateway-compare-20260918T083336-817149/gateway-fallback-cli.json`。
- 回退报告 SHA256：`51fb6be720bbae2debfe0ce7fbff61e4833dd3d8aefd1bfb64e5c3fbf4f55687`。
- 同目录 result.json SHA256：`989ce0fedf951fef7a497ceb89964569c080a1da2294db923906da85c49c3379`。

20 项回退断言全部为真，用例耗时 10.89 秒，仅为本地样本。最终配对共有 26 个
native 顶层测试和 1 个 Go CLI 兼容性顶层测试，共 27 个顶层全部通过，零跳过、
无 OOM、容器已清理，验证前后源 hash 不变。Go CLI 兼容性顶层含 3 个子场景，
不能另算为 native 顶层测试。

最终依据是从已验证 d690 基线只加入本轮两项测试得到的 explicit 候选。先前
fee45 混入其它任务在途变更，其整库失败不能写成最终成功；它仅保留历史观察，
本说明不借用其通过结果。
