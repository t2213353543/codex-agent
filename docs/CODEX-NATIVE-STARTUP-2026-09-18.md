# 原生 helper 启动前校验

本轮仅本地源码与合成验证，不使用真实账号、不调用模型上游、不部署。继续采用官方 CLI 0.155.0 对应的已封存 Rust release，未修改 Rust 生产实现或重新构建 helper。

## 变更

- `configureGatewayNativeHTTP` 在发布 `NativeHTTP` 和 `NativeWebSocket` 前启动一次独立 helper。固定 origin 为 `http://127.0.0.1:1`、proxy 为 null；只写 stdin 启动配置，不建立 HTTP/WS 请求 IPC。
- 沿用 SHA256、协议、UDS 路径/权限、body/active 上限及生命周期检查；要求 `request_zstd_v1`，启用原生 WS 时再要求 `websocket_message_v1`。不按版本字符串伪造兼容性，也不静默回退到 Go。
- 成功与失败均关闭探测进程并等待回收。成功还要求退出码 0、临时目录已删除；异常退出、停止超时被 kill、缺少能力或错误 CA 均拒绝启动。失败时关闭未发布 pool 的 reaper，成功时不保留探测 route、连接池或进程。
- 修复 helper 已绑定 UDS、但尚未给出合法 ready 就失败时的残留：进程回收后，若尚无记录的 socket inode，只检查私有目录中的 socket 路径；已记录的 inode 不重新采样。对 ready 已记录的路径保留替换 socket，不删除普通文件；未记录 inode 时不宣称能识别同 UID 主动替换者。

校验仅用于工厂未发布 pool 的单线程阶段，不是可与 pool.Close 并发运行的管理接口。ready 阶段使用既有 10 秒期限，停止等待为 2 秒；文件哈希、文件系统和 OS 进程回收不因此获得严格整体墙钟上限。

## 固定产物

- Go 快照：`.local/source-20260918/gateway-overlay-20260918t065038-5660d3/source`。
- Go SHA256：`88ff8d9bfb21fe59b65998946aa5d17b94aed9c6483f88bc3dc2337a0a555cdb`。
- 管理基线为此前已验证的 `5a818844...`，叠加网关所有者文件，不等于管理任务当前在途工作树或最新管理 UI。
- Helper SHA256：`6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`。
- 官方 Linux CLI SHA256：`660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`。

## 验证边界

同一 Go 快照全库测试、`go vet`、Windows/Linux amd64 构建通过。本地 Linux PG/Redis `-race`：283 个顶层测试、627 个测试事件全部通过，0 失败/跳过/缺失，159.831 秒，临时资源全部清理。证据：`.local/source-20260918/local-linux-race-20260918t065208-ccec67/report.json`。

新增 Go 回归覆盖能力/生命周期/协议不匹配、启动超时、EOF 后异常退出或不退出、启动前取消、探测进程不进入 serving pool、未 ready UDS 清理以及已知 inode 被替换时保留替换项。

实际 release helper 的 factory 集成包含 strict/pooled 两种模式与 WS 开关的四种成功组合、两种错误 CA 环境失败，共 6 个子场景。该项已与官方 CLI HTTP、WS、压缩及工具回传等既有链路一起通过 20 项本地联调，0 跳过，容器已清理。证据：`.local/native-transport-build/gateway-compare-20260918T065109-adc725/result.json`。

独立 Rust 启动探针另有 5 项通过，0 个请求 envelope、固定 origin 监听器观察到 0 次连接。新版 runner 两项清理单元测试通过，实际容器已清理。报告 `.local/native-startup-probe/20260918T065226Z-115d0caf/report.json`；详见 [独立探针记录](NATIVE-STARTUP-PREFLIGHT-2026-09-18.md)。监听器仅覆盖固定 origin，无其它 dial 的结论还基于启动调用链源码审查，不冒称系统级全量抓包。

预检证明本地可执行文件、TLS/client 构造配置、IPC ready 合同及退出可用，不证明目标 TCP/TLS、真实账号代理、生产证书、模型接受、缓存收益或完整设备指纹。即使预检成功，后续配置变化或进程故障仍可导致请求失败，原有请求级能力检查和无隐式重放规则保留。

## 本地复跑

```powershell
python tools/native_transport_compare.py --native-run 20260918T054243Z-b43907cb --source-dir .local/source-20260918/gateway-overlay-20260918t065038-5660d3/source --suite gateway
```

独立无请求启动探针：`python tools/native_startup_probe.py`。这会在本机隔离容器中启动已封存 helper，不接触账号或生产服务；新增探针源码与原 release 源码快照分别记录，不将当前整个 native 目录哈希冒充原 release 构建来源。
