# 官方客户端使用网关委托认证（Codex 0.155.0）

已用未修改的官方 0.155.0 二进制证明：客户端可只保存网关密钥，通过官方 `personalAccessToken` 认证模式进入 Codex backend 功能路径，无须保存账号池的 access token、refresh token，也无须构造 JWT。服务端需实现下述身份元数据与账号绑定协议。

这称为“网关委托认证”，不表示网关密钥是 OpenAI 签发的 PAT。它使用官方客户端已经支持的 PAT 装载、身份查询及请求构造流程。

## 配置与启动

先将网关密钥放入 `CODEX_GATEWAY_KEY` 环境变量，生成独立客户端目录：

```powershell
python tools/appserver_parity_client_profile.py --write-profile .local/gateway-client --gateway-origin https://YOUR_GATEWAY_ORIGIN --model gpt-6-astra
```

启动客户端的一条命令：

```powershell
python tools/appserver_parity_client_profile.py --run-profile .local/gateway-client --codex C:/path/to/codex.exe -- exec --skip-git-repo-check "你的任务"
```

将 `exec ...` 换成 `app-server` 可启动客户端本地 App Server。启动器读取独立目录内的网关密钥，并设置 `CODEX_AUTHAPI_BASE_URL`、`CODEX_CONNECTORS_TOKEN` 和客户端目录；不会启动服务器会话进程。它移除继承的 `OPENAI_API_KEY`、`CODEX_API_KEY`、`OPENAI_BASE_URL`，避免环境切换认证模式。

生成的关键配置如下，未配置 `env_key`、`experimental_bearer_token` 或 provider command auth；这些字段会使官方 `uses_codex_backend` 条件不成立：

```toml
model = "gpt-6-astra"
model_provider = "gateway-appserver"
cli_auth_credentials_store = "file"
chatgpt_base_url = "https://YOUR_GATEWAY_ORIGIN/backend-api"
features.apps = true
features.image_generation = true
features.token_budget.enabled = true
features.token_budget.use_history_notes_extension = true

[model_providers.gateway-appserver]
name = "OpenAI"
base_url = "https://YOUR_GATEWAY_ORIGIN/backend-api/codex"
wire_api = "responses"
requires_openai_auth = true
supports_websockets = true
supports_standalone_web_search = true
```

`token_budget` 的两项开关在这里用于官方 History/Notes 扩展的功能条件，没有配置数值 token 预算。独立 `auth.json` 仅含：

```json
{"auth_mode":"personalAccessToken","personal_access_token":"GATEWAY_KEY"}
```

环境变量：

```text
CODEX_AUTHAPI_BASE_URL=https://YOUR_GATEWAY_ORIGIN
CODEX_CONNECTORS_TOKEN=GATEWAY_KEY
```

`CODEX_CONNECTORS_TOKEN` 必需：官方 MCP 只向 ChatGPT 及其 staging 域名自动附加环境内的 ChatGPT 认证。自定义网关域名使用这个官方支持的显式 bearer 配置。它取同一个网关密钥。

## 网关身份接口与账号归属

客户端首次和后续重新读取认证时会发送：

```http
GET /v1/user-auth-credential/whoami
Authorization: Bearer GATEWAY_KEY
```

响应形状由 `login/src/auth/personal_access_token.rs` 定义：

```json
{
  "email": "optional-real-account-email",
  "chatgpt_user_id": "ACTUAL_USER_ID",
  "chatgpt_account_id": "ACTUAL_ACCOUNT_ID",
  "chatgpt_plan_type": "ACTUAL_PLAN",
  "chatgpt_account_is_fedramp": false
}
```

`email` 可省略或为 null；其他字段必须按上述类型提供。取值来自网关实际授权并绑定的账号，不能虚构订阅、身份或权益。查询结果会影响模型和功能选择，也会进入本地缓存归属。

需要将委托模式密钥绑定到稳定的上游用户及工作区归属，并在每个后续模型、系统及 MCP 请求重新授权。已绑定历史、工具状态不能因为令牌失效而迁移到另一归属；同账号刷新可以继续。普通未使用此模式的池化 API key 不应被隐式改成固定账号。

