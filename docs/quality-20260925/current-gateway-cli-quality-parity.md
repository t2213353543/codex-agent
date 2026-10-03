# 当前 a52ad 网关与 CLI 0.155.0：v12 请求对照边界

2026-09-25。已核对实际发布二进制`a52ad225ce147692d6b0355b460e0ad2b63d59e8cc19ddbaac4a849ef32672a6`、发布source manifest`374d772b…ccc9`及archive`6b533ee7…aa6e`。十个相关生产源文件在archive、manifest、v12隔离gateway-source三处摘要相同。[逐文件来源证明](../../.local/research-20260925/gateway-direct-controls-v12/review/source-proof.json)

**v7“仅新增 instructions 和改 cache key”的预期不能沿用。** a52ad已经部署新的账号/客户key会话投影：默认或显式cache key、session/thread正文和对应头都会按各自规则改变。这些是本轮需要测量的现行策略，不是自然CLI逐字节透传。它们是否改变答案只能由实测判断，源码差异本身不证明质量原因。

## 两路范围

固定同一份新CLI完整首轮原题body，顺序为`direct-new2,gateway-new1,gateway-new2,direct-new1`，再完全反向，每账号每路两次、合计8。CLI来源固定0.155.0 SHA`660e159a…b501`，native helper固定`9110b869…ccce8`。direct每格fresh helper直连；gateway采用已发布源码的单账号隔离fixture与当前生产settings，禁自动refresh/401recovery保证一次dispatch。它测试**当前生产settings下的隔离网关代码链**，不能叫生产监听端口、真实多账号调度或完整线上客户namespace测试。

普通fixture客户key ID为`quality-account-pair-key`，不是生产客户key或admin身份。a52ad把客户key ID纳入投影，因而即使user/workspace相同，隔离fixture也不能冒充实际生产客户的缓存分区。每账号的两次gateway请求应稳定使用同客户key、相同真实owner、原session；API-key ID是本地客户身份，不是官方OpenAI APIkey认证，出站仍使用账号ChatGPT OAuth AT。

## 已确定的投影合同

| 边界 | 固定CLI自然请求 | a52ad gateway的预期 | 核验证据 |
|---|---|---|---|
| 模型和推理控制 | 原model/medium、reasoning/context、tier、stream/store | 本次原生首轮保持原语义；无凭空reasoning预算或tier变更 | 每个完整字段canonical hash/类型/presence |
| 原题及完整input | 包含开发者/用户消息、工具、item ID、内部metadata | 缓存投影不改input，所有item原内容及顺序保持 | input总hash、逐item/field hash |
| instructions | Lite通常缺失，指令在input中 | 当前builtin prompt启用时按设置新增/组合；其值必须等于实际配置策略，不能仅凭字段名放行 | source/effective settings、安全prompt hash、实际新增值hash |
| prompt_cache_key | 默认=`client_metadata.session_id`；内部override可独立 | 所有非空key都重新投影。原key精确等于原body session时kind=`session`，否则kind=`prompt-cache`；显式override不再原样透传 | 原kind判定、预计映射摘要与实际值摘要 |
| body client_metadata | 原session/thread、installation/turn/window等 | 只映射session_id/session-id及thread_id/thread-id | 每字段hash，其他键/存在性不变 |
| body嵌入x-codex-turn-metadata字符串 | CLI完整元数据JSON字符串 | 只改其session/thread原值span；其他内容、大数字词法、转义保留 | 新嵌入字段摘要及原字符串hash |
| hyphen session-id/thread-id头 | CLI会话/线程ID | 分别映射为相同kind UUID | header值hash与同kind预计值 |
| underscore session_id/conversation_id头 | CLI原capture决定是否存在，不能预设 | 普通HTTP从**原cache key**导出同一16hex，覆盖或新增两头；不是取已映射body key再hash | 两头各恰一项、相等、预计hash |
| X-Codex-Turn-Metadata头 | 有界compatibility metadata；可能省略tool inventory | 只改该头自身的session/thread | 该头原→出站逐字段hash，不与body嵌入全文强求相等 |
| installation/turn/window/request/device | 真实CLI捕获值 | 此缓存策略不改，不做设备画像收敛 | body/embedded/header各位置独立比较 |
| UA/originator/version | 捕获到的真实CLI身份 | fingerprint关闭且native UA可识别时保留；开启时必须按已登记runtime profile解释 | 真实settings、出站header数量/值hash |
| 压缩 | CLI启用压缩且Codex backend/OpenAI provider时zstd | 只依输入Content-Encoding选择；若Python网关入口identity则上游none，不会从UA推断zstd | ingress编码、native IPC request_compression单列 |

UUID映射的确定性种子是JSON tuple `["codex-agent:codex-account-identity:v1",keyID,namespace,kind,trimmedOriginal]`；取SHA256前16byte并设UUID version/variant位。完整owner的namespace为`chatgpt:<workspace>:user:<user>`，不再包含本地account row ID或owner revision；未知owner使用不可逆凭据摘要分隔。HTTP underscore头单独使用SHA256(`u<keyID>:a<namespace>:<trimmedOriginalKey>`)前8byte十六进制。两种值有意不同，不能为“统一”强改成一个值。[gateway_prompt_cache.go](../../.local/research-20260925/gateway-direct-controls-v12/gateway-source/gateway_prompt_cache.go:27)、[gateway_cache_headers.go](../../.local/research-20260925/gateway-direct-controls-v12/gateway-source/gateway_cache_headers.go:13)

