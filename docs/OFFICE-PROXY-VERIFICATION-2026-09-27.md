# PowerPoint / Google Sheets 请求反代实测

后续PowerPoint验证已通过additional_tools原生自定义工具及真实Codex执行链；本文的422结论仅对应顶层tools写法。最终结果见[PowerPoint + Codex验证](POWERPOINT-CODEX-VALIDATION-2026-09-27.md)。

2026-09-27（北京时间）。**两者的账号模型请求均已脱离办公宿主成功调用；完整 Office / Sheets 工具执行器尚未实现，当前验证也没有证明标准自定义 tools 可透传。**

## 实测结果

用户授权使用本项目线上的两个测试账号。运行时实际有 3 条账号记录，其中恰好 2 条 enabled；仅使用这两条，未使用停用账号。使用现有 access token 在服务器内发出请求，没有刷新、替换或将凭据下载到本地。

| 边界 | PowerPoint | Google Sheets |
| --- | --- | --- |
| 现有 Codex OAuth token 访问 BPS | 两账号均 HTTP 200，allowed=true | 两账号均 HTTP 200，allowed=true |
| 独立 HTTP / SSE 文本请求 | 两账号均成功，2/2 | 两账号均成功，2/2 |
| 指定输出与终端事件 | BPS_PROBE_OK，response.completed，status completed | BPS_PROBE_OK，response.completed，status completed |
| 服务端官方工具调用 | 返回 list_slides | 返回 read_sheets_metadata |
| 回传 function_call_output 后续聊 | HTTP 200，正常完成 | HTTP 200，正常完成 |
| 标准自定义 function tools + tool_choice | HTTP 422 | HTTP 422 |
| 去掉 tool_choice，仅保留同一自定义 tools | HTTP 422 | HTTP 422 |
| 浏览器执行真实工具模块，无真实宿主 | PowerPoint APIs are unavailable in this runtime. | Sheets RPC parent bridge is unavailable. |

文本测试使用鉴权后的目录实际列出的 `gpt-6-luna`、`low`，未依赖静态默认模型。两账号的 plan 声明均为 `self_serve_business_prolite`；令牌的公开 OAuth client_id 为 Codex 客户端，BPS 仍实际接受了这两条现有令牌。这是本次账号样本的结果，不代表所有账号或所有凭据类型。

工具续轮仅在第一个账号测试。首轮是模型真实产生的官方 function_call；脚本不执行文档读写，而回传明确标记的“测试环境没有宿主”错误，再附用户指令要求结束。两者均完成第二轮。这证明工具输出结构可被接收、会话可继续，**不证明真实文档操作成功，也不单独证明模型理解了错误内容**。

自定义 tools 的两个变体均收到 `422: Invalid request body.`，没有更具体的字段诊断。因此准确结论是“本次标准 Responses function schema 未通过此入口”，而不是“任何自定义工具永远不可能支持”。

## 实际请求链

```text
测试客户端（本次为服务器 Python）
  -> https://bps.openai.com/basispoints/api/responses
  -> BPS 依据 editor/profile 与工具版本设置办公任务配置
  -> SSE 文本输出或 function_call
  -> 客户端执行宿主工具并回传 function_call_output
  -> 下一轮 /responses
```

PowerPoint 与 Sheets 共用 BPS 后端及前端主模块，分别选择不同 profile。不是两套已经证明拥有独立额度的模型服务。

- 访问和实际模型目录：`GET /responses/access?include_models=true`。
- 推理：`POST /responses`，`Authorization: Bearer ...`、`X-Basispoints-Auth-Mode: chatgpt`、账号标识头及客户端 profile 头。
- body：`model`、`input`、`stream: true`、`store: false`、`reasoning_effort`、`model_selection`。
- metadata：`task_id`、`turn_id`、`agent_iteration`、`bps_tools_version_id`。
- 官方前端默认不发送调用者 tools；服务器返回最终 prompt/tools 的版本与 SHA256。省略 tools 不代表关闭办公工具。
- 工具续轮保持 task_id / turn_id，递增 agent_iteration，携带完整前轮 output 与匹配 call_id 的 function_call_output。

