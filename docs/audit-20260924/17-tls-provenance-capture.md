# 17 · TLS 实测与独立来源审计

2026-09-24，基准为官方 Codex `rust-v0.155.0`。本专项新增测试与校验工具，没有修改 production Go/Rust、vendor 或 Cargo。结论分为官方源码校验、构建输入校验、同平台运行时观测，三者不能相互替代。

## 已交付

- `gateway_tls_parity_audit_test.go`：官方 CLI 直接 WSS 与官方 CLI → Go 网关 → Rust helper WSS 的同服务端、同证书、同 localhost origin 对照。
- `tools/verify_native_provenance.py`：以独立的只读官方源为依据检查来源声明、锁文件白名单、vendor 全集、唯一 adapter、生产 manifests、实际 feature 树及编译输入。
- `tools/test_verify_native_provenance.py`：7 项反例/正例测试，包含同时篡改源码与声明 hash、同时篡改 lock 与 allowlist 的反例。现有 `check_lock.py` 只检查内部一致性，本工具补上独立官方原文复核。
- `.local/gateway-audit-20260924/17-native-provenance.json`：本地实际校验报告。

## 独立来源与依赖结果

| 检查 | 实际结果 |
| --- | --- |
| 官方 workspace version/tag | `0.155.0` / `rust-v0.155.0` |
| 官方 `codex-rs/Cargo.lock` SHA256 | `81eb29f6276d5b3d1b39f1588a26059dadfbba717952247efeb8a575f271f569` |
| 官方 `codex-rs/Cargo.toml` SHA256 | `ef622a916ae57de62cf3adb1ab985047c31b05cf2691a91dc357df0d9863f437` |
| 裁剪后的 lock | 265 包，其中 261 registry/git 包和 4 本地包；所有外部 name/version/source/checksum 与官方锁一致 |
| `LOCKED-PACKAGES.json` | 与官方锁的全部外部包集合完全一致，不仅检查当前选中包 |
| vendor Rust 文件 | 30 个官方源逐字节一致，1 个独立记录修改；没有遗漏官方 Rust 文件，也没有未登记 Rust 文件 |
| 唯一修改 | `codex-websocket-client/src/lib.rs` 的 immutable explicit-route adapter；以官方原文加 `websocket_adapter.rsinc` 重建完全一致，diff 与 `WEBSOCKET-ADAPTER.patch` 完全一致 |
| 三个 vendor Cargo manifests | 结构上只删除 dev-dependencies 并关闭自动 bin/test/bench 发现；生产依赖及其 feature 声明未改 |
| 两个 WS fork | tokio-tungstenite `0e5b2d73aa18dd9f0a50ee9ff199d5aef7594186`；tungstenite `4fffad30fe373adbdcffab9545e9e9bf4f2fc19f`，与官方 workspace 一致 |
| 实际解析 features | rustls `aws_lc_rs/std/prefer-post-quantum`，reqwest `default/cookies/json/stream/rustls-tls-native-roots`，WS `proxy/rustls-tls-native-roots/deflate`，musl OpenSSL vendored 均存在；reqwest socks/gzip/native-tls-alpn 均未启用 |
| 构建输入关联 | 当前 Rust/Cargo/adapter/toolchain 内容与本轮 `native-source-manifest.json` 全部一致 |
| CLI SHA256 | `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501` |
| helper SHA256 | `f72e5c0ff8798c02f430f7aa36f74009dad40fade6111dad722390891239b556` |

直接传输版本：reqwest 0.12.28、native-tls 0.2.14、rustls 0.23.36、hyper 1.8.1、h2 0.4.16、openssl-sys 0.9.111、openssl-src 300.6.1+3.6.3、rustls-native-certs 0.8.3、tokio-tungstenite 0.28.0、tungstenite 0.27.0。

`rustls-native-certs 0.8.3` 源包另以 Cargo.lock checksum 校验后只读检查。`src/lib.rs:118-123` 在 `SSL_CERT_DIR` 存在时走 `CertPaths.load`；`:201-209` 解析目录，`:266` 起读取目录中的 PEM 文件。故成功的默认 WS TLS 用例只设置临时 `SSL_CERT_DIR`，不设置 `CODEX_CA_CERTIFICATE`/`SSL_CERT_FILE`，不修改系统信任。它验证的是 WS 默认 **ExplicitCodexTls + native-root loader**；不能把该结果称为 HTTP 的 OpenSSL/Schannel native TLS 结果。

