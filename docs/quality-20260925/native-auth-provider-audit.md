# Codex 0.155.0 原生认证与 HTTP 对照路径审计

审计日期：2026-09-25。范围为固定版本本地源码、现有测试工具和已保存的脱敏结果；本审计没有读取真实令牌，没有发起模型请求。源码根目录为 `C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs`。

**结论先行：此前 8 次官方 CLI HTTP 对照没有走 `env_key` / API-key 认证分支。** 实际工具使用 `name="OpenAI"`、`requires_openai_auth=true`，并向隔离的 `auth.json` 写入 `auth_mode="chatgpt"`。它保留了 ChatGPT 模型目录、请求压缩和多数 Codex 专属请求构造逻辑。需要修正的是认证生命周期和基准描述，不能把未经核对的“自定义 provider 导致模型降级”作为根因。

初审定位的另一个重要缺口是：8 次 CLI 对照只保存了模型目录中目标模型存在与 `tool_mode` 等摘要，没有保存该账号模型记录的全部行为字段及摘要哈希。因此相同问题、模型名、配置，不等于已经证明最终 instructions、Responses Lite 与默认参数完全相同。随后已完成八账号原生 `/models` GET 对照：**当前九模型目录的所有字段在 28 对账号组合中完全相同**，独立核验见第 8 节。这收窄了下一步，不能再把当前公开模型目录差异当作已发现根因。

## 1. 审计对象和实际执行路径

审计开始时 [quality_official_cli_probe.py](C:/Users/Administrator/Desktop/项目/codex-agent/tools/quality_official_cli_probe.py:94) 的 SHA-256 为 `975f1fd783dd343dc84e3765e09aae38c004ae187feab4063a38716e6ea4a84d`，与 `live-account-controls-v3/tooling-manifest.json` 和 `environment-tooling-validation/tooling/quality_official_cli_probe.py` 的冻结副本一致。随后本审计修复了该工具的显式 ID token 用户混配检查；冻结副本和既有实验结果没有修改。

`official_cli_followup_remote.py` 以及 Q0 补充对照调用的是这个工具，传入 `--websocket false`。实际配置为：

```toml
model_provider = "official_http_reference"
[model_providers.official_http_reference]
name = "OpenAI"
base_url = "https://chatgpt.com/backend-api/codex"
wire_api = "responses"
requires_openai_auth = true
supports_websockets = false
supports_standalone_web_search = true
request_max_retries = 0
stream_max_retries = 0
[model_providers.official_http_reference.http_headers]
version = "0.155.0"
```

没有 `env_key`、`experimental_bearer_token`、provider `auth.command` 或 AWS 认证。隔离环境也不继承 `OPENAI_API_KEY`、`CODEX_API_KEY`、代理、CA、用户配置和 `CODEX_*` 路由变量。

源码 [model-provider-info/src/lib.rs](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/model-provider-info/src/lib.rs:526) 的 `is_openai()` 检查的是 provider 的 **name 等于 OpenAI**，不是配置 ID 等于 `openai`。`merge_configured_model_providers()` 对已经存在的内置 OpenAI provider 使用 `entry(...).or_insert(...)`，所以设置 `[model_providers.openai]` 不能覆盖内置参数；此前为了关闭 WS 和重试使用另一配置 ID 有具体源码依据。

内置 provider 另外带有 `OpenAI-Organization` / `OpenAI-Project` 环境 header 映射。当前隔离环境不提供这两个变量，所以该差异在本轮无实际 header 值影响。

## 2. 请求行为逐项比较

