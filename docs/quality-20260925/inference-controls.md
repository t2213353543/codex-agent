# 推理控制研究：合法代码历史被 JSON 自行膨胀后拒绝

2026-09-25。以已部署 `30634195…df95be` 的请求路径为起点，从入站、policy、prepare、真实 HTTP/WS 出站检查 model、reasoning effort/summary/context、service_tier、include、text 与历史。**普通两轮控制参数没有发现降档；发现合法的约 6 MiB 代码工具历史被网关 HTML 转义膨胀至约 36 MiB，触发网关自己的 32 MiB 限额。** HTTP 返回 413，启用 prompt policy 的 WS 返回 400，第二轮没有到达上游。

这是合法 JSON 和普通推理控制的可达性缺陷，不依赖未知大小写字段或错误类型。修复将网关 API 正文按独立 JSON 编码，避免不必要的 HTML 转义。没有扩大 32 MiB 限额、截断历史、改 effort/tier 或更改题目；没有调用真实模型、SSH 或真实账号。仍不能据此声称已有糖果题 29 是该缺陷造成。

## 基于官方源码的控制对照

固定 `.local/codex-source-0.155.0/codex-rs/core/src/client.rs:783` 构造实际 Responses 请求，模型、reasoning、tier/include/text 为独立字段；`:1796` 的 WS request 可以在后续帧带 previous_response_id 与增量历史。HTTP 使用 `codex-api/src/endpoint/responses.rs:115` 的 EncodedJsonBody；`http-client/src/request.rs:23` 调 `serde_json::to_vec`。WS `codex-api/src/endpoint/responses_websocket.rs:920` 调 `serde_json::to_string`。它们发的是独立 JSON，不是嵌入 HTML 的脚本文本。

本次新增的两个实际回合：

- 第一轮显式 model=fixture-first、effort=medium、summary=auto、include encrypted reasoning、低 verbosity、parallel_tool_calls=true。
- 第二轮显式切换 model=fixture-second、effort=xhigh、summary=detailed、context=all_turns、tier=priority、JSON schema text、高 verbosity、parallel_tool_calls=false，并带 previous_response_id、opaque reasoning、加密工具参数与工具输出。

真实 loopback HTTP 捕获逐字段断言这些控制与输入值一致，同时验证 content-type、账号认证、压缩选择。HTTP 普通与 zstd 分别跑两回合；真实 Rust helper 有独立 Linux opt-in 分支，从网关经 native IPC 到 loopback，而非只验证预测正文。WS 同一连接也跑两个实际回合，实际 frame 的控制参数不变。该对照在 red 阶段已经通过普通小正文；因此不把找到的长度问题错误描述为 effort 被降低。

## 真正的失败边界

第二轮工具输出用 6 MiB `<>&` 组成的合法 source/markup 内容，连同 model、controls、tools、history 等完整 JSON 为 **6,292,601 字节 HTTP**、**6,292,612 字节 WS**，均远小于网关声明的 32 MiB 输入上限。测试采用有界合成数据，只证明网关传输契约，不声称模型的上下文窗口一定接收该长度。

旧 `json.Marshal` 会将每个 `<` / `>` / `&` 编码为六字节的 `\u003c` / `\u003e` / `\u0026`。HTTP `prepareBody` 总会再编码；policy 有改动时也编码整个 body，所以：

| 真实路径 | 旧结果 |
| --- | --- |
| HTTP，prompt/cache policy 关闭 | 第二轮 413，prepared request exceeds 32 MiB，上游只收到第一轮 |
| HTTP，prompt policy 开启 | 同样 413 |
| WS，policy 不改变正文 | 两轮都完成，作为不重编码控制 |
| WS，prompt policy 开启 | 第二轮 error/status=400，prepared request exceeds 32 MiB |

完整 red 日志 `.local/quality-20260925/inference-controls/fail-before.jsonl`：1 个顶级控制通过、2 个顶级测试失败；含子项 4 pass、5 fail、2 native 环境 skip，package 0.946 秒。

## 最小修复与保留契约

新增 `marshalGatewayRequestJSON` 只供网关 API 请求及其 compact replacement JSON 使用，调用标准 `json.Encoder.SetEscapeHTML(false)`，移除 encoder 的末尾换行。没有自制 unescape、JSON 扫描器或浮点重编码。

只替换这些明确的序列化点：

- `proxy.go::prepareBody` 的字符串 input 适配与外层正文。
- `gateway_policy.go` 的 instructions 拼接结果及外层正文。
- `gateway_compatibility.go` 的 system→developer message 和 input 数组。
- `gateway_compact_v2.go` 的字符串 input、trigger input 数组、外层请求，以及 legacy compact 返回中保留的 replacement history。

