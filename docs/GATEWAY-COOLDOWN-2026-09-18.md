# 上游限流冷却验收

网关收到上游 HTTP `429`、WebSocket 结构化限流错误，或 HTTP `200` SSE 中的结构化限流终态后，会记录账号与模型的临时冷却。已发送请求不因 `429` 自动重放；当前 HTTP/SSE 状态和模型终态仍按原协议返回。

## 行为

- 独立请求跳过冷却中的账号，可选择其他可用账号。已有会话保留原账号，返回 `429` 与向上取整的剩余 `Retry-After`。
- HTTP Responses 与 compact 按模型共享冷却；模型目录使用独立作用域。WebSocket 每个 `response.create` 都检查冷却。尚不知模型的 Upgrade 限流作用于该账号的连接级作用域，后续模型请求也会检查该作用域。
- 接受秒数或 HTTP-date 格式的 `Retry-After`。缺失或格式错误时默认 4 秒；显式 `0` 或过去日期不产生额外冷却。有效长延迟在 1 小时以内不会被缩短，超过 1 小时会截断到 1 小时。并发较短限流响应不会缩短已有较长期限。
- 身份键包含稳定账号 ID、持久化凭据版本、实际使用的 access token 和模型作用域，最终只保存 SHA256 摘要。账号改名及一般管理修改不会清空冷却，实际凭据替换可以解除旧冷却。旧 JSON 兼容存储不持久化凭据版本，因此使用实际 token；相同 token 的重复导入本身也是导入 API 的无操作分支。
- `401` 恢复后记录实际新凭据的冷却；流式失败观察器通过响应 Request 的私有内存 context 获得同一凭据身份。已有 WebSocket 按其实际握手凭据检查，不会因为管理快照换 token 就错误解除旧连接的冷却。
- 结构化 `status: 429`、`rate_limit_exceeded`、`usage_limit_reached`、`rate_limit_error` 用于识别限流，不检查错误消息中的关键词。普通 `response.failed` 不产生冷却。
- Responses、models、WebSocket 的本地容量忙状态改为类型化错误，统一返回 `429`；HTTP 和 WS 错误事件提供 `Retry-After`，不再通过匹配 `busy` 文本判状态。

## 共享状态与边界

配置 Redis 时，每个命名空间使用一个有界 sorted set 存储哈希键和期限，Lua 使用 Redis 时间原子更新，只延长期限并清除过期项。Redis 读取失败返回 `503`，不允许在未知共享状态下偷偷切换会话账号。没有 Redis 时使用进程内冷却；没有新增 PostgreSQL schema 或管理界面配置。

本机最多保留 16,384 个活跃条目；Redis 最多 16,384 个条目加一个容量标记。达到容量时采用临时保守拒绝，直到容量标记到期，避免驱逐仍有效的长冷却期限。这是极端容量下的可用性取舍，可能暂时影响其他账号或模型；不会无限增长，也不会提前遗忘已收到的有效期限。

冷却不能取消已经获准并发执行的请求，也没有新增请求队列或自动转发重试。首次上游 `429` 的状态、错误正文及原有头保持透传；缺少头时的 4 秒默认值用于后续准入提示。

## 本地验证

通过 10 个顶层冷却回归（含 11 个子用例）与 1 个真实 PostgreSQL/Redis 选择器回归，共 22 个测试节点。使用独立临时 PostgreSQL schema 与隔离 Redis 测试命名空间，未访问生产账号或模型。

```powershell
go test -run '^Test(PG)?GatewayCooldown' -json -count=1 -timeout 90s .
go test -run '^TestPGGatewayCooldownSelectionBindingAndReplacement$' -json -count=1 -timeout 2m .
```

集成测试需要显式提供 `TEST_DATABASE_URL`、`TEST_REDIS_URL`、`TEST_REDIS_PREFIX`；未配置时相应集成用例会跳过。URL 和凭据不写入文档或日志。

覆盖独立请求选择其他账号、固定会话不迁移、模型隔离、秒数/日期/默认/长时间/过期、凭据替换、内存与 Redis 上限、Redis 失败、models 容量忙、`401` 恢复后的 HTTP 与 SSE 限流、HTTP `200` 的结构化失败、WS 每轮检查与 Upgrade 限流。

最后新增的 PG 用例首跑因使用仅管理用途的 fixture、未显式开启网关 AdminAPI 而失败；改用现有网关专用 fixture 后通过，未为此改动生产逻辑。最终 PG 用例耗时 36.20 秒。完整证据索引见 [JSON 验收记录](GATEWAY-COOLDOWN-2026-09-18.json)。Linux race 由独立审计会话汇总，本文件不把普通 Windows 测试当作 race 结果。