| 项目 | 现有 HTTP 对照 | 内置 OpenAI + 外部 ChatGPT AT | 源码判断和影响 |
|---|---|---|---|
| 认证对象 | `CodexAuth::Chatgpt` | `CodexAuth::ChatgptAuthTokens` | `auth_mode()` 均归一为 Chatgpt；`api_auth_mode()` 区分具体生命周期。 |
| 目标 endpoint | 显式官方 Codex base URL | ChatGPT 认证选择官方 Codex 默认 URL | `to_api_provider()`，不是 public `/v1` 路由。 |
| Authorization | 真实 AT bearer | 同一真实 AT bearer | 都通过 `auth_provider_from_auth()`；不会把 identity token 用作请求 bearer。 |
| 账号 header | auth tokens 的 account ID | 客户端提交的真实 workspace ID | 两者都会生成 `ChatGPT-Account-ID`；必须在探针入口核对其与 AT claims 相符。 |
| 计划、email、用户 | 从 `tokens.id_token` 解码 | 官方构造函数从 AT 解码 | 允许显式 plan override 的外部 API 不应在对照中人为填入更高计划。 |
| 模型目录 | 会获取账号的 `/models` | 会获取账号的 `/models` | `uses_codex_backend()` 为真；不会因自定义 provider ID 自动退成 API-key 目录。 |
| 模型 cache | provider + 认证具体模式 + 用户/账号/计划等绑定 | 同左，但精确认证模式改变 cache identity | 两种模式不能直接共用旧目录缓存。本批每调用 fresh home。 |
| instructions / prompt | 来自目标 ModelInfo、配置和会话构造 | 同一机制 | 是否相同依赖模型目录内容与最终会话上下文，不能仅靠模型名认定。 |
| Responses Lite | 由 `model_info.use_responses_lite` 控制 | 同一机制 | 不是简单的 “自定义 provider=false”。 |
| HTTP zstd 请求压缩 | 默认开启条件成立 | 默认开启条件成立 | 要求 feature 开启、认证 uses Codex backend、provider name 为 OpenAI；不要求 provider ID 为 openai。 |
| WS | 明确关闭 | 内置支持，存在默认 WS 与 fallback 行为 | 这是本轮基准真实且重要的 transport 差异。 |
| 重试 | request/stream 重试都设为 0 | 使用内置策略 | 真实模型对照必须记录实际尝试数；不可把隐藏重试声称为单次调用。 |
| routing hint | 条件成立，会按请求 model/tier 构造 | 同左 | `uses_codex_backend()` 还要求 requires_openai_auth 且无显式 env bearer 等 override。 |
| metadata | 官方会话/turn/installation 等生成 | 同一核心机制；app-server 会话来源不同 | 两条路径不能按整个 body 字节相同来描述。 |
| attestation | provider 可支持，但需要实际 attestation provider | external ChatGPT 同样可支持 | 缺少 host attestation provider 时不会凭空生成 `x-oai-attestation`。 |
| agent identity bootstrap | 只有 `use_agent_identity=true` 时托管 Chatgpt 才可能进入 | external AT 不进入该托管认证专属 bootstrap | 0.155.0 此 feature 默认 false；现有隔离配置没有启用，不能把理论分支说成已发生。 |
| remote compaction | name=OpenAI，支持 V2 | 同左 | 本轮单轮糖果题没有证明发生压缩会话调用；不要混淆 HTTP zstd 与会话 compaction。 |
| 明确 cyber access program | 有一个 provider ID 检查不会接受自定义 ID | 内置 ID 允许该字段进入后续条件 | 当前题目未提交该选项，不是已证实差异来源。 |

主要源码作用点：

- [login/src/auth/manager.rs:483](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/login/src/auth/manager.rs:483)：effective mode、精确 mode、ChatGPT/Codex backend 判断。
- [model-provider/src/auth.rs:198](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/model-provider/src/auth.rs:198)：显式 bearer 优先级、托管认证及原生 header 生成。
- [model-provider/src/models_endpoint.rs:75](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/model-provider/src/models_endpoint.rs:75) 和 [models-manager/src/manager.rs:511](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/models-manager/src/manager.rs:511)：模型目录获取、认证边界与目录应用。
- [model-provider/src/models_identity.rs:15](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/model-provider/src/models_identity.rs:15)：模型 cache 的真实身份绑定字段。
- [core/src/client.rs:758](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:758)：reasoning 与 Responses Lite 请求体构造。
- [core/src/client.rs:977](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:977)：Codex backend、routing hint 和 Guardian 条件。
- [core/src/client.rs:1449](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:1449)：HTTP 请求压缩。
- [core/src/session/session.rs:1539](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/session/session.rs:1539)、[features/src/lib.rs:1771](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/features/src/lib.rs:1771)：agent identity feature 默认值及入口。
- [core/src/session/turn_context.rs:1120](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/session/turn_context.rs:1120)：明确 cyber program 的 provider ID 特例。

### Responses Lite 不是无关紧要的开关

`use_responses_lite=true` 时，官方会把 tools 和 base instructions 作为带稳定 item ID 的输入前缀，顶层 `instructions` 为空，顶层 tools 不按普通模式序列化，并设置 `reasoning.context="all_turns"`。普通模式保留顶层 instructions/tools，reasoning.context 留给服务端默认值。HTTP 使用 `x-openai-internal-codex-responses-lite` header；WS 还通过 `ws_request_header_x_openai_internal_codex_responses_lite` client metadata 投影。对应分支位于 `core/src/client.rs:758–889`、`:1213–1245`、`:2074`。

