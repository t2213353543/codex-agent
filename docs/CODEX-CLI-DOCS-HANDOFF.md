# Codex CLI 文档侧协作记录

更新时间：2026-09-17。此文件由“参考最新 CLI 文档”的会话维护，供同时研究 CLI 源码的会话读取。

## 当前分工

- 文档侧：官方协议核验、SSE 终态/错误解析与指标一致性、真实 CLI `exec` / `exec resume` 验收工具、接入说明。
- 源码侧：CLI 版本/身份请求头、传输编码、上游请求构造、路由及与 CLI 源码相关的兼容改进。
- 文档侧提供 `response_events.go` 共享终态辅助及 `response_events_test.go` 回归；`stats.go` 最终解析器由源码侧整合，`proxy.go` 由源码侧维护。
- 文档侧对 `gateway.go` 仅接入两处：非流结果使用 `recordResponseOutcome(&rec, result)`；流转发正常结束后调用 `scanner.Finish()`。源码侧重构时请保留这两个调用。
- 文档侧新增 `tools/codex_cli_smoke.py`、`docs/CODEX-CLI-SMOKE.md`，不修改生产服务/数据库/部署配置。
- 修改共享文件前重新读取，避免覆盖另一个会话的更新。本目录不是 Git 仓库。

## 已核验的官方来源

- https://learn.chatgpt.com/docs/config-file/config-reference ：自定义 provider、`env_key`、`wire_api = "responses"`、`supports_websockets`、请求/流重试配置。
- https://learn.chatgpt.com/docs/config-file/config-advanced ：自定义代理 provider 接入。
- https://learn.chatgpt.com/docs/developer-commands?surface=cli ：`exec --json` 与按会话 ID `exec resume`。
- https://developers.openai.com/api/docs/guides/streaming-responses ：Responses 语义 SSE 事件。
- https://developers.openai.com/api/docs/guides/conversation-state ：无状态多轮 input/output 保留。

## 已同步与处理

- compact JSON 被误当 SSE 的问题已由源码侧加入独立分支，文档侧同步用量与终态处理。
- POST 请求丢弃 provider `query_params`，模型列表只保留 `client_version`；请核对是否需要保留允许的查询参数。
- 源码侧已将 CLI 指纹更新为 `0.154.0`，本轮实测使用独立官方 `0.154.0` 可执行文件；本机默认安装仍为 `0.153.4`。
- 文档侧验收默认 `supports_websockets = false`；独立设置会话正在实现可选真实 WS，验证范围见其 `GATEWAY-SETTINGS-COORDINATION.md`。
- 固定 10 分钟流总超时已由源码侧改成流读空闲超时，普通 JSON/模型/OAuth 请求继续有界。
- 标准顶层 `error`、嵌套错误、`response.failed`、`response.incomplete` 已统一处理；未完成原因不会丢失，失败响应的 usage 也由源码侧补齐。
- 收集器和流观察器均改用受限字节缓冲，避免大量小 `data:` 字段造成字符串列表额外内存增长。

## 状态

源码侧的 compact 分支、会话绑定和请求头改动已在共享文件出现；文档侧已保留。非流 Responses 的 `recordResponseOutcome` 曾在并发编辑中落到 compact 分支，现已补到两处分支。

文档侧接受源码侧 `sseScanner.Finish()` 的严格语义：没有空行分隔符的 EOF 尾部不能算 CLI 成功，回归测试已对齐。非流 `collectResponse` 保留旧有宽容行为（完整 JSON 终态允许缺最终空行），这是网关非流便利模式的独立行为。

已实际运行官方 `codex-cli 0.154.0` → 当前本地 Proxy → 已部署网关 → 真实上游，结果在 `docs/CODEX-CLI-LIVE-2026-09-17.json`。最终证据明确为 `http_status_counts: {"401": 1}`、`upstream_authentication_failed`。CLI 正确启动 thread/turn，但上游账号 OAuth 无效，首轮失败，无真实 usage。只读请求 `/v1/models` 返回 401：`Encountered invalidated oauth token for user, failing request`。`.local/production-client-key` 本身有效，能通过 `/api/session`，不要将其误判为网关 KEY 失效。账号 `accounts-sub2-test` 状态为 error。未修改远端账号、密钥或服务；失效凭据不再重试。

文档侧还新增 `codex_cli_integration_test.go`，以 `CODEX_CLI_UPSTREAM_URL/KEY/MODEL` 显式启用，调用真实 CLI smoke，隔离当前本地 Proxy 后验证真实 usage；不等同源码侧 `real_cli_test.go` 的本地模拟上游。

最终验证（20:13）：新增 8 组 Responses 协议测试，含流/非流失败用量一致性；7 项 Python smoke 断言测试通过；`go vet ./...` 通过。源码侧已对两处精确 body 测试明确关闭新增策略，生产默认值保持设置会话的选择。随后全量 `go test -json -count=1 ./...` 通过：110 个顶层测试通过、24 个需外部环境/显式启用的测试跳过、0 失败。汇总见 `docs/CODEX-CLI-DOCS-VERIFICATION-2026-09-17.json`；真实模型验收仍因上游 OAuth 401 未通过。

系统设置会话已启动：分工及指纹补充见 docs/GATEWAY-SETTINGS-COORDINATION.md；正在实现真实 WS 开关、内置提示词、可配置粘性会话及稳定缓存键，独立新增处理器并最小接入 gateway.go，保留 SSE 终态解析。

新增 iOS 指纹会话：用户已明确目标是 iPhone/iPad（不是 macOS）。核验与协作见 docs/CODEX-IOS-COORDINATION.md；尚未证实 iOS CLI/profile，不会编造 UA 或覆盖在途传输/设置修改。请有相关资料或修改边界反馈时追加该文件。


本轮交付完成：统一CLI请求身份开关默认关闭，linux/macos源码参考配置；GET profiles由后端clientVersion统一生成，覆盖HTTP/SSE/JSON/compact/models/WS与OAuth（含源码会话401刷新复用refreshAccountWithSettings）。WS身份切换下一轮409重连；入站attestation在身份改写后移除；原账号client_id/会话/缓存字段保留。说明见docs/CODEX-FINGERPRINT-SETTINGS.md；iOS证据见docs/CODEX-IOS-EVIDENCE.md。

验证：go test -count=1 ./... PASS（7.590s）；go vet ./... PASS；独立指纹专项PASS（0.301s）；隔离前端build PASS（.local/fingerprint-ui/dist），npm test 32/32 PASS，frontend/tests/fingerprint-browser.mjs 6组PASS（320/1440无溢出、失败/409/开关/预选行为）。测试使用本地模拟上游与隔离临时设置，未宣称iOS真机或官方真实推理验收，默认需要环境变量的PG/Redis/真实CLI测试未在本轮重新执行。前端报告见frontend/test-results/fingerprint/results.json。未重启/部署线上，未覆盖frontend/dist。后续集成请保留全部新字段、独立组件与WS409逻辑。
