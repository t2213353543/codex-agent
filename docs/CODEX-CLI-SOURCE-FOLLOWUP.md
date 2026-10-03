# Codex CLI 源码侧后续改进（2026-09-17）

本轮沿用已核验的官方正式版 `rust-v0.154.0`，提交 `6b9826e3aa83b1a5947db50f4332cb9c65f1b340`。本文件补充首次交付，不将早期检查的文件摘要当作后续代码的验证证据。

## 已修复

- 收到并成功发送完整 SSE 终态后立即结束请求、关闭上游并释放账号准入，不继续等待上游 HTTP EOF。官方 `codex-api/src/sse/responses.rs` 同样在 Completed 后返回。客户端此时关闭不再把成功或模型失败改记为连接中断。
- 非流式 SSE 收集器支持 CR、LF、CRLF；失败终态即便没有 error 详情仍保留 response 中的实际 usage。
- 记录实际发送给客户端的 HTTP 状态，包括成功 200、模型目录 304、校验失败 400、收集失败 502。ResponseController 的 Flush 和 deadline 能穿透包装器，真实 TCP 首段输出已验证。
- 仅 HTTP 401 可以在同账号、同代理、相同请求体及会话头下重载或刷新凭据，并且最多重试一次。刷新失败保留原 401 正文和响应头；403、429、5xx、网络错误及已经接受的 SSE 错误不触发重放。覆盖 Responses SSE/JSON、compact 和 models。
- 按 `login/src/auth/manager.rs` 的 RefreshRequest 使用 JSON 提交 client_id、grant_type 和 refresh_token，不另加 scope。保留账号原 client_id 与用户可选的身份设置。
- RefreshResponse 不要求 expires_in；优先采用返回值或 access JWT exp，没有已知期限时留空。成功轮换的 refresh token 必须持久化；仅返回 refresh_token 时保留旧 access token 和旧到期时间。
- 过期刷新及 401 恢复均核对账号身份和 enabled 状态；管理槽位在刷新期间被另一上游账号替换时，旧请求不会被送给新账号。成功刷新失效本地账号缓存。
- JSON compact 按官方默认 `4 × 300s` 使用 20 分钟总期限和响应头期限，避免原来的 90 秒 header/10 分钟总超时提前截断。客户端取消仍立即传播；专用 transport 不改变共享 client 并在关闭响应时释放空闲连接。
- 根据真实账号验收发现的错误格式，识别顶层 JSON detail 字符串作为诊断。原始错误响应保持透传，指标沿用脱敏和长度上限，非字符串 detail 被忽略。
- 与设置会话合并非流式输出聚合修复：当完成事件省略 output 时，从 output_item.done 还原完整项目，保留工具、加密历史、实际 usage 与失败结果。源码侧补充官方无索引事件的证据与有界聚合审查，合并后的常规协议回归已通过。

## 验证

- 真实官方 CLI 的 HTTP 工具执行及 resume、WS 关闭后的 HTTP 回退及 resume、自动 compact 及 resume 三项再次全部通过，总计 127.991 秒。证据：[受控上游测试](CODEX-CLI-SOURCE-FOLLOWUP-REAL-CLI-2026-09-17.json)。这些协议测试使用真实客户端与本地受控模型，不等同真实模型验收。
- 针对真实模型测试暴露的 Windows 执行差异，额外使用官方 CLI 的 `read-only` / `never` / `windows.sandbox=unelevated` 完成工作目录文件读取、工具输出回传和同会话恢复，42.769 秒通过。该测试保持受限权限，只使用官方正常沙箱初始化；证据见 [Windows 受限沙箱](SOURCE-CLI-READONLY-WINDOWS-2026-09-17.json)。未因此放宽真实模型验收的工具断言。
- 隔离 PostgreSQL/Redis 七项集成通过：刷新跨账号并发、同账号串行、CAS 和重启恢复、会话共享与可配置续租、跨实例 Settings。凭据只从私有本地环境文件读取，没有写入报告或改变服务 ACL。
- 新增有针对性的生命周期、身份竞态、刷新契约、状态记录、超时和错误诊断回归。最终 Go 检查及产物摘要记录在 `CODEX-CLI-SOURCE-FOLLOWUP-VERIFICATION-2026-09-17.json`。
- 合并后的全量 Go 回归 431 个通过条目（178 个顶层测试），零失败；go vet、Windows 候选构建及 8 项 Python 验收断言通过。未配置的条件集成测试列在 JSON 报告中，不计作通过；Windows CGO 关闭，未执行 race detector。

## 跨会话验收

账号导入与真实模型验收由账号/文档会话独占，统一入口为 TestCodexCLIIntegration，证据为 [导入账号验收](CODEX-CLI-IMPORTED-ACCOUNT-2026-09-17.json)。源码侧不重复导入或并发调用同一测试账号。官方 CLI 0.154.0 + 新账号 + gpt-5.6-luna 已完成完整三轮：首轮、同 thread resume、真实 shell 读取全部通过；4 次 HTTP 200，输入 31074、输出 262、缓存 25600 tokens，耗时 63.943 秒。该结果是真实模型验收，与上文受控上游测试分别记录。

真实工具失败期间，源码侧先用官方受限沙箱验证执行路径，账号会话再定位到 Python 临时目录的 owner-only ACL，只修正一次性合成工作目录的 RX 权限，保留 read-only/never 策略及独立 CLI home 的权限。未降低工具输出、退出码或随机 marker 匹配要求。

设置会话在合并聚合修复后，真实 HTTP JSON、HTTP SSE、原生 WebSocket 各两轮共 6 次也已通过；记录见 [设置实网验证](GATEWAY-SETTINGS-LIVE-2026-09-17.json)。Linux/macOS 应用身份各自的官方 CLI 三轮实测亦由指纹会话完成，见 [身份验证](CODEX-FINGERPRINT-LIVE-2026-09-17.json)。这些报告保留各自验收范围，不宣称设备或 TLS 仿真。

系统设置/原生 WebSocket 及身份配置继续按各自协作文件分工维护。本轮不部署。

## 后续边界

当前到期前刷新仍使用通用 OAuth client，只有 401 恢复明确继承本次选定的会话代理。代理出口配置的连接池目前每请求创建，未实现跨请求的有界缓存。这两点记录为后续传输改进项，不能把应用层身份模板宣称为真实设备或 TLS 指纹。

已向设置 owner 交接 WebSocket 握手 401 复用同账号恢复 helper，以及非流式聚合中混合显式/隐式 output_index 的覆盖边界；当前官方常规事件与真实 CLI 三轮均通过，不能据此宣称所有非标准兼容事件已覆盖。
