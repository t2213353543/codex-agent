# PowerPoint 网关设置

当前版本：2026-09-27。模型推理固定使用 PowerPoint BPS，Codex 客户端负责本地工具执行。设置页、运行诊断和账号运行状态读取后端返回的实际配置；未取得的值显示“未提供”，不把部署默认值当成已生效配置。协议与状态边界见[当前架构](architecture/POWERPOINT-GATEWAY.md)。

## 部署配置

| 环境变量 | 默认 / 作用 |
| --- | --- |
| `BPS_BASE_URL` | `https://bps.openai.com/basispoints/api`；生产要求 HTTPS |
| `BPS_TOOLS_VERSION` | `tools-powerpoint-core-2026-08-10-2b886486`；按已验证版本更新 |
| `BPS_WEB_SEARCH` | `enabled`；`disabled` 增加请求约束并拒绝搜索返回项 |

推理入口没有切回 Codex 上游的选项。修改这些环境变量后重启进程，并在设置页的“PowerPoint 服务 → 高级部署配置”核对。`inference` 对象在 `/api/settings`、`/api/diagnostics` 和账号运行状态的 `effective_policy` 中展示 `backend`、`profile`、`base_url`、`tools_version`、`web_search`、`codex_fallback`。

## 管理设置

更新携带 `settings_version`，冲突返回 409。开关和模式选择即时保存；文本、期限、超时需点击保存。刷新或冲突会保留未保存草稿，失败不会显示保存成功。

| 设置 | 默认值 | 当前行为 |
| --- | --- | --- |
| `websocket_enabled` | `false` | 接受新的 Codex Responses WS 连接；关闭返回 426 |
| `http_stream_enabled` | `true` | HTTP SSE；关闭返回 403 |
| `http_json_enabled` | `true` | HTTP 非流式 JSON；关闭返回 403 |
| `compact_enabled` | `true` | 独立 compact 与 CLI 压缩请求；关闭返回 403 |
| `models_enabled` | `true` | 基于账号实际 BPS access 目录提供模型列表 |
| `builtin_prompt_enabled` / `builtin_prompt` | 开启 / 稳定简短文本 | 与客户端 instructions 按选定方式组合，再适配为 developer 消息；最多 32768 UTF-8 字节，空文本不附加 |
| `builtin_prompt_mode` | `prepend` | `prepend`、`append`、`if_empty`；不重复注入 |
| `sticky_session_ttl_seconds` | `604800` | 会话绑定滑动期限，60–2592000 秒 |
| `compatibility_normalization_enabled` | `true` | 受支持的客户端入口参数规范化；PowerPoint 必需的协议适配始终生效 |
| `automatic_token_refresh_enabled` | `true` | 临期令牌自动刷新；关闭不影响管理端显式刷新 |
| `upstream_401_recovery_enabled` | `true` | 明确 401 时同一账号身份至多一次凭据恢复 |
| `upstream_stream_idle_timeout_seconds` | `300` | 上游流空闲等待，10–3600 秒；持续有数据不以此限制总生成时长 |
| `upstream_response_header_timeout_seconds` | `90` | 响应头等待，5–600 秒 |
| `upstream_compact_timeout_seconds` | `1200` | 压缩响应头与整体期限，30–3600 秒 |

`admin_api` 仍决定管理密钥是否可调用网关；推荐给客户端创建独立分组 API Key。密钥轮换使用单独接口，未提交的设置字段保持不变。

PowerPoint 会话绑定**固定开启**：加密历史不能随意换到其他账号。旧 `sticky_sessions_enabled=false` 不关闭新推理路径的绑定；有效期设置仍生效。缓存键由网关按调用 Key 与选中账号身份强制隔离；`prompt_cache_key` 被 BPS 接受不等于缓存必然命中。旧 CLI 指纹和缓存隔离关闭开关已从界面移除，兼容存储字段不是当前推理控制。

## HTTP、WebSocket 与压缩

HTTP SSE 实时传递原生增量；JSON 在完整终态后返回。Codex WebSocket 的分流、预热、取消与增量历史由网关适配，模型请求复用 BPS HTTP 路径；这不是旧版 Codex 上游的 WS 直通。客户端需配置 `supports_websockets = true`，provider 的 `name = "OpenAI"` 用于开启原生远程压缩分支。

`previous_response_id` 在授权和身份检查后展开历史。独立 `/responses/compact` 与 `compaction_trigger` 都向 BPS 发送 `model/input/metadata`，其余原生历史和加密项目保留。压缩结果、工具调用和续聊状态以真实返回验证，失败状态不会转成成功。

工具通过 `input.additional_tools` 声明，保留 function/custom 项目与 call_id。namespace 仅在唯一匹配时恢复；未声明的办公工具被拒绝。上游不接受的工具选择、禁止并行等控制由请求约束和输出校验处理，不能当成原样透传。结构化输出、服务档位和额外工具族的支持以实现及验收为准。

## 验收

```powershell
go test ./internal/bps ./internal/server
npm --prefix frontend test
npm --prefix frontend run build
node frontend/tests/settings-browser.mjs
python tools/bps_validate.py
```

最后一项使用当前 Go 构建和真实 Codex 0.155，在私有远端隔离环境完成账号推理验收，产生模型用量；细节见[工具索引](../tools/README.md)。没有显式 live 环境时 Go live 测试显示 SKIP，不能记为推理通过。模型目录、ChatGPT 订阅额度、合成协议测试、真实模型调用和生产部署分别记录。

旧版设置、原生 Codex 传输和缓存调查已原样归档：[2026-09-18 设置记录](history/2026-09/GATEWAY-SETTINGS-2026-09-18.md)。其中旧部署、路径、测试数量和开关语义不代表当前版本。
