# 09：请求策略、JSON、compact 与 zstd

审查日期：2026-09-24。固定源码基准为 `.local/codex-source-0.155.0/codex-rs`。本专项修改 `gateway_policy.go`、`gateway_request_compression.go`、`gateway_compact.go`、`gateway_compact_v2.go`，以及 `proxy.go` 的 `prepareBody` / `readBody`。`gateway_compatibility.go` 已审查，无需更改。新增测试文件：`gateway_policy_compression_audit_test.go`。

## 已修复

1. **Guardian 压缩选择丢失**：原先仅 `/responses` 保留来访请求的 `Content-Encoding: zstd`，`/guardian` 与 `/guardian-classifier` 会被解压后不再压缩。现三个 Responses 推理端点共享压缩意图门；仍不根据 UA、路径或账号猜测 OAuth 模式，也不给无压缩请求强制添加压缩。Guardian 不执行普通对话的内置提示词/生成缓存键策略，避免改变审批专用上下文。
2. **取消信息丢失与额外工作**：正文读取在进入、底层读取前后、解压输出读取前后检查请求 context，保留 `context.Canceled` / `DeadlineExceeded`，不再误报为损坏 zstd。压缩前后也检查 context，已取消请求不会交给 native helper。同步 `EncodeAll` 已开始后仍需完成当前有界编码；本次没有宣称可以中断任意阻塞 Reader 或单次 codec 调用。
3. **大小写编码头歧义**：统一解析所有大小写形式的 `Content-Encoding`，同时用于解码与压缩意图判断。正常网络头和中间件直接构造的 Go header map 行为一致；重复 `zstd`、`zstd + identity`、大小写别名叠加均拒绝，单独小写字段仍被识别。
4. **准备函数的正文上限**：policy、普通准备、compact v2 在解析前检查 32 MiB；普通准备对重新序列化后的正文也检查，例如 JSON HTML 转义和字符串 input 转换可能扩大正文。统一返回 `errGatewayPreparedBodyLimit`，保留调用方 413 分类。
5. **compact 字符串输入不一致**：compact v2 的字符串输入现在构造成 `content: [{type: "input_text", text: ...}]`，与普通 Responses 准备一致。
6. **畸形 compact 输出被跳过**：拒绝 null、数组、数字、字符串、缺失/空类型项；compact / compaction_summary 必须只有一个且包含非空字符串 encrypted_content。其他未来类型的 encrypted_content 不作字符串解析，避免错误拒绝合法 opaque 字段；v2 适配仍按 CLI 行为只选取 compaction 输出。
7. **zstd 热路径分配**：最多缓存 2 个单并发 level 3 encoder，仅 <= 1 MiB 正文使用缓存；大正文临时创建的编码器不进入缓存，避免峰值历史缓冲区永久留在全局池。编码完成结果独立返回，已有 401 重试继续复用完全相同的 wire bytes。

## 源码依据与一致性

| 边界 | CLI 0.155.0 证据 | 网关结论 |
| --- | --- | --- |
| 请求字段/原始工具 | `codex-api/src/common.rs:259` 的 ResponsesApiRequest；`:316` 的 WS request；tools 使用 raw value | RawMessage 保留未知字段、工具 schema、reasoning、encrypted_content、大整数及数值字面量，不转换为 float64。重新序列化可改变空白、键序、HTML 转义，不能宣称与原始来访正文逐字节相同。 |
| stream / store | `core/src/client.rs:879` 构建 store=false、stream=true | HTTP 上游始终流式且不存储；下游 stream=false 由网关收集 SSE。legacy JSON compact 则移除 stream/store，v2 适配后再恢复 true/false。 |
| instructions | `codex-api/src/common.rs:262`、`:315` 对空 instructions skip_serializing | 网关现有 HTTP 兼容契约仍在缺字段时补空字符串；内置提示词为明确启用的网关功能，非 CLI 行为。未为表面字节相同移除旧后端适配。 |
| 字符串/null input | CLI input 是 Vec<ResponseItem> | 网关接受通用 Responses 字符串并转换为 user/input_text；普通请求的 null 不偷偷改成字符串或空数组，由后端验证。compact v2 缺失/null 明确拒绝。 |
| guardian 路由 | `codex-api/src/endpoint/responses.rs:43`–`:45` 列三路；`core/src/client.rs:1490`–`:1520` 同用压缩选项 | 本次补齐 Guardian 压缩；其专用 prompt/cache 不注入普通策略。Rust helper 压缩准备不限制 HTTP path，无须修改 Rust。 |
| 压缩选择 | `core/src/client.rs:1450` 仅功能开启、uses_codex_backend、OpenAI provider 才选择 zstd | 网关无法从一般 HTTP 请求可靠重建客户端 auth/provider 配置，因此保留实际 Content-Encoding 意图，不猜测、不全局强压。 |
| 压缩格式与重放 | `http-client/src/request.rs:192`–`:224` 拒绝已有编码头并用 zstd level 3；prepared body 可复用 | Go 使用同级 zstd，解码后的 prepared JSON 一致、重试 wire bytes 一致。Go 与 Rust 编码器的最终帧不保证字节相等；native helper 路径保持 Rust 实现。 |
| compact trigger | `core/src/compact_remote_v2_attempt.rs:76` 添加 CompactionTrigger | v2 适配移除既有 trigger 后只在末尾添加一个；未知历史/tool/reasoning 内容不重写。 |
| compact 结果 | `core/src/compact_remote_v2.rs:427`–`:486` 等待 completed，允许其他输出项，要求恰好一个 Compaction | 网关保留该核心条件；新增 malformed item 校验。compaction_summary 是 `protocol/src/models.rs:1214` 的别名，继续接受且保留 opaque 内容。 |
| compact 保留历史 | `core/src/compact_remote_v2.rs:491`–`:696` 依赖 harness metadata、用户消息分类、agent_message 类型、image budget、64,000 token 预算和边界截断 | **legacy compact v2 适配并非完整 CLI history manager。** HTTP 缺少本地 metadata，网关保留 user/developer/system 完整消息的最近 256,000 字节后缀，并附 opaque summary；不伪造 metadata、不拆坏结构化内容。修正注释明确此差异。native CLI 自己发往 `/responses` 的 v2 历史仍由 CLI 处理，不经过此重建。 |
| 超时 | 原 0.154 JSON compact 可无早期字节；0.155 v2 走普通 Responses SSE | legacy JSON compact 入口保留较长总期限，包括通过 v2 适配的旧客户端。原生 0.155 `/responses` 使用 SSE idle timeout。 |
| 缓存/提示词幂等 | CLI 原生 prompt_cache_key 与上下文来自其 session | 网关自定义策略保留显式键；生成键按 keyID 隔离、会话或稳定前缀计算；对象键序/Unicode 转义不影响摘要，数组顺序和精确 JSON numbers 保留。prepend / append / if_empty 重复执行不叠加提示词。 |