这说明应比较模型目录及最终发出的结构，而不应靠 UA 相同就认定协议输入一致。反过来，此前 16 次固定字节重放已固定这个因素；其中的账号差异不能重新解释为每次 CLI 自动换了目录。

### TLS 与指纹的边界

两种认证模式进入相同 HTTP client factory，认证模式本身没有选择 TLS 后端的分支。当前 HTTP 对照没有 CA override；源码 custom CA 分支可能强制 rustls，WS 则走其 rustls connector。切换 builtin WS 会同时改变 transport、握手、连接复用、WS 会话状态和增量请求，不能把它包装成只改一个 auth_mode 的实验。

本审计没有捕获 ClientHello，也没有以源码推导冒充实际线上 TLS 指纹相同。相关入口见 [http-client/src/custom_ca.rs:296](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/http-client/src/custom_ca.rs:296) 和 `core/src/client.rs` 的 HTTP/WS transport 创建。

## 3. 现有 auth.json 的真实问题

工具 `load_credential()` 在无独立 ID token 时，把**未经修改的完整 AT**放入 `tokens.id_token`，并写 `auth_mode="chatgpt"`、空 refresh token、当前 last_refresh。没有制造 JWT claims 或签名。固定版本 `parse_chatgpt_jwt_claims()` 可以解析这个字段，而官方外部 AT 构造函数自身也把 AT 解析成内部 `IdTokenInfo`；所以不能把这一行为错误描述为伪造 JWT 或必然认证失败。

不过，这个容器把 externally supplied AT 放进了 managed Chatgpt 生命周期。官方 `Chatgpt` 支持即将过期时主动刷新；真正 `ChatgptAuthTokens` 不使用这个主动刷新分支，而由宿主处理更新。现有探针以“有效期大于 timeout + 300 秒”和关闭 refresh endpoint 降低碰到该分支的机会，但它仍不是原生 external auth API。

此外，发现并修复了一个独立入口缺口：若输入显式带 `id_token`，旧工具只检查 ID token 与 AT 的 workspace 相同，未检查两者稳定 user identity 一致。同工作区不同用户的 ID token 可混配，导致 CLI 本地 account metadata 和模型 cache 归属错误。修复后按官方字段优先级比较 `chatgpt_user_id`，缺失/null 时使用 `user_id`；缺少可比较用户、格式无效或用户不相同都拒绝。email 与 `sub` 不用作用户身份替代，避免语义不一致的错误匹配。

本轮 24 格输入没有独立 ID token，所以这不是本批错答的已证实原因；单 AT 原样用于本地 claims 的路径保持不变。新外部 AT 探针只从该 AT 取身份，避免这类混配。

官方源码位置：[login/src/auth/manager.rs:1738](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/login/src/auth/manager.rs:1738)、[login/src/token_data.rs:174](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/login/src/token_data.rs:174)、[login/src/auth/manager.rs:2957](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/login/src/auth/manager.rs:2957)。

## 4. 原生 external ChatGPT token 的正确入口

固定版本 app-server 实现了 `account/login/start` 的 `chatgptAuthTokens` variant，接受已有 AT 和 workspace ID，不需要独立 ID token 或 refresh token。这是该版本源码实现的实验接口，源码把它标为内部实验用途；不能称为稳定公开的跨版本保证。

建议使用这个 API，保留内置 `model_provider="openai"`，让 CLI 原生代码构造认证对象。没有必要在 probe 中自己制造 ID token。

启动进程命令：

```text
codex app-server
```

stdin/stdout 是逐行 JSON-RPC。最小零 turn 序列如下；`<EXISTING_ACCESS_TOKEN>` 和 `<REAL_WORKSPACE_ID>` 只在私有进程内填入，不能写入公开日志：

```json
{"id":1,"method":"initialize","params":{"clientInfo":{"name":"codex_quality_native_auth_probe","title":null,"version":"1"},"capabilities":{"experimentalApi":true,"requestAttestation":false}}}
{"method":"initialized","params":{}}
{"id":2,"method":"account/login/start","params":{"type":"chatgptAuthTokens","accessToken":"<EXISTING_ACCESS_TOKEN>","chatgptAccountId":"<REAL_WORKSPACE_ID>"}}
{"id":3,"method":"account/read","params":{"refreshToken":false}}
```

实现必须逐步等待对应响应，不能只是一次写入所有行后按固定顺序读取。异步 notifications 可以插入响应之间。

应验证：

