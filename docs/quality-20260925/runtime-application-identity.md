# 真实运行环境与应用身份修复

2026-09-25。本专项仅本地源码、已有证据和合成 loopback；未 SSH、未联网研究、未发送模型请求。现场查询与部署由主任务负责。

## 确认的实际矛盾

生产此前启用 `codex_fingerprint_enabled=true/profile=linux`，而 `codex_fingerprint.go:28` 固定发送 `codex_cli_rs/0.155.0 (Linux 6.8.0; x86_64) unknown`。这只是历史参考模板，不是宿主探测。主任务现场确认 CentOS Stream 9/x86_64、内核 5.14.0-539，service UID 61787，无 TERM/TERM_PROGRAM。

主任务在同 UID、独立网络 namespace、固定官方 CLI 0.155.0 下执行无模型 initialize，得到：

```text
codex_cli_rs/0.155.0 (CentOS 9.0.0; x86_64) unknown
```

证据：`.local/research-20260925/environment-fidelity/official-runtime-identity.json`；官方可执行文件与实际进程 SHA256 都为 `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`，UID 一致、网络 namespace 不同、正常退出、model_calls=0。initialize 使用 NON_ORIGINATING 名称 `codex-backend`，避免额外 originator/suffix。

这里 **CentOS 9.0.0 是实际官方 CLI 输出的 OS 信息，不是把 5.14 内核改写成 9.0**。固定源码 `login/src/auth/default_client.rs:164–174` 使用 `os_info::get()` 的 OS type/version/architecture，再附 terminal user_agent；terminal 检测来源在 `terminal-detection/src/lib.rs:234–260`。不能用任意发行版名称加 uname kernel 手拼后声称等价。

## 实现与数据流

不修改两种历史模板的定义和显式启用语义；主任务在生产关闭模板，让入站原生 CLI 使用自身身份。

新增 `codex_runtime_identity.go`：`CODEX_GATEWAY_IDENTITY_FILE` 指向 operator 从同环境固定官方 CLI 捕获的 manifest。`main.go:48` 在任何 DB 操作之前验证并发布 immutable atomic snapshot。

1. manifest 只接受八个精确字段：schema、cli_path、source_cli_sha256、cli_version、originator、user_agent、platform_os、platform_arch；拒绝重复键、未知键、大小写替代键、尾随 JSON、非字符串、空值、CR/LF/NUL。
2. 配置为绝对路径、regular file、最多 16 KiB；CLI source 为绝对路径、regular file、非空、最多 512 MiB；启动时实际读二进制并核 SHA256。
3. schema 固定 `codex-runtime-identity-v1`，版本必须等于网关固定 CLI baseline，originator 为 codex_cli_rs；platform_os/arch 必须等于运行 Go 的 GOOS/GOARCH；UA 必须匹配该 prefix/version、具有 CLI 平台与 terminal 结构、为不超过 1024 字节的 printable ASCII。
4. 验证全部通过才原子发布；异常只返回固定原因，不把完整路径或 UA 记录到错误。每请求只 atomic load，不读配置、不执行 CLI、不重新探测终端。配置文件之后变化不会悄悄改变运行中身份；重启时重新校验。
5. 没有显式配置则保持旧短 UA，不假称有已验证环境。配置不能验证成功时启动失败，不退回模板继续服务。

`gatewayDefaultApplicationIdentity()` 仅进入以下 gateway 生成身份的路径：

| 路径 | 接入位置 | 保留的 endpoint 语义 |
| --- | --- | --- |
| 非原生调用方的兼容回退 | `proxy.go:173` | UA/originator 改用已验证本地 capture；Version 仍为 baseline，旧 opaque attestation 被删除。 |
| 管理 models/usage/reset/测试请求 | `admin_account_upstream.go:153` | 只替换默认 UA/originator 来源；账号 token/workspace、Accept、原有 Version 保持各自逻辑。 |
| OAuth refresh | `gateway_oauth_headers.go:31` | 使用本地 capture；继续不发送模型 provider 的 Version，不引入 inference bearer/workspace。 |
| OAuth authorization_code | `gateway_oauth_headers.go:24` | 仍是官方 raw auth 路径，无 UA/originator；不套用 runtime identity。 |

**原生入站透传不套本机 UA。** Windows CLI、exec 的 codex_exec、自定义 thread originator 和 MCP suffix 属于调用方进程；网关宿主 CentOS 并不使这些信息成为错误。`copyRequestHeaders` 仅在既有非原生 fallback 分支使用本机 capture。`copyCodexRequestHeaders` 在 fingerprint disabled 时不解析 dormant profile，旧无效 profile 不会阻止原生请求；新增测试覆盖。

## HTTP/WS、metadata 与账号边界

