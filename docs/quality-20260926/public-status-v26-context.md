# v26 官方公共状态背景：未找到同窗事故佐证

检索及实际抓取时间：**2026-09-25 20:27–20:32 UTC**（北京时间9月26日04:27–04:32）；只取3个官方页面。对照本地 v26 第7格：请求19:54:38开始、报告19:55:03结束，批次19:55:04停止，模型为 **`gpt-5.6-sol`**，HTTP200流内error；保存的code/type SHA分别精确匹配 `server_is_overloaded` / `service_unavailable_error`。未恢复未知错误message。

## 状态记录能否匹配

截至20:30 UTC实际抓取，官方历史页的最新事故发表于9月24日；提取到的93条记录中，没有9月25日发表的事故，也没有记录时窗覆盖本次19:55:04事件。下表时间是官方记录的**首次发布时间→恢复更新时间**，不是独立测得的故障实际起止。[OpenAI Status历史](https://status.openai.com/history)

| 官方记录涉及范围 | 首次发布→恢复（UTC） | 与本次关系 |
|---|---|---|
| GPT-6 Astra Pro错误率 | 9月24日18:59:44→19:46:28 | 前一日且非本次模型 |
| Codex与ChatGPT Work错误率 | 9月14日02:46:48→03:57:19 | 已恢复，不同日期 |
| GPT-5.6 Sol API错误率 | 9月11日09:08:15→11:21:43 | 同模型名称，但无时间重合 |
| Codex 5.6-sol服务过载错误 | **7月17日**17:19:22→20:42:53 | 搜索标题容易误读为近期；不是9月25日 |

没有检索到能与本次时段对应的官方Codex质量退化公告。该“未找到”只限定本次查询及已抓取的历史内容，不证明单次服务错误不存在、所有用户都正常，也不能把旧事故归为本次29/23错答的根因。[OpenAI Status历史](https://status.openai.com/history)

## effort与usage的证据边界

精确模型页列出 `gpt-5.6-sol` 支持的effort包含medium与xhigh，medium为默认值。本记录不替换成其他模型，也不以通用页面枚举覆盖本地Codex目录所报告的全部档位。[GPT-5.6 Sol模型页](https://developers.openai.com/api/docs/models/gpt-5.6-sol)

官方说明effort引导模型投入多少推理，模型也会按任务复杂度自适应使用token；`usage.output_tokens_details.reasoning_tokens`是已生成推理token的数量。推理token占上下文并按output计费，`max_output_tokens`同时约束推理、可见输出及不可见格式token。页面没有给出由effort回显或单次token数反推出私有集群、硬件时间、固定算力上限或账号风险标记的公式；“这些字段不足以证明内部算力相同或被限制”是本审核的证据边界判断，不冒称为官方已确认某种降级机制。[Reasoning模型指南](https://developers.openai.com/api/docs/guides/reasoning)

两份文档正文未显示明确发布/修订日期。指南HTTP `Last-Modified`为2026-09-25 20:03:38 UTC，只记录为响应元数据，不当作内容首次发布日期。三页公开HTML、提取文本、HTTP状态/时间/SHA以及事故结构化时窗保存在 `.local/research-20260926/public-status-v26-context/`；结论及本地第7格原报告SHA见 `official-review.json`。没有社区材料作为官方确认，没有新增模型、OAuth或生产操作。