实际捕获证明 MCP 显式 bearer 请求没有 `ChatGPT-Account-Id`。所以网关必须从认证后的密钥绑定找到 owner，不能只根据入站账号头判断是否启用归属保护。其余表内 backend 路由会携带 whoami 返回的账号 ID；网关应检查它与绑定一致。

| 捕获路径 | 方法 | Authorization | ChatGPT-Account-Id |
| --- | --- | --- | --- |
| `/v1/user-auth-credential/whoami` | GET | 网关 key | 无 |
| `/backend-api/codex/models` | GET | 网关 key | whoami 账号 |
| `/backend-api/codex/responses` | POST | 网关 key | whoami 账号 |
| `/backend-api/codex/images/generations` | POST | 网关 key | whoami 账号 |
| `/backend-api/codex/alpha/history/v2/list_windows` | POST | 网关 key | whoami 账号 |
| `/backend-api/codex/alpha/notes/v2/thread_hint` | POST | 网关 key | whoami 账号 |
| `/backend-api/ps/mcp` | POST | 网关 key | 无 |
| `/backend-api/connectors/directory/list` | GET | 网关 key | whoami 账号 |
| `/backend-api/wham/settings/user` | GET | 网关 key | whoami 账号 |

## 已执行的验证

复跑命令：

```powershell
python tools/appserver_parity_client_profile.py --remote
```

该命令使用已有集成主机和强隔离 `unshare --net`，只开启 loopback。官方二进制 SHA256 固定为 `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`。所有账号与模型响应均为本地 synthetic fixture；无真实账号、上游 token、JWT 或真实模型调用。

最终认证及功能运行记录在 `.local/appserver-parity/client-profile/20260927T125942.320919Z`：

- 官方 App Server：29 个请求；`account/read` 显示 fixture 中的 ChatGPT 账户信息；完整图片工具、应用 MCP 工具和 History/Notes 流程成功。
- 官方 `codex exec`：17 个请求；同样完成图片工具、应用 MCP 工具和 History/Notes 流程，进程退出码为 0。
- 两条路径均真实发送了一次 MCP `tools/call`，其 `SYNTHETIC_APP_TOOL_RESULT` 返回值进入下一轮 Responses input；历史工具的 `SYNTHETIC_HISTORY_RESULT` 也进入后续 input。
- 全部请求只携带同一 synthetic 网关 key，外部 CONNECT 为 0。模型、图片、历史和笔记请求的账号 ID 与 whoami 返回值相同。
- `verified-profile.json` 记录逐路由认证字段及原始请求文件 SHA256。App 请求文件 SHA256：`34820f9a599e08ddf3769ec1e6a8045d7bc4f90269fc38a83d5c19dbe5e751b4`；exec：`76c0a1da9d4888dfc7709e844ebd7acd408b3493ae1d7e7b3053fc9fc40210cc`。

这是客户端 writer 与认证功能条件的实际证明，不代表上游已授予所有 API 权益，也不是生产网关部署验证。真实 whoami、账号归属、MCP 转发和服务端全部路径需由网关实现并另测。

## 仍需区分的官方认证分支

`AuthMode::Headers` 虽然属于 backend auth，但官方 0.155 明确禁止从 `auth.json` 装载；它只供嵌入式 Rust API 外部认证使用，不能用一个 CLI 配置文件启用。

`ChatgptAuthTokens` 要求解析 JWT 形状的 access token，不能直接接收普通网关 key。以上 profile 使用官方支持的 opaque PAT 形状，未伪造 JWT。

官方 PAT 与 OAuth 的每个内部分支仍不完全相同。例如 `core/src/client.rs:set_guardian_metadata` 的 `guardian_credits_requested` 只对 `Chatgpt`/`ChatgptAuthTokens` 插入，不包括 PAT。本验证证明列出的 backend 功能条件和调用链已打开，不把它扩张为所有可选内部调度元数据都完全一致。
