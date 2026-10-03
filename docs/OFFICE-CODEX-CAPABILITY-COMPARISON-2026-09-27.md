# BPS 三入口与 Codex 通用反代能力比较

后续验证已更新自定义工具结论：PowerPoint接受input中的additional_tools，真实CLI文件/MCP/压缩链路已通过。下文保留早期顶层tools测试事实，最终结果见[PowerPoint + Codex验证](POWERPOINT-CODEX-VALIDATION-2026-09-27.md)。

2026-09-27，基于前两轮模型/工具验证和本轮真实服务器请求。**若限定三选一做通用对话/研究原型，优先 PowerPoint；若目标是现有 Codex CLI 无感切换、完整自定义工具和会话协议兼容，当前不能将任一 BPS profile 作为等价替代。**

本轮补测继续使用用户授权的第一个已启用账号，模型 `gpt-6-astra` / `xhigh`，请求直达官方 BPS。没有部署服务或修改线上凭据。

## 能力矩阵

| 能力 | Excel | PowerPoint | Google Sheets | 对通用反代的含义 |
| --- | --- | --- | --- | --- |
| 单轮文本、SSE | 已成功 | 已成功 | 已成功 | 可转发 |
| 输入完整历史续聊 | 压缩输出续聊已成功 | 普通双轮与压缩输出续聊已成功 | 压缩输出续聊已成功 | 网关可以管理历史；未证明所有 Codex 增量会话字段直接通用 |
| 独立 /responses/compact | HTTP 200，加密 compaction | 同左 | 同左 | 需要 BPS metadata，不能盲目原样转发 Codex 请求 |
| 压缩后恢复仅存在于被压缩部分的测试事实 | 成功 | 成功 | 成功 | 短合成上下文上的语义保留通过；未验证长工具历史/跨 profile 或跨账号压缩项 |
| 网页搜索 | 真实 web_search_call + 引用 | 同左 | 同左 | 服务端实际提供搜索，不能只据此前“未测试”就判不支持 |
| 标准 Responses 自定义 function tools | HTTP 422 | 前轮两变体均 HTTP 422 | 前轮两变体均 HTTP 422 | 当前最大兼容缺口；这不证明所有可能的适配方法均不可行 |
| 官方办公工具 | 缺真实 Excel 执行器 | 工具调用/错误回传已成功，真实Office执行器仍缺 | 工具调用/错误回传已成功，Apps Script桥仍缺 | 模型输出工具调用不代表本地 shell/apply_patch/MCP 工具已接通 |
| 原生 Codex WS 增量续接、重连、取消、并行工具 | 未验收 | 未验收 | 未验收 | 尚无完整替代依据 |

## 本轮新增实测

### 网页搜索

对三个 profile 发送相同指令，要求实际联网查 OpenAI 官方 Codex CLI 安装命令并给来源，禁止操作办公文件。

- Excel 返回 3 个 completed web_search_call（search/open_page/find_in_page）及 url_citation。
- PowerPoint 返回 3 个 completed web_search_call 及 url_citation。
- Sheets 返回 4 个 completed web_search_call 及 url_citation。
- 全部 HTTP 200、response.completed；依据是结构化事件，不是模型自称搜索或仅生成一个网址。

### 压缩

第一轮普通 API 形状 `{model,input}` 对三个 profile 均得到 HTTP 400，明示缺少 `metadata`。根据该实际错误补充 task_id / turn_id / agent_iteration / bps_tools_version_id 后，三个入口都 HTTP 200，输出包含 message 与带 encrypted_content 的 compaction。

普通生成接口接受该压缩输出，三者均能正常完成后续回答。为了排除答案仍在保留的明文 message 里，进一步把测试码只放在压缩前的 assistant 回答中：三个压缩结果的明文部分均不含测试码，续聊仍能恢复测试码。这是本次的语义保留检查。

这补充了静态代码的不足：实际前端虽然没有独立 compact 调用点，但真实后端支持该端点。前端通常发送 `context_management:[{type:'compaction',compact_threshold:200000}]` 做内联压缩；本次仅在 PowerPoint 的小上下文续聊中验证该字段被接受，未触发阈值，因此不声称已验证大上下文自动压缩。

### 工具和会话

本轮 Excel 的标准 function tools 请求同样返回422，与前轮PowerPoint、Sheets现象一致。SDK存在tools参数不等于该业务代理接受同样schema。

PowerPoint 普通双轮测试输入完整历史，第二轮正确回忆第一轮的测试码。前端采用 store:false 与完整input回放，未观察到 previous_response_id 的产品调用路径。Codex更完整的WS会话亲和、增量续接、取消及错误恢复，需要单独做兼容层验收。

## 选择理由与实现方向

PowerPoint 的暂时优势是办公工作流干扰较少：前一轮糖果原题，两个PowerPoint账号直接答题，Excel和Sheets都先要求读工作表。但这是小样本的profile行为差异，不能据此宣称它使用更强模型、拥有更多额度或一定更稳定。增加统一纯对话要求后，三者都答对。

三者共享BPS后端，真正应维护的是一个BPS适配器和三个profile，而不是三套重复反代。适配器可负责账号与会话绑定、完整历史、BPS metadata、SSE、搜索事件、compact输入输出。必须保留原生工具调用和加密上下文项，不能仅拼接文本或伪造工具成功。

如果希望提供Codex CLI等价体验，决定性的下一项是找到合法可用的自定义工具schema/路由，并实际通过“客户端提供工具定义→模型调用→客户端执行→回传结果→继续生成”的完整闭环，同时确认用户system/developer instructions如何与服务端办公提示词合并。此项未过前，套一个/v1/responses外壳不能保证shell、apply_patch及MCP能力正常。

可以自己另建Agent层提供外部搜索、摘要和工具执行，但这属于功能适配，不能称为原生Codex协议透明透传。生产主通道继续使用已验收的Codex路径，BPS更适合作为对话/研究的补充通道，直到工具和会话兼容验收完成。

## 证据与复跑

- [搜索、普通会话、参数与Excel工具结果](../.local/office-proxy-verification/capability-review/results.jsonl)
- [metadata修正后的独立压缩与续聊](../.local/office-proxy-verification/capability-review/compact-results.jsonl)
- [压缩明文不含测试码的语义保留验证](../.local/office-proxy-verification/capability-review/compact-semantics-results.jsonl)
- [前端静态核查](../.local/office-proxy-verification/capability-review/static-notes.md)
- [前轮三入口糖果题](OFFICE-CANDY-TEST-2026-09-27.md)
- [前轮PowerPoint/Sheets工具边界](OFFICE-PROXY-VERIFICATION-2026-09-27.md)

从项目目录复跑压缩语义检查（输出文件须为新名称，会产生少量模型用量）：

```powershell
python .local/office-proxy-verification/capability-review/run_capabilities.py --script remote_compact_semantics.py --output compact-semantics-rerun.jsonl
```

长期令牌刷新、生产并发、所有模型、长上下文压力和完整Codex客户端兼容均未在本轮验证。