服务器实测返回：

| profile | tools version | prompt version |
| --- | --- | --- |
| PowerPoint | tools-powerpoint-core-2026-08-10-2b886486 | prompt-basispoints-powerpoint-2026-08-10-c39ea244 |
| Sheets | tools-sheets-core-2026-06-01-34ba0624 | prompt-basispoints-sheets-2026-06-01-ca67edb0 |

实际完整 hash 已保存在生成回执的 resolved_headers。它们证明本次服务器选择了相应办公配置。

## 真实前端与宿主边界

[统一 Office manifest](https://bps.openai.com/basispoints/api/office/manifest.xml) 当前版本 2.0.0.1，发布目录 `360590d7-f8f9-4d88-bf75-0edfe0a4b9f3`，build 1790362980。当前 Office 加载项还涵盖 Word；Word 不在此次模型实测范围内。

- [PowerPoint 入口](https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/powerpoint/) 加载 powerpoint-u0FoMu0V.js。
- [Sheets 入口](https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/sheets/) 加载 sheets-Cj41PwVg.js。
- 两者在浏览器运行时实际加载同一个 x-square-DUrhLSGN.js，SHA256 与资源清单一致。
- 无宿主时显示安装/宿主提示；显式模拟 Office host 或 Sheets host-ready 后出现各自真实登录 UI。这里只模拟宿主身份，没有模拟服务器认证。
- 实际导出的 PowerPoint list_slides executor 缺 PowerPoint API 时失败；Sheets read_sheets_metadata executor 缺 google.script.run / 父 iframe RPC bridge 时失败。两项都在浏览器中实际调用并记录错误。
- Sheets 工具使用 Apps Script 或 bps_sheets_rpc / bps_sheets_rpc_result 消息桥；PowerPoint 工具依赖 Office / PowerPoint API。只转发模型 HTTP 请求不会自动提供这些执行器。

## 证据与复跑

所有文件相对于项目目录：

- `.local/office-proxy-verification/production-inventory.jsonl`：线上账号数量与启用状态。
- `.local/office-proxy-verification/production-access.jsonl`：两个账号 × 两个 profile 的访问许可与目录，账号标识已脱敏。
- `.local/office-proxy-verification/production-generate.jsonl`：4 次 HTTP 200、完整终端事件及指定文本。
- `.local/office-proxy-verification/production-custom-tools.jsonl`：自定义 tools + tool_choice 失败。
- `.local/office-proxy-verification/production-custom-tools-only.jsonl`：仅 tools 的独立对照失败。
- `.local/office-proxy-verification/production-native-tools.jsonl`：官方工具调用及显式模拟错误回传后的续聊。
- `.local/office-proxy-verification/browser-evidence.json`：实际加载与宿主工具错误。
- `.local/office-proxy-verification/powerpoint/sources.json`、`google-sheets/resource-hashes.json`：公开前端来源及 SHA256。
- `.local/office-proxy-verification/remote_script_audit_after.json`：探针成功判定的离线核查。

一条命令复跑两账号 × 两入口的最小文本测试（会产生少量模型用量）：

```powershell
python .local/office-proxy-verification/run_remote.py .local/office-proxy-verification/remote_verify_bps.py --mode generate --output .local/office-proxy-verification/production-generate-rerun.jsonl
```

使用既有生产 SSH helper；账号凭据仅在远端进程内存处理，输出不包含 token。脚本要求恰好两条 enabled，否则停止，避免意外扩大范围。工具与前端版本固定为本次捕获值；将来遇到版本拒绝时应重新读取 live manifest。

此次没有部署反代服务，没有修改线上账号凭据、服务配置或办公文件。管理接口读取和模型调用可能产生正常审计及用量记录。未验证长期令牌刷新、并发、所有模型、WebSocket 实际生成，以及真实 Office / Google 工作簿的完整工具操作。先前两个子任务报告保留了无凭据阶段的结论；本报告与带账号回执是最终结果。
