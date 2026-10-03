# Realtime 官方出站请求实测（0.155.0）

固定官方二进制 SHA-256：`660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`。捕获编号 `20260927T115657.454227Z`。七个 case 均成功交换合成文本，并由官方 App Server 的 RPC transcript 通知收到夹具响应；四个 WebRTC case 也收到 SDP answer 通知。

| Case | HTTP 创建 | WebSocket 加入 |
| --- | --- | --- |
| WebSocket v1 | 无 | `/v1/realtime?intent=quicksilver&model=gpt-realtime-1.5` |
| WebSocket v2 | 无 | `/v1/realtime?model=gpt-realtime-1.5` |
| WebSocket v3 | 无 | `/v1/live?model=gpt-realtime-1.5` |
| WebRTC v1 API | `/v1/realtime/calls?intent=quicksilver&architecture=avas`，multipart | `/v1/realtime?intent=quicksilver&call_id=rtc_fixture` |
| WebRTC v1 backend | `/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas`，JSON | 同上 |
| WebRTC v3 API | `/v1/live`，multipart | `/v1/live/rtc_fixture` |
| WebRTC v3 backend | `/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas`，JSON | 同上 |

multipart 包含 `sdp` 和 `session` 两个 part，固定 boundary 为 `codex-realtime-call-boundary`。JSON 使用 `{sdp,session}`。v1 创建请求的 session 包含 model，但后续 `session.update` 不包含 model。v3 WebRTC 在加入 sideband 时不重复发送 `session.update`；v3 直接 WebSocket 启动仍发送更新。v1/v3 使用 `openai-alpha: quicksilver=v1/v2`；v2 使用独立的会话协议。完整原始 body、SHA-256、请求头及解析后的 multipart 见 [writer vectors](../internal/server/testdata/appserver_realtime_vectors.json)。JSON body 重建为原始紧凑 JSON 后逐条核对原始 SHA-256；未更改 body 字节。

测试使用 `unshare --net` 新建网络命名空间，并记录内核 namespace ID、接口和路由；只有 `lo` 与 loopback 路由。没有真实账号、真实模型调用、实际音频采集或外部出站。v1/v3 的 `outputModality:audio` 只是官方允许的协议组合，夹具仅交换文本。v2 WebRTC 被官方 writer 拒绝；v1 + text 也被拒绝。失败记录保留在早期捕获中，未当作成功证据。

复跑全部七项并生成 oracle：

```powershell
python tools/appserver_parity_realtime_loopback.py --remote
```

该命令调用已有 SSH 连接工具，在隔离集成目录中运行固定官方二进制，不改变线上配置。每次 run 保存 summary、每个 case 的 requests 和 writer-oracle。仅需重新导出已有本地记录时：

```powershell
python tools/appserver_parity_realtime_loopback.py --export-run .local/appserver-parity/realtime/20260927T115657.454227Z
```

## 账号与客户端配置边界

以下是固定 0.155.0 源码核验，不是假称使用真实账号验证：

- `core/src/client.rs:412` 明确要求 WebRTC sideband 复用创建 call 的认证：ChatGPT bearer 加 account id；默认 sideband 主机是 `api.openai.com/v1`（`codex-api/.../realtime_websocket/methods.rs:60,784`）。创建 call 使用当前模型 provider，ChatGPT 默认为 `chatgpt.com/backend-api/codex`。
- `core/src/realtime_conversation.rs:1773` 的独立 WebSocket 启动要求 API key。不能把已捕获的合成 API-key WebSocket 成功推断为 ChatGPT bearer 也可独立创建此类会话。
- 客户端只修改普通 provider 的 `base_url`，不能保证 sideband 会经过中转。官方配置 `experimental_realtime_ws_base_url` 会覆盖直接 WS 和 WebRTC sideband 目标（`realtime_conversation.rs:1252-1258,658`）；中转客户端应同时将其指向网关的 `/v1`。`experimental_realtime_webrtc_call_base_url` 可独立覆盖创建请求目标。
- 本 oracle 证明官方 writer 的路径、body 和帧构造，以及官方 RPC 能解析夹具返回。它不证明真实媒体 ICE/DTLS 连通性、真实账号权益、上游服务是否接受当前账号，或音频质量。

## 官方 0.155.0 客户端配置示例

