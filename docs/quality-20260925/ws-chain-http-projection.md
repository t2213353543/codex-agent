# v15：真实 WS 预热与增量帧派生 HTTP 对照

本工具只离线投影，不启动helper、访问凭据、联网或调用模型。[quality_ws_chain_projection.py](../../tools/quality_ws_chain_projection.py)的输出是**派生的完整逻辑HTTP请求**，不是官方CLI原始HTTP抓包，也不是把合成previous_response_id送到真实服务器。

## 输入与必要前提

输入为原始warmup/frame字节、原始inference delta字节、无认证WS应用头，以及分别固定的frame SHA。warmup必须`type=response.create, generate=false, store=false, stream=true`，没有previous_response_id；推理必须同type/store/stream，generate省略或true，previous_response_id恰等于明确提供的合成completion.id。两帧要求完整原生Lite字段、原session/thread/installation/window连续，重复JSON成员（含嵌套、转义同名）、错误类型、未知WS专有header投影、server turn-state均拒绝。

调用者还必须提供完整`warmup_completion`事件及显式`warmup_output_item_count=0`。本次合成响应要求status completed、output=[]、usage input/output/total均为整数0，存在details时值也均为整数0。不能只从completion.output=[]推断此前没有OutputItemDone；否则拼接会漏服务器输出。

固定CLI源码`core/src/client.rs:1248`的增量baseline是先前request.input加`last_response.items_added`；`:2155`把OutputItemDone加入集合，`:2188`在Completed封存。故本受限捕获合同的公式为：

```text
derived_HTTP.input = captured_warmup.input + [] observed synthetic output items + captured_inference.input
```

这不授权真实WS链忽略预热输出；真实chain probe必须独立确认warmup终态和输出，再把第二帧绑定真实返回ID。如果真实warmup出现新增output item，当前对照边界不满足，应保存证据后停止，不假拼两个原input。

## 字节与字段处理

HTTP顶层去掉`type/previous_response_id/generate`；input用原两个数组内部的字节按顺序拼接，item不解析重编码。推理帧其他所有非WS顶层字段的原JSON value bytes保留，包括未知扩展、大整数词法、null与字符串转义；不改题、不新增instructions、不改model/effort/tier。顶层对象键名与逗号属于派生framing，不声称完整字节相同。

两帧非input稳定属性需相同，未知扩展也必须同。`client_metadata`允许按阶段变化；`stream_options/access_programs`按CLI源码`:308`明确属于response级而不参与incremental属性比较，所以保留推理帧值并将任何跨帧变化单列。它们不被默默复制warmup值。

保留推理帧client_metadata中每个非WS值的原bytes，删除仅：

| WS metadata | HTTP处理 |
|---|---|
| `x-codex-ws-stream-request-start-ms` | 移除，仅为WS发帧时间 |
| `ws_request_header_x_openai_internal_codex_responses_lite=true` | 移除metadata项，设HTTP `x-openai-internal-codex-responses-lite:true` |
| `ws_request_header_traceparent/tracestate` | 移除metadata项，按推理帧真实值投HTTP trace头；不复用旧warmup trace |

该映射来自`core/src/client.rs:717`、`:1818`、`:2037`、`:2073`以及`codex-api/src/common.rs:344`。其他metadata保留；未知`ws_request_header_*`因无确定HTTP映射而拒绝，不能误送成普通HTTP字段。

## HTTP应用头来源

实际WS握手的UA/originator/version和会话身份保留；严格核session/thread/x-client-request-id/window与两帧关系。删除WS beta与HTTP实体/握手层头，HTTP新设Accept=text/event-stream、Content-Type=application/json；本对照明确采用CLI默认分支的zstd request compression。`x-responsesapi-include-timing-metrics`在固定源码仅WS握手分支`:1181`设置，转换时删除。

WS握手的X-Codex-Turn-Metadata属于**预热阶段**，不能当作推理HTTP头。工具先验证其语义等于warmup嵌入metadata的compat投影，再从**推理帧**嵌入JSON重建该头：按`CodexTurnMetadataPayload`固定字段顺序，移除`tool_namespaces_info`，其余字段保持；非ASCII字符串按官方ASCII JSON formatter转义，精确数字词法保留。对应`responses_metadata.rs:347`和`:509`。

源码同时支持`#[serde(flatten)] extra BTreeMap<String,String>`。工具仅接受符合该源码合同的最多16个额外string条目（短ASCII名称、UTF8值最多128byte），保留到compat头；未知名称只在报告中hash，不默默丢字段。其余未知类型拒绝。parent-thread/subagent compatibility头也从推理metadata明确值生成；没有推理对应字段却存在旧头时拒绝，以免继承旧状态。

原warmup与推理头的差异、原WS专属字段删除、每个保留值的hash、完整input摘要、两帧项目数、合成prior摘要均进入安全receipt。原session、token、workspace、题面及unknown字段名/值不输出。

## 验证结果与边界

本机15项合成测试通过，包括真实lifecycle capture validator接收派生HTTP、空输出证据、错误prior/generate/hash拒绝、控制字段与unknown字段变化拒绝、重复字段、Unicode和大数字保真、compat头当前turn/inventory省略、trace阶段更新和敏感值不出report。独立reviewer已使用实际projection/control core跑完整driver main的6项、34叶故障场景全部通过，另对chain实际_exchange执行12项、23叶场景；范围与hash在[独立预检报告](../../.local/research-20260925/http-ws-chain-controls-v15/review/preflight-review.json)。

root已对服务器保存的真实两帧执行零模型投影，观察warmup2项+delta5项=HTTP7项，11顶层字段；HTTPbody SHA为`bd3c90982e377d403d046f0e1ef18a00f1ec5a6442a5b30d46ac37c5696f3bca`。最终冻结版本再次对同raw验证通过，见[projection-preflight.json](../../.local/research-20260925/http-ws-chain-controls-v15/projection-preflight.json)。工具SHA为`68d207334dd1f6230dc86ba6b4c42a2974459e5195c81b9c8a36f05f033200f2`，测试SHA为`983f3961187a41e7194599fef740e774c2b005a180838f648e66f67001e30eb2`，与该批model-tooling-manifest相同。

即便完整输入逻辑相同，WS真实warmup是额外服务器动作，HTTP只有一次完整推理；连接、压缩、发帧时间、服务器缓存/会话状态不同，不能称纯网络编码单变量。CLI比较前缀时还会忽略某些internal message metadata，工具保存的是捕获warmup原item，不冒称重建了未被抓到的自然HTTP item。后续报告必须保留这些差异。

调用接口：

```python
body, head, evidence = derive_http_comparison(
    warmup_bytes, inference_bytes, non_auth_ws_head,
    expected_warmup_sha256=warmup_sha,
    expected_inference_sha256=inference_sha,
    expected_prior=synthetic_id,
    warmup_completion=synthetic_completed_event,
    warmup_output_item_count=0,
    http_request_compression="zstd",
)
```
