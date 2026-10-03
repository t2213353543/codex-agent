# 网关粘性会话与缓存核查

核查日期：2026-09-17。实现对象是本项目的 ChatGPT Codex 网关；本地参考源码为 `.local/codex-source-0.154.0`。下面区分公开模型 API 文档、CLI 可观察契约与网关自身的策略，避免把三者视为同一个缓存服务。

## 官方文档结论

[OpenAI Prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching) 要求复用一致的前缀、历史和工具定义。旧模型可用稳定 `prompt_cache_key` 辅助路由；GPT-5.6 及以后自动路由，键主要用于隔离缓存计量。键不保证命中。缓存寿命、可缓存长度、断点和计费依模型而异。

这些公开 API 说明没有证明 ChatGPT Codex 后端接受全部缓存选项，因此网关不擅自注入 `prompt_cache_retention`、`prompt_cache_options`、断点或 `store=true`。账号亲和、出口稳定和键稳定能减少网关引入的变化；不能据此承诺具体命中率。

## CLI 源码契约

以下路径均相对 `.local/codex-source-0.154.0/codex-rs`，未修改 vendored Rust 源码。

| 源码位置 | 核查结果 | 对网关的要求 |
| --- | --- | --- |
| `core/src/session/session.rs:602` | `session_id` 由根线程及全部子线程共享；`thread_id` 标识具体线程 | 账号亲和先用 `session-id`，再兼容旧头、`thread-id` 和原始显式缓存键 |
| `codex-api/src/requests/headers.rs:5` | 独立生成 `session-id` 与 `thread-id` | 不互相覆盖，也不用逐请求 UUID 代替 |
| `core/src/client.rs:504` | 缓存键优先显式 override；内部任务可能使用 `source:parent_thread_id`；通常使用 session ID | 尊重已有 `prompt_cache_key`，只补缺失值 |
| `core/src/client.rs:273` | `x-codex-turn-state` 在同一 turn 内重放，跨 turn 重用违背协议 | 请求/响应透传；不得存成跨 turn 的持久会话标记 |
| `core/src/client.rs:313` | WS 增量复用比较 model、instructions、tools、reasoning、cache key 等属性 | 不随机化内置提示词或重新排序工具/历史；模型或提示词改变后不要自行强行复用旧增量状态 |
| `core/src/client.rs:1833` | WS 请求可能只发送增量项并引用 `previous_response_id` | WS 连接必须持续使用同一账号；无法确认上游是否已执行时不得自动重放生成 |
| `codex-api/src/sse/responses.rs:143` | 从 usage 中读取 `input_tokens_details.cached_tokens` | 统计真实上游 usage，不推测或伪造缓存命中 |

## 本次路由实现

`gateway_sessions.go` 保留现有 Redis 原子 claim，按 `SHA256(keyID + NUL + session)` 隔离网关键与会话，仅存账号引用。文件模式使用受锁保护的有界内存表，不能用于多进程共享；数据库模式缺失 Redis 或 Redis 失败时返回错误，不退回本地随机账号。

`sticky_sessions_enabled` 默认开启；`sticky_session_ttl_seconds` 默认 604800，设置范围为 60 至 2592000 秒。查找和 claim 都续期。这里的 TTL 是网关路由绑定有效期，与模型缓存保留时长无关。TTL 到期允许重新选账号，因此需要恢复旧加密历史的会话应在到期前续用或设置足够长的 TTL。

首绑前按原有权限、优先级、权重与并发上限选号，并验证凭证可用。并发首绑通过 Redis Lua/内存锁选出唯一赢家；失败的提议释放本地并发名额，再回到赢家。选号返回 busy/unavailable 时，再读取可能已经由并发请求建立的绑定，避免错误分类忽略刚发生的首次绑定。

长时间 HTTP 生成通过 `gateway_session_lease.go` 每 20 秒续租；绑定冲突或路由存储失败时取消上游请求上下文。结束请求时停止并等待续租协程退出。这样短 TTL 设置不会仅因单次生成超过 TTL 就在处理中失去绑定。

已有绑定每次重查当前账号启用状态与网关键分组权限。绑定账号忙、凭证不可用、被禁用或不再获准时返回明确错误，不默默切换账号。上游请求发送后保留绑定，即使连接断开或上游报错也不自动解绑，因为网关无法从网络失败断定服务端没有处理请求。

`pickSessionProxy` 对 `keyID + session + accountRef + proxyID` 使用 rendezvous hashing，仅选 `ProxyStore.Get` 返回的已启用代理。相同配置在不同进程中得到相同出口；代理顺序调整不会改变结果。删除或禁用代理只迁移原先选中该代理的会话。添加代理会迁移新代理成为最高分的部分会话；管理员修改代理池会改变这些会话的出口。代理变化不会改变账号绑定。没有可用代理时遵循原先的直接连接行为。

无原始会话头/显式缓存键的请求继续常规选号与出口轮换；生成的缓存键不反向充当会话身份。关闭粘性会话恢复原路由策略，但不删除已有绑定；重新开启时未过期绑定仍有效。因此在有账号绑定的加密历史场景下，不应在同一条对话中随意切换该设置。

## 请求稳定性与统计边界

`gateway_policy.go` 先读取原始会话身份，再补缺失缓存键；自动键按网关键隔离，并保持确定性。显式缓存键由客户端负责其分组语义。内置提示词以稳定内容附加到 instructions，开关和文本变更会真实改变请求前缀；后续请求应保持配置一致。

同一 session 的路由不会因模型、提示词、历史长度变化而换账号。这是账号绑定历史的兼容要求，不意味着不同模型可以共享同一模型缓存。

项目已有 `response_events.go` / `stats.go` 的实际 cached/input token 统计。评估使用一组固定模型与长前缀的多轮请求，先预热，再比较 `sum(cached_tokens) / sum(input_tokens)`、首字延迟和上游错误率。短提示词、冷启动、自然过期、工具集变更及 compaction 应单独观察；不能仅凭开关已启用就标记为命中。

HTTP SSE 和非流式响应仍是 HTTP 传输；非流式可能由网关收集上游 SSE。TLS 是其安全层。WebSocket 使用独立 Upgrade/连接生命周期；不应为提升缓存率盲目把 HTTP 改写成 WS。

## 验证范围

`gateway_cache_test.go` 覆盖配置 TTL 与续期、关闭/重开、到期重绑、分组授权撤销、Redis 不可用时禁止本地降级、稳定出口跨实例/代理重排、禁用代理最小迁移与关闭时恢复轮换。`gateway_cli_test.go` 已有根 session 优先级、账号繁忙/禁用不换号、并发首次 claim、key 隔离及真实 Redis 共享绑定测试。

`gateway_session_lease_test.go` 使用短周期验证长请求续租、绑定冲突和 Redis 不可用时取消，以及关闭/无会话时的旁路。真实 Redis 测试还覆盖不同 Proxy 实例按配置 TTL 续租。

这些测试验证路由和协议行为，不代表真实生产缓存命中率已经提高。真实 Redis 测试需要 `TEST_REDIS_URL`；没有该环境时会跳过。受限 ACL 可通过 `TEST_REDIS_PREFIX` 指定允许的测试前缀；每次测试仅访问及清理自身随机子前缀下的已知键，不使用 KEYS/SCAN 或通配删除。
