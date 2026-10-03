# 官方 CLI 本地传输观察

`source_native_capture_test.go` 只启动显式指定的独立官方 CLI 和 loopback 受控上游。普通 `go test ./...` 会跳过需要真实 CLI 的场景；隐私筛选与 ClientHello 截断检查仍会执行。

2026-09-18 实测的二进制为官方 npm `0.154.0` 与本机桌面应用附带的 `0.154.0-alpha.6.2`。后者作为新进程执行；没有附加或读取现有桌面会话。二进制来源、版本和 SHA256 记录在 [实测报告](CODEX-NATIVE-TRANSPORT-2026-09-18.json) 中。

## 已验证结果

- 两个二进制共 9 个基线场景通过，网关修复后重新编译的 1 个场景通过。共观察到 16 次受控 Responses 请求，包含工具输出回传。
- Windows 默认信任路径的首个 ClientHello 完整捕获：两个二进制均为 142 字节、未提供 ALPN。探针收到完整 Hello 后主动结束，所以退出码 1 是预期行为；这不能证明最终协商出的 TLS 版本或默认 TLS 下的 HTTP 行为。
- 设置临时 `SSL_CERT_FILE` 会使官方源码选择 rustls。本地自签证书下实际协商 TLS 1.3 / HTTP/2；这些样本不能冒充默认 Windows TLS 栈。
- 临时 `LANG/LC_ALL=en_US.UTF-8`、`TZ=America/New_York` 没有改变采集到的白名单应用头，也没有改变 Windows 区域设置或设备出口。
- 本实验使用自定义 provider 的原生 `exec`，发送 `originator: codex_exec` 和含完整版本的 User-Agent，没有独立 `version` 或 `Accept-Encoding`。网关修复前增加了 `version: 0.154.0`、`Accept-Encoding: gzip`；修复后两轮请求均与该原生样本的白名单应用头一致。**内置 OpenAI provider 不同**：`model-provider-info/src/lib.rs:398` 明确添加 `version`，网关仍保留客户端显式发送的值。不能把本次自定义 provider 的缺省推广为全部 CLI 请求。Go TLS 的 cipher、group、extension 信息仍不同，不宣称整个传输与 CLI 相同。

## 复跑

在 PowerShell 中设置下列变量。它们只进入该命令启动的测试进程；测试为各 CLI 子进程另建临时 `CODEX_HOME`，不会重用用户身份。

```powershell
$env:CODEX_NATIVE_CAPTURE_BIN = (Resolve-Path .local/codex-cli-0.154.0/node_modules/@openai/codex-win32-x64/vendor/x86_64-pc-windows-msvc/bin/codex.exe).Path
# 可选：显式指定桌面安装附带的 codex.exe，不能填写进程命令行。
# $env:CODEX_NATIVE_DESKTOP_BIN = '<desktop-bundled-codex.exe>'
$env:CODEX_NATIVE_CAPTURE_REPORT = (Join-Path (Get-Location) '.local/native-transport-new.json')
go test -run '^TestSourceNativeCLITransport$' -json -count=1 -timeout 6m . |
    Set-Content -LiteralPath .local/native-transport-new.jsonl -Encoding utf8
```

仅重跑网关转发样本：

```powershell
$env:CODEX_NATIVE_CAPTURE_REPORT = (Join-Path (Get-Location) '.local/native-transport-after-header-fix.json')
go test -run '^TestSourceNativeCLITransport$/official_npm_release/custom_ca_via_go_gateway_us_env$' -json -count=1 -timeout 2m . |
    Set-Content -LiteralPath .local/native-transport-after-header-fix.jsonl -Encoding utf8
```

保留首次完整报告为 `.local/native-transport-before-header-fix.json` 后，可以生成带修复前后证据的合并报告：

```powershell
python tools/native_cli_capture_report.py `
    --baseline .local/native-transport-before-header-fix.json `
    --followup .local/native-transport-after-header-fix.json `
    --output docs/CODEX-NATIVE-TRANSPORT-2026-09-18.json
```

## 采集边界

只持久化白名单应用头、ID 是否存在、请求 schema 键、工具类型计数、缓存选项是否存在，以及 TLS 协议元数据。Authorization、Cookie、账号和会话 ID 值、请求正文、命令输出、文件路径、TLS 随机数和密钥材料不会写入报告或测试日志。TLS 原始字节仅在内存解析完整首个 ClientHello 后丢弃。

临时 CA 只由独立 CLI 子进程通过 `SSL_CERT_FILE` 信任；网关夹具只替换测试客户端内存中的 RootCAs。不安装系统证书，不改全局代理或 Codex 设置。工具轮次仅执行固定无害 `echo`；Windows 使用临时配置中的官方 `read-only`、`never`、`unelevated` 路径，不执行手工 ACL、管理员安装、服务或系统用户配置。

本实验没有真实模型推理、生产网络、OAuth、WebSocket、认证路径下的 Zstd 请求压缩、HTTP/2 原始帧参数或真实美国出口验证。证据适用于这些独立本地样本，不覆盖既有桌面推理流量。
