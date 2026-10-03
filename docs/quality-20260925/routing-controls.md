# 路由控制研究：实际正文与自动 routing hint 一致性

2026-09-25。确认并修复一处请求构造缺陷：`gatewayPreparedRoutingHint` 使用 Go struct 解码，未知 `Model` / `Service_Tier` 被当成精确协议成员 `model` / `service_tier`。**实际正文保留 canonical 值时，自动 hint 可能缺失、凭空带 tier，或在 WS 原始首帧路径中带另一个 model/tier。**

唯一生产修改为 `gateway_routing_hint.go` 的 prepared controls 读取，改成精确 JSON member 的 RawMessage map。没有修改 model、effort、tier、显式 hint、session 选择或观测日志字段。未 SSH、未调用真实模型。大小写扩展是本地构造的触发条件，不声称官方 CLI 会生成这些字段，也不把协议缺陷当成糖果题 29 的已证根因。

## 官方依据与实际路径差异

固定 `.local/codex-source-0.155.0/codex-rs/core/src/client.rs:1023` 的 `build_routing_hint_header` 直接使用已构造请求的 model/service_tier；HTTP `:1547` 从 `request.model` / `request.service_tier` 传入，WS `:1701` 同样从实际请求构造 handshake metadata。它不是再次从任意 JSON 字段做大小写不敏感猜测。

网关 `prepareBody` 通过 map 读取精确 `model` 并重新序列化整个 HTTP 正文，而旧 hint helper 对同一正文用 struct 解码。这两层语义不一致。实验保留第一方生产形态 BaseURL，但实际 RoundTripper 强制改写目的地到 loopback；没有 ChatGPT DNS 或外部连接。出站 HTTP handler 同时捕获真实正文及 hint，逐字段核对未知字段、大整数与 canonical reasoning 均保留。

HTTP 排序和 WS raw path 必须分开：HTTP `json.Marshal` 排序通常把 ASCII 大写扩展排在 lowercase canonical 之前，因此 `Model="shadow"` 并不必然覆盖真正 model。该控制本次确实通过。WS 在未发生 policy 变更时保留首帧原顺序；后置扩展则能覆盖旧 struct 值。

## Fail-before 的实际观察

| 输入条件 | 旧 HTTP 实际结果 | 旧 WS lazy 首帧实际结果 |
| --- | --- | --- |
| canonical model/tier | 正确生成 | 正确生成 |
| model=sol，后置 Model=shadow string | canonical model 正确，作为排序控制通过 | hint 错变 model=shadow，raw frame 仍 model=sol |
| model=sol，Model 为对象 | hint 因 UnmarshalTypeError 消失 | hint 消失 |
| 无 canonical tier，Service_Tier=priority | 凭空生成 tier=priority | 同样凭空生成 |
| 无 canonical tier，Service_Tier 为对象 | hint 消失 | hint 消失 |
| canonical tier=priority，后置 Service_Tier=flex | canonical tier 正确，作为排序控制通过 | hint 错变 tier=flex |
| canonical tier=priority，Service_Tier 为数字 | hint 消失 | hint 消失 |
| canonical tier=null/空串，后置有效 Service_Tier | HTTP 保留 canonical 语义 | WS 错使用后置扩展，生成额外 tier |

HTTP canonical model 缺失/null/空串时，现有 `prepareBody` 原本就返回 400且无上游请求；本次没有虚构这种请求已发出。helper 单独调用此前仍可能使用 Model alias 生成 hint，现一并拒绝。canonical tier 缺失/null 正常只生成 model；空串/错误类型不新增拒绝逻辑，仍沿用“跳过自动 hint，让上游校验 body”的契约。

WS 实验每组跑两个实际回合：第一帧 warmup（generate=false），第二帧 previous_response_id + input=[] 且改变 model。观察只建立一条上游连接；修复不因后续模型变化重新连接或重写已有 handshake。所有首帧/后续帧逐字保留。

红日志 `.local/quality-20260925/routing-controls/fail-before.jsonl`：**1 个顶级通过、3 个顶级失败**，包含子项 26 条。失败包括真实 HTTP、真实双 WS 路径及 helper 非法 model 控制。修改前文件保存于同目录 `gateway_routing_hint.before.go`。

## 修复边界

`gatewayPreparedRoutingHint` 改用 `map[string]json.RawMessage`，只反序列化 `model` 与 `service_tier`。未知大小写变体原样留在发往上游的 body；它们既不能替代 canonical controls，也不能通过自身错误类型导致 hint 缺失。保持既有 ASCII/长度/分隔符验证、第一方路由限制、认证前置和 preconnect 无正文时延后处理。

显式 hint 仍由调用者拥有：真实 HTTP 及真实 WS lazy/preconnect 分别验证匹配、冲突、未来扩展值都不改写，body 不动。没有因这次一致性修复去“纠正”调用者显式选的 hint，更没有凭空提高 effort/tier。

session 的 prompt_cache_key struct 折叠和若干纯日志 metadata 解码也需要各自按实际作用区分。本专项未修改它们；session 归属另由主任务/Q12 独立研究，观测日志问题不能当成上游真实参数已被改写的证据。

## 验证与交付

新增 `gateway_research_routing_controls_test.go` 共 5 个顶级测试；与既有 routing scope、HTTP/WS 字段透传及 WS 多轮研究回归联跑：**18 个顶级通过、0 失败、0 跳过**，包含子项 93 条，package 0.620 秒。green 原始日志为 `.local/quality-20260925/routing-controls/green-associated.jsonl`。

```powershell
go test ./... -run '^(TestResearchRoutingControls|TestQualityQ01RoutingHint|TestQualityQ01HTTPWire|TestQualityQ01WebSocketWire|TestResearchWSSemantics)' -count=1 -timeout=90s
```

冻结 SHA-256：

- `gateway_routing_hint.go`：`1984484c636525eced2183f47413bd7b0812d59d4fcab4eefdfdc9b00893726c`
- `gateway_research_routing_controls_test.go`：`18f052db25061e6cb20de2e3c4b42b0d593ef479ebeb00ec7b080534989ba221`

已交主任务全量集成验证。这些回归证明特定 body/header 不一致被修复；不证明曾有真实账号触发，不证明后台按错误 hint 降档，也不代替原题恢复验收。

最终主任务回填：Linux race/PG/Redis/native 全套 **873 项通过**，固定 CLI 独立 **7 项通过**，已部署 `cdc07d09…39752`；线上 HTTP models 200、WS 101 正常。[完整上线证据](../CODEX-GATEWAY-CONTROL-RESEARCH-2026-09-25.md)保留单项与全量验证、协议正确性与原题质量的区别。
