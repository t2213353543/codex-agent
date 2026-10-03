# 被动日志观测

2026-09-18，用户明确允许增加被动数据采集用于日志观测；主动观测由用户自行解决。本轮不增加探测、回放、定时推理或配额请求，不启用现有手动/自动检查，也不改变路由、账号选择、刷新、重试和请求/响应内容。

## 图像用量

记录 `image_input_tokens`、`image_output_tokens`（nullable非负整数），以及各自 `*_source`。只读取响应中明确的usage计数，按顺序选择主 `usage.input_tokens_details/output_tokens_details.image_tokens`、兼容prompt/completion details，再到 `tool_usage.image_gen` 对应details。

明确0保留，缺失、非法和未采集为未知。同一次响应重复出现不累加；同连接后续WebSocket轮次不继承前轮值。图像Token不另加进原input/output/total，不改变业务计量；不存图片内容、base64、图片URL或推算尺寸/张数。

统计新增 `image_input_samples`、`total_image_input_tokens`、输出同名对应字段，分别返回样本数和nullable合计。整数合计溢出时该合计未知，并以 `image_input_total_overflow` / `image_output_total_overflow` 明确标记，不回绕或显示伪零。

## 客户端关联

`client_request_id` 仅来自已有、唯一的 `X-Client-Request-ID` 请求头，规范为 `[A-Za-z0-9._:-]{1,128}`。无头、重复、多值、超长或非法输入不采集。不从Authorization、OAuth client_id、会话ID、上游请求ID或模型正文补造；不生成、不回传新头、不改变现有头转发规则。

`client_request_id_source=request_header`；HTTP作用域 `client_request_id_scope=request`，WebSocket为 `connection`，因此多轮可共享同一握手关联值，不能当每轮唯一ID。业务事件ID、上游RequestID、ResponseID继续独立。

`client_request_id`是独立精确筛选，贯穿列表、统计、快照、导出和清理。默认脱敏导出使用稳定摘要；显式身份导出才含原关联值，来源和scope保留。

## 实际耗时

新增nullable毫秒及 `timing_source=gateway_handler_v1`：

- `authentication_ms`：HTTP已有API Key验证的开始至结束；WebSocket每轮已有授权验证（包含其原有账号存储读取）的开始至结束。不是OAuth耗时，也不是从其他耗时相减得出。
- `response_transfer_ms`：HTTP获响应头后，到该响应读取/解析/下游写入处理结束；WebSocket每轮第一条匹配上游事件至终态观测，或已开始接收后取消。为保持原有记录顺序，WebSocket不包含终态事件最后一次下游写入。无匹配事件时未知。

统计分别提供 `authentication_samples/average_authentication_ms` 和 `response_transfer_samples/average_response_transfer_ms`，只使用实际样本。原准备、账号选择、代理选择、头等待和TTFT保留各自含义；区间可重叠，不相加为总耗时，不填充未采集的DNS/TLS阶段。

## 准入错误

单独 `GET /api/ingress-events`，不记入业务日志、usage_events、请求成功率、Token或账号最后状态。只记录当前网关准入路径中实际发生的：`invalid_api_key`、`settings_unavailable`、`method_not_allowed`、`unknown_endpoint`、`websocket_disabled`。

事件白名单：独立事件ID/序号、`time_ms`、真实HTTP码、原因枚举、已知标准端点、合规客户端关联ID、nullable授权毫秒。不记录原始路径/query、正文、Authorization、Cookie、IP或账号/KEY身份。

采集采用独立有界队列256、单worker最多256待写批次，持久文档保留最近1000条。队列满只累计丢弃，不阻塞业务；持久化提交结果未知时不自动重放，计入 `persistence_unconfirmed`，不能称作确定丢失。关闭服务先排空原HTTP/WS处理，再最多5秒处理观测队列；失败明确报告。

返回 `scope=retained_ingress_events`、`source=gateway_ingress_v1`、精确留存匹配数、淘汰数及采集状态。队列、失败和未确认计数只对应当前进程，重启清零，不冒充历史总量。游标固定首次读取上界；并发新增不混页，留存淘汰或文档重置时409要求刷新。

支持原因、HTTP状态、客户端关联ID、开始/结束Unix秒及每页条数；详情中的发生时间为Unix毫秒。前端点击入口才读取管理API，无上游请求，不提供重放/探测动作。

## 保留与验证

旧数据缺字段保持未知，不回填。CSVv3在原61列后追加上述10个业务观测字段至71列；准入事件不混入业务CSV。本轮测试仅使用合成本地上游和本地存储，需同时验证实际采集、持久化、管理查询以及原请求/响应字节、请求次数、Token累计不变。