`gateway_compatibility.go` 仅在开关开启时移除顶层 temperature/max_output_tokens，将 message 的 system role 改为 developer。不会误删工具 schema 内同名字段，不修改未来类型的 role，不处理 opaque 历史项。这是 ChatGPT 后端兼容策略，而非通用 Responses schema 的限制。

## 限制与官方补充资料

入口压缩 wire 和解压正文仍各限 32 MiB；出口压缩 envelope 有 33 MiB 余量以容纳不可压缩数据的帧开销；解压内存/窗口有界。空帧最终仍须通过 JSON object 验证，损坏、截断、校验和错误、尾部垃圾拒绝；合法串联 zstd 帧按标准解码。

[OpenAI production best practices](https://developers.openai.com/api/docs/guides/production-best-practices) 当前文档说明公共 Responses 压缩上限为 128 MiB，另有压缩比上限；这里保留网关已定义的更小限制，不将公共 API 新规则擅自等同于固定版本 ChatGPT backend 的客户端行为。[Compaction 文档](https://developers.openai.com/api/docs/guides/compaction) 要求 standalone compact output 整体延续；网关对真正 standalone JSON compact 保持原结果转发，只有 legacy-to-v2 适配需要自行重建窗口，两者不可混淆。

## 回归和基准

本专项已通过 Windows Go 测试：

```powershell
go test ./... -run 'Test(PolicyCompressionAudit|GatewayNativeActualCompressionGuardianRoutes|PrepareBody|GatewayPolicy|GatewayCompatibility|GatewayRequestCompression|GatewayCompactV2|SourceCompact|CLITransport)' -count=1
```

新增覆盖：未知/opaque fields、大整数与 1e1000、三个 prompt 模式幂等、null/string input、cache Unicode/键序归一与整数区分、入口/变换后上限、取消前及读取中取消、大小写/重复编码头、损坏/截断/CRC/尾部垃圾、串联帧、两个 Guardian 路由、12 路并发编码隔离、畸形 compact 结果与未来项的 opaque 字段。复用现有双上限、native 能力门、OAuth 隔离、401 byte-for-byte 重放、compact 成功后继续会话/失败/保留历史边界测试。

新增真实 helper 测试 `TestGatewayNativeActualCompressionGuardianRoutes`；Windows 会明确 skip。Linux 主集成需设置 `CODEX_NATIVE_HTTP_TEST_BIN` 后运行此测试，不能把本地 skip 当作真实 helper 通过。Linux race 由主任务统一运行。

```powershell
go test . -run '^$' -bench '^BenchmarkGatewayRequestCompressionAudit$' -benchmem -benchtime=150ms -count=1
```

Windows/amd64，Ryzen 7 9700X，**可压缩重复文本夹具**，单次 150 ms benchmark：

| 正文 | 每次新建 ns/op | 有界复用 ns/op | 新建 B/op | 复用 B/op |
| --- | ---: | ---: | ---: | ---: |
| 4 KiB | 116,093 | 2,593 | 1,650,261 | 4,136 |
| 128 KiB | 84,661 | 16,981 | 1,777,220 | 131,088 |
| 1 MiB | 775,909 | 79,860 | 18,432,281 | 8,309 |

这些数字只证明减少该 Go 编码热路径的初始化/分配成本，不代表生产总延迟同比改善，也不代表不可压缩数据的相同吞吐。缓存条目数固定、请求大小有阈值；并发超出 2 时临时创建额外编码器，完成后不扩大保留池。
