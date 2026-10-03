# 01 — CLI 基准与应用头指纹审计

审计日期：2026-09-24。基准为工作区只读官方 `.local/codex-source-0.155.0/`，不是 PATH 中的 CLI。本文行号以本专项完成时文件为准；`codex-rs/...` 指基准源码目录内路径。未操作 production。

## 修复

1. `codex_fingerprint.go:45`：启用 profile 后，按大小写不敏感名称移除所有 User-Agent、Originator、Version 与 X-Oai-Attestation，再写入唯一规范键。此前 `Header.Set/Del` 只处理 canonical map key，内部代码传入小写或混合大小写 map 时，会留下第二套 identity 或旧 attestation。新增真实 loopback HTTP 请求验证线上序列化结果。
2. `codex_fingerprint.go:54`、`:73`：nil 目标头返回包装 `errInvalid` 的错误，消除 panic；disabled 的 apply 仍是无操作。
3. `codex_fingerprint.go:77`：profile 非法时，copy 在修改目标头之前返回；此前会先覆盖 turn state 等再失败。
4. `codex_fingerprint.go:10`：修正过时 0.154 注释，明确应用头与 native/Go TLS transport 独立，避免把 header profile 误说成 TLS 模拟。attestation 注释改为网关不跨身份重放 opaque proof 的策略，不假设公开源码未证明的签名内容。

新增 `codex_fingerprint_boundary_test.go`，未修改其他 agent 负责的 gateway/proxy/store/native 源码。

## 一致性矩阵

| 项目 | 官方源码证据 | 当前网关与判断 |
| --- | --- | --- |
| 版本基准 | `codex-rs/Cargo.toml` workspace version 为 0.155.0 | `proxy.go:22` 的 clientVersion、native workspace、UPSTREAM tag 均为 0.155.0，静态一致。 |
| 实际 PATH CLI | 执行 `codex --version`，入口为 `D:\Dev\npm-global\codex.ps1` | **实际为 `codex-cli 0.153.4`**，与本次源码基准不一致。真实 0.155 验收必须显式指定已校验的二进制；本专项未替换系统 CLI。 |
| UA 结构 | `login/src/auth/default_client.rs:164`：originator/version + OS type/version + architecture + terminal，可附加 suffix；`:195` 清理非法字符 | `codex_fingerprint.go:25` 的两个 profile 结构一致。Linux 6.8.0/x86_64 与 Mac OS 15.0.0/arm64、unknown terminal 是确定性参考值，不是宿主实时检测；这属于有意差异。 |
| unknown terminal | `terminal-detection/src/lib.rs:179`、`:234` | unknown 是合法 CLI terminal 值。不能据此声称实际终端、实际 OS 或真实设备与该 profile 相同。 |
| originator 默认与 override | `login/src/auth/default_client.rs:40`、`:63`、`:137`：默认 codex_cli_rs，环境可覆盖，线程非默认值可覆盖 | disabled 保留原生客户端头；enabled 固定 codex_cli_rs，覆盖客户端自定义 originator/suffix，有意管理策略。网关不能套用宿主 CODEX_INTERNAL_ORIGINATOR_OVERRIDE，否则会污染客户身份。 |
| Version 头 | `model-provider-info/src/lib.rs:443`、`:455`：built-in OpenAI provider 增加 Version；default auth client 本身不增加 | enabled profile 模拟 built-in provider Version；disabled 原生 custom-provider 请求缺省 Version 时应保持缺省。已有 copyRequestHeaders 实现与测试覆盖该契约。 |
| HTTP/WS 优先级 | `codex-api/src/endpoint/responses_websocket.rs:490`：extra > provider > default；`:1145` 有对应测试 | 网关 disabled 继承入站已经合并好的头；enabled profile 最后覆盖身份，是有意差异。`gateway.go:695`、`:927` 与 `gateway_websocket.go:60` 使用同一 copy/apply 入口。 |
| Attestation | `core/src/attestation.rs:7`、`:15`、`:24`：host integration 接口，输入 thread_id；`core/src/client.rs:732` 条件生成 | disabled 原生 identity 保留 opaque proof；enabled identity 改写删除 proof，且现在全部大小写均删除。公开源码未证明 proof 加密绑定具体 UA，报告不作此断言。网关不生成伪 attestation。 |
| 能力与会话头 | `core/src/client.rs:152` 及 `:2052`：安装、路由、turn-state、turn-metadata、beta features 等 | apply 只处理四种身份相关头；会话、trace、residency、beta 等保持。copy 的 allowlist 和 obsolete beta 移除继续由 proxy 层管理。新增 slice 所有权测试确认下游后续修改不反向污染入站。 |
| Models client_version | `gateway.go:904`、`:910` | enabled 用 profile.Version 同步 query/header；disabled 保留有效客户端 query，否则使用网关基准。原有 HTTP routes 用例覆盖。 |
| Refresh JSON | `login/src/auth/manager.rs:1599`、`:1604`、`:1613` | JSON grant_type/refresh_token/client_id 与 Content-Type 一致。网关保留账户发行时 ClientID，默认才使用官方值，属于多账户网关必要差异。原有 OAuth 测试验证不会被 profile 改写。 |
| Refresh identity | `login/src/auth/default_client.rs:324`、`:335`；auth 只有默认 originator/UA/residency，未自动 Version | `proxy.go:314` disabled refresh 使用短 UA，缺 OS/terminal；enabled profile 提供完整参考 UA 并额外 Version。**这是既有差异，不应标成逐字对齐。** 本专项未擅自改变公共 refresh 契约，已报告协调者。 |
| Admin upstream | `admin_account_upstream.go:152`、`:154`、`:162` | 管理动作先设短默认 UA/Version，后应用统一 profile。enabled 与网关一致；disabled 是网关生成请求，不能声称来自实际 CLI。 |
| WS settings 修改 | `gateway_websocket.go:339` | identity 切换/启停后必须 reconnect，避免在已建立握手上宣称新身份；disabled 时仅预选 profile 不打断连接。原有四种设置切换测试通过。 |
| API 配置校验 | `store.go:648`、`:663`；`admin.go` settings endpoint | 仅 linux/macos；默认 disabled/linux；持久化兼容旧配置。内部 apply disabled 可不读取未启用 profile，公共 settings 更新仍严格校验，有意分层。 |
| TLS 范围 | `admin_account_runtime.go:234` 对外声明 application_headers_only | 指纹 profile 不控制 TLS backend、ClientHello 或 HTTP/2 设置。此处静态源码/锁校验通过并不能代替 HTTP TLS、WS TLS 的运行时捕获验收。 |