1. `initialize` 返回的 `userAgent`、`platformFamily`、`platformOs` 和 `codexHome`；只保存必要的脱敏事实。
2. login 响应 `type="chatgptAuthTokens"`。
3. `account/updated` notification 的 `authMode="chatgptAuthTokens"`；login completed notification 成功。
4. `account/read` 返回 `requiresOpenaiAuth=true`、`account.type="chatgpt"`，计划和可用 email 与输入 AT claims 一致。
5. AT 的 token/account/user SHA-256 与此前登记的该账号绑定一致。`account/read` **没有 workspace/user ID 字段**，不能由它替代入口绑定。
6. 全程不发送 `thread/start`、`turn/start` 或任何自动恢复/重试模型调用。读取完成后退出并删除私有临时数据。

email 提取顺序是 JWT 顶层 `email`，否则 `https://api.openai.com/profile.email`；user 提取顺序是 `https://api.openai.com/auth.chatgpt_user_id`，否则同对象的 `user_id`。不提供 `chatgptPlanType`，让官方从原 AT claims 推导；缺失时官方使用 unknown。

这些回读只是 **本地认证状态装载成功**，不证明 AT 已获服务端接受，不证明模型权限，不证明回答质量。login 成功通知和 account/read 都可以基于本地解析完成。app-server 的 cloud config、插件缓存等可能自行访问非模型 endpoint，所以“零模型”不能写成“整个进程绝无网络”。真正离线验证要在无网络隔离环境运行，并提供仅用于本地解析的合成凭据。

若服务器发出 `account/chatgptAuthTokens/refresh`，参数为 `reason="unauthorized"` 与 `previousAccountId`。零模型诊断应停止并记录该事件，不悄悄换账号或刷新。正式宿主集成则返回同账号新的 AT/account ID/可选 plan；external bridge 等待上限为 10 秒。相关源文件：[account.rs:88](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/app-server-protocol/src/protocol/v2/account.rs:88)、[account_processor.rs:824](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/app-server/src/request_processors/account_processor.rs:824)、[external_auth.rs:32](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/app-server/src/external_auth.rs:32)。

**app-server 的 UA 不是 codex exec 的 UA。** `initialize.clientInfo.name` 会成为 originator，并添加 `name; version` UA suffix；`codex exec` 设置 originator 为 `codex_exec`。应如实记录实际探针名称和官方返回 UA，不能为了看起来一致伪装成另一个客户端。源码：[initialize_processor.rs:123](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/app-server/src/request_processors/initialize_processor.rs:123)、[exec/src/lib.rs:260](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/exec/src/lib.rs:260)。

### 直接写 auth_mode=chatgptAuthTokens 的界限

固定版本反序列化支持这个模式，auth.json 可使用官方内部形状：`tokens.id_token` 为原 AT 字符串、`tokens.access_token` 为同一 AT、空 refresh token、真实 account ID。源码可以将其装载为 `ChatgptAuthTokens`，无需签名伪造。但直接写文件没有安装 app-server 的 `ExternalAuthBridge`，401 时无法获得完整宿主刷新协议；官方 external API 还会使用进程内 ephemeral storage。

因此不能把“手工换 auth_mode 后 login status 成功”说成完成正式外部认证集成。`codex login status` 也只证明本地装载，不证明服务端权限。此审计建议以 app-server API 作为新的原生外部 AT 基准。

## 5. 模型目录必须比较的字段

现有 [quality_catalog_compare.py](C:/Users/Administrator/Desktop/项目/codex-agent/tools/quality_catalog_compare.py:203) 已按模型和字段保留规范化哈希，敏感 instruction 只保存哈希。它可接收真实网络返回的 catalog，在内存中比较，避免把原始 prompt 或账号身份落盘。它标注 `required_model_fields_not_full_cli_serde`，不能误当成官方完整反序列化验证。

| 字段 | 影响位置与应解释的意义 |
|---|---|
| `model_messages`、兼容 `base_instructions` | 模型 prompt 模板、personality 变量、工具/权限/协作/上下文 instructions；优先比较完整对象哈希，差异时再逐子字段哈希。 |
| `use_responses_lite` | tools/instructions 的位置、稳定 input item ID、`reasoning.context` 和 Lite header。 |
| `default_reasoning_level`、`supported_reasoning_levels` | UI/defaults；显式 medium 在该版本不会自动归一成较低档，Ultra/Persistent 才有特殊归一逻辑。 |
| `supports_reasoning_summary_parameter`、`default_reasoning_summary` | 是否发送 summary 参数及其默认值；summary tokens 不等于所有内部计算。 |
| `support_verbosity`、`default_verbosity` | 是否与如何发送 `text.verbosity`。 |
| `service_tiers`、`default_service_tier`、`additional_speed_tiers` | tier 可用性和 defaults；最终请求会滤掉不支持的 tier 以及显式 default。 |
| `tool_mode`、`shell_type`、`experimental_supported_tools`、`apply_patch_tool_type`、`web_search_tool_type` | 实际工具表面和工具提示，即使问题无工具调用也可能改变输入。 |
| `include_skills_usage_instructions`、`include_plugin_usage_instructions`、`include_apps_usage_instructions` | 注入 usage instructions 的条件。 |
| `context_window`、`max_context_window`、`auto_compact_token_limit`、`effective_context_window_percent`、`comp_hash` | 上下文容量、会话压缩触发和兼容性；不直接代表模型计算集群。 |
| `supports_experimental_context`、`model_messages.token_budget`、`guardian`、`node_repl_*` | 模型拥有的上下文/审批/工具配置，可间接改变会话输入。 |
| `visibility`、`supported_in_api`、`priority`、`upgrade`、`availability_nux` | 目录筛选、默认展示和升级信息；不要把 display_name 的差异当作计算下降。 |

