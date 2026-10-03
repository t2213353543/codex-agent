# 进程内模型目录预取与基础设施 cookie 三组对照

日期：2026-09-25。固定官方源码版本 0.155.0，固定 native helper SHA-256 `9110b869e65db6bf7664e39fcfe3b2ec17fa7a0207ddbeccf28d73dbc12ccce8`，HTTP lifecycle 为 `cli_request`。本文件是预登记建议、源码核对与零模型验证要求；不表示这 12 次真实推理已经执行，也不将计划视为结果。

## 1. 要回答的具体问题

比较同账号、同完整请求、同上游出口和固定 transport 下，模型 POST 前的 `/models` GET 及该 GET 的 helper 进程是否持续存在，会不会改变糖果题结果。最终预登记账号为 q0 和 g7，各组两次，原题原文不改；答案按用户预定 21 评分，同时保留原始回答和无输出/错误状态。

账号选择在本轮任何模型回答产生之前调整：最新 v6 inventory 中 g2、g4 的 usage 请求为 401，q0、g7 为 200 且 allowed。原方案 g2 的所有顺序位置统一替换成 g7，其余条件与镜像结构不变；绑定 v6 当前 user/token/workspace epoch。401 是认证失败，不能记作答题下降，也不通过刷新失败账号来改变本轮凭据状态。

| 组别 | 已登记操作 | 推理 POST 的进程状态 |
|---|---|---|
| `cold` | 新 P 进程，仅发一次完整 POST | 新进程，无该进程的预先 GET |
| `same_helper` | 新 P 先 GET `/backend-api/codex/models?client_version=0.155.0`；完整读取响应并确认 END，再经新的 IPC 请求发一次 POST | GET 与 POST 同 P，`cli_request` 每次仍创建新 HTTP client，进程内基础设施 jar 可延续 |
| `separate_helper` | 新 G 完整执行相同 GET，G 正常退出且 socket 清理完成；再新 P 发一次 POST | POST 进程没有接收 G 的 jar；服务端已观察相同账号/出口的一次 GET |

同 P 的 GET 后必须关闭该次 IPC stream，再打开独立 IPC 请求；不能把“同一 IPC 单请求管道里塞两条请求”误作同一 helper 的客户端生命周期。所有 P/G 只用于单格，不跨账号或重复格复用。

总预算为 **12 个模型 POST、8 个模型目录 GET**。不能在额度检查、探针 prewarm、失败重试、WS fallback 或工具调用里隐藏额外模型请求。GET 失败、非 200、模型目录不可解析、helper 不干净退出或身份 epoch 改变时，停止该格及依赖工作，不转换成 cold 后继续以同组记账。

## 2. 建议预登记镜像顺序

| 次序 | 账号 | 组别 | 重复 |
|---:|---|---|---:|
| 1 | q0 | cold | 1 |
| 2 | g7 | same_helper | 1 |
| 3 | q0 | separate_helper | 1 |
| 4 | g7 | cold | 1 |
| 5 | q0 | same_helper | 1 |
| 6 | g7 | separate_helper | 1 |
| 7 | g7 | separate_helper | 2 |
| 8 | q0 | same_helper | 2 |
| 9 | g7 | cold | 2 |
| 10 | q0 | separate_helper | 2 |
| 11 | g7 | same_helper | 2 |
| 12 | q0 | cold | 2 |

后六格是前六格完全逆序。每个账号/组别的两次位置之和均为 13，减少单向时间漂移与条件顺序完全重合。样本量小、顺序非随机，仍不足以消除服务端时变、采样随机性和残留状态影响；不能把两次都相同答案当成永久账号标签。

所有序列、模型、effort、完整请求 SHA、非认证 header SHA、helper/CLI SHA、账号 user/token/workspace epoch 应在看到本轮模型回答前写入不可覆盖的 plan。发生故障后重跑必须创建新 attempt 并保留原记录，不能从分母删除失败格或修改原顺序。

## 3. GET 与 POST 的官方 header 差异

官方链路是 `OpenAiModelsEndpoint::list_models()` → `ModelsClient::list_models(request_url, HeaderMap::new())` → `EndpointSession::execute_with(GET, body=None)`。模型目录请求没有会话/turn 的 extra_headers，也不带 JSON body。

