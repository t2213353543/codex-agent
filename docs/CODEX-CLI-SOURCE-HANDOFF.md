# Codex CLI 源码侧交付

后续账号协作与源码改进见 [后续交付](CODEX-CLI-SOURCE-FOLLOWUP.md)。下文保留首次交付时点的验证与限制；当前账号及隔离存储状态以新报告为准。

2026-09-17 对照官方最新正式发布 `rust-v0.154.0` 完成本轮网关兼容改进。发布版本由 GitHub Releases 与 npm registry 分别核对；源代码提交为 `6b9826e3aa83b1a5947db50f4332cb9c65f1b340`。本机原全局 CLI 0.153.4 未变更，验收使用独立安装、未经修改的官方 0.154.0。

- [官方源码](https://github.com/openai/codex/tree/rust-v0.154.0)
- [固定源码版本与文件摘要](CODEX-CLI-SOURCE-BASELINE.json)
- [真实 CLI 协议测试证据](CODEX-CLI-SOURCE-TEST-2026-09-17.json)
- [最终 Go 检查、二进制与源码摘要](CODEX-CLI-SOURCE-VERIFICATION-2026-09-17.json)
- [真实模型失败证据](CODEX-CLI-LIVE-2026-09-17.json)

## 已完成行为

- 保留原生 `session-id`、`thread-id`、turn-state、子代理及模型/推理/额度/ETag 头；备用身份版本更新至 0.154.0，保留客户端提供的原生身份。
- 支持原生 zstd 请求，压缩输入和解码结果均受 32 MiB 限制；不把已解压请求标成压缩内容。
- `/responses/compact` 独立接收 JSON，缺省/false/true 的 stream 输入均得到正确 JSON 响应；请求去掉 stream/store。工具、加密 reasoning、compaction 和会话历史不作格式转换。
- 默认 WebSocket 关闭时，认证后的升级请求返回 CLI 源码指定的 426，触发官方 HTTP 回退；普通 GET 仍返回405。可选原生 WS 由系统设置会话实现与验收。
- HTTP 会话绑定优先根 `session-id`，兼容旧头和缓存键；按 gateway key 隔离。Redis 原子绑定跨进程共享，文件测试存储使用有界内存映射。忙时返回429，账号失效或权限改变时失败，不静默换号。
- 绑定包含可用的上游 ChatGPT AccountID 摘要，正常令牌刷新保留绑定，重新导入另一上游账号时旧会话被拒绝。未提供可识别 AccountID 的凭据无法据此区分上游身份。
- 修复并发准入检查与计数非原子、权重随机重复抽取漏选可用账号、首次使用绑定代理触发 nil map panic。
- 流式传输使用每次上游读的300秒空闲超时，活跃输出不再受十分钟总时限限制。取消传播到上游；SOCKS5握手在30秒内结束且响应取消。compact、模型目录和OAuth继续使用有界请求。
- 与文档会话合并 SSE 错误/终态/usage 解析。流式原文透传，观察器内存有界，支持跨块 CR/LF/CRLF；没有终止空行的EOF记录aborted。非流便利接口保留完整JSON终态在EOF的历史容错行为。
- 保留失败响应的实际usage，修复长错误体在诊断截取边界丢字节，模型目录304保留ETag且不记为失败。

## 验证结果与边界

最终 `go test -json ./... -count=1` 有263个通过条目（包含子测试）；`go vet ./...`、Windows构建通过。候选产物位于 `.local/codex-agent-cli-compat.exe`，本轮未部署。

另外显式启动官方0.154.0完成三项端到端客户端测试，合并后再次通过，总计127.811秒：HTTP工具执行及resume、真实WebSocket升级后HTTP回退及resume、实际触发自动compact后继续与resume。CLI实际运行无害shell命令且退出码为0，工具输出抵达上游，原历史及opaque压缩内容恢复正确。上游为受控本地协议夹具，不能将其称为真实模型推理成功。复跑见 [测试说明](CODEX-REAL-CLI-TEST.md)。

真实模型验收使用文档会话的 `codex_cli_integration_test.go` / `tools/codex_cli_smoke.py`。官方0.154.0确实连接本次本地网关，并到达已有线上网关；上游返回401，独立目录请求确认 `token_revoked`。网关key仍有效，失效的是上游OAuth。没有成功生成、工具或真实usage，需恢复有效上游账号后重新验收；未刷新或替换生产凭据。

本轮未运行 PostgreSQL/Redis 实例集成：本地服务不可用，连接原隔离测试主机的直连SSH超时。Windows工具链未配置CGO/C编译器，未执行race检测。这些检查不算通过，条件测试保留用于后续环境复跑。

## 并行交接

文档侧的终态辅助、实际CLI验收工具和证据已合并；重复live入口已移除。系统设置会话新增提示词/缓存策略后，两处严格透传测试显式关闭可选策略以继续验证传输本身，未改变用户要求的策略默认值。

系统设置会话继续负责可配置TTL、代理粘性和启用后的原生WebSocket。已在 `GATEWAY-SETTINGS-COORDINATION.md` 交接每轮权限重检、活动连接/长HTTP请求续租及短TTL不应使活跃会话过期的问题；该会话正在修改共享文件，后续修改需重新验证其检查范围。本报告中的最终文件摘要对应实际检查时点。
