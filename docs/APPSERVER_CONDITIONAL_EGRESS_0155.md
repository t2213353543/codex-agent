# Codex 0.155.0 条件出站边界

这份证据补充主请求捕获，不把未触发接口标作已验证。固定官方可执行文件 SHA256 为 `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`；AppServer 与 `codex exec` 都直接运行该文件。没有重新编译大型 Rust 工程。

## 已完成的本地官方 runtime 捕获

最终 run 为 `20260927T112529.284995Z`，服务器目录 `/opt/codex-agent-integration/appserver-parity/conditional/20260927T112529.284995Z`。整组 Python HTTP fixture、MCP fixture、官方进程运行于 `unshare --net` 新网络命名空间，仅启用 loopback。没有真实账号、token 或对外网络通路；图像响应是固定 1px PNG。不能据此宣称真实图片生成、Guardian 远端接受或计费行为已验证。

| Provider 相对路径 | 运行时捕获 | 真实 writer 与格式 | 已观察到的特殊字段 |
| --- | --- | --- | --- |
| `images/generations` | AppServer / exec 各 1 个 POST | `ImageGenerationTool` → `CodexImagesBackend` → `ImagesClient`，JSON | `model=gpt-image-2`、`prompt`、`background/quality/size=auto`；`x-codex-image-turn-id` |
| `images/edits` | AppServer / exec 各 1 个 POST | 同一后端的 `ImageEditRequest`，JSON，并非 multipart | generation 字段加 `images:[{image_url:data:image/png;base64,...}]` |
| `guardian-classifier` | 两路各 1 个真实 `response.create`，另外各 8 次 WS 预建握手 | Guardian V2 `LunaSampler` → `ResponsesWebsocketClient` | `model=gpt-5.6-luna`、`reasoning.effort=low/context=all_turns`、`parent_response_id`、`x-openai-subagent=guardian`、Responses Lite metadata |
| `guardian` | 两路各 1 个 POST | `ModelClient` → `ResponsesClient` endpoint selector，SSE | `model=codex-auto-review`、`text.format=json_schema`、`required:[outcome]`、`strict=false`、parent/session/turn 元数据 |

6 个触发场景（两种 frontend × 图像生成/编辑/Guardian）均 `turn completed`。Guardian 场景同时触发同步 review 和异步 classifier；8 次握手与 1 个分类请求分开计数，不称为 9 次推理。所有正常模型回复也由 loopback fixture 返回。

图像两个 frontend 的 JSON body 完全相同；默认 `originator/user-agent` 不同，AppServer 为此次 `initialize.clientInfo` 的 `conditional-fixture`，exec 为 `codex_exec`。Guardian 的模型、请求 schema 与 writer 相同，但会话 UUID、时间、父线程元数据和上下文存在各自运行时值；不宣称字节级相同。

可复用向量位于 `integration/appserver_parity/conditional_vectors.json`：8 个官方 runtime 捕获向量，加 1 个明确标记为 source-only 的 memory 向量。只替换合成 Authorization、UUID 和隔离 workspace 路径，保留协议字段、JSON body、originator/UA、原 body hash。`source_files_sha256` 绑定固定版本关键 writer 源码。

## Memory：源码证据，未触发 runtime

`codex-api/src/endpoint/memories.rs:33` 定义 `memories/trace_summarize`；`summarize_input_posts_expected_payload_and_parses_output` 单元 fixture 验证 POST 和 `traces` 字段。`common.rs` 用 serde 将 `raw_memories` 重命名为 `traces`，每项包含 `id`、`metadata.source_path`、`items`，顶层 `model` 和可选 `reasoning`。响应 `output[]` 包含 `trace_summary`/`memory_summary`；解析器也接受旧 `raw_memory` 别名。

`core/src/client.rs:633` 的 `summarize_memories` 复用 `MemoriesClient`、provider auth 和 `build_subagent_headers`；后者添加 originator、适用时的 subagent，以及 MemoryConsolidation 的 `x-openai-memgen-request=true`。本次源码搜索只找到该入口及 `client_tests.rs` 调用，没有建立公开 AppServer RPC/exec 触发入口。未运行此 Rust 单元测试，也未将 Python 手工构造请求伪装成官方 runtime 捕获。

## 出站覆盖账本

