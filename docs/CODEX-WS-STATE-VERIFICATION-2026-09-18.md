# CLI 多轮状态与预热观测

本轮继续使用官方 CLI 0.155.0 和封存的 Rust release，只在本地合成环境验证，不使用真实账号、不调用模型上游、不部署。

## 实现

`RequestRecord.WebSocketWarmup *bool` / `websocket_warmup` 是纯被动标识：仅 WS `response.create` 的 `generate:false` 为 true，缺省或 true 为 false；null、非法值、旧日志及 HTTP 保持未知。传输类型仍是 `websocket`，不改消息、准入、请求总数、token 用量或聚合。即使标记为预热，上游已报告的用量也不会被丢弃。

此字段保存在请求日志 JSON，不新增数据库迁移。JSONL 重启及 PG 聚合重建测试检查三态不混淆，并保持 3 请求、17 tokens。PG 重建时请求明细尚存；不声称删除明细后还能从 `usage_events` 恢复预热标识。既有总请求/成功率/延迟包含预热，不将其重命名为已排除预热的推理统计。没有恢复管理侧已退役的计费功能。

## 官方 CLI 状态链

两组真实 CLI 场景分别测试握手头和 `response.metadata` 的 turn-state 来源，每组执行一次真实 echo 工具与一次新进程 resume。服务端保存每条连接已完成的上下文，逐次验证：

- `previous_response_id` 精确指向同连接最新已完成响应，不仅判断字符串非空。
- 工具续接只带一个对应 `call_id` 的 `function_call_output`，没有重复已知 input/output。
- resume 按前一输入、前一输出、当前增量重建后的上下文仍含原始任务、工具结果和后续任务。
- 网关透传握手 turn-state，但 CLI 0.155 该调用路径传入 `turn_state: None`，不会将它自动放入 `client_metadata`。这不是网关丢失头部。
- `response.metadata` 的首个 hint 被 CLI 消费，后来的不同 hint 不覆盖它；resume 使用新连接的首个 hint，不粘旧状态。

最终观测：4 条连接、4 次预热、6 次生成响应、6 个精确 previous-response 引用、2 次真正工具执行及工具增量回传、0 次 Go 上游回退。报告中的 `tool_output_requests=4` 包含恢复历史中再次出现工具结果的请求，不等于 4 次工具调用。

本次 CLI 实际产生的空增量为 0。另一个独立原生 TLS 合成用例 `TestGatewayNativeActualWebSocketEmptyDeltaAndOpaqueItems` 验证预热后 `input:[]`、混合 function/custom tool output、加密 reasoning、9007199254740993 大整数、显式 null 与未知字段的精确字节保留；它不是 CLI 自行产生空增量的证据。

官方 CLI 在请求属性变化时选择全量输入，不足以证明服务端禁止携带 previous ID 的新 instructions。因此本轮没有凭此修改网关策略或虚构 409 重连限制。源码细节见 [状态来源复核](CODEX-WS-STATE-SOURCE-2026-09-18.md)。

## 固定来源

- Go：`eb242227dfba5a377d83cbaf8b6caed8432b5acdb5424beaad4b3fe39d037d87`。
- 冻结目录：`.local/source-20260918/gateway-overlay-20260918t071245-10fe84/source`。
- 管理基线已从旧 5a818 切到已验证、无计费的 `bdfbf92ba3102028440c00f75fb8d728f117fc5f4f3f227782429198b2147c82`，保留 CRS 原子提交等管理实现；不是当前管理在途源码或前端整合包。
- Rust helper：`6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`，未改生产实现、未重建。
- 官方 CLI：`660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`。

## 验证

同一冻结源码全库测试、`go vet`、Windows/Linux amd64 构建通过。本地 Linux PG/Redis `-race`：290 顶层测试、722 事件通过，0 失败/跳过/缺失，159.826 秒，临时资源已清理。包括预热文件重启、PG 重启/重建、既有请求/token 聚合和无计费接口合同；报告 `.local/source-20260918/local-linux-race-20260918t071417-79f57f/report.json`。

固定 Go/helper 配对的 21 项原生联调全部通过，0 跳过，容器已清理。包括新增精确状态链和 TLS 空增量用例，并重新覆盖 HTTP、WS、压缩、OAuth 合成刷新、代理及崩溃回收。原始报告：`.local/native-transport-build/gateway-compare-20260918T071306-c8f465/result.json`，状态链细节在同目录 `gateway-ws-cli.json`。

所有输出、用量和缓存值由本地合成服务提供，不据此声明模型质量、生产成功率、真实缓存收益或完整美国设备环境已验证。

## 复跑

```powershell
python tools/native_transport_compare.py --native-run 20260918T054243Z-b43907cb --source-dir .local/source-20260918/gateway-overlay-20260918t071245-10fe84/source --suite gateway
```