官方字段定义和应用见 [protocol/src/openai_models.rs:399](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/protocol/src/openai_models.rs:399)、[models-manager/src/model_info.rs:29](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/models-manager/src/model_info.rs:29)、[protocol/src/openai_models/reasoning_effort.rs:10](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/protocol/src/openai_models/reasoning_effort.rs:10)、[protocol/src/openai_models.rs:924](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/protocol/src/openai_models.rs:924)。

当前比较器只接受 `q数字` 匿名别名。新 `g1…g7` 映射到比较器匿名 ID 时必须保留单独的一一绑定，并始终保存实际 user/token/workspace 的登记哈希；不能误用此前同名 q1…q7 历史账号。

## 6. 已完成的离线验证与证据边界

本审计实际运行了现有工具的配置生成函数并用 `tomllib` 解析，检查了六组配置关系：HTTP provider name/auth/env_key、关闭 WS、零重试、version header、builtin 分支没有覆盖内置 provider、两种 transport 配置之外的公共配置一致。还核对了冻结工具 SHA 和 8 份保存的 CLI report。

8 份 report 全部满足：模型 cache 文件存在、可解析、client version 一致、identity 存在、`gpt-5.6-sol` 存在且不重复、`tool_mode="code_mode_only"`；没有证明其完整模型记录逐字段一致。全部配置哈希为 `10f21abf0fab92c0818ebb4332163fc703524bc75490e38776a39ca302626373`。这组检查零网络、零模型调用。

本审计支持的结论是：现有 HTTP 直连结果仍可证明“绕过 Go 网关仍发生答 29”，但其描述必须保留“隔离配置、自定义 ID 的 OpenAI HTTP provider、手写 managed auth 容器”边界。它不能证明与官方默认 builtin WS/完整原生登录生命周期完全一致，也不能证明服务端存在某种账号风险标记或弱算力集群。

原生 external auth 零 turn 装载与模型目录 GET 都已完成八账号验证，结果分别见第 10 节和第 8 节。下一次真实模型对照应只改变已经明确记录的必要因素；新路径正确装载本身不代表答题质量已修复。

## 7. 本轮身份入口修复验证

先补回归，原实现出现 **14 个失败断言**，覆盖同工作区不同用户、单边或双边缺少稳定用户、无效用户字段，以及 email/sub 不能替代用户身份。再加入入口检查：单文件 38 项测试运行，35 通过、3 项 POSIX 专属测试在 Windows 跳过；所有质量工具相关回归共 360 项运行，348 通过、12 跳过、0 失败。这里包含新原生 auth 工具的当时版本测试；不将这些离线测试声称为真实账号服务端验收。

重跑命令：

```powershell
python -m unittest discover -s tools -p 'test_quality*.py'
```

对 `native-auth-provider-v4/catalog_remote.py` 的独立审阅确认：请求写入器固定 GET 且 body_length=0，唯一替换是 `/models?client_version=0.155.0` path，单次 native dispatch 无自动重试，前后 epoch 约束真实 token/user/workspace，helper 实际运行 SHA 被检查。这一收集过程不会发送 Responses 请求。审阅同时指出 fetch 后若 epoch 校验抛错应先保存 transport 收据、`read_response` 的 1 MiB 上限可能不足完整目录、helper cleanup 状态需参与有效性判定；已经及时交给收集脚本负责人处理。

## 8. 八账号模型目录实证与独立复核

实际观察区间为 `2026-09-25T04:46:52Z` 至 `04:46:57Z`（北京时间 12:46）。root 完成收集，本审计另外直接读取 `catalog-plan.json`、`catalog-execution.json`、`catalog-comparison.json`、`catalog-review.json` 以及此前 24 格计划，运行 **73 项独立断言**，不是只读取 summary 的成功标记。