| 字段 | `/models` GET | `/responses` POST |
|---|---|---|
| 路径/查询 | `/backend-api/codex/models?client_version=0.155.0` | `/backend-api/codex/responses` |
| Authorization、ChatGPT-Account-Id | 同一真实账号 bearer/workspace | 同一真实账号 bearer/workspace |
| Version | provider 的 `0.155.0` header | 同一 provider version |
| User-Agent | 默认 client 实际 process UA | 默认 process UA；不能人为换平台 |
| Originator | 默认 process originator | 默认值或明确 thread originator override |
| Residency | 默认 client 的受管地区要求存在时携带 | 同源地区要求 |
| Content-Type、Content-Encoding | 无 JSON entity，因此源码不添加 JSON media type 或 zstd | application/json；本轮按真实 CLI 捕获保持 zstd |
| Accept | ModelsClient 未显式设置；v6 真实 raw GET 观察为 `*/*` | ResponsesClient 显式 text/event-stream |
| session-id/thread-id/x-client-request-id | ModelsClient 不添加 | 官方会话投影 |
| x-codex-turn-metadata/window-id/beta-features/routing-hint | ModelsClient 不添加 | 当前会话和请求投影 |
| Responses Lite header | ModelsClient 不添加 | Lite 模型 POST 为 true |
| Cookie | 不由探针硬编码；允许的基础设施 jar 自行填入 | 同进程 jar 根据域/path/secure/expiry决定 |

**不能把现有 `usage_head()` 的 `Accept: application/json` 直接当作官方 ModelsClient 的最终合同。** 源码没有显式加这个 header；reqwest 可能有默认行为，需以真实 GET 捕获为依据。

v6 新捕获已经消除该项未知：[raw-models-head.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cookie-controls-v6/capture/raw-models-head.json) 记录真实 `/v1/models?client_version=0.155.0` 入站，四个非认证应用 header 为 `User-Agent: codex_exec/0.155.0 (CentOS 9.0.0; x86_64) unknown (codex_exec; 0.155.0)`、`Originator: codex_exec`、`Version: 0.155.0`、`Accept: */*`。没有观测到 residency header。原始捕获 model_requests=0；上游 path 为该版本 ModelsClient 的标准路径。这个证据来自实际 CLI HTTP parser 入口，不能扩大为原始网络 header 顺序/大小写的抓包证明。

同一个 `codex exec` 进程默认 originator 是 `codex_exec`，UA 由它和真实 OS/arch/终端构成。其模型 GET 不是强制 `codex_cli_rs`；此前单独 default-client usage 探针与 exec POST 的 UA 差异不能套到这次同进程官方 model GET。POST 的 thread originator override 也不能反向推断成 GET 的 process originator。

源码位置：

- [model-provider/src/models_endpoint.rs:75](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/model-provider/src/models_endpoint.rs:75)：当前账号认证、provider 与空 extra headers。
- [codex-api/src/endpoint/models.rs:37](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/codex-api/src/endpoint/models.rs:37)：精确版本 query 和 GET/body=None。
- [login/src/auth/default_client.rs:251](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/login/src/auth/default_client.rs:251)、[default_client.rs:335](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/login/src/auth/default_client.rs:335)：共同 client 构造与默认 headers。
- [http-client/src/request.rs:153](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/http-client/src/request.rs:153)：body=None 不追加 JSON Content-Type；有 JSON body 的路径才添加。
- [exec/src/lib.rs:260](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/exec/src/lib.rs:260)：exec 默认 originator。

## 4. 为什么 cli_request 仍可延续 cookie

helper `exchange()` 在 cli_request 模式为每次请求创建新 HTTP client，但 `build_http_client()` 为每个 client 注入官方 `with_chatgpt_cloudflare_cookie_store()`。该 store 是 Rust process-global `LazyLock<Arc<...>>`，所以 client 新建不等于 jar 清空。

该 jar 只接收官方允许的 HTTPS ChatGPT host，且名称限定为基础设施 cookies：`__cf_bm`、`__cflb`、`__cfruid`、`__cfseq`、`__cfwaitingroom`、`__oailb`、`_cfuvid`、`cf_clearance`、`cf_ob_info`、`cf_use_ob` 以及 `cf_chl_` 前缀。域/path/secure/过期仍由 cookie Jar 决定；不会把 account/session/auth cookie 自动持久化。helper 被退出后新的进程不会继承内存 jar。

