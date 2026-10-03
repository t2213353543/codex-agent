# 原生 HTTP/WSS 生命周期与 TLS 对照

已联合上线：Go `5687aa1c…` 与 helper `bf920a0b…`，2026-09-26 01:21:51（北京时间）验收通过。实际 HTTP models 返回200及9个模型，WebSocket握手101且正常关闭、in-flight归零；没有发送模型 turn。原生 HTTP 生命周期仍为 `cli_request`，WS保持启用。

## 两处实际缺陷

HTTP adapter 在解码了一个 IPC 数据块后，即使调用方 `Body.Close()`，后续 `Read` 仍能返回此前缓冲区内的字节；取消后也可能丢失 `context.Canceled`。与标准 HTTP transport 的实际对照先复现失败。修复增加独立关闭标志，在读取前检查取消/关闭，停止交付已弃用响应的残留数据；已完成响应继续保留 EOF，Close 仍解阻阻塞读取且只释放一次资源。这里不宣称能撤销已经开始、与 Close 并发的 Read。

Rust helper 统一解析的 `total_timeout_ms` 在 WebSocket 分支被忽略。真实 loopback TLS、代理 CONNECT 和已连接 WSS 三个场景均复现：显式预算过期，旧 helper 仍不结束。修复用同一预算包围连接建立与消息处理，返回原有限错误码 `total_timeout` 并保留准确的握手/已接受阶段；Go 对该码识别为 timeout。没有显式预算时不增加会话总时长限制，当前 Go 生产 WS 也没有新增该字段，因此不能称原生产 WS 都会无限挂起。

## TLS 与 CLI 0.155.0 的实际边界

同一份冻结测试脚本比较官方 CLI、旧 helper 和候选 helper。旧 helper 18项仅上述3项显式 WSS 预算失败；候选18项全部通过，官方CLI的8项参照通过。另6项 CA 选择/优先级、4项 HTTP/HTTPS代理拒绝验证通过。全部使用隔离 loopback 网络和合成凭据，没有模型调用。

实际覆盖可信CA、未知CA、主机名错误、SNI、HTTP/HTTPS CONNECT、SOCKS5/5h、取消及拒绝后无直连回退。CA失败不会被当成通过认证；代理407未被误当成目标站401，也没有直接重试目标。测试服务器只提供 HTTP/1.1 ALPN，本轮不宣称覆盖完整 HTTP/2 SETTINGS。固定 CLI 的 legacy HTTP 构造在无效自定义CA时可能回退系统根，而 helper 明确拒绝启动，此保留差异已记录；不能笼统宣称全部 CLI 行为一致。

HTTP/WS 官方 vendored TLS 源码、依赖版本与 lockfile 未因这次修复改变。生产 Rust 变更仅 `src/websocket.rs` 的显式超时处理；没有编造浏览器指纹或关闭证书验证。

## 测试产物

上线 Go SHA `5687aa1c3fe766f80f1632900f245a442ee207ad6b469bbff4a6cab07b6c8e63`；上线 helper SHA `bf920a0bd578afa5180d05d60c47e98349cd35490100067eae9a35553f8d4863`。

- Linux race/PG/Redis/旧 helper 完整套件：3089项通过、26项跳过、0失败；vet通过。
- Windows完整套件：2705项通过、276项跳过、0失败；vet通过。
- 同冻结Go源码配新helper的独立组合回归：126项通过、2项CLI专项跳过、0失败，包含真实HTTP关闭、WSS、TLS/SOCKS、取消及cookie边界。
- Rust：43项通过。TLS/代理测试与证据复核90项全部通过。

跳过项不计为通过，真实新旧 helper 的覆盖分别记录。新helper组合首次测试容器因缺 python3 在运行测试前退出；改用已有 `sha256sum -c` 做同源检查后再启动，原失败回执保留。

## 发布与回滚

首次联合切换后触发 `environment_changed`，自动恢复旧 Go 与旧 helper，readiness 正常。原因由实际 systemd 环境来源和独立瞬时 unit 验证：已有 `EnvironmentFile` 会覆盖 drop-in 中的 `Environment` 值，导致新 helper 路径未按意图生效。原3个环境文件及设置未修改，临时 smoke group/key已清理。

第二、三次尝试同样安全回滚。第三次新增的脱敏差异记录精确证明：新Go已经启动，但只有两个helper变量仍是旧值，其余非动态环境完全一致。最终用真实 unit 文件解析及不重启的生产配置暂存核对定位到第二个发布缺陷：systemd252会将 `EnvironmentFile="/absolute/path"` 判为非绝对路径并忽略；`systemd-analyze verify` 甚至返回0，警告只在stderr。此前瞬时服务走D-Bus属性而非unit文件解析，所以不足以覆盖这处语法问题。

最终修复为独立的 `EnvironmentFile` 路径生成规则：仅接受已知安全绝对路径，直接输出无引号指令，不复用 ExecStart 的参数引用函数。daemon-reload之后、重启之前先核对新环境文件已实际成为最后一项；若此时失败，仅恢复配置并清理新文件，不重启服务。

第四版已成功在同一最后drop-in中切换Go并追加仅含两个helper变量的环境文件；原三个环境文件的哈希和顺序保持，其它环境、设置、frontend及账号元数据核对一致。临时smoke group/key和本地key副本已删除。发布脚本16项测试及真实systemd解析验证通过；前三次失败和回滚回执均保留，没有将失败描述为首次成功。

实际运行 `/proc/<pid>/exe` 和 helper 文件再次校验分别为上述SHA，PID为2564125。五个审查源码/测试文件已在写入前核对基线哈希后同步共享工作区，其它未提交文件未被覆盖。

[Go构建证据](../.local/research-20260926/tls-parity/go-build/build-manifest.json) · [native独立实测](../.local/research-20260926/tls-parity/summary.json) · [第一次发布及回滚](../.local/research-20260926/tls-parity/release/attempt1-env-precedence/activation-failed.json) · [systemd真实优先级证明](../.local/research-20260926/tls-parity/review/systemd-precedence-proof.json)

[实际解析差异](../.local/research-20260926/tls-parity/review/unit-envfile-quote-parser.json) · [最终联合发布](../.local/research-20260926/tls-parity/release/activation-result.json) · [HTTP/WS线上验证](../.local/research-20260926/tls-parity/release/runtime-smoke.json) · [最终运行哈希](../.local/research-20260926/tls-parity/release/final-runtime.json) · [临时资源清理](../.local/research-20260926/tls-parity/release/smoke-cleanup.json)

这些修复解决明确的生命周期及发布问题，没有证据表明它们解释此前账号错答，也没有据此宣布模型质量恢复。
