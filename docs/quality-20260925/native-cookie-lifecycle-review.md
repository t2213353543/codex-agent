# 官方 CLI / native helper 的 Cookie 生命周期核对

2026-09-25，只读源码与已保存的无敏感证据；本专项没有修改工具、网关或 helper，没有发起 GET、模型请求或新对照实验。

结论：**官方 CLI 的基础设施 CookieJar 在整个进程内共享。`cli_request` 每次新建 HTTP client 不会清空该 jar；native helper 使用同一份上游代码，也有相同语义。相同 helper 内先 GET 再 POST 与两个全新 helper 各执行一次请求，Cookie 生命周期不同。** v5 固定 HTTP 请求每格只启动一个新 helper 并直接 POST，没有完整 CLI 可能执行的 models GET。这个差异值得单独控制，但现有证据不能把它认定为 q0 的 429 原因。

## 源码链路

| 边界 | 源码事实 | 影响 |
| --- | --- | --- |
| 官方共享 jar | [chatgpt_cloudflare_cookies.rs:14](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/http-client/src/chatgpt_cloudflare_cookies.rs:14) 是 `static LazyLock<Arc<ChatGptCloudflareCookieStore>>`；`:109` 把 `Arc::clone` 交给 `cookie_provider` | 重新构造 reqwest client 或 factory 不等于建立空 jar |
| 默认 HTTP client | [default_client.rs:296](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/login/src/auth/default_client.rs:296) 总是启用 `with_chatgpt_cloudflare_cookie_store`；[client_builder.rs:291](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/http-client/src/client_builder.rs:291) 选择该共享 provider | 普通 Codex HTTP 请求可继承本进程先前响应设置的允许 Cookie |
| factory 的额外 Cookie | [outbound_proxy.rs:197](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/http-client/src/outbound_proxy.rs:197) `new` 的 `chatgpt_cookie_store=None` 指没有额外配置；`:205` 创建的额外 store 内部仍 clone 基础设施全局 jar | `None` 不能解释为“禁用所有 Cookie”；配置 Cookie 与允许自动收集的基础设施 Cookie 是两层 |
| models GET | [models_endpoint.rs:109](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/model-provider/src/models_endpoint.rs:109) 构建 transport；`:200` 调用 `create_client_for_route_async`；[models.rs:46](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/codex-api/src/endpoint/models.rs:46) 发 GET | 若真实 models GET 响应设置允许 Cookie，后续另一 client 的 Responses POST 可读到 |
| Responses POST | [client.rs:1039](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:1039) 每次构造 API transport 时调用 `create_client_for_route` | 每次请求新 client，但它们仍在同一 CLI 进程中共享 jar |
| helper 初始化 | [main.rs:283](C:/Users/Administrator/Desktop/项目/codex-agent/native-transport/src/main.rs:283) 建 State；`:336` 每个 HTTP client builder 都使用 `with_chatgpt_cloudflare_cookie_store` | jar 属于 helper 进程的静态变量，不局限于单个 IPC socket 或 State 的 client 字段 |
| helper `cli_request` | [main.rs:443](C:/Users/Administrator/Desktop/项目/codex-agent/native-transport/src/main.rs:443) 每次 HTTP 请求重新 `build_http_client` | TCP/TLS 连接池生命周期改变，进程全局 CookieJar 没有因此被清空 |
| helper `pooled` | [main.rs:298](C:/Users/Administrator/Desktop/项目/codex-agent/native-transport/src/main.rs:298) 保留 State client | 同时复用 client 与进程 jar；不能把 `pooled` 与 `cli_request` 的差别全部归为 Cookie |

限定域与内容：jar 只对允许的 **HTTPS ChatGPT 主机** 工作，[chatgpt_cloudflare_cookies.rs:112](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/http-client/src/chatgpt_cloudflare_cookies.rs:112) 与 [chatgpt_hosts.rs:3](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/http-client/src/chatgpt_hosts.rs:3)。名称白名单在前文件 `:158`，包括 `__cf_bm`、`__cflb`、`__cfruid`、`__cfseq`、`__cfwaitingroom`、`__oailb`、`_cfuvid`、`cf_clearance`、`cf_ob_info`、`cf_use_ob` 和 `cf_chl_` 前缀。仍遵守 Cookie domain/path/expiry 规则；不能只见 Set-Cookie 头就假定下一条 POST 一定带它。这里的 `__oailb` 在源码中被定义为路由 Cookie，不能据此推断其路由的是算力等级。

helper 的 vendored `chatgpt_cloudflare_cookies.rs`、`client_builder.rs`、`outbound_proxy.rs` 与锁定 CLI 0.155.0 原文件逐字一致。本次实际计算原文件摘要分别为：

- `070912085118365df3a9944f8b595527110c4113b34b3a3e6e7e35d80c9a8011`
- `a135ed62bb9e55a8bf091a57d0c5760e3ad453776825d86b2a40300a3303667d`
- `0486978806a9cd5c8f552e985ed9b0c987392cdcdae35ec8b0a555539221aa52`

