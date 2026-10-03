# reasoning.mode 端点诊断

本轮在8次请求的预登记上限内，实际只发送2次便停止：new2 的 `standard/medium` 完整答21；new1 的首个 `pro/medium` 请求返回HTTP400、`invalid_request_error`，没有模型答案。后续6格未发，不重试、不刷新、不修改任何账号或网关默认参数。因此没有取得pro模式质量对照，也不能由此断言账号风险状态或权益。

开始前实际阅读了官方文档：GPT-5.6家族的 `reasoning.mode` 可取standard/pro，mode与effort独立；[Sol模型页](https://developers.openai.com/api/docs/models/gpt-5.6-sol)确认模型归属。[推理指南](https://developers.openai.com/api/docs/guides/reasoning)展示的是公开Responses API能力，不保证ChatGPT OAuth Codex端点接受同一设置。本次不支持把该公开能力当作网关的默认“质量修复”。

原题、`gpt-5.6-sol`、medium、工具、会话元数据及非认证请求头固定。只在同一真实CLI捕获的正文中插入或替换 `reasoning.mode`；独立字节区间检查和actual IPC正文/认证/headers核对通过。第一格的响应回显standard、报告3006 reasoning tokens；第二格189字节错误体以安全结构摘要和hash保留，未猜测未知错误code或完整message。

请求级失败与评分分开：400不是答案29、不是能力测量，也不记作账户恢复失败。该端点拒绝了本次pro请求；仅凭错误类别还不能确定是否因为模型、账号、接口或其它参数约束。

新增投影及runner通过18项离线测试，包括usage失败后停止、未知mode回显、body/header意外变更、重复启动、HTTP400/401/403和超时不重试。执行unit已退出，两次helper均清理，13份服务器产物哈希核对一致，4份临时原始捕获已删除。已完成网关日志查询的两个时间窗均无重叠记录，该查询不覆盖外部直连或未完成请求。

[逐次执行](../.local/research-20260926/reasoning-mode-controls-v24/models/execution.json) · [HTTP400安全摘要](../.local/research-20260926/reasoning-mode-controls-v24/models/02-pro-new1/report.json) · [清理及哈希](../.local/research-20260926/reasoning-mode-controls-v24/cleanup-verified.json)
