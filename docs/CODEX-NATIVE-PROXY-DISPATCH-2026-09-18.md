# 原生请求发送前代理复检

本轮修改 Go 原生 HTTP 适配器的代理配置校验，不修改 Rust helper、账号选择或请求重试策略。所有测试使用本地合成数据，不使用真实账号、不部署。

## 已复现问题

原入口 guard 只在 `RoundTrip` 开始时检查代理。随后读取 body、等待共享 helper 冷启动可能耗时；在这段时间修改代理密码、停用或删除代理后，原请求仍可能按旧快照发送。

三个受控冷启动用例在修复前均实际失败：本应拒绝的 password/disabled/deleted 场景都返回成功响应。unchanged 对照正常，证明不是启动夹具自身失效。

## 修复范围

`gateway_native_factory.go` 将原 guard 的检查方法传给原生 transport，仍由同一 store/ID/fingerprint 作为校验来源。原入口检查保留，新增两处复检：helper acquire 完成后、IPC 连接建立但尚未写请求头时。

配置缺失或指纹变化时返回既有 `errGatewayProxyUnavailable`，不替换该请求的代理、不自动重放 body，也不回退直连或 Go transport。前一检查失败直接释放 active 槽位；后一检查失败通过现有 Body.Close/once 清理 IPC 和槽位。请求 body 保持单次关闭。后续独立请求可以重新选择当前配置。

未建立“管理更新与完整网络写入”的原子事务：最后一次检查之后仍可能发生并发更新。此改动堵住已经复现的 body/helper/IPC 等待窗口，不能宣称对任意时刻修改具有绝对线性化保证。已发出的流不因此被重放或强制迁移；旧 helper 的回收仍遵守既有退休和 idle 策略。

## 本地验证

两个新增单元测试连续三轮通过，覆盖密码更新、停用、删除、未变对照、后续新请求，以及 acquire 后第一次复检通过、随后变更、写 IPC 头前第二次复检拒绝。请求 body 关闭一次，active 槽位归零，既有 request pin/guard 回归通过。

实际封存 Rust helper 的 `TestGatewayNativeActualProxyRotationBeforeDispatch` 使用两个本地 HTTP 代理和直连捕获目标。旧请求冷启动等待时同时旋转代理地址和用户名/密码，检查结果为旧代理请求/连接 0、新代理请求/连接 0、直连请求 0、Go fallback 0。再发新的独立请求，只有新代理收到一次连接和请求，absolute URI、body、新认证头与响应均符合预期。关闭后进程池为空。

上述代理专项为明文 loopback HTTP 代理，不是新增 TLS、SOCKS 或生产出口验证；TLS/其他代理路径由同套既有测试单独覆盖。本轮也不据此宣称生产成功率或缓存率的量化收益。

## 固定来源

- Go 源码：`278e0051354026814af8b8e67756b860756e26e85e8610f5f9c3b1074e0827e6`。
- 快照：`.local/source-20260918/gateway-delta-20260918t084948-9b6ef5/source`。
- 明确基线：已验 `834800a8...`，只叠加两个生产文件和两个新增测试文件，不卷入其他任务在途被动采集/管理改动。
- Rust helper：`6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`，未重建。

同一冻结源码的原生联调为 27 项 native 顶层测试加 1 项 Go CLI 顶层测试（3 子场景），共 28 项全部通过、0 跳过，六份必需场景报告存在且通过，临时容器已清理。新增真实代理派发用例耗时 0.10 秒，仅为本地样本。报告 `.local/native-transport-build/gateway-compare-20260918T085005-1e041a/result.json`。

全库测试、`go vet` 和 Windows/Linux 构建通过。同一源码本地 Linux PG/Redis `-race`：317 项顶层测试、774 个事件全部通过，0 失败/跳过/缺失，169.771 秒，临时资源清理完成。报告 `.local/source-20260918/local-linux-race-20260918t085236-3c92c3/report.json`。Linux 构建 SHA256 为 `798ba3b21f389689d404ce84a847655dad360323ad5e79e9389caadfba795b3f`。

## 复跑

```powershell
python tools/native_transport_compare.py --native-run 20260918T054243Z-b43907cb --source-dir .local/source-20260918/gateway-delta-20260918t084948-9b6ef5/source --suite gateway --with-go-cli
```

真实账号测试保持取消。本轮没有改动生产服务、美国出口配置或其他任务正在编辑的共享文件。