## helper、网关与 WS 的不同作用域

| 执行方式 | 后续 HTTP POST 能否继承前请求的允许 Cookie |
| --- | --- |
| 完整 CLI，同一进程内 models GET 后 POST | 可以；是否实际带 Cookie 还取决于响应内容与 path/domain/expiry |
| 一个 native helper，两个不同 IPC 连接分别 GET 和 POST，`cli_request` | 可以；新 HTTP client 共享该 helper 的静态 jar |
| 两个全新 native helper，先在 A GET 再在 B POST | 不会继承 A 的进程 jar |
| v5 usage-before helper 后另起模型 helper | usage helper 的 Cookie 不会进入模型 helper |
| 网关同账号 HTTP models 与 Responses | helper key 的账号、代理、origin、timeout 等都相同且 helper 仍存活时可以继承；不是保证每个 GET 与后续 POST 都选同一账号 |
| HTTP GET 后同 helper 发 WSS upgrade | 官方及 native WS connector 没有自动接入 reqwest CookieJar；不能把 HTTP jar 语义直接套到 WS |

网关 [gateway_native_http.go:744](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_native_http.go:744) 的 helper key 包含账号作用域、指纹/代理、origin、timeout；[gateway_upstream.go:40](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_upstream.go:40) 在 dispatch 注入实际所选 owner。`forwardModels` 的 [gateway.go:946](C:/Users/Administrator/Desktop/项目/codex-agent/gateway.go:946) 只尊重已经存在的显式 session 绑定，不为未绑定 GET 创设会话。因此多账号网关中，不可仅按先后顺序认定 models 与 POST 的 Cookie 来源相同。

这项隔离比官方单进程共享基础设施 jar 更严格，是网关多账号设计。它不保存用户会话/认证 Cookie。是否应在真实多账号池中改变这种隔离需要独立需求与验证，不能为了复刻单 CLI 生命周期而把所有账号放进一个 jar。

## 已有真实 native 合成证据

本专项只读取既有 [tests.jsonl](C:/Users/Administrator/Desktop/项目/codex-agent/.local/gateway-audit-20260924/go-identity-cache-newnative-20260925/tests.jsonl)，没有重跑。

四组 `TestGatewayNativeActualCookieCredentialIsolation`、`…SeparateProcessesControl`、`…TrustedOwnerContinuity`、`…HTTPWSBoundary` 共 **8 个 lifecycle 子项全部 PASS**（另有四个父项 PASS）。对应 [gateway_native_cookie_identity_test.go:224](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_native_cookie_identity_test.go:224) 等真实 TLS loopback 测试表明：

- `cli_request` 的 same-A 请求已建立第二条 HTTP/2 连接，`tls_resumed=false`，仍拿到 A 先前允许的 Cookie；因此 Cookie 连续性不依赖 TLS 会话复用。
- 两个 helper 进程分别保存 A/B 的 jar，互不继承。
- 同工作区不同用户也不串 jar；已知 owner 正常刷新保持连续性，unknown-owner 凭据更换分隔作用域。
- HTTP→WS 的合成三次 upgrade 中 `cookie_empty=true`，后续 HTTP 仍保留原 HTTP jar；WS 返回的 Set-Cookie 没有替换 HTTP jar。

这些验证针对合成 Cookie 生命周期，不证明生产 q0 或其他账号取得了任何特定 Cookie，也不证明 Cookie 导致 429。

## v5 捕获和 q0 现有结果能证明什么

原捕获包装器 [raw_capture_test.go:52](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/http-ws-controls-v5/raw_capture_test.go:52) 会排除 Cookie，且仅在实际看到该 header 时把名称放进 `excluded_header_names`。保存的 [raw-capture.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/http-ws-controls-v5/capture/raw-capture.json) 该数组仅为 `authorization`、`chatgpt-account-id`、`content-encoding`、`content-length`，**不包含 Cookie**。所以本次捕获入口没有可见 Cookie 被静默过滤。

但捕获的目标是 `http://127.0.0.1` 的 capture-only 服务，[capture_remote.py:61](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/http-ws-controls-v5/capture_remote.py:61) 修改 custom provider base URL 到该地址。该目的地不满足官方 jar 的 HTTPS ChatGPT 条件；捕获不能证明自然官方直连 POST 没有 Cookie，也不能保留一次真实 `/models` 响应对真实 ChatGPT 域 jar 的影响。

此外，`quality_native_ipc_capture.summarize_head` 只观察进入 Rust 之前显式 header。即使 IPC 摘要没有 Cookie，Rust cookie_provider 仍可能在构建实际 HTTP wire request 时添加 Cookie；它不是最终 wire header 抓包。

现有 q0 记录：