本轮完整首轮是HTTP；WS不可套用其underscore规则。WS握手按原`session-id → session_id → conversation_id → 原cache key`选session，仅原有conversation_id才单独投影conversation头。已建立socket不会因后续key变化重新握手。compact也有独立fallback，不用普通HTTP计划解释其他transport。

缺失cache key在a52ad不再自动补出；兼容模式去掉OAuth不支持的retention，generic客户端cache options可移除，native opaque options保持。这些若未出现在本次capture中，不制造成已发生差异。local sticky routing从原会话头/key决定，content fallback仅本地，不应新增上游session/input。[gateway_policy.go](../../.local/research-20260925/gateway-direct-controls-v12/gateway-source/gateway_policy.go:17)

## 头优先级与官方源码依据

`copyGatewayHeaders`先去Connection提名的hop字段，按casefold分组，canonical拼法赢scalar冲突，每个scalar只保首值，list值保留；cache投影读取的是这个过滤后的头，不从原始入站头复活已删身份。HTTP随后用原cache key设两个underscore头；hyphen和body元数据保持各自对应kind，不把`X-Client-Request-Id`当session一起改。[proxy.go](../../.local/research-20260925/gateway-direct-controls-v12/gateway-source/proxy.go:283)

官方`ModelClient.prompt_cache_key()`优先内部override、其次内部代理parent key、最后原session；`CodexResponsesMetadata.client_metadata()`生成flat及JSON字符串元数据，compatibility_headers有界且可省tool_namespaces_info。自然CLI不实施本gateway的owner/keyID哈希；内部override存在也不等于公开CLI配置可任意指定本轮投影。[client.rs:497](../../.local/codex-source-0.155.0/codex-rs/core/src/client.rs:497)、[responses_metadata.rs:306](../../.local/codex-source-0.155.0/codex-rs/core/src/responses_metadata.rs:306)、[headers.rs:6](../../.local/codex-source-0.155.0/codex-rs/codex-api/src/requests/headers.rs:6)、[压缩选择](../../.local/codex-source-0.155.0/codex-rs/core/src/client.rs:1450)

`prepareHTTPBody`会重新编码顶层对象，因此即使业务内容保持也不能把gateway完整body wire hash与原capture强求相等。应分别验证canonical逐字段精确数值语义、input全树和声明的身份变化；只有direct分支要求实际IPC body原字节hash匹配。zstd/none属于独立传输条件，不能藏进“全部请求完全一致”结论。

## 已接入的嵌入证据与独立工具

新增[quality_nested_session_evidence.py](../../tools/quality_nested_session_evidence.py)并接入实际[quality_native_ipc_capture.py](../../tools/quality_native_ipc_capture.py)两入口，只新增安全字段：

- header摘要的`turn_metadata_summary`。
- body摘要的`client_metadata_turn_metadata_summary`。
- 每个结果含source UTF8 hash、complete/parse_status、字段数量，以及每字段canonicalhash、json_type、presence。session/thread/install/turn/window/request等固定键名可见，未知键名hash；任何原token、session值或正文不输出。

使用原IPC摘要的ExactJSONNumber和canonical回调，任意深度duplicate拒绝；非object/非法UTF8/非有限数、深度/字段/1MiB限制返回明确incomplete，不用部分结果宣称相等。body和header各自前后比较：允许变的只有session/thread，其他字段应保持；旧工具冻结报告不回写。

[parity_analysis.py](../../.local/research-20260925/gateway-direct-controls-v12/review/parity_analysis.py)接收两个安全JSON envelope `{body:<body_summary>,head:<head_summary>}`及policy，输出逐字段差异。只有字段名属于预期策略面时，仍标`actual_value_unverified`，必须提供独立预计digest才能称value verified；input变化、embedded window/installation等变化直接列异常，细粒度摘要缺失则列evidence gap。不把整个client_metadata允许变化当作全字段放行。

新嵌入工具7项实际测试通过；原IPC41项中35通过/6平台跳过，exact21项中19通过/2平台跳过；独立parity5项通过，包含恶意附改window/input的拒绝。Linux平台验证由主任务的冻结bundle执行，不把Windows跳过算通过。三工具文件已冻结，摘要见[核验清单](../../.local/research-20260925/gateway-direct-controls-v12/review/expected-ipc-checklist.json)。

隔离Go fixture新增官方origin合成transport与真实native loopback两个key模式；前者核HTTP头官方origin gate，后者核实际helper body/SSE。fixture owner已实跑Linux12 pass、1仅真实账号entry opt-in skip、vet与编译通过。二者证明不同边界，不能把loopback host自动说成完整官方头投影；本审阅只读其代码与manifest，没有发模型。

截至此交付未发现必须先修生产代码的具体缺陷。实际八格报告还应逐格核原始epoch、唯一实际POST、native运行hash、source/effective settings、所有body及header差异和清理；这些是结果验收，不由当前源码审计预先代替。
