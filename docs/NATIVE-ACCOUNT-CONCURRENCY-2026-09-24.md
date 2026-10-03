# 原生传输跟随账号并发设置

2026-09-24 的真实压测已经证明：账号 `concurrency=80`，但 Go route 活跃计数和 Rust helper semaphore 都固定为 32，导致同一 route 每轮 80 请求只有 32 条进入上游，其余 48 条在网关返回 capacity。另有固定 16 活跃 route，以及 SOCKS 桥 32 连接的独立限制。

本次修复将请求并发统一交给已有账号准入逻辑。Go 不再为 route 设置第二个活跃请求上限，Rust 不再为 HTTP/WebSocket 设置 32 槽 semaphore；SOCKS CONNECT 与 HTTP 转发也不再限于 32 连接。账号 concurrency 的校验、饱和拒绝及请求释放逻辑保持有效。同一 route 被多个账号共用时，不会因它们共享出口而额外削减可用并发。

活跃 route 按需创建 helper；原来的 16 进程硬上限改成最多缓存 16 个空闲 helper。最后一个请求释放后，超出缓存预算的空闲 helper 立即退出；其余依然由空闲超时清理。共享启动、请求取消、代理退休、进程异常退出及服务关闭的回收路径保持原有语义。

ready 协议要求显式 `max_active: 0`，表示并发由账号准入管理。新版 Go 拒绝旧版 `max_active: 32` 或缺失该字段的 helper，并提示升级，避免只替换 Go 后仍然在 Rust 内排队。Go 主程序和 helper 必须作为一组更新；原有正文大小、帧大小、IPC 鉴权和超时约束未放宽。

已在 Windows 通过 Go native 全套可执行回归和 `go vet ./...`。新增用例保持 96 条 HTTP 同时活动并取消回收；创建 20 个同时活动 route，释放后只保留指定的 3 个空闲 helper；SOCKS 两轮各 96 连接并验证关闭无残留。缺失/旧并发握手均被启动检查拒绝。

Linux 真实 helper 集成用例使用合成上游，验证 96 请求同时到达上游、第 97 请求仍被 `concurrency=96` 拒绝，以及 96 WebSocket 同时握手、消息回显和释放。执行命令如下；须设置为本次构建的新 helper，Windows 自动跳过这些真实 Rust 用例。

```sh
CODEX_NATIVE_HTTP_TEST_BIN=/absolute/path/to/codex-native-transport go test ./... -run 'TestGatewayNativeActual(AccountConcurrency96|WebSocketConcurrency96)' -count=1 -timeout=120s
```

Rust loopback 的取消/立即补充探针也提升至默认 96 活跃流、10 次补充，可用 `--concurrency` 指定其他负载。测试上游监听队列扩为 256，避免合成服务器默认队列 5 造成非目标限流。以上源码修复与合成测试本身不代表真实模型 80 并发已经通过；真实模型结果以发布后的独立压测为准。
