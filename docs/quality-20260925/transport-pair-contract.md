# 固定 Codex 0.155.0：HTTP 与 WebSocket 首轮对照契约

本契约限定于 `gpt-5.6-sol` 当前启用 Responses Lite 的**完整首轮**请求。问题、指令、工具、input item ID、session/thread/cache 和既有 client metadata 全部取自同一份真实 CLI 捕获，不重新生成。HTTP 与 WS 具有不同握手、序列化外层和连接状态，因此这里验证的是明确字段投影，不能写成两种协议请求完全同字节。

实现：[quality_transport_pair.py](C:/Users/Administrator/Desktop/项目/codex-agent/tools/quality_transport_pair.py)，测试：[test_quality_transport_pair.py](C:/Users/Administrator/Desktop/项目/codex-agent/tools/test_quality_transport_pair.py)。函数不联网、不读凭据、不调用模型，也不写原始问题或响应。

## 1. 先纠正 stream 的实际版本契约

**这个固定版本的 WS `response.create` 保留 `stream:true`。** `codex-api/src/common.rs` 的 `ResponseCreateWsRequest` 明确有 `stream: bool`，`From<&ResponsesApiRequest>` 复制原值。最终 `serialize_websocket_request()` 直接 `serde_json::to_string()`，没有删除 stream 的后处理；同文件原生测试把 HTTP request JSON 加上 type/previous_response_id/generate 后直接与 WS JSON 比较。

`background` 在此版本 HTTP/WS 原生请求结构中都不存在。转换器不会把遇到的 background 静默删除并声称对齐；它会拒绝非该版本原生契约的输入。任何“不加判断删除 stream/background”的通用转换都不适用于此处。

源码：[common.rs:260](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/codex-api/src/common.rs:260)、[responses_websocket.rs:920](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/codex-api/src/endpoint/responses_websocket.rs:920)、[responses_websocket.rs:943](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/codex-api/src/endpoint/responses_websocket.rs:943)。

## 2. 完整 body 的最小投影

| 字段 | 投影规则 |
|---|---|
| `model`、`input`、`tool_choice`、`parallel_tool_calls` | 保留原 value 的完整 JSON bytes，包括 input 中指令、工具、原题、内部 metadata、数字词法和字符串转义。 |
| `reasoning`、`include`、`store`、`stream` | 原样保留。Lite 的 reasoning.context 必须为 all_turns，stream=true，store=false，include 为 reasoning.encrypted_content。 |
| `service_tier`、`prompt_cache_key`、`text`、`stream_options`、`access_programs` | 若存在则原样保留；不新增高档 effort/tier，不重设 cache key。 |
| `instructions` / 顶层 `tools` | 原生 Lite 使用 input 前缀，空 instructions 和 None tools 被 serde 省略。转换器拒绝包含这些字段的“已被网关规范化”输入。 |
| `type` | WS 增加 `response.create`。HTTP 必须没有此字段。 |
| `previous_response_id` / `generate` | 完整新首轮无这两个字段；拒绝旧 continuation、增量请求或 warmup 被误当新推理。 |
| 既有 `client_metadata` | 每个原有键值均保留，不重建 turn metadata 字符串。 |
| Lite WS metadata | 增加 `ws_request_header_x_openai_internal_codex_responses_lite="true"`。 |
| WS 发帧时间 | 增加字符串 `x-codex-ws-stream-request-start-ms`，由调用者传入发帧前实际毫秒时间。 |
| W3C trace | 若受控 HTTP capture 带 traceparent/tracestate，将值投影到 `ws_request_header_traceparent` / `ws_request_header_tracestate`；不把它们当模型内容。 |

实现使用原文 span 拼接：除顶层 client_metadata 对象外，各原字段 value bytes 完全不变；该 metadata 对象内部所有既有 value bytes 也完全不变，只在末尾加入官方 WS 字段。报告同时保留每个原字段的规范化哈希和原 value bytes 哈希，metadata 每个键以键名哈希标识、值只保存哈希。支持大于 JavaScript 安全整数的 JSON 数字，保留 `1.2300e+20` 等词法，不经 float 往返。

核心源码：[core/src/client.rs:758](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:758)、[client.rs:719](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:719)、[client.rs:1760](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:1760)、[client.rs:2037](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:2037)。

## 3. 握手 headers 与压缩

输出 head 使用 native WS probe 契约：

```python
{
    "method": "GET",
    "path": "/backend-api/codex/responses",
    "headers": [["Header-Name", "base64-of-original-value"], ...],
}
```