来源：[native-transport/src/main.rs:309](C:/Users/Administrator/Desktop/项目/codex-agent/native-transport/src/main.rs:309)、[main.rs:435](C:/Users/Administrator/Desktop/项目/codex-agent/native-transport/src/main.rs:435)、[官方 chatgpt_cloudflare_cookies.rs:10](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/http-client/src/chatgpt_cloudflare_cookies.rs:10)。helper 9110 的构建记录表明此次变更仅修 WS output-drain，TLS 依赖与 vendor HTTP cookie 源文件保持固定版本，见 [build-manifest.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/lite-instructions-validation/build-manifest.json)。实际批次仍需核对 `/proc/<pid>/exe` SHA 与配置 lifecycle，不能仅信文件名。

## 5. 最便宜的零模型真实 cookie 边界验证

现有 [gateway_native_cookie_identity_test.go](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_native_cookie_identity_test.go:46) 已有可复用的真实 TLS/CONNECT fixture：只拨 loopback，以临时 CA 校验虚拟 `chatgpt.com` 证书；返回 `__oailb`、`__cf_bm` 和一个不应保存的 `session` cookie；上游记录实际 Cookie、连接 ID、HTTP 协议和 TLS resumed。没有真实账号或外部模型。

最小已有回归命令，在已经配置固定 helper 的 Linux 环境中运行：

```sh
go test -count=1 -run '^TestGatewayNativeActualCookie(CredentialIsolation|SeparateProcessesControl)/cli_request$' -v .
```

`CredentialIsolation/cli_request` 每次新建 Go client，断言同 owner helper 仍带基础设施 cookies，同时每次实际连接 ID 不同；这证明延续来源不是偷偷把 cli_request 变成 pooled。`SeparateProcessesControl/cli_request` 证明两个 helper 的 jar 互不串用，但其原案例同时换了凭据，因此尚不是本轮“同凭据 GET 进程退出”的精确对照。

本审计实际读取了最近固定 helper 9110 的 Linux 全测 [tests.jsonl](C:/Users/Administrator/Desktop/项目/codex-agent/.local/gateway-audit-20260924/go-lite-instructions-final-20260925/tests.jsonl)：这两个具体测试分别在 `06:51:23Z` 与 `06:51:24Z` 通过。CredentialIsolation 的九个请求依次使用连接 ID 1…9，HTTP/2、TLS resumed=false，cookie_isolation_correct=true；其中 first_A → same_A 明确由空 jar 到保留 route cookie，未通过连接复用偷换 cli_request 语义。这是已完成的旧 fixture 实证，和下一段新工具自证分开记录。

新 runner 的真实性还需要一个直接调用新工具的最小 fixture：

1. 合成同一 AT/workspace，真实 TLS 虚拟 chatgpt.com `/models` 返回允许基础设施 cookie + 不允许 session cookie + 合法完整目录，读到 END。
2. 同一 helper、独立新 IPC、实际 POST 接收正确的 allowed cookies，且连接 ID 不同；body/hash 一致，不允许 caller Cookie header。
3. 先令 GET helper 正常退出/回收，再以完全相同 AT/workspace 新建 POST helper；实际 POST Cookie 为空。
4. cold 新 P 同样为空；三个 POST 都只打 loopback合成响应，零真实模型。
5. 暂时禁用 GET 或提前重建 helper 的 fault injection 必须让 same_helper 断言失败，避免工具只是把“预热成功”写进报告却没有共享 jar。

自定义 CA 会改变测试 TLS backend 选择，因此这个 fixture 能证明 cookie、process/client/IPC 生命周期和秘密隔离，**不能证明生产无 CA 的 ClientHello 与测试完全相同**。生产批次不得继承 fixture 的 CA/代理环境。

## 6. 执行和结果解释边界

两个 GET 组的 GET header、完整目录响应哈希、HTTP status/version、响应 Cookie 名称或值摘要、读取 END 与 helper 状态都应保留。不能输出 Cookie 原值、AT、workspace/email明文。若线上未观测到 Set-Cookie，应记录“GET响应无已观测 cookie”，不能仍说同helper组一定使用了不同 cookie。

同 helper 的两个 IPC 必须在同一已核验进程实例中执行；separate 的 G/P 必须不同、G 已退出再启动 P；每个模型 POST 只能 dispatch 一次。PID 数字可被操作系统重用，应结合进程启动记录、Popen 句柄、`/proc/<pid>/exe` hash 和私有 socket identity 验证，不仅比较两个 PID 数字。这里“完整 GET”指成功收到实体末尾、没有 frame error/timeout/提前关闭，再检查目录 schema，不是只读 200 headers 后开始 POST。

GET 先行除了 cookie 也可能改变后端账号、出口、限额、缓存和路由状态；两个请求还引入了时间差。因此：

