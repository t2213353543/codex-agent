# 管理界面观测字段语义

本说明只读核对 source owner 的 `CODEX-ATTEMPT-OBSERVATION-CONTRACT-2026-09-18.md`、`gateway_attempt_observation.go`、`gateway_attempt_hooks.go` 与当前 `RequestRecord`。没有修改网关文件或请求行为。

## 来源

| 字段 | 可支持的说明 | 不可推断 |
|---|---|---|
| inbound/model | 入站请求的模型与允许列表路径类别 | 上游最终接受值 |
| sent_model/upstream_path | 准备后的实际发送模型与规范路径类别 | 已成功接受或最终返回模型；完整URL/主机/查询链 |
| returned_model | 响应实际报告模型 | 缺失时从入站或发送字段补值 |
| attempts | 已观测到的实际 request/refresh/websocket 发送尝试，按开始顺序，最多4条 | 每条都被上游接受；刷新锁等待、缓存命中或凭据重载也是发送 |
| status_code/header_ms | 该尝试实际响应头状态与时间 | 无头时用旧尝试状态兜底，或把HTTP200当完整生成成功 |
| duration_ms | 尝试 hook 的实际开始至完成边界 | 必然是整个响应体/推理耗时 |
| preparation/selection/proxy stage | 实际记录的阶段区间 | DNS/TCP/TLS拆分，或可相加还原总耗时 |
| attempts_truncated/error_truncated | nil未观察、false明确未截、true已截 | missing等于false，或可从保留前缀恢复原错误全文 |

HTTP尝试通常在响应头/dispatch边界完成；OAuth owner可在解析刷新响应后完成，头时间仍独立。401成功恢复后的总体 `upstream_header_ms` 已包含恢复和第二次发送，不再叠加attempt或OAuth阶段。刷新失败时保留第一401实际头时刻；重试无最终头则总体头时间未知。

WebSocket握手只分配给首个已接纳turn，后续turn不重复计入；握手可早于首turn开始，因而可能大于该turn总耗时。这些阶段不是互斥的计费时间段，UI不能叠加成新总耗时。

真实零时间是可观察的0；未采集保持未知。attempt统计仅表示发送动作，不能取代现有终态success/error/aborted。上游返回模型/tier/effort各按自己的来源展示，不用兼容字段冒充sent/returned。

旧 `CODEX-GATEWAY-OBSERVATION-HOOKS-2026-09-18.md` 的“不引入有界attempt历史”描述针对较早hooks阶段；当前新增collector、apply hook及schema字段已推进该能力。collector说明中的“未声明所有hook均接入”仍不能由管理UI测试代替，真实hook接入/转发不变需source owner端到端与冻结race证据。本次只读复核未把该旧阶段描述当作当前字段不存在。