| 接口族 | 最高证据层级 | 未覆盖或限制 |
| --- | --- | --- |
| Models catalog、Responses HTTP/WS、续轮、compaction、函数/custom/namespace/structured output | 真实上游捕获 + 项目兼容测试 | TLS/HTTP2 由 MITM 重建，无法证明原始指纹；模型与时间影响不能混作代码因果 |
| 标准 Responses `web_search` 与原生 `alpha/search` | 真实上游捕获；最终轻量 run 实际 search/citation 与 alpha 结果均验证 | alpha payload 由客户端 runtime 产生；服务端没有工具循环 |
| `images/generations`、`images/edits` | 两个官方 frontend → 强网络隔离 loopback 捕获 | 无真实上游图片成功证据；透明转发是否接入项目需单独测试 |
| `guardian`、`guardian-classifier` | 两个官方 frontend → 强网络隔离 loopback 捕获 | 无真实上游 review/classifier 接受证据；不改变客户端判断策略 |
| `memories/trace_summarize` | 固定源码 writer + 未执行的上游单元 fixture | 没有 runtime 触发或真实上游证据 |
| Realtime WebSocket、`realtime/calls`、API `live` | 固定源码存在 | 没有启动音频、SDP、实时会话或验证双向消息；列为未验证 |
| 插件发现、GitHub 下载、账户 settings | 早期 AppServer 真实启动流量捕获 | 不等同模型 provider 请求；产品生命周期由客户端承担，不能任意重放账户写操作 |
| OAuth/refresh、遥测、远端控制、云任务、MCP、浏览器、shell 任意网络 | 不在本次条件 fixture 范围 | 未触发账户变更、任务创建；未宣称“进程所有可能出站完全覆盖” |

## 当前项目仍缺失的请求变体

条件捕获发现官方客户端实际发起 `GET /guardian-classifier` WebSocket Upgrade，随后在连接内发送 `response.create`。此前项目只有 POST，会漏掉该变体。现在 `/guardian` 与 `/guardian-classifier` 的 GET/WS 均已接入固定路径转发，原始消息不经过 Responses 专用提示词/缓存改写，并记录真实 endpoint；两者的普通及 native Rust 传输验证分别记录。

本轮主模型请求池、身份快照、images/memories 辅助 POST 与 Guardian WS 的验证仍不能扩写为所有条件出站都已兼容。Realtime、外部配置、账户和插件生命周期等边界继续按账本列明，不能以某个 POST 成功替代全部传输变体。

## 离线身份快照与计数

`tools/capture_appserver_identity.py` 使用同一固定 binary、`initialize.clientInfo=codex_agent_gateway/1.0`，独立 `unshare --net` 初始化一次，不登录、不建线程、不发模型请求。capture runner 在读取真实测试账户与 generation strace 之前运行它，并将 `CODEX_APP_SERVER_IDENTITY_FILE` 传给轻量入口。

报告拆分 `bootstrap_official_init_count` 与 `generation_official_exec_count`。generation 中零官方 exec 不表示准备身份快照时也从未运行官方进程。目标离线快照的 UA hash 为 `5327208b125c539d11fa97c6812f4ebaed3fa360298d46f039f2ba806c49909b`，UA 为 `codex_agent_gateway/0.155.0 (CentOS 9.0.0; x86_64) unknown (codex_agent_gateway; 1.0)`，与原真实 `official_app_server` pilot 握手的 UA/originator 逐字符串相等，见 `identity-target-comparison.json`。

早期探测显式设置 `TERM=dumb` 得到 hash `cf534acd…`；该快照有有效初始化来源，但不等于 gateway 的 `officialEnvironment`（不传 TERM）。已保留原证据并明确不作为目标匹配证明。修正后的 helper 不传 TERM，平台/版本/terminal 描述均来自实际 initialize 返回，不猜测 OS。

## 重跑

```powershell
python -B tools/appserver_parity_conditional.py --remote --case images-generation --case images-edit --case guardian-classifier
python -B tools/appserver_parity_conditional_vectors.py /opt/codex-agent-integration/appserver-parity/conditional/<run>
python -B -m unittest discover -s tools -p test_appserver_parity_conditional_vectors.py
```

没有生产服务、账户配置或模型调用变更。条件 fixture 生成的文件只在各自隔离目录；真实抓包仍留在原私有服务器目录。
