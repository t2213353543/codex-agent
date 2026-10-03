# OAuth 正常请求路径研究：区分 raw 与 default auth client

2026-09-25。发现并修复两项 **正常 OAuth 执行路径**与固定官方 0.155.0 的应用头契约差异：refresh profile 错带模型 provider 的 `Version`；authorization-code exchange 复用管理模型请求头，错带 Codex UA/originator/Version。无需构造异常 JSON 即可复现。

本专项只发送 loopback 合成 OAuth 请求，不使用真实 AT/RT、不调用模型、不 SSH。修复缩小应用身份差异，不宣称 OAuth 已导致账号被标记，也不宣称能因此恢复糖果题答案。

## 官方运行路径而非头名称猜测

以下路径相对于固定 `.local/codex-source-0.155.0/codex-rs/`：

| 正常动作 | 官方证据 | 真实构造 |
| --- | --- | --- |
| 已登录账号刷新 | `login/src/auth/manager.rs:383`、`:1599` | auth state 创建 `create_default_auth_client`；refresh POST JSON，只加 Content-Type，body 为 client_id/grant_type/refresh_token。 |
| default auth identity | `login/src/auth/default_client.rs:324`、`:335` | 默认 UA、originator，及明确配置时的 residency；没有模型 provider Version。UA 在 `:164` 由进程 originator、构建版本、OS/架构、终端组成。 |
| authorization code 兑换 | `login/src/server.rs:826`、`:832` | `create_raw_auth_client`，form-urlencoded POST；raw factory 不安装 Codex default headers。 |
| raw auth factory | `login/src/auth/default_client.rs:314`；`http-client/src/client_builder.rs:68` | route-aware Auth client、关闭请求日志；由无 default_headers 的 builder 构造。 |
| 模型 provider Version | `model-provider-info/src/lib.rs` 的 OpenAI provider 构造 | Version 属于模型 provider headers，不是 token endpoint 默认身份的组成。 |

因此不能把所有 `/oauth/token` 调用统一套模型指纹头，也不能简单认为“OAuth 都不要 UA”。refresh 要保留其 default client identity，authorization code 则使用 raw client identity。

## 真实 loopback fail-before

`gateway_research_oauth_identity_test.go` 直接执行当前真实刷新入口 `p.refreshAccountWithFingerprint`，以及管理 code/refresh import 共用的 `p.managedOAuthRequest`，由实际 HTTP server 断言请求头、路径和 body。三种 settings：profile 关闭、linux、macos。

- profile 关闭的普通 refresh 原本没有 Version，作为控制通过；linux/macos refresh 原本带 Version=0.155.0，与 default auth client 不符。
- 管理 authorization code 三种设置都安装了 Codex UA/originator/Version，而官方使用 raw client。
- 管理 JSON refresh import 三种设置都带 Version，因为原来复用 `managedUpstreamHeaders`；default auth 不应有该模型头。
- 每次都核对真实 POST `/oauth/token`，refresh JSON 仅三个标准字段，保留账号发行 client_id、RT 和 grant_type；code form 的 code/verifier/client_id 不变；成功响应仍返回新 AT/RT。没有用无效 grant 或未知成员触发差异。

红日志 `.local/quality-20260925/oauth-identity/fail-before.jsonl`：两个顶级失败；包含子项为 1 通过、10 失败，package 0.105 秒。

## 修复

新增 `gateway_oauth_headers.go::applyGatewayOAuthHeaders`，供两个 token 构造点使用：

1. `proxy.go::refreshAccountWithSettings`：默认保留既有 UA/originator 策略；开启 profile 时保留所选完整参考 UA；只移除模型 provider Version。
2. `admin_account_oauth.go::managedOAuthRequest`：两个现存私有调用点按现有 content type 区分 code form 与 JSON RT import。code 使用 raw identity；RT import 使用 default identity。停止调用管理模型请求 header builder，因而不添加模型 Version/Authorization/Account-ID 或管理用 Accept。

raw 请求的 `User-Agent` map key 设为零个 value：Go transport 因 key 已存在而不自动补 `Go-http-client/1.1`；native IPC serializer 因没有 value 而不发一条空 UA。这比发送空字符串更接近无应用 UA 的官方 raw client。普通 loopback 已断言 header 本身不存在；Linux native 实际 helper 验证另设明确测试入口，未用 Windows 结果冒称已覆盖 Rust。

未改 POST body、client_id、proxy 选择、endpoint、scope、token 合并或账号 owner。管理 PAT/session 不调用这一 token helper，其已有契约不动。更新旧 `TestCodexFingerprintOAuthRefresh` 的 Version 期待，避免把旧差异继续固定为正确行为。

