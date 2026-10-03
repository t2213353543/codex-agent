# 共享原生启动取消隔离

本轮修复 Go 原生 helper 池的请求取消归属。Rust release 未修改，所有验证只用本机合成服务，不使用真实账号、不调用模型上游、不部署。

## 已复现问题

同一线路首次创建 helper 时，其启动 context 来自首个请求。第二个请求已经等待同一个 ready 结果后，首个请求取消会中止共享启动，使仍有效的第二个请求也失败。受控 command-factory barrier 和等待通知复现了该问题，不靠任意 sleep 推测时序。

## 修复

- 启动使用进程池拥有的有期限 context，当前配置默认 10 秒，不再继承创建者取消或请求元数据。
- 所有启动等待者在锁内登记并单次退出计数；各自取消立即返回自己的错误，不影响其他仍在等待的请求。
- 最后一个等待者在 helper 未 ready 时离开，才标记 abandoned 并取消启动。reservation 一直保留到清理完成，不突破进程数上限。
- 此后到达的请求先等待 abandoned reservation 的 `finished`，再新建 helper。此前尚未发送 IPC/上游请求，不是重放已发送 POST。
- 池关闭和路由退役仍可取消启动。ready 已完成时，最后一个等待者取消不会杀掉其他请求可能即将使用的健康 helper；无请求使用时由既有 idle 机制回收。
- 自己的 context 错误优先；共享启动期限到达对仍有效调用者返回 helper 不可用，不伪装成客户端超时。启动准备及 command 构造后再查 context，已全取消时不无谓启动 OS 子进程。

既有单进程活动数、总进程数、代理配置隔离和无隐式请求重放规则不变。10 秒是启动 context 的期限，不是对同步文件哈希、文件系统、OS 进程回收或测试自定义阻塞 factory 的严格墙钟保证。

## 覆盖

五项新增回归覆盖创建者取消、第二等待者取消、全部取消后新调用者、Close 保留 pending reservation，以及 ready/取消结果竞争。它们与旧并发创建和慢启动测试一起连续三轮通过。另有原启动失败回归验证共享 deadline 不变成仍有效请求的 deadline。详情见 [等待者回归记录](CODEX-NATIVE-STARTUP-WAITERS-2026-09-18.md)。

`TestGatewayNativeActualSharedStartupCreatorCancellation` 使用封存 Rust helper 和真实 loopback TLS/HTTP2：取消创建者，保留同一 startup，存活请求精确发送一次，启动次数 1、上游请求数 1、Go fallback 0。Close 后 helper 被正常回收，entries 和私有 IPC 目录为空，reaper 已退出。该用例使用受控本地 barrier，不模拟生产启动耗时分布。

## 固定来源

- Go 源码：`d690b33f22b6417aec5edae72597a7e99ef9c6ca9f9cd689cc77c3ec018baf0e`。
- 快照：`.local/source-20260918/gateway-overlay-20260918t080317-d156f9/source`。
- 管理基线仍为已验证无计费 bdfbf92，不是管理任务当前后续 UI 整包。
- Helper：`6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`。

同一固定 Go/helper 配对已有 24 项原生顶层测试和 1 项 Go CLI 顶层测试（3 个子场景）通过，共 25 项、0 跳过。文件编辑、V2 自动压缩、WS 多轮续接等既有场景一并复跑；五份要求的场景报告全部存在且通过，临时容器已清理。原始结果 `.local/native-transport-build/gateway-compare-20260918T080329-e2be13/result.json`，实际共享启动子项耗时 0.08 秒，仅为本地样本。

同一快照全库测试、`go vet`、Windows/Linux 构建通过。本地 Linux PG/Redis `-race` 为 314 项顶层测试、764 个事件全部通过，0 失败/跳过/缺失，179.856 秒，临时资源已清理；原始报告 `.local/source-20260918/local-linux-race-20260918t080546-d742eb/report.json`。Linux 构建 SHA256 为 `406f089d76276d2b6cb50cc5fa40161189f4b46ed2c6ef8fd47fd4150bfdd579`。

## 复跑

```powershell
python tools/native_transport_compare.py --native-run 20260918T054243Z-b43907cb --source-dir .local/source-20260918/gateway-overlay-20260918t080317-d156f9/source --suite gateway --with-go-cli
```

这证明受控并发启动中的取消隔离，不证明所有故障下的 exactly-once、生产成功率增幅或完整设备环境一致性。
