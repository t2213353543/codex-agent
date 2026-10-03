# 原生 WS 到 HTTP 回退验证

本轮仅新增本地协议/CLI 测试和验收工具，不修改生产转发逻辑或 Rust release，不使用真实账号，不部署。

## 实际 CLI 流程

官方 CLI 0.155.0 在 WebSocket 建连收到 HTTP 426 时有显式 HTTP 回退分支。此回退由 CLI 选择，网关的 WebSocket 和 HTTP 上游传输始终由封存 Rust helper 执行，不是静默改用 Go，也不是在 WS 消息被接受后重放请求。

两个 CLI 进程使用全新 HOME/CODEX_HOME 与合成凭据，执行初始工具任务和同线程 resume。实际顺序为：WS 426、HTTP 工具请求、HTTP 工具结果续接、新进程 WS 426、HTTP resume。

20 项断言全部通过：

- 2 次被拒 WS 握手、3 次 Responses POST、1 个真实 shell 工具结果。
- 被拒连接 request body 为 0 字节，响应头后读取到 EOF，未收到 WS 数据。
- CLI 收到真实 426 以及允许透传的 turn-state/request ID 头；未把 WS previous-response/generate 控制残留到 HTTP envelope。
- 首次会话绑定后新增高优先级合成账户，对照验证无绑定选择会选中它；回退、工具续接及 resume 仍保留原账户、session/thread 和缓存 key。
- 初始工具输出在 resume 历史中保留且未重复，Go 上游 fallback trap 为 0。

这里只证明本次显式 426 路径。401、其它建连错误、已接受 WS 后的错误有独立规则，不能推广为所有失败都能自动回退或 exactly-once。稳定 cache key 也不等于真实缓存命中率提升。

## 拒绝状态边界

普通 Go 与实际 Rust/TLS 两个测试分支分别检查目标服务返回 200、426、407：有 session 头时先连接上游，200 非 101 映射为 503，不伪造升级；426/407 保留状态和允许的响应元数据。无 session 头、只能从 body 选路时，下游先接受 101，再发送携带 stream_id 的错误帧；不能把这种错误帧当成升级前 HTTP 426。

上述每种拒绝只发生一次 GET，不刷新 OAuth、不发送模型 POST。407 是目标 HTTP 响应，不是本轮测试 HTTP CONNECT 代理拒绝的证据。

## 并行协作边界

最初宽文件匹配冻结的 fee45 候选混入另一个任务的在途被动观测代码，但管理基线尚未包含对应 query，`TestManagedPassiveClientIDQuery` 全库失败并重复复现。即使该候选原生联调通过，也不能称整体验收通过。原失败快照及 `build-verification.json` 保留，未修改或回退其他任务的共享文件。

最终改用已验证 d690 网关快照，只叠加本轮 `gateway_native_cli_fallback_test.go` 和 `gateway_native_ws_rejection_test.go`。新 `gateway_freeze_delta.py` 校验基线完整哈希与通过记录、显式文件名单，拒绝重复、路径及通配符，并再次确认复制后的源未变。两项工具单测通过。它不将另一个任务在途源码从共享目录删除，仅不纳入本轮独立快照。

## 固定验收

- Go SHA256：`834800a86730e17c663763f9ffb3a489d4254d4a5d6ea6a6d73b1624652378b6`。
- 目录：`.local/source-20260918/gateway-delta-20260918t083313-9480c0/source`。
- 上一已验网关基线：`d690b33f...`，其管理基线为无计费 bdfbf92；不是最新管理 UI 整包。
- Linux 二进制：`406f089d76276d2b6cb50cc5fa40161189f4b46ed2c6ef8fd47fd4150bfdd579`，生产源码与二进制均和 d690 相同。
- Rust helper：`6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`，未重建。
- 26 项原生顶层测试 + 1 项 Go CLI 顶层测试（3 子场景）通过，共 27 项，0 跳过，容器已清理。六份必需场景报告全部存在且通过。
- 联调报告：`.local/native-transport-build/gateway-compare-20260918T083336-817149/result.json`，回退详情在同目录 `gateway-fallback-cli.json`。

同一明确增量快照全库测试、`go vet` 和双平台构建通过。本地 Linux PG/Redis `-race`：315 项顶层测试、768 个事件通过，0 失败/跳过/缺失，170.15 秒，临时资源清理完成。报告 `.local/source-20260918/local-linux-race-20260918t083612-19bc46/report.json`。验收报告门槛的 5 项 Python 单测及精确增量冻结工具的 2 项单测均通过。

## 复跑

```powershell
python tools/native_transport_compare.py --native-run 20260918T054243Z-b43907cb --source-dir .local/source-20260918/gateway-delta-20260918t083313-9480c0/source --suite gateway --with-go-cli
```

所有上游回复、用量、竞争账户均为合成数据，不证明真实账户、生产缓存收益、美国出口或完整设备指纹。