## WSS 捕获边界

两个成功场景分别为 `default_rustls_native_roots` 与 `codex_custom_ca`。每个场景，官方直接 WSS 连 3 次，网关 native WSS 连 3 次；每条连接完成 `generate:false` warmup、引用 exact previous_response_id 的生成请求及最终回复。使用同一 listener、DNS-only localhost 叶证书及 server TLS session-ticket 配置。服务器真实协商 permessage-deflate 并发送重复文本，由 CLI/Rust 解压消费；未把协商成功冒充压缩字节完全一致。

比较以下实测字段：

- ClientHello legacy version、cipher wire order、supported versions/groups、signature algorithms、ALPN offer、SNI 是否出现及 hello 长度；只对 extension ID 顺序排序，保留原顺序于报告。随机数/key share 内容不比较、不保存。
- 服务器协商的 TLS version/cipher、HTTP/1.1、SNI `localhost`、空 ALPN，以及 deflate offer/accept。
- 3 次新连接 `DidResume=false`，每条连接 2 条模型协议消息、1 warmup、1 final、1 exact previous link。
- Go `RoundTripper` 设为禁止出口的 guard，要求调用数为 0；成功场景要求完全没有 HTTP fallback。
- 最终测试还只读检查三轮的 pool child 对象相同，避免“helper 每轮重启”造成没有 session resumption 的假通过；不输出进程 ID。

两个负向场景分别为错误 CA 和已信任 CA 签发但 DNS 为 `wrong.invalid` 的证书。官方 CLI 与 native adapter 都必须真正抵达 TLS 并发送 certificate rejection alert，HTTP/WS application handler 调用数必须为 0。服务端只分类 `remote error: tls: bad certificate` / `unknown certificate[ authority]`，不保存原日志。每条 TCP 在 Accept 时绑定 owner，晚到的官方告警不能被归给 native。

首轮负向 harness 曾错误要求 CLI 整个 `exec` 在 30 秒内退出，导致两项仅 `no_cli_timeout` 失败；正向两项已经通过。官方 `stream_idle_timeout_ms` 不约束整个启动/退出流程，源码 `core/src/client.rs:1081-1096` 有独立 connect timeout，preconnect/prewarm/重连亦是不同路径。这不足以推断具体哪个 CLI 生命周期步骤造成等待，因此没有将其归因于某个确定生产缺陷。修正后，在服务端收到明确拒证 alert 后有界结束官方探针，独立记录 `cli_stopped_after_certificate_rejection`；人工结束不计为正常 CLI 失败或完成，仍保留 `no_cli_timeout` 断言。native 则使用真实 `dialNativeGatewayWebSocket` 并要求返回错误、无 response/connection 及对应拒证 alert。

## 执行证据与最终快照

本地 Windows：测试文件 Go 编译通过；由于动态测试明确要求 Linux 与两个显式二进制，本地 Go 执行跳过该 opt-in 测试。来源核查真实通过，Python 7 项测试全部通过。

主任务实际 Linux race runner `go-cli-ws-tls-final` 回传四个场景全部通过，7.25 秒，无证书 probe 超时；本专项随后读取 `.local/gateway-audit-20260924/cli-reports/cli-ws-tls-final-codex_native_ws_tls_report.json` 逐项确认。该次冻结的测试 SHA256 为 `c99211998cc14b9b09a07b504de4780cfe6f958a8c0c9e5e8d549bd6678dc1fb`，源集合 SHA256 为 `393a7a79bab127b468c6ee78b734a59234ef706399dd451b20544032ad32fa17`。本地同时保留 run/source manifests。

之后仅补了 TCP owner 归属和同 helper child 断言，最终测试文件 SHA256 为 `b64a0072d113ed7a8c050dbc234d76b19a62f66d92c03bdebbcf1ae29c4fbc14`，本地编译通过；主任务将在最终 CLI 全量回归纳入此版本。本报告不把上一快照的动态通过冒称为后一快照已执行。