## 官方依赖和 vendored 源码实核

执行现有检查器 `python native-transport/tools/check_lock.py`，exit 0：

```json
{"dependency_versions_match_official_lock":true,"packages":265,"unchanged_upstream_rust_sources":30,"recorded_adapter_sources":1,"verified_tls_features_present":true,"websocket_forks_match":true}
```

另独立读取 `.local/codex-source-0.155.0/codex-rs/Cargo.lock`、`Cargo.toml` 与 vendored 每个文件，避免只信本地 allowlist：

- 261 个 external package 的 `(name, version, source, checksum)` 全部存在于官方 lock，无漂移；其余 4 个为显式本地 package。
- 官方 Cargo.lock 与 Cargo.toml SHA256 均匹配 UPSTREAM.json。
- 30 个未修改 Rust 源文件同时匹配本地官方源码与声明 hash；唯一 `vendor/codex-websocket-client/src/lib.rs` 原始 hash 和声明 patch 后 hash 均吻合。
- 0.154.0 与 0.155.0 的 `login/src/auth/default_client.rs` 字节完全相同；本轮修正注释没有隐含改变 UA 规则。
- reqwest 0.12.28、native-tls 0.2.14、rustls 0.23.36、hyper 1.8.1、h2 0.4.16、openssl-sys 0.9.111 与官方锁一致；两个 WS fork revision 检查通过。

本专项未重新构建 native helper，未抓取生产 TLS，也没有运行真实上游账户。正在运行的二进制是否由这些文件构建，应由其他 runtime 专项验证。

## 边界与测试结果

新增六组测试覆盖超过六个具体边界：

1. linux/macos 两种 profile，canonical/lowercase/mixed-case 身份重复键：上游真实 HTTP 仅收到一组正确值，attestation 全部消失，turn state 保留。
2. nil destination，分别验证 enabled/disabled apply 和 copy；不 panic，错误可用 errors.Is 识别。
3. 空 profile、LINUX、windows、带 CRLF 注入的 profile：报错且目标头完全不变。
4. disabled 保留自定义 originator、MCP suffix、opaque proof、residency 与 beta features，且不要求解析未启用 profile。
5. 复制后的多值 capability slice 不与入站共享。
6. 原始 identity 含 CR、LF、NUL：enabled profile 替换后可合法序列化，不残留注入值。

重跑命令：

```powershell
go test . -run 'TestCodexFingerprint|TestCLITransportRequestHeaders|TestRequestHeaderIdentity' -count=1
```

最终 JSON 记录：`.local/gateway-audit-20260924/01-fingerprint-tests.jsonl`。2026-09-24 16:09 +08:00：exit 0，package elapsed 0.311s；16 个顶级测试 PASS，计入子测试共 72 个 PASS，0 FAIL。唯一 SKIP 为需要显式真实 CLI 配置的 `TestCodexFingerprintCLIConversation`；不会将其写成真实 CLI 或上游接受验证。

## 已转交的跨文件项

`proxy.go:copyRequestHeaders` 在浏览器/curl 身份回退时会重写 UA/originator/version，旧实现仍保留 attestation；建议与 enabled profile 一样避免跨身份重放。原函数对大小写重复 singleton 键按 Go map 遍历覆盖也需要独立确定性策略。以上已发送协调者，未跨越文件所有权修改；最终状态以 proxy 专项报告为准。