| 原 HTTP header | WS 处理 |
|---|---|
| User-Agent、Originator、Version | 保留捕获到的真实值；不会换成浏览器设备身份。 |
| session-id、thread-id、x-client-request-id | 保留；转换前核对与 body client_metadata 的 session/thread 一致。 |
| x-codex-window-id、x-codex-turn-metadata、其他明确源码支持的兼容 header | 保留原值，不把 body 的完整 metadata 再序列化覆盖兼容 header。 |
| x-codex-routing-hint | 保留并检查与 model 和可选 service_tier 一致。 |
| x-openai-internal-codex-responses-lite | 不带入握手，按上表投影至 WS frame metadata。 |
| OpenAI-Beta | 设置 `responses_websockets=2026-02-06`，不混入旧 HTTP beta 字符串。 |
| Accept、Content-Type、Content-Length、Content-Encoding、Accept-Encoding、Host | 属于 HTTP entity/传输层，不复用给 WS handshake。 |
| traceparent / tracestate | 不作为捕获的 HTTP attempt header直接复用；按源码 WS client_metadata 命名投影。 |
| Authorization、ChatGPT-Account-Id | 输入 capture 不得带入；分别由执行探针从该格登记凭据追加。 |
| Connection、Upgrade、Sec-WebSocket-* | 调用者不得提供；由真实 native connector 负责新握手、nonce 和扩展协商。 |

`x-codex-turn-metadata` header 是兼容投影，官方会从中省略无界工具 namespace inventory；body 里的 canonical metadata 可能更完整。不能仅因二者不逐字节相同就判断指纹冲突。[responses_metadata.rs:345](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/responses_metadata.rs:345)

HTTP 使用 zstd entity compression，WS 使用官方 connector 的 permessage-deflate 协商，两者不可互换。转换器记录 HTTP request_compression，但不把 zstd bytes 当 WS 文本或设置 WS Content-Encoding。[responses.rs:151](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/codex-api/src/endpoint/responses.rs:151)、[responses_websocket.rs:559](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/codex-api/src/endpoint/responses_websocket.rs:559)

`x-responsesapi-include-timing-metrics` 只在 CLI RuntimeMetrics feature 打开时加到 WS handshake；不能仅从 HTTP body 推导。0.155.0 此 feature 默认 false，函数默认 false，调用方应按实际捕获配置明确记录。[features/src/lib.rs:1093](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/features/src/lib.rs:1093)

## 4. 首轮状态、时序和 runtime 证据边界

该转换只接受没有 x-codex-turn-state 的新完整请求。官方首次 connect 在 `core/client.rs:1087` 向 API connect 传入 `turn_state=None`；不会把一个新 101 response 的 turn-state 自动当作已有 turn-state 填入首帧。后续生成事件、复用连接和 previous_response_id 的状态机不属于这次首轮转换，不能跨账号复用。

官方在 handshake 完成、发送帧前 stamp 当前时间。调用者若先转换再执行长时间 TLS handshake，就不能声称时间戳完全等价。执行探针应在确认 101 后、发送首帧前受控更新该唯一时间字段，并保存最终 body SHA；也可在该时点调用转换函数。不会因更新 timing 字段改动原题或模型可见输入。

同一个逻辑 session/cache 在两个账号间用于实验，不能证明服务端缓存或路由状态互相独立。native helper 应按该格凭据隔离连接/cookie 状态；一旦使用已有 WS 连接、warmup 或增量 history，就需要另一套对照契约。

## 5. 可执行验证与不等价项

```powershell
python -m unittest discover -s tools -p test_quality_transport_pair.py -v
```

当前 13 项离线测试全部通过。覆盖原始数字词法/Unicode 转义/工具和题目 bytes 不变、完整字段哈希、明确 WS 差异、session/cache/routing 不一致拒绝、重复 JSON key 拒绝、凭据 header 拒绝、非原生控制字段拒绝、已有 turn-state/continuation 拒绝、HTTP 控制与 WS 扩展归属。独立 reviewer 用真实 WS probe consumer 接收转换结果，3 项独立测试通过；此前独立测试暴露“数字词法包装类继承 str，数字 model/cache 被错误接受”的 2 个失败断言，已改为严格 type 检查并补回归。

此前 `live-account-controls-v3` 原始题请求记录来自 **Go → native IPC**，其中含 `instructions:""`。这份资料能证明那一跳发了什么，但不能用作“官方 CLI 入站 JSON 完全原样”的证明：官方 Lite serializer 应省略空 instructions。v5 因此新增 CLI → loopback HTTP 入口 raw tee，先取得 entity wire bytes，必要时仅 zstd 解压，业务 JSON 不经过网关 policy 或重序列化；此新 body 才是当前函数的输入。该入口的 Go HTTP parser 不能保留 header 原始大小写/线上顺序，报告必须保留这一边界。

