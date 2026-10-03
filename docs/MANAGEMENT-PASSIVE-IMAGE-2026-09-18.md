# 被动图像用量与观测展示

本轮获得用户被动采集授权。图像观测只读取已有上游响应，不产生探测请求，不从输入图片、URL、base64、尺寸或输出数量推算 Token，不保存图片内容。普通 HTTP SSE、收集为 JSON 的响应、compact JSON 和 WebSocket 轮次复用既有 `recordResponseOutcome -> observeGatewayResponse` 路径，不改变转发字节、重试次数、路由或总 Token 计量。

## 真实来源

参考本机 Sub2API `backend/internal/service/openai_gateway_response_handling.go` 中 `extractOpenAIUsageFromJSONBytes`、`openAIUsageFromGJSON` 与 `mergeHostedImageGenToolUsage`，明确返回图像输入和输出明细的路径如下。本站只解析现有 Responses 根响应或 `response` 包装，不扩展其他协议入口。

| 字段 | 优先来源 | 兼容来源 | 工具来源 |
| --- | --- | --- | --- |
| `image_input_tokens` | `usage.input_tokens_details.image_tokens` | `usage.prompt_tokens_details.image_tokens` | `tool_usage.image_gen.input_tokens_details.image_tokens` |
| `image_output_tokens` | `usage.output_tokens_details.image_tokens` | `usage.completion_tokens_details.image_tokens` | `tool_usage.image_gen.output_tokens_details.image_tokens` |

字段为可空 int64，只接受 JSON 明确非负整数；null、字符串、负数、分数、指数表示和超 int64 数值不当作有效计数。按上表顺序寻找有效计数，主路径显式 0 不被兼容或工具值覆盖。重复响应观察替换同源计数，不重复累加；后续低优先级信息不覆盖已观察到的高优先级明确值。

`image_input_tokens_source` / `image_output_tokens_source` 保存具体 JSON 路径枚举。未观察到有效计数时对应值为 nil，来源为空。工具图像用量不是通过总输入/输出推算的子集，因此管理合计只称“图像输入/输出已记录合计”，不把它加到已有 input/output/total Token，也不据其存在设置原请求 UsageKnown。

## 管理契约

列表、详情和 JSONL 保留原始可空计数及来源。统计总计、各维度和时间序列增加：

- `image_input_samples`、`total_image_input_tokens`；`image_output_samples`、`total_image_output_tokens`。有效 0 计为一个样本，无样本时合计为 null。
- `image_input_total_overflow` / `image_output_total_overflow` 仅溢出时为 true；超过 int64 的合计返回 null，样本数照计，不回绕、不假 0。PG 先 numeric 求和，文件模式检查加法上限。
- `authentication_samples`、`average_authentication_ms`；`response_transfer_samples`、`average_response_transfer_ms`。只统计明确非负阶段耗时，0 纳入平均；阶段可能重叠，不能相加当成总耗时。
- `available.image_input_tokens`、`available.image_output_tokens`、`available.authentication_ms`、`available.response_transfer_ms` 表示是否存在实际样本。

Root 所有的请求观测合同：`client_request_id` 只来自已有合法 `X-Client-Request-ID`，来源 `request_header`，HTTP 范围 `request`、WS 范围 `connection`；不生成或改写请求头。`authentication_ms` 为 API KEY 验证耗时，不含 OAuth 刷新；`response_transfer_ms` 为 HTTP 获得响应头至业务处理结束，或 WS 每轮第一上游帧至终态下游写入完成，包含读响应、本地处理和下游传输，不宣称纯上游处理时间。至少观测一个新阶段时 `timing_source=gateway_handler_v1`。

CSV 仍为 `codex-usage-v3`，原 61 列后按顺序追加 `client_request_id`、`client_request_id_source`、`client_request_id_scope`、`authentication_ms`、`response_transfer_ms`、`timing_source`、`image_input_tokens`、`image_output_tokens`、`image_input_tokens_source`、`image_output_tokens_source`，共 71 列。未知计数为空字符串，明确 0 输出 `0`。脱敏导出将客户端 ID 转为稳定伪名，所有新增来源字符串经过秘密样式清理，避免导入的任意字符串绕过隐私保护。

前端只精确显示 JavaScript 安全整数范围，超出范围提示“超出前端精确显示范围”；CSV 保持后端 int64 精度。合计溢出与普通未知使用不同提示。

## 验证

本地真实 loopback 覆盖 SSE 字节保持、非流式 JSON 内容保持、compact JSON、WebSocket 两轮隔离、额外请求数为零、原输入/输出/缓存总量不变，以及私有 prompt/图片URL/base64 不进入日志。

最终独立 PostgreSQL 16.15 七项专项通过，共 15 项含子测试、无跳过、无失败：图像与新阶段字段管理链路、MaxInt64+1 合计边界、客户端 ID 查询、真实网关到持久化/管理查询、严重级别、无计费合同和 file/PG 聚合一致性。报告在 `.local/management-parity/passive-observation-20260918/passive-details-pg-final-summary.json`，临时 tmpfs 容器已清理，无生产连接或真实账号。

首轮共享源码中其他新增测试调用不存在的 `Metrics.Total` 导致编译未执行，失败日志单独保留；修正为真实聚合接口后才取得上述通过结果。最终完整验证继续以冻结归档绑定，未部署或修改旧预览。