- 8 个不同 user、6 个 workspace；alias 与此前 q0/g1…g7 一一绑定，token/user/identity/workspace 哈希在执行前后与此前登记完全一致。
- 8 次原生 GET 全部 HTTP 200，单次 dispatch、无重试、零模型调用。运行 helper SHA 与登记值一致；每次正常退出 code 0、已回收、socket 已移除、没有强杀。
- 每账号目录 9 个模型。28 对组合均可比较，所有共享模型逐字段相等，无单侧新增模型。每份原始响应均为 522,612 字节，body SHA-256 相同：`49eca6f6349a44b0b0db31f3dd5bb313ce235d2f0abcfed9051af52b104d40a0`。
- 规范化 models 数组 SHA 为 `5073f7dab908eba4d6f22de2acb683bc49ee486d2672c01936147d8a2be1d0d0`；目标 `gpt-5.6-sol` 记录 SHA 为 `c89e3c71d1e5575ef4e1ab6cb38e26da814caece01d4558467a155413fc04a3d`。
- 目标记录的 `use_responses_lite=true`、`tool_mode="code_mode_only"`、`default_reasoning_level="medium"`、`default_service_tier="priority"`。支持 effort 列表为 low、medium、high、xhigh、max、ultra；`base_instructions` 和整个 `model_messages` 对象哈希也相同。

这证明**本次观察时**这些账号收到的公开模型目录一致，不能重建先前推理时刻的目录，也不能证明隐藏 routing/compute 相同。目录 `default_service_tier=priority` 是元数据事实；此前固定请求实际 response 的 tier 为 standard，二者属于不同观察层，不能直接推断被降级路由。

证据：[catalog-review.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/native-auth-provider-v4/catalog-review.json)、[catalog-execution.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/native-auth-provider-v4/catalog-execution.json)、[catalog-comparison.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/native-auth-provider-v4/catalog-comparison.json)。review 记录的三个输入文件摘要已逐一重新计算匹配。

## 9. 实际网关凭据路径审计与最小修复

审计继续从测试工具追到生产代码，发现 [admin_account_identity.go](C:/Users/Administrator/Desktop/项目/codex-agent/admin_account_identity.go:106) 原来只读取 `https://api.openai.com/auth.chatgpt_user_id`，漏掉官方支持的 `user_id` fallback。这与仅仅“没有校验任何用户”不同：双侧都使用主字段的已有冲突检查是有效的，但 fallback 会绕过它。

合成 JWT + 本地 loopback OAuth response 已实际证明：

1. AT 的 `user_id=user-a` 与 ID token 的 `chatgpt_user_id=user-b` 会被导入并存成 user-b。
2. 双侧只有不同 `user_id` 时，原实现接受，最终 UserID 为空。
3. 同样问题贯穿 `parseAccount` 直接导入、`parseManagedSessionCredentials` 的 camelCase/nested session 导入、`managedOAuthRequest` 的实际 loopback token exchange。

当前候选生产补丁只修改 `admin_account_identity.go`：主字段缺失或 null 时读取 `user_id`，并进入原有冲突检查。主字段存在时仍优先，不拿 email 或 JWT sub 替代用户。合法显式 selected workspace 行为保持不变；官方 `from_external_access_token()` 自身也接受 caller workspace 并覆盖本地 token account metadata，所以不能仅凭 token 默认 workspace 不同就一律拒绝。

新增 [gateway_research_identity_fallback_test.go](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_research_identity_fallback_test.go:17) 覆盖 10 种情形 × 3 个入口，共 30 个叶子测试。先运行原实现，15 个叶子失败；补丁后全部通过。进一步 Identity/OAuth/Account/Owner/Refresh 相关回归：170 个顶层测试通过、37 个环境相关顶层测试跳过、0 失败；包含子测试共 582 pass 事件、76 skip 事件。日志：[gateway-fallback-related-tests-summary.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/native-auth-provider-v4/gateway-fallback-related-tests-summary.json)。这是候选源码验证，不代替后续隔离发布全测和实际部署收据。

冻结 SHA：生产文件 `7df533a401d1774400dd3d932f610a42f5a7d7e6669c9c4a54ab6172503a9eea`；测试文件 `1ca360c46de59d17ebccd36834fba090d8ffedac885118fc0de7ad345ae71802`。既有 24 格并未证实使用了混配凭据，因此不把这个确定存在的缺陷冒充既有错答根因。

### 另一个已复现并已实现候选修复的 refresh 身份问题

