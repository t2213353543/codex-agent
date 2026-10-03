# 07 HTTP 头部、地址和错误边界审计

审计日期：2026-09-24。源码基线：`.local/codex-source-0.155.0`，只读。行号为本专项完成时的位置，其他专项继续编辑可能使行号移动。

## 实施结果

| 边界 | 原问题 / 核对结果 | 修复和验证 |
| --- | --- | --- |
| 混合大小写头部 | 直接遍历 Go Header map 并赋值 canonical key；重复大小写随机覆盖，列表值丢失 | `proxy.go:269` 排序并优先 canonical spelling；列表合并，已知 scalar 只保留首值；实际网络和 50 次确定性回归 |
| Singleton 与未来扩展 | 重复 UA / Originator / version / opaque turn state 可形成多种身份或状态 | `proxy.go:144,250` 将已知 scalar 明确列出；未知 `x-codex-*` 保持多值，不猜测未来语义 |
| Connection nominated headers | allowlist 中的字段即使被 Connection 声明为逐跳也会转发 | `proxy.go:269` 双向扫描所有大小写 Connection 值；被提名字段不进入新 hop；真实 httptest 上游验证 |
| Originator override | 原逻辑可保留 matching process override，但自定义 process UA + 不同 thread override 会回退 | `proxy.go:203` 保留完整 CLI 风格 UA（版本、OS/arch、terminal），允许 thread Originator 与 UA 前缀不同 |
| UA 回退 | UA 任意位置出现 codex 就视作 native，浏览器说明文字也会跳过回退 | 改为 product 前缀、matching Originator 或完整 CLI 形状；明确是网关兼容启发式，不是认证机制、官方检测器或 TLS 指纹证明 |
| Attestation | 隐式 UA/version/Originator 回退后仍保留 opaque caller proof，与显式 fingerprint override 策略不一致 | `proxy.go:177` 身份回退或 Originator 补全时移除 `X-Oai-Attestation`；未改身份时原样保留 |
| OpenAI-Beta | 需仅删除 obsolete `responses=experimental`，其余能力保留 | 大小写别名合并后按逗号解析，大小写不敏感删除旧 token；其余 beta 的值与顺序保留 |
| Response metadata | 需保留 CLI 所消费的 model、request id、turn state、rate limit、ETag 等字段 | `proxy.go:236,326` 保留既有 allowlist / metered suffix；case-insensitive rate-limit helper；未知扩展多值保留；已知 scalar 确定性处理 |
| BaseURL query / fragment | 字符串拼接将 `/responses` 或 `/models` 放入 query/fragment；models 还可能出现第二个问号 | `gateway_upstream.go:13` 解析并拒绝 RawQuery、ForceQuery、fragment、userinfo、非 http(s)、空 host；保留 escaped path；`gateway.go:683,923` 两处接入 |
| 模型版本 query | 客户端 `client_version` 必须正确转义，不能改变路径 | 实际网络验证 `custom+version` 编为 query，`prefix%2Ftenant` 不变成额外目录 |
| Client IP | 按最后一个冒号切分会损坏无端口 IPv6 | `proxy.go:426` 使用 `net.SplitHostPort`；无端口回退保留完整 literal；忽略未可信的 X-Forwarded-For |
| API error framing | 检查从上游 copy metadata 后生成本地 JSON error 的 Content-Type | 原 `apiError` 正确覆盖 Content-Type，无需改写；新增回归覆盖旧 SSE Content-Type；Content-Length/Encoding 本来就在转发排除范围 |
| Path redaction | Windows UNC 路径完全遗漏，引号包裹的空格路径仅隐藏第一段，CR 可吞并后续诊断 | `proxy.go:50,81` 识别 UNC/device 路径、带单双引号路径和 CR 边界；保留 `/models` 这类单级路由文本 |
| Transport empty result | 回调异常返回 `(nil,nil)` 时 `res.StatusCode` 会 panic | `gateway_upstream.go:37` 首次和重试空响应均转明确错误；首次不触发认证刷新或重放 |