同轮已存在的 HTTP 证据 `.local/gateway-audit-20260924/http-fingerprint.json` 则覆盖 HTTP/2 SETTINGS、WINDOW_UPDATE、priorities 和强制重连 TLS session reuse。默认 native 路径没有协商 h2，custom CA rustls 路径 h2 相关字段相符；CLI/default helper 都不恢复 session，显式 pooled control 恢复 3 次。两类 HTTP/WS 证据分别成立，不交叉代替。

## 明确保留的差异及文档修订建议

1. 源码完整复用不等于全网关行为完全相同。immutable explicit route、无环境代理绕过、32 MiB WS message 上限、IPC 有界背压/写期限、网关账号鉴权和重试治理均是明确的网关差异。CLI fork 的 message 默认 64 MiB；frame 默认 16 MiB。`pooled` 是可选性能模式，与 CLI 每次新连接创建 TLS 配置不同。
2. `native-transport/README.md:77-79` 保留 2026-09-18 / CLI 0.154 历史测量是合理的，但应增加本轮 0.155 的 HTTP h2/resumption、WSS 12 次完整握手及 4 个拒证边界，并明确历史样本不可代表当前 helper SHA。
3. README `:105` 的“还需 Go WS adapter 与最新官方 CLI 真正对照”在本轮最终通过后应改为具体证据及剩余范围；`:107` 的旧 4 WS/24 loopback 计数应与本轮 35 Rust/26 loopback/8 HTTP 边界结果区分。
4. `native-transport/ws-plan/README.md:3` 是 0.154 设计阶段快照，仍写“not a running native WebSocket implementation”。应显式标为历史设计文档，并链接当前实现/0.155审计，避免读者认为 WS 尚未实现。
5. `source_native_capture_test.go` 固定 0.154 的 `SourceFindings` 是旧 HTTP capture harness 元数据；新测试不使用其旧来源描述。未来维护其 schema 时应将声明绑定实际执行版本，不能因版本号替换而将旧 capture 升格。
6. `vendor_source.py` 的模块说明仍称“only pinned HTTP crates”，现在处理三个 HTTP/rustls/WS crates；建议改为 transport crates。上述文档均由主任务统一修订，本专项没有改生产或 vendor。

尚未覆盖：生产真实域名/账户、HTTPS/SOCKS proxy 的同平台 CLI 抓包、TLS HelloRetryRequest、真实 raw header 顺序、精确压缩 wire bytes、TCP/IP 指纹、跨 Windows/macOS 编译与运行。因此不能把本轮 PASS 表述为“任何平台/任何服务器/全部设备指纹完全相同”。

## 一条命令复跑

独立来源与构建证据：

```sh
python tools/verify_native_provenance.py --features .local/gateway-audit-20260924/native-resolved-features.txt --build-manifest .local/gateway-audit-20260924/native-source-manifest.json --cli-bin .local/native-transport-build/official-codex-linux-0.155.0 --helper-bin .local/gateway-audit-20260924/codex-native-transport --output .local/gateway-audit-20260924/17-native-provenance.json
```

同平台 WSS（Linux；替换二进制绝对路径）：

```sh
CODEX_NATIVE_CAPTURE_BIN=/path/to/official-codex-linux-0.155.0 CODEX_NATIVE_HTTP_TEST_BIN=/path/to/codex-native-transport CODEX_NATIVE_WS_TLS_REPORT=/tmp/ws-tls-parity.json go test -race -run '^TestGatewayNativeActualWebSocketTLSParity$' -count=1 -timeout=5m -v .
```

## 主任务最终验收回填

最终固定CLI批次13顶级测试通过，包含本专项新增/强化用例；最终完整Linux race+PG/Redis+native helper为821顶级通过、0失败、0 DATA RACE（19外部opt-in逐项跳过，CLI和network-none OAuth已独立补跑）。WSS最终报告的同helper进程、late-alert归属与全部四场景checks均为true；PG管理修复冷/热缓存已在完整最终批次通过。当前所有Go生产源码与这两个最终快照一致。证据见`.local/gateway-audit-20260924/go-full-final/`、`go-cli-final/`、`cli-reports/cli-final-codex_native_ws_tls_report.json`。