修复前 `refreshAccountWithSettings()` 只解码 AT、RT 和 expires_in，忽略返回 id_token；替换 AT 后继承原 `a.Identity`。文件存储 `Store.Refresh → mergeAccountTokens → modify` 和 PostgreSQL `pgRefreshContext → mergeAccountTokens → pgWriteTokens` 都依赖 `advanceGatewayOwnerRevision()`。

`sameGatewayOwnerRevision()` 在 stored workspace 与 `Identity.UserID` 完整且不变时，会保持旧 owner revision，不再比较新 token 的真实 user claims。`recoverUnauthorized()` 的前后检查又使用这个 owner ref。因此新 AT 即使已属于另一个 user，陈旧 Identity 仍可能遮住该变化。

临时 Go overlay 测试已经通过真实 loopback OAuth response 及文件 Store.Import → Store.Refresh → 磁盘回读复现：响应 AT/ID token 都为 user-b，但新凭据仍存于 user-a 身份，持久化 OwnerRevision 和 gatewayAccountRef 均不变。复现文件在 `.local/research-20260925/native-auth-provider-v4/gateway_credential_identity_repro_test.go`；随后按这一反例实现生产候选补丁。

正确修复不能简单在 OAuth 成功后 return error：RT 可能已经旋转，丢弃新 RT 会损坏账号。新增 [gateway_refresh_identity.go](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_refresh_identity.go:16) 和 `proxy.go` 的最小调用补丁已实现：

- 新 AT 或明确返回 ID token 时，从当前凭据重建身份，不以旧 Identity 作为解析初值，不继承旧 Verified、组织或 agent 信息。
- 新 AT 能提供用户身份，或者 opaque 新 AT 带有同响应的有效 ID token 时，可建立未验证的用户元数据。两种 token 都有可比较 user 时必须一致。
- 新 user 或无法证明的 owner 会导致实际持久化 owner revision 改变；冲突或无法解析的明确 ID token 不允许旧用户填补空缺。HTTP 的旧 session/401 replay 与 WS 授权使用现有 owner gate 阻断。
- 已旋转 AT/RT 仍返回给 Store 成功提交。metadata 不可用不会在 OAuth 成功后制造错误并丢掉新 RT。
- 仅 RT 更新，或返回完全相同 AT 且无 ID token，保留已有身份与 owner。合法 selected workspace 不改变。
- 真正 ID-only 响应可更新 identity/plan；它与保留 AT 的用户冲突时降为 unknown。若没有 AT/RT、仅返回格式/类型错误的 ID token，则没有旋转凭据需要保留，返回固定 invalid 错误且旧身份不变。

新增 [gateway_refresh_identity_test.go](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_refresh_identity_test.go:23) 的原实现红测有 15 个叶子失败；完成主体修复后，追加 invalid ID-only 边界再复现 3 个失败并修复。当前 27 个叶子通过：15 个真实文件 Store.Refresh 场景、9 个独立 ID-only/错误响应场景、1 个 unchanged opaque AT 场景、2 个 HTTP 旧 session/WS 授权阻断场景。同一 15 场景也提供 PG opt-in 回归；PG fixture 特别通过 `Get → UpdateTokens` 写入加密 credential Identity，没有误用只操作管理字段的 `UpdateAccount`。

冻结 SHA：`proxy.go` 为 `fdd146193d59138f3de06677af1aec342e6f5523c87e8630e2a4169034d2f256`；新 helper 为 `e4f128958d2fd595e7a124d5e1e472cdac221a86928121f6064e2d37835f957f`；新测试为 `b3d26471348628d7c7f32ce7a1a7b23e12a01e53654abc9e08e91de985df8c61`。这些是候选源码与本地验证状态，实际 Linux/PG/native 全测、独立 wire 复核及部署结果以随后发布记录为准。

截至本节更新，root 已确认最终隔离源码快照 `08119c97968953…` 包含上述最终文件，Windows 与 Linux 完整测试正在同一快照运行。尚未将“运行中”记为通过；也未把包含其他并发工作的当前共享目录直接打包部署。

另一 agent 独立增加 overlay 验证，实际运行 6 个 HTTP 401、10 个 WS handshake/body 401 和 2 个失败响应场景，18 叶子全部通过；检查了原 inference/upgrade 不被错误重放、旋转 RT 已持久化、文件 Store 重新打开后的 owner revision、新 thread 可用与错误日志不含合成 secret。其故意禁用身份重建的 mutation overlay 产生 6 个失败叶子，实际抓到 HTTP 错误 replay 与 WS 错误接通，表明测试能检出本次缺陷。已读取 [wire-green.log](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/refresh-identity-review/wire-green.log)；这个独立 wire 结果仍不替代全平台与 PG/native 测试。