所有复制都独立复制值切片，不修改调用方 source Header。过滤排序只针对允许转发的字段，避免对所有无关请求字段排序。

## CLI 源码对应与保留差异

- `login/src/auth/default_client.rs:137` 的 `add_originator_header` 是 thread 范围 override；`:164` 的 `get_codex_user_agent` 使用 process originator。两者可以不同。因此不能只按 `UA starts_with Originator + '/'` 决定保留。
- `model-provider-info/src/lib.rs:455` 为内置 provider 添加 version，而自定义 provider 可以缺少该头。保持 native 请求缺省 version，不凭网关版本补入；发生网关身份回退时才使用网关参考版本。
- `codex-api/src/requests/headers.rs:5` 生成 session/thread headers；请求 allowlist 保留这些字段以及兼容 `session_id` / `conversation_id`、turn state、subagent、tracing metadata。
- `core/src/attestation.rs:7,24` 仅定义 opaque header 和宿主 `AttestationProvider`。公开代码不能证明 attestation 与 UA 之间存在何种密码学绑定。此处删除是网关“不跨身份改写重放 opaque proof”的策略一致性修复，不声称复制了 CLI 的证明生成算法。
- `codex-api/src/sse/responses.rs:31-67` 消费 `x-codex-turn-state`、`openai-model`、`x-request-id` 等；响应转发继续覆盖它们。`codex-api/src/rate_limits.rs:263` 通过 `-primary-used-percent` 发现扩展 meter，网关还保留该 meter 的 window/reset/name 配套字段。
- `codex-api/src/provider.rs:53` 把 endpoint 加到 provider base，再使用独立 `query_params` 配置；`:35` 的 `endpoint/models.rs` 在已有 query 后用 `&` 添加版本。网关没有独立 provider query_params 配置，因此拒绝 BaseURL 内嵌 query，以明确失败替代误路由。未来若增加 query_params，应显式建模，不恢复字符串拼接。
- `codex-api/src/endpoint/responses.rs:28,40` 包括 `/responses`、`/guardian`、`/guardian-classifier`；网关已有这三类入口，另有 `/responses/compact` 和 `/models`。Guardian body/compression 一致性由专项 09 继续审查。
- CLI 的 Rust `HeaderMap` 对名字大小写不敏感。Go 内部构造的 Header map 允许非 canonical aliases；本次排序、scalar 去重、Connection 剥离是网关边界补强，不是 CLI 在正常网络请求上额外生成的字段。
- Connection 所提名 session header 的本地 sticky-key 处理已交接专项 11；Go WS 地址构建同等校验已交 root 接入；native WS 相关校验由专项 05 审查。

UA 回退仍是兼容启发式，无法从任意自定义 UA 严格判断其是否由 CLI 生成。路径脱敏覆盖常见绝对路径，不宣称对任意非结构化字符串提供完整敏感数据识别。

## 回归

新增 `gateway_headers_audit_test.go` 的 10 个测试函数，覆盖真实 HTTP 头转发、错误 URL 零派发、编码 path/query、证明处理、大小写列表与 scalar、多次确定性结果、无 source mutation、IPv6、错误 framing、脱敏和空 transport response。

已执行并通过：

```powershell
go test ./... -run 'TestGatewayHeadersAudit|TestCLITransport(RequestHeaders|ResponseHeaders|PreservesOriginatorOverride)|TestRequestHeaderIdentity|TestSourceNativeExecHeaderAbsence|TestCodexFingerprint|TestGatewayRecoveryAuthorizationPrecedesBodyCloneAndRetry|TestWeb' -count=1
```

结果：`ok codex-agent 0.752s`；native-transport/tests 包无该组 Go 测试。未运行真实生产请求，未修改 CLI 源码、生产配置或部署。