- same_helper 对 cold 有差异，只能先说明“GET 加同进程状态”相关，不能单独锁定 cookie。
- separate_helper 对 cold 有差异，说明即使 POST jar 为新进程，GET 顺序/服务端或时间因素仍可能参与。
- same_helper 对 separate_helper 有差异，更接近“客户端进程内状态”因素，但仍不是 pure-cookie 随机因果证明；还需上游 Cookie 实际证据与更多复现。
- 三组相同不能证明 cookie 从不影响结果，只能收窄本批固定条件下的解释。

使用固定捕获 body 和固定 session/cache key 能减少客户端输入变量，但服务端 cache/history 的状态不因此独立。原生 helper fresh process 不等于服务端“全新会话”。新捕获阶段应把模型 POST 截获在本地 synthetic peer，只允许模型目录 GET 出网，完整保存新 capture SHA 与边界；上一轮已删除的 raw 请求不得从摘要伪造重建。捕获阶段本身会产生一个先前模型目录 GET，所以 cold 只表示该次 P 无预先 GET，不表示该账号或出口此前从未请求目录。

v6 新 raw HTTP body 已实际捕获，SHA-256 为 `f8147e587f877b1a518e2f0e5651d3b1c860e6c63950c83f4c2c884dd3256f3d`，11 个顶层字段。原问题 SHA 仍为 `cd771ba38f8f75696ba703c7d3e91e5273588c91cb8c4145e3ce4b077c075c3f`。这些新 pins 需写入最终 12 格计划；不能继续引用 v5 已删除 body 的旧 pins。

## 7. 新工具独立审阅记录

本审计只读审阅 [quality_cookie_lifecycle_probe.py](C:/Users/Administrator/Desktop/项目/codex-agent/tools/quality_cookie_lifecycle_probe.py) 和其测试，没有修改工具。确认 GET 和 POST 经独立 IPC socket，same_helper 保持同 scope/PID，separate_helper 在 GET 正常 END 且 G 清理成功前不启动 P。process startup 固定 cli_request，执行文件实际 SHA 校验；没有自动刷新、重试或 transport fallback，原 POST body 不重序列化。

审阅发现并交由作者修正：显式捕获 GET headers 原先仅检查 UA/originator，漏 Version/residency一致性；partial-start 的已关闭管道可能因 ValueError 使 cleanup 逸出；模型目录原先仅检查数组对象，不能证明目标模型存在。新版本分别补齐身份 header 一致性、收据内清理异常处理、目录 required schema 与目标 model 唯一检查，并保存完整模型对象的摘要，不记录原始 prompt。

本审计实际运行当时版本的 22 个工具测试：20 通过、2 个真实 helper 测试在 Windows 跳过；同时运行另一 reviewer 的 7 个独立 fault-injection 测试全部通过。覆盖 GET 截断/错误/重复字段/无目标模型阻断 POST、GET 花掉deadline不重置POST预算、helper意外退出不偷偷重启、separate清理失败不继续、post完成但cleanup失败不能标可评分、Set-Cookie/error秘密摘要。随后作者在 Linux 固定 helper 9110 上完成最终 23 项工具测试，23 通过、0 跳过、0 失败；本审计已直接读取下方 summary 与实际日志，补齐这两项实际 helper 边界。

新增 TLS 测试源码已独立检查并已实际通过：临时 CA 签发独立 `CA:false` 的 chatgpt.com SAN 叶证书、真实 SNI、loopback CONNECT 限定、完全相同合成 AT/workspace 运行三组，same_helper POST 携带 __oailb 与 __cf_bm，cold/separate POST 无 cookie，session cookie 和错误 path cookie 均未发送；五次实际 TLS handshake 的 SNI 均为 chatgpt.com。CA/proxy只在测试的进程启动注入，公开执行接口保持无自定义 CA/代理。首次把自签 CA 直接用作叶证书遭验证拒绝，测试正确改为独立签发叶证书后通过，没有关闭证书验证。

最终工具 SHA 为 `22c78f9ed0293633a8da74e89cb22ff95d539c6e5d3a1ffc152d7e1b4745be31`，测试 SHA 为 `962c5177cef92da155c40e9b39e8941003f3cf1e3675b2f6d1ebc49365d99474`。证据：[summary.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cookie-controls-v6/lifecycle-tool-loopback/summary.json)、[tests-final.log](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cookie-controls-v6/lifecycle-tool-loopback/tests-final.log)。这完成工具的真实 cookie 生命周期自证，不改变第 6 节线上 GET 干预并非纯 cookie 因果的限制。