内部生成字符串也必须使用相同编码器。只改最外层不能消除先前已写进 RawMessage 的转义；本次独立测试了 prompt 拼接、字符串 input、兼容 message、compact string input 和 compact public output。原样传入的 RawMessage 保留 nested duplicate members、大整数和既有合法 Unicode spellings；不把 `\u003c` 反向改写为 `<`。

**缓存 identity 算法不改。** 静态前缀摘要的 json.Marshal/canonicalGatewayCacheJSON 继续使用原实现；同一内容的缓存键不因传输编码修复而改变。管理 API、OAuth JSON、错误消息编码也不作机械替换。所有既有 prepared/result 32 MiB 检查保留，真正超过限额仍拒绝。

标准库的一个明确差异保留：新生成 Go string 的 U+2028/U+2029 仍编码为 `\u2028`/`\u2029`，RawMessage 可保留入站 literal；新增测试分别验证拼写和解码结果，不通过手写 unescape 追求字节表象一致。引号、反斜杠、换行及非法 RawMessage 检查也保留。

## 验证

新增 6 个顶级测试：实际 HTTP/native/zstd 两轮 controls、实际 6 MiB HTTP policy 开关、native 大正文 opt-in、64 字节编码与 Unicode/非法 JSON 边界、所有生成/归一化适配点、实际 WS 6 MiB 续接。large payload 只用约 6 MiB 固定规模，不无限增大测试数据。对传输后的大工具输出比较长度/摘要，不在失败日志倾倒正文。

关联回归本机结果：**44 个顶级通过、0 失败**；含子项 148 pass、5 个明确 native 环境 skip，package 2.910 秒。保存于 `.local/quality-20260925/inference-controls/green-associated.jsonl`。由于 skipped 子项所在顶级仍有普通路径执行，不能只看顶级 pass 就声称 native 全部已测。

```powershell
go test . -run '^(TestResearchInferenceControls|TestResearchBody|TestPolicyCompressionAudit|TestGatewayRequestCompression|TestGatewayCompactV2|TestResearchContext|TestQualityQ01|TestProxyStreamHeadersAndBody)' -count=1 -timeout=120s
```

旧 `gateway_research_body_fidelity_test.go` 的完整 JSON tree 保真仍保留；仅把“实际长度必须等于 HTML 转义后的长度”这一旧实现期待改为“不凭空膨胀”。旧 `gateway_policy_compression_audit_test.go` 不再把合法字符转义超限视作正确拒绝，真正 oversized input 的检查照旧。根任务持有官方 CLI 真实序列化对照，将在完整 Linux/native/CLI 验证中补充实际链路证据。

冻结生产摘要：

| 文件 | SHA-256 |
| --- | --- |
| proxy.go | `a7c3c39d3fc9b666beb9e6dfd9520451a4bdbd70ad61f4c5bedb5de72c0daf41` |
| gateway_policy.go | `69d7f650d54d51c44d9795c3a178759ad2d66f907759fe6428b21cbbb240f3ad` |
| gateway_compatibility.go | `4ab127973a1495a296a956dddc45f8b52bc16f1aeca1f2db17d850b3dbb2075f` |
| gateway_compact_v2.go | `c0419e41b849518e70b75580db5acff1ddfe214883a06423d31c4798e6b76000` |
| gateway_request_json.go | `96c652a703e3c22ccba642c687d08b4f120a1191de7ea89409a69e16bf1b17ef` |

此次结果是合法上下文不再被网关自行转义成超限请求，普通已发送控制仍按客户端原值保留。它不是原题质量恢复证明，不解释内部集群，也不宣称已部署；最终集成与发布由主任务记录。

主任务随后执行的首轮完整 Linux race 验证为 899 pass、1 fail、22 skip。唯一失败是新 WS 6 MiB 测试两个 policy 子项使用通用 `gatewayWSRead` 的 5 秒测试等待上限；其余 native 大历史与 native 两轮分支已通过。主任务仅将该测试的回复读取改为独立 30 秒有界 context，再执行完整 v2 回归，生产序列化代码不变。上述 44 项本地关联通过不等同首轮全量通过；最终 v2 状态由主任务集成记录提供。

最终集成记录已确认 v2 **Linux race/PG/Redis/native 900 顶级通过、0 失败、22 opt-in 跳过**，native 两轮、压缩、大历史和 WS 状态叶子分支实际执行通过；固定 CLI 独立 7 项通过。后端 `f8edfc3e…9a3db7` 已上线，完整范围见[请求 JSON 修复与发布报告](../CODEX-GATEWAY-REQUEST-JSON-2026-09-25.md)。这补充发布状态，不更改首轮失败事实或原题质量验收范围。
