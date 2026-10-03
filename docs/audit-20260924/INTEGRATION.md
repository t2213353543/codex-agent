# 集成验证日志（2026-09-24）

此文件持续记录本轮实际执行结果；最终结果以最终源码快照校验和为准。

- 初始 Windows Go 1.26.7：完整 `go test ./... -count=1` 通过，27.475 秒；`go vet ./...` 通过。
- 原生 helper Linux x86_64-musl / Rust 1.95.0 release：35 Rust、26 Python loopback、8 HTTP/TLS/HTTP2 边界测试全部通过。二进制 SHA256 `f72e5c0ff8798c02f430f7aa36f74009dad40fade6111dad722390891239b556`。来源快照和日志在 `.local/gateway-audit-20260924/native-*`。
- 官方 Linux CLI 0.155.0：npm archive SHA512 与已保存 provenance 相符；可执行文件实际报告 `codex-cli 0.155.0`，SHA256 `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`。系统 PATH 中 0.153.4 未作为此版本验收依据。
- 第一批 Go Linux race + 实际 Rust helper：149 顶级通过，1 失败，8 CLI opt-in 跳过，共405测试事件；未出现 DATA RACE。失败为旧 `TestGatewayNativeActualWebSocketBudgetResume` 发送20MiB单帧，与此次对齐的CLI16MiB frame上限冲突。
- 背压测试夹具修正：每条20MiB消息分为两个10MiB frame，仍跨越32MiB IPC预算验证阻塞与恢复。该测试单独 Linux race 通过，1.01秒，随后go vet通过。生产frame上限未为测试放宽。
- root整合URL构造：HTTP/Go WS/native WS共同调用 `gatewayUpstreamURL`，保留escaped prefix并拒绝配置query/fragment/userinfo。

隔离测试目录：`/opt/codex-agent-integration/checks/audit-20260924`；使用已有合成PG/Redis测试环境，源码按快照打包。没有将本轮修改部署到生产。

- HTTP/TLS 本轮同平台官方 CLI 对照通过，6场景各4请求：default native及custom CA，各含CLI、cli_request helper、pooled helper。custom CA场景双方协商h2，SETTINGS/WINDOW_UPDATE/priorities均一致；CLI与cli_request每次完整握手，pooled对照恢复3次。default native没有协商h2，不能作h2一致证明。报告`.local/gateway-audit-20260924/http-fingerprint.json`。
- 实际CLI第一批：9顶级通过、2工具执行夹具失败、1压缩网络隔离skip。原因是原race容器缺Python；仅在独立测试容器安装Python后，文件修改/失败修正/恢复及并行工具场景全部race通过，未修改模型或网关代码来规避断言。
- 合成OAuth官方CLI压缩在`--network none`容器执行，真实request compression及工具后续测试通过，30.563秒。网络隔离由测试自行检查，不以环境flag代替实际检查。

- 认证/会话批次在真实合成 PostgreSQL/Redis + Linux race 下：109个顶级测试全部通过，零失败、零跳过，共339个测试/子测试终态。
- HTTP ClientHello 独立测试 `TestSourceNativeHelperTLSComparison` 通过：默认native首次Hello与custom-CA/rustls Hello字段比较（扩展随机顺序归一化），custom-CA协商完成；该测试不把默认首包捕获当作默认信任握手成功。

- 运行时/池/关闭/真实CA批次 Linux race：49顶级、134含子测试全部通过，零跳过；代理/SOCKS/观测批次亦通过，11.503秒。
- 独立CLI契约新增测试 Linux race通过，2.28秒：冲突索引的两个真实工具均执行且SSE逐字节保真；缺response终态使CLI失败、网关无重放。
- WSS四种信任/拒证场景均通过，7.25秒；对应捕获版本12成功连接24消息与4个证书拒绝告警。最终same-child与late-alert归属强化将在最终CLI gate再次运行。
- Windows完整候选测试：681顶级通过、0失败、142外部环境opt-in跳过，2149含子测试终态，42.698秒；不会将opt-in skip计为通过。

## 最终冻结结果

- 最终Linux全量：821顶级pass、0fail、19skip、2297含子测试终态，260.891s；race零报告，vet通过。Go/SQL快照SHA256 `6b94afef213f50aa83b47f8bf58e6200dda98a7f99c1171f742ac4478f143f17`，工作区逐文件一致。
- 最终CLI批次13顶级通过（含稳定WSS probe和新增contract），30.950s；network-none压缩两顶级通过、零skip，30.548s。两批生产输入与最终工作区一致。
- 一键Windows最终：698顶级pass、143skip，1991通过/191跳过的含子测试终态；Go/vet/provenance全过，源码稳定；分类仅统计真实leaf并要求明确opt-in，native缺配置不会假报pass。
- release-candidate包含Windows/Linux网关和实测Linux musl Rust helper、manifest.json；未部署。