## 同步检查但未扩张修改的边界

- **账号与刷新 context**：`refreshAccountWithSettings` 保留既有 preflight，以及 admission 后独立 45 秒 `WithoutCancel` 预算，保证旋转 RT 可保存；携带同账号 native scope、禁用入站请求压缩。已知 owner 换 AT 保持 scope，身份迁移走既有 fail-closed 守卫。新增真实 refresh 断言返回后 owner ref 未改变。
- **出口**：官方 AuthRouteConfig 按 `ClientRouteClass::Auth` 选择系统/PAC/默认策略；网关仍使用被选账号明确绑定的代理，管理 code 可选指定代理。这是多账号网关的路由策略，不把它改成宿主环境自动代理。官方 code exchange 使用 issuer 的路由，网关当前则由已选择 account/proxy 构造 client；未盲目增设另一路径。
- **cookies**：default auth client 使用官方 ChatGPT 基础设施 jar，但 `chatgpt_cloudflare_cookies.rs:112` 只接受 HTTPS 的允许 ChatGPT 主机；`auth.openai.com` 不在允许集合。网关 native helper 同样复用该 host 规则。不能因 refresh 不携带 `__oailb` 就认为丢失了 auth.openai.com 所需 cookie，也不把模型 cookie 手工复制到 OAuth。
- **residency**：官方只有明确设置 requirements residency 时才加该头；当前 gateway settings 没有等价配置。没有证据支持从某次下游请求猜测并跨账号复制 residency；本修复不伪造它。
- **重定向**：网关客户端/native helper 明确不自动 follow，以保持已选路由和 OAuth 凭据边界；官方 route-aware builder 另有自己的 redirect 策略。本次没有为应用头一致性改变 token POST 的重定向和重试行为。
- **未开启 profile 的短 UA**：仍是旧策略 `codex_cli_rs/0.155.0`，不等同真实 CLI 的宿主 OS/terminal 字符串；开启 profile 也是确定性参考值，并非采集到的实体设备。本次不为了外观相同凭空模拟 Chrome、操作系统或终端。

## 测试与交付范围

Windows 本地关联回归 **64 个顶级通过、0 失败、5 个环境限定跳过**；包括子项共 199 通过、24 跳过，package 2.183 秒。green 日志为 `.local/quality-20260925/oauth-identity/green-associated.jsonl`。跳过包含 Linux native 与 PG/Redis 条件，不能计为通过。

```powershell
go test ./... -run '^(TestResearchOAuthIdentity|TestCodexFingerprint|TestManagedOAuth|TestSource.*Refresh|TestSource401|TestGateway.*Refresh|TestGateway.*Recovery|TestAudit.*Refresh|Test.*OAuth)' -count=1 -timeout=120s
```

`TestResearchOAuthIdentityNativeActualHTTPContracts` 提供 Linux opt-in 的真实 Rust helper → loopback HTTP 验收，覆盖三种 profile 下普通 refresh、code exchange、RT import 共九个最终子例。主任务在提供 `CODEX_NATIVE_HTTP_TEST_BIN` 的 Linux 验证中执行，本专项没有 SSH。

冻结摘要：

| 文件 | SHA-256 |
| --- | --- |
| proxy.go | `003c0993f5d9ef01d409d0429591ba9ccec5199260ebe2b80f1348365f867bd3` |
| admin_account_oauth.go | `e02e52514e87ccfa8de246c7af64effd3d4ce838bd983027ef745cff10223112` |
| gateway_oauth_headers.go | `fb1e2eee0dff8ac3ead80a71313c324373e87e531d8a48b5f439d1be99974945` |
| gateway_research_oauth_identity_test.go | `53b893d6d18dbe0996fbae58468aeeb1070d3672ba25b472ecd546e9a4f8e675` |
| codex_fingerprint_test.go | `a3a022e9227e7b20f4bfa28c5410935776b1b612d77a9d46ea678fa14b136ccd` |

这些证据证明正常 OAuth 应用头的两个确定差异及局部修复。真实 TLS 后端、官方完整 OAuth 的网络流量、服务端风险标记与账号解题恢复均不是这些 loopback 测试能够证明的命题。

最终主任务已回填：Linux 全套 880 顶级通过，其中九种 grant/profile 的实际 Rust→HTTP 叶子用例全部执行通过；CLI 独立 7 项通过。后端 `33f4535f…cdbe5e` 已上线，HTTP 200/WS 101 正常。[集成、发布与质量对照](../CODEX-GATEWAY-AUTH-CATALOG-RESEARCH-2026-09-25.md)保留完整证据边界。