将下面配置合并到**客户自己的用户级 `CODEX_HOME/config.toml`**。两个 `experimental_realtime_*_base_url` 是 TOML 根字段，要写在任何 `[table]` 之前；它们不属于 `[model_providers.gateway]`。固定版本的配置加载器会忽略项目 `.codex/config.toml` 中的 provider 设置和 Realtime WS URL，因此不能仅在当前代码仓库放一份项目配置就认定客户端生效。此处 `example.invalid`、环境变量名和 key 值全部是占位符，没有读取任何实际凭据。

```toml
# config.toml 根字段；保留客户原有的 model / reasoning 等其他设置。
model_provider = "gateway"
experimental_realtime_ws_base_url = "https://example.invalid/v1"
experimental_realtime_webrtc_call_base_url = "https://example.invalid/v1"

[model_providers.gateway]
name = "Gateway"
base_url = "https://example.invalid/v1"
env_key = "CODEX_GATEWAY_API_KEY"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = true

[features]
realtime_conversation = true
```

客户在自己的终端提供**网关客户端 key**，然后启动 CLI：

```powershell
$env:CODEX_GATEWAY_API_KEY = "<YOUR_GATEWAY_CLIENT_KEY>"
codex
```

这是客户端到网关的 key；不是服务器池中账号的 ChatGPT bearer。已有 `[features]` 或相同 provider 表时合并字段，不重复创建同名 TOML 表。CLI 的 `/voice` 功能由 `features.realtime_conversation` 控制，默认关闭；启用后重新启动 CLI。0.155.0 TUI 明确通过 App Server 发起 **WebRTC + v3 + `clientManagedHandoffs=true`**（`tui/src/app_server_session/realtime.rs:28-48`），无需改成独立 WebSocket 启动，也不需要在示例中额外设置 `[realtime].version`。安装包还必须包含平台支持的 Realtime 媒体 runtime；`realtime-webrtc/src/session.rs:83` 会检查这一点。

| 字段 | TOML 位置与 URL 语义 |
| --- | --- |
| `base_url` | `[model_providers.gateway]` 内。用于普通 HTTP API，使用 `https://example.invalid/v1` 这类 base；writer 会追加 `/responses`、`/models` 等路径，不填写完整 endpoint。 |
| `experimental_realtime_webrtc_call_base_url` | 根字段，可选显式覆盖。HTTP call-create base，使用 `https://example.invalid/v1`；v1 追加 `/realtime/calls`，v3 追加 `/live`。省略时使用当前 provider 的 base。这里不能使用 `wss://` 代替 HTTPS，也不应把 `/realtime/calls` 或 `/live` 写进 base。 |
| `experimental_realtime_ws_base_url` | 根字段。接受 `http://`、`https://`、`ws://`、`wss://`；HTTP/HTTPS 自动转换为 WS/WSS。推荐同样填写 `https://example.invalid/v1`，v1/v2 规范化为 `/v1/realtime`，v3 为 `/v1/live`，加入已有 call 时再追加 query 或 call id。 |
| `env_key` | `[model_providers.gateway]` 内，是环境变量**名称**，不是 key 本身。`requires_openai_auth=false` 使用这个 provider 的 key，不要求客户端登录服务器池中的 ChatGPT 账号。 |

以上 base URL 不附带 query、fragment 或额外业务前缀。HTTP provider 的路径拼接会去除末尾 `/`；WS 对 `/v1` 和 `/v1/` 都有明确规范化分支。示例统一使用不带末尾斜线的 `/v1`，与本项目公开的精确路由一致。`supports_websockets=true` 指普通 Responses transport 能力，不会替代 Realtime URL 配置，也不自动授予独立 Realtime WS 权限。

源码依据：根配置定义在 `config/src/config_toml.rs:411-415`；根字段加载测试在 `core/src/config/config_tests.rs:12537`；provider 字段在 `model-provider-info/src/lib.rs:100-148`；HTTP 拼接在 `codex-api/src/provider.rs:51`；WS scheme 与路径规范化在 `codex-api/src/endpoint/realtime_websocket/methods.rs:1077-1208`；项目层过滤的回归测试在 `core/src/config/config_loader_tests.rs:3876`。这些 URL 字段在 JSON schema 中仅标为 string，正确路径还要经过实际 endpoint writer；仅 TOML 解析成功不足以证明连通。