| 记录 | 实际结果 | 已保存 Set-Cookie 头存在性 |
| --- | --- | --- |
| `01-h-q0` | HTTP 429，294 ms | 存在 |
| `12-h-q0` | HTTP 429，319 ms | 存在 |
| `04-w-q0` | WS 101、完成、29 | 握手响应存在 |
| `09-w-q0` | WS 101、完成、29 | 握手响应存在 |

以上仅通过现有 `response_header_hashes` 的名称 SHA 与 `sha256("set-cookie")` 比较得到，不读取/披露 Cookie 原值。无法从这些摘要识别名称、属性、是否允许或后续发送情况；429 响应自身设置 Cookie 也不等于请求前缺 Cookie 就是 429 原因。

自然 CLI HTTP q0 第一格的 [report.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/http-ws-controls-v5/q0-http-natural-followup/01-q0/cli/report.json) 显示 fresh isolated home、无预注入 catalog、结束时 `models_cache.json` 存在且解析有效、版本匹配、请求模型存在。源码 [models-manager/manager.rs:460](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/models-manager/src/manager.rs:460) 的 `OnlineIfUncached` 会尝试缓存后 fetch。二者支持本次 CLI 获得了 catalog，但报告不是 raw HTTP trace，不能凭它精确证明 models GET 次数、完成在 POST 前、Cookie 内容或 POST 携带情况。

## 下一步最小对照设计（尚未执行）

首先沿用已验证 helper `9110b869e65db6bf7664e39fcfe3b2ec17fa7a0207ddbeccf28d73dbc12ccce8`、同一真实平台、相同账号 epoch、官方 HTTPS origin、`cli_request`。不使用显式 Cookie 注入、不改 TLS/profile、不重写题目，不在 429 后重试。

三种预声明条件足以分开“先 GET 的服务端影响”和“同进程 jar 连续性”：

1. **cold**：全新 helper P，只发固定完整 POST，一次，结束并回收 P。
2. **same-helper**：全新 helper P 发一次 `GET /backend-api/codex/models?client_version=0.155.0`；完整收到 HTTP header/body/END 后，保留 P 进程和 stdin 存活，关闭该 IPC socket；重新连接 P 的同一 UNIX socket，一次发送同 bytes 的 POST；之后回收 P。
3. **separate-helper**：全新 helper G 发送同 GET 并完整完成，关闭和回收 G；全新 helper P 发送同一次固定 POST；之后回收 P。

关键执行约束：helper IPC 每个请求使用独立连接；**不能再次调用当前 `native_request` 来实现 same-helper**，因为该函数每次必建新进程。需要把 helper lifecycle 提升到一个 cell session 的外层；只在其中复用 process/socket-path，HTTP GET 与 POST 仍各有独立 client（`cli_request`）与 IPC transaction。GET 失败、身份 epoch 变化、超时或清理失败都结束该 cell，不补发 POST。所有模型 dispatch 最多一次，记录每次 helper PID 的摘要/作用域以证明是否同进程。

一个有重复的有界设计是 q0 与 g2 各三种条件，前六格按已登记随机排列，后六格反向镜像：**12 个模型 POST 上限，same/separate 条件各自仅 1 个先行 GET**。usage-before/after 继续在独立 helper 中进行，让三组都不把 usage jar 混入推理进程。登记模型、effort、完整 body hash、非认证 header 摘要、账号 user/token/workspace hashes、GET path、所有实际调用顺序和 cleanup，再看结果。也可先仅 q0 三条件跑一轮（3 POST），但不能由单轮估计稳定率。

需要记录的安全证据：models GET HTTP status/完整性、Set-Cookie **名称与属性分类/值 SHA**（不得原值落盘）、模型 POST status/时序/final/usage、helper 生命周期与前后 epoch、完整 request body hash。若不能观察 Rust 最终出站 Cookie，只能称为“先行 GET + helper 生命周期干预”，不能声称实际 Cookie 是唯一改变；同进程还可能保留其他过程状态，先行 GET 也可能改变服务端状态。若后续必须隔离纯 Cookie 因果，应先做显式受控的合成 HTTPS loopback，再设计保持其他状态相同的单变量试验，不凭本报告直接修改生产。

现成的无模型 synthetic 复跑入口（本次未调用）：

```bash
CODEX_NATIVE_HTTP_TEST_BIN=/opt/codex-agent/native-research-releases/9110b869e65db6bf7664e39fcfe3b2ec17fa7a0207ddbeccf28d73dbc12ccce8/codex-native-transport go test . -run 'TestGatewayNativeActualCookie(CredentialIsolation|SeparateProcessesControl|TrustedOwnerContinuity|HTTPWSBoundary)' -count=1 -v
```

这次调查找到了需要控制的具体环境差异；尚未执行上述冷/暖 POST 对照，不能据此认定 q0 被标记、进入某类集群，或断言 Cookie 修复会把糖果题答案恢复为 21。