- HTTP `gateway.go:721` 和 WS `gateway_websocket.go:83` 都使用 `copyCodexRequestHeaders`。同一禁用设置下两条链不制造新的设备、安装或会话标识；既有 fingerprint WS 切换检查会要求已握手旧身份重连，不把新设置套在旧握手上。
- 官方 CLI `exec/src/lib.rs:259` 在 exec 模式设置 codex_exec；gateway-generated runtime identity 的 codex_cli_rs 来自 app-server 默认 initialize，不能拿它覆盖真实 exec。
- `installation_id` 是 `CODEX_HOME/installation_id` 文件中的持久 UUIDv4（`core/src/installation_id.rs:19–62`），不是硬件指纹，不按 token 或账号生成。跨账号同一客户端安装可能合理复用它；网关不能据此编造每账号不同设备。
- `responses_metadata.rs:306` 由统一 snapshot 生成 client_metadata；`:345` 投影 window/turn 等兼容头；`session/mod.rs:4327–4333` 的 window ID 为 thread_id:window_number。这些由客户端生命周期产生，网关继续透传；本次没有改 JSON body、client_metadata、environment prompt、installation/window/root_turn/session。
- native pool 的账号 scope 由 `gateway_native_identity.go:18` 的已选 Account 决定，不由 caller 设备标识决定；可信 owner 包含账号记录、workspace、user 和 owner revision，未知 owner 还含凭据边界。HTTP/WS 共用该边界；更改 UA 来源不改变账号/cookie 隔离。针对性现有 scope/owner 回归通过。
- 已启用模板会删除所有大小写的 X-Oai-Attestation；非原生 fallback 同样移除旧 proof。官方公开 attestation 接口仅给出 host integration 与 thread_id，不能从源码证明其具体签名覆盖 UA/硬件/token。本次不增加伪 proof，也不擅自删除原生透传 proof。
- UA profile 与 TLS 是不同层。此次不把 CentOS UA 当成 HTTP/WS ClientHello 指纹一致证明；native 固定 helper/真实 TLS 验收由主任务单独负责。

## 部署 manifest 契约

```json
{
  "schema": "codex-runtime-identity-v1",
  "cli_path": "/opt/codex-agent/runtime-identity/codex-0.155.0",
  "source_cli_sha256": "660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501",
  "cli_version": "0.155.0",
  "originator": "codex_cli_rs",
  "user_agent": "codex_cli_rs/0.155.0 (CentOS 9.0.0; x86_64) unknown",
  "platform_os": "linux",
  "platform_arch": "amd64"
}
```

来源文件是 operator 提供的本地 capture；loader 的二进制 hash/platform 验证**不是**“任意 manifest UA 都由官方二进制产生”的密码学证明。现场此次有独立同 UID、无网络 initialize 证据。GOOS/GOARCH 相同但发行版/终端环境改变时，必须重新 capture；loader 不在请求内运行 CLI，也不会猜测升级后的版本值。生产文件 root-owned 只读、source CLI 固定 SHA 与上线状态由主任务落实并另记。

## 验证

新增 `codex_runtime_identity_test.go` 四组测试，包括 27 种坏 manifest、启动先于 DB 失败、source binary 改变拒绝、已加载 snapshot 不逐请求读磁盘、实际 loopback wire、未配置 fallback、native UA/thread originator/suffix/attestation/installation/window/turn 全保留、无效 dormant profile 透传、managed/OAuth endpoint 区分。测试使用明确 synthetic source bytes，未冒称运行了官方 CLI。

```powershell
go test . -run 'TestGatewayRuntimeIdentity|TestCodexFingerprint|TestResearchOAuthIdentity|TestGatewayNativeAccountScope|TestGatewayOwnerIdentity|TestResearchSession' -count=1 -timeout=90s
go vet ./...
```

实际均 exit 0。Go 定向 **32 个顶层 PASS、含子项 143 PASS、0 FAIL**；11 个需要显式真实 CLI/native 的分支 SKIP，不把 Windows Go 结果写成 Linux native 验收。日志 `.local/research-20260925/environment-fidelity/application-identity-tests.jsonl`。vet 无输出通过。

修改文件：`codex_runtime_identity.go`、`codex_runtime_identity_test.go`、`main.go`、`proxy.go`、`admin_account_upstream.go`、`gateway_oauth_headers.go`。本专项没有改生产设置、现有模板定义或 endpoint Version 契约。

主任务冻结初版全套后，另追加独立 `codex_runtime_identity_native_test.go`，生产源码未变。`TestGatewayRuntimeIdentityNativeActualHTTPAndWebSocket` 在真实 Rust helper 上做 loopback TLS HTTP、TLS WS 和管理自产请求；fallback 与 native 两组共六条实际 wire，逐项比较 UA/originator/version/attestation、installation/session/window/turn metadata、无伪造 ID，并用 Go fallback guard 排除绕过 Rust。Windows 编译 exit 0、测试明确 SKIP；Linux helper/race 的实际结果由主任务补充，不混进前述初版 143 PASS。

```powershell
go test . -run '^TestGatewayRuntimeIdentityNativeActualHTTPAndWebSocket$' -count=1 -race -v
```

追加测试源 SHA256：`87a2bb6bc7b5f905fbf74283979b88411718c823aced1b8bc346ca5894ad3f02`。原生产源码快照见 `.local/research-20260925/environment-fidelity/application-identity-source-sha256.json`。

主任务已完成最终验收：Linux 追加真实 native 测试 1 顶级 / 3 含子项 PASS、0 skip，1.829 秒；同生产源码完整 Linux 912 PASS、官方 CLI 7 PASS；Windows 790 PASS。后端 `9468c12b…7646de3` 已于北京时间 04:55:34 上线，HTTP/WS 握手及实际 usage 往返通过，指纹模板关闭。详情见 [环境修正主报告](../CODEX-QUALITY-ENVIRONMENT-2026-09-25.md)。