旧回归测试中，有一批“已知同用户正常 refresh”使用不带用户 claims 的 opaque mock AT，却期待 owner 不变。另一个 agent 将 29 个测试文件中 46 个此类成功 OAuth mock 补上显式同用户、同 workspace 的合成 ID token；保留原 AT 字符串和断言，没有修改 unknown-owner 或冲突场景来掩盖新行为。该批定向回归当时为 451 个 pass 事件、47 个 opt-in skip、0 fail，覆盖 134 个顶层测试；其生产快照早于最后的 ID-only 小修，不能替代最终统一全测。

## 10. 新原生认证探针的独立边界复核

已独立复核 `quality_native_auth_probe.py` 的首个修正冻结版本 `dcbe594e1625d4774f8993b98d813e35237c5db7de92520feef5da3209ae6505`，并实际重跑其 19 个 mock-child 测试，全部通过。系统配置拒绝检查使用 `/etc/codex` 和 Windows 官方 KnownFolder 路径，不依赖可污染的 ProgramData 字符串；version 与 app-server 各自记录 child_reaped/io_threads_joined；version 清理失败不会被后续成功掩盖。

实际进程第一次零网络验证遇到非认证 notification 后正确停止。后续诊断确认是源码已知的 missing_system_bubblewrap 以及 remoteControl/status/changed 初始化通知。最终规则只针对零 turn 允许精确 warning 常量、明确 null details、null/缺失 path/range 和一次数量上限；对 remoteControl 和 skills/changed 使用精确 schema 与数量上限。其他 configWarning、未知通知、所有 server requests 继续拒绝。结果保留 warning，始终 `model_reference_clean=false`，不将本地认证验证扩展成完整工具沙箱或模型环境健康。

真正 Linux 零网络验证应在新的网络 namespace 内运行，示例回归入口：

```sh
unshare --net -- python3 -m unittest discover -s tools -p test_quality_native_auth_probe.py
```

应记录新/父 `/proc/self/ns/net` 不同、namespace 仅有 loopback 与无默认路由；unshare 失败时不得自动回退成普通联网执行。真实凭据必须在隔离前经 epoch 核对并仅通过 stdin 传给子进程，隔离后不再连接 gateway 取凭据。仍只完成 initialize → external login → account/read，不发送 thread/turn；它证明原生本地装载身份，不证明服务端认证、模型权限或回答质量。

### 八账号实际原生装载结果

`auth-verified-3` 在 `2026-09-25T05:08:50Z` 至 `05:08:58Z` 完成 q0、g1…g7 全部八账号。每个子进程由 `unshare --net --pid --fork --kill-child` 创建，并在内部断言网络 namespace 与父进程不同。没有 OAuth 网络请求、没有模型调用、没有 thread 或 turn；认证始终使用未修改的已有 AT，经官方 external login 构造身份。

八个结果全部为 `completed_with_known_zero_turn_warning`：`account/read` 回读 `type=chatgpt`、精确认证模式 `chatgptAuthTokens`、计划 `self_serve_business_prolite`，email/user/token/workspace 摘要与登记的对应账号匹配。每次只有同一条已知 bubblewrap warning，remote control 状态 disabled；CLI version 子进程和 app-server 均已回收，I/O 线程已 join，临时目录已清理，退出码 0。主 systemd 单元已 inactive/dead、MainPID 0、ExecMainStatus 0。

本审计读取实际执行、计划、冻结工具、收据，完成 **150 项独立断言**，包括前后 epoch 与旧 24 格登记一致，以及四份实际远程脚本按 SFTP 原始 bytes 回收后 SHA 与冻结 manifest 全部一致。早期本地展示副本中两个 launcher 的 CRLF 差异已明确识别；`.executed` 原字节副本验证补齐后未重跑任何账号或模型。

固定执行 probe SHA 为 `4c3a6902f5e31976d0ccebe59a6aae3dcee36647dde4be2bdbb70a0fc72e6699`，固定 CLI SHA 为 `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`。冻结 native-auth 工具在 Linux 的 24 项 mock-child 测试全部通过。

证据：[auth-independent-review.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/native-auth-provider-v4/auth-verified-3/auth-independent-review.json)、[auth-execution.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/native-auth-provider-v4/auth-verified-3/auth-execution.json)、[executed-source-verification.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/native-auth-provider-v4/auth-verified-3/executed-source-verification.json)。这些结果消除了“已有 AT 无法进入该版本原生 external auth”的疑问，**不证明账号推理质量恢复或隐蔽集群路由相同**。
