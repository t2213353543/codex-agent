# 本轮收尾

用户已取消后续真实账号/上游推理测试。本轮没有再检查生产测试入口、请求账号、换用其它组或发起真实模型调用，也没有部署。以下仅为此前在途代码、release 产物和本地合成回归的收尾证据；未实网验证不再作为用户待办。

## Release 产物

`native-transport` 使用原 Cargo release 配置（thin LTO、4 codegen units、strip），没有放宽依赖锁或调高 2 CPU / 3 GiB 构建限制。可分发包为 `.local/native-releases/0.155.0-linux-x86_64-ffbfae2a3af7.tar.gz`，5,714,793 字节，SHA256 `9611fac32443b44c68c4b9106e817ba10261a7c884e3ec734b34a2e7802673f7`。

包内 Linux x86-64 helper 为 14,290,944 字节，SHA256 `ffbfae2a3af7a452e30df333f06908836fc9f55d4a0e168ed545fcd83d6a2d1c`。ELF 检查确认 static PIE、无 PT_INTERP、无 DT_NEEDED；只含该二进制、无 libc 和加载器的空根文件系统启动/ready/EOF 检查通过。不是在实际生产 CentOS 上运行或 TLS 信任链完整部署的证明。

25 项 HTTP/UDS 测试使用该 release 可执行文件通过；4 项 WS socket 测试在 `cargo test --release` 运行通过。包保存来源、固定锁、WS adapter patch、ELF/测试证据和逐文件 manifest。它不是仅将 debug 文件重命名，也不声称经过签名。

## 生命周期修复

helper 意外退出后，旧实现可能将路由、IPC 目录和 SOCKS 桥保留至空闲超时。现已监听实际进程退出，及时回收；同路由尚未发送的新请求可选择新进程。已经发送的 POST/WS 不重放。启动、退出、活动引用及身份校验仍受原有有界进程池约束。

本地 release 联调包含连续三轮同一 helper 内活动 HTTP 和 WS 被终止、两边及时报失败并释放，后续新进程只执行新请求；另验证 parent Close 可取消挂起的 SOCKS 握手。其它仍活动的 helper 不受牵连。

## 未知用户边界

完整 owner 要求非空 UserID 与 workspace。未知 owner 的会话引用现在附加由当前 AT/RT/ClientID 计算的域分隔摘要，不记录原凭据；元数据、名字和普通版本字段不影响它。

未知凭据变化后，旧线程返回 409，不复用旧历史。刷新生成的 AT/RT 先持久化，新线程可使用；初始无绑定请求在刷新后建立新引用并保持已选代理。401 恢复如果改变未知 owner 凭据，不再重发原 POST 或 WS 握手。已知同用户/workspace 的正常轮换仍保持稳定。

此摘要不是持久单调 owner epoch：精确 A -> B -> A 恢复同凭据、以及没有持久痕迹的注销/重新登录，仍不能保证等同官方 CLI 的本地代次状态机。该剩余差异明确保留，不宣称全部身份生命周期已完全一致。旧无摘要的未知绑定不自动迁移，需要新线程。

通用传输夹具现在明确提供完整合成 owner，专用测试显式清除身份覆盖未知规则。PG 冷却测试的初始/替换凭据也明确为同用户/workspace，以验证其本来要求的“同 owner 凭据替换解除冷却”；没有为了旧测试通过而撤销未知 owner 隔离。

## 快照与范围

最终 Go 源码目录 `.local/source-20260918/gateway-overlay-20260918t050352-1bd6d1/source`，SHA256 `648ca8b4314053416f34d2b94d1eceb27f81280c5bd983a754a87e07f1fed171`，基于固定管理 5a818 与明确网关 overlay，不包含其它会话后续管理改动。原生产稳定 956 和旧报告均未覆盖。

该快照全库测试、vet、Windows/Linux 构建通过；实际隔离 Linux PG/Redis `-race` 238 顶层、507 通过事件，零失败/跳过/遗漏，109.006 秒。未知 owner 重启隔离、已知 owner 冷却替换、退出资源回收和 SIGTERM 持久化均实际运行。先前一轮旧冷却夹具与新未知 owner 规则冲突的失败报告保留，修复夹具后才完成这次全量通过。

最终 Go 与 release helper 配对本地联调 15 顶层全部通过，包含实际 CLI 程序的合成工具流程、HTTP/WS、SOCKS、cookie、严格生命周期及崩溃清理。Linux gateway binary SHA256 `5fdeee97346f6dd919b9e74519bd0dd6ec4703e3a1f71fd5fade8d11127e777d`。所有本轮验收容器、临时网络与合成凭据已清理；编译缓存和发布包保留。

本地 CLI 测试用官方 0.155.0 二进制，但模型响应来自 loopback 合成上游，没有真实账号推理。包默认不启用，需要显式配置网关 native helper 路径/hash；本轮没有开启这些生产配置。

详细证据：`NATIVE-RELEASE-ARTIFACT-2026-09-18.md`、`CODEX-UNKNOWN-OWNER-2026-09-18.md` 及本文件同名 JSON。按用户要求，后续不会继续安排真实账号测试。