新的真实官方 CLI 0.155.0 raw HTTP capture 已完成并通过转换。原始解压后 body 为 36,330 bytes，SHA-256 `7e8d4732aaa166959a513be1c11c26459f5b4bbec220407c17e20b2c1434131b`，含 11 个顶层字段，不含空 instructions/顶层 tools；HTTP request_compression 为 zstd。转换后的那次预检 WS frame 为 36,474 bytes。10 个非 metadata 字段的完整原 value bytes 相同，7 个既有 client metadata value bytes 相同；新增只有 type 与 Lite/timing 两个 WS metadata 字段。

本审计另外完成 26 项独立断言，把实际捕获 summary 的每字段规范化哈希与转换预检逐项比较，核对 body pin、字段数量、原文哈希、新增字段集合和零模型调用。[transport-pair-capture-review.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/http-ws-controls-v5/review/transport-pair-capture-review.json) 保存这一复核；[transport-projection-preflight.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/http-ws-controls-v5/transport-projection-preflight.json) 保存完整字段投影。预检中的 timestamp 只是该次预检时间，实际 WS 发帧应按第 4 节刷新并保存最终 body hash。

尚未将 Python 合成 fixture 当成实际官方 CLI 的完整 HTTP/WS 双侧捕获。当前转换有固定源码及其直接序列化原生测试作为依据，真实 CLI HTTP 捕获也已通过；若需要把结论扩大到“官方默认 WS runtime 也生成这些字段”，仍需另一次官方 CLI 直达本地 WS fixture 的完整首帧捕获。报告明确 raw_cli_http_observed=true、raw_cli_ws_observed=false，不能用 source-derived 转换冒充已经完成的原生 WS capture。

冻结实现 SHA-256：`41c2a476a89bd6dd96807af74f8b35abb5f745bf1eb71a084bd2807ecc5c8791`；冻结测试 SHA-256：`a05b833579d14825acf126e3c02397668095f5bcdbe4f17f6d8bcfd53470dc3d`。

## 6. 已由原始捕获定位的网关 Lite 字段差异

v5 同一调用的入口 raw body SHA 为 `7e8d4732…`，Go 处理后备用捕获为 `aae53b32…`。入口不存在顶层 instructions，Go `prepareBody()` 却无条件加上 `instructions:""`；字段差异与固定 CLI 的 Lite serializer 不一致。该捕获没有转发上游模型 POST，因此不是通过修改题目或得到某个答案来推断问题。

对应候选修复只改 HTTP prepare 调用：新增 `prepareHTTPBody(raw, compact, headers)`，并保留原 `prepareBody(raw, compact)` 作为 headerless 兼容包装。只有非 compact 且 instructions 缺失时，按现有 `copyGatewayHeaders` 的 scalar 规则投影 Lite header，最终值精确为 `true` 才保留字段缺失。

这保持 canonical header 键优先、同键取首值、大小写 header 名合并和 Connection-nominated 过滤的一致性；不把 `True`、`TRUE`、`true,false` 当 true。显式 instructions 字符串、空字符串、null 或配置策略加入的 prompt 都保留；传统 API 继续补默认空 instructions；compact 与 WS 行为未改。

新增 [gateway_research_lite_body_test.go](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_research_lite_body_test.go:19) 用与真实 CLI 相同的完整 Lite 首轮结构复现。修复前 6 个叶子失败：4 个真实有效 scalar header 分支，以及未压缩/zstd 两种 instructions 缺失请求。修复后本地 29 个叶子通过、2 个真实 Rust helper 测试因 Windows 环境跳过；另有 CLI transport、完整 body fidelity、推理 controls、request compression、exact capture 相关回归通过。gzip 仍为原有 415 拒绝且不发送上游，未借这项修复新增非官方压缩分支。

证据：[lite-body-summary.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/http-ws-controls-v5/review/lite-body-summary.json)。冻结候选生产文件 `proxy.go` SHA 为 `39ca04fcddb398d0f32815bcddb874fbbf49109f3239d474b051cccd1f7b1304`，`gateway.go` 为 `9f60fa436151977d02e98d019509e05707a411fe14352214a6ecd5fef6cb4dab`；测试为 `047734410aa4b818aba126140b2126634fd4a4fdeda0ed020eb6e78c953c6fc7`。最终 Linux/native 与隔离发布由 root 继续验证，不把本地跳过写成通过。

这修复了确定的源码/请求字段不一致。**尚未证明增加空 instructions 是先前答 29 的原因，也不能据此宣称账号质量恢复。**