只修改 `base_url` 会让官方 WebRTC sideband 继续连接默认 `api.openai.com`，客户端手里的网关 key 不能在那里充当上游凭据，因此必须同时配置 `experimental_realtime_ws_base_url`。示例将三个入口都显式指向同一网关；当选中的 provider 的 `base_url` 已是同一地址时，`experimental_realtime_webrtc_call_base_url` 可以省略。当前服务器池使用 ChatGPT 账号；网关支持先创建 WebRTC call，再通过绑定账号加入 sideband。**直接请求不带 call id 的独立 `/v1/realtime` 或 `/v1/live` WebSocket 不属于这条 ChatGPT 账号链路**，会返回 API-key-auth 错误，不能用合成 API-key 测试成功声称它已支持。

这份配置说明验证了官方 writer 和网关路由如何衔接。实际 `/voice` 的音频设备、媒体 ICE/DTLS/datachannel、真实账号是否接受创建请求仍须单独实测；本地文本、SDP 和 native WS 夹具成功不等于真实音频通话成功。

原始集成目录：`/opt/codex-agent-integration/appserver-parity/realtime/20260927T115657.454227Z`。当前证据独立于先前冻结的普通 Responses 回归报告。

## 轻量通道实现与验证范围

新增 `gateway_appserver_realtime.go` 提供固定的 call-create 和 sideband 入口。multipart 转为 backend JSON 时保留原始 session JSON（包括未知字段和大整数），JSON 调用直接保留原始 body。创建请求使用已有账号优先级、权重、并发、代理和失败换号；成功后将 call id 和 API key 对应的账号引用保存到现有 Redis 路由存储。活跃 sideband 独立续租，不受普通 sticky 开关影响。它只在同一账号内处理 401 凭据刷新，不把已有 call 转移给其他账号。

sideband 仅连接固定 `api.openai.com` 路径，使用 native Rust transport，独立协商禁用 WebSocket 压缩；这是七条官方捕获中没有 `Sec-WebSocket-Extensions` 所要求的区别。客户继续控制 session updates、文本、二进制内容、工具交接和重连。这里只保存账号归属与响应 ID 摘要去重数据，不运行官方 agent 会话。

只读统计从实际 `response.done`/`response.completed` 提取用量，按 `response.id` 去重。支持 Realtime 的单数 `input_token_details` 和 Responses 的复数 `input_tokens_details`。缓存字段缺失时 `cached_tokens_known=false`，保留已观测到的 input/output，首输出时间来自实际输出 delta，不用握手时间充当首字。[官方 Realtime 事件字段](https://platform.openai.com/docs/api-reference/realtime-server-events/response/done)。WebRTC 的媒体及用量也可能只发给客户端 datachannel，本桥不能保证观察到全部账单；缺失字段保持未知。

本地回归命令：

```powershell
go test ./internal/server -run '^TestAppServerRealtime' -count=1
```

其中统计测试使用明确合成的 usage，并验证重复终止消息不重复计费、原始消息不变。完整本地 HTTP call-create → WebSocket 测试覆盖跨 key 隔离、sticky 关闭后的账号归属、并发占用、401 同账号恢复以及文本/1 MiB 二进制回传。`TestAppServerRealtimeNativeActualSideband` 另需 Linux 下的 `CODEX_NATIVE_HTTP_TEST_BIN` 指向支持 `websocket_no_compression_v1` 的新 helper；未提供环境时会明确 skip，不计为实际 native 验证。

2026-09-27 的 `unified-interop-20260927T122826Z` 已实际执行并通过 `TestAppServerRealtimeNativeActualSideband`，所在批次 13 项顶层测试通过、0 skip、0 failure。它使用 Docker `network=none` 中的本地 TLS 服务，核验固定官方 sideband 目标、账号认证、`OpenAI-Alpha`/`X-Session-Id`、没有 WebSocket 压缩扩展以及文本/二进制逐字节往返。测试先断言生产目标为 `https://api.openai.com/v1/live/rtc_native`，然后只在夹具中替换为本地 TLS endpoint。此项记录绑定 Go artifact SHA-256 `b04971520d84f31568d624b96a276489dfbcc7bab2621b3c1bad7c69bce39a10` 和 native helper SHA-256 `361b0df32ce0e74d5a07bad697abf49cb59e295eb1795797ae4c2a18548948ee`。

可复核结果在 `.local/appserver-parity/native-unbounded/unified-interop-20260927T122826Z/result.json` 与 `tests.log`。后续 Go 版本以其独立 artifact 记录为准；这次离线互操作没有拨打真实 `api.openai.com`，也没有产生真实媒体流或账单。
