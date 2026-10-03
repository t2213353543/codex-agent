# 专项 12：调度、请求出口与冷却

日期：2026-09-24。源码基线：本地只读 `.local/codex-source-0.155.0/codex-rs`。本专项修改 `gateway.go` 的调度函数、`gateway_cooldown.go`、`gateway_request_route.go`；没有修改 HTTP 入口、OAuth 刷新实现、会话归属文件或生产环境。

## 修复结果

| 位置 | 原问题 | 修复后 |
| --- | --- | --- |
| `gateway.go:327` `weightedPick` | 多个接近 MaxInt 的权重累加溢出；负溢出固定选首账户，正溢出也可能使账户失选。空数组直接越界。 | 普通权重保留原来的精确整数抽样；累加即将溢出时使用浮点加权 reservoir 抽样，保持所有大权重账户可选且无额外数组分配；空数组安全返回零值。极端权重路径使用浮点概率，有通常的浮点精度限制。 |
| `gateway.go:220` `pickAccount` | 一个正常账户并发满、另一个账户冷却一小时，返回一小时 Retry-After；客户端会在正常账户已释放后仍等待。 | 只要有其他条件合格的 busy 账户，返回 typed busy 429 + Retry-After: 1。真正只有冷却候选时仍取最早冷却期限。 |
| `gateway.go:166` `acquire` | 无效 limit 每个新 key 都分配计数条目，即使永不准入。 | limit < 1 在分配前拒绝。正常账户默认并发仍由 effectiveConcurrency 归一为 1。 |
| `gateway_cooldown.go:66` | Retry-After 向上取整使用 duration 加法；极远时间可能溢出后变为 1 秒。 | 先除再处理余数，不进行可能溢出的加法。 |
| `gateway_cooldown.go:163` | 本地冷却读忽略取消；Redis 期间取消统一被改写为 shared-store unavailable；内部异常 delay 可超出共享键 TTL 保护范围。 | 读前保留 context 错误；Redis 出错时优先返回调用方取消；内部记录 delay 也封顶一小时。网关自身共享存储超时仍是 typed 503。 |
| `gateway_cooldown.go:214` | 已收到上游 429 后，下游恰好取消会取消 Redis 记录，其他节点不知道此次冷却。 | 写冷却使用 WithoutCancel 保留 scope/context values，共享写仍受内部 3 秒 timeout 约束；未来读仍在 Redis 故障时 fail closed。 |
| `gateway_cooldown.go:261` | headers 使用 map[string]string，任意数字/布尔 quota header 导致整个事件解析失败并漏记 429；忽略 CLI 支持的 status_code。 | headers 用 RawMessage 容忍非字符串值；Retry-After 接受字符串及数字；识别 status_code 别名，显式非零 status 优先。 |
| `gateway_cooldown.go:314` | 未知 named lane、冲突 response ID 找不到归属时退化为 connection 冷却，污染同账户全部模型。 | Named event 只匹配自己的 FIFO head；无 lane 的已知 response ID 可匹配活动 head；unknown/mismatch/completed scoped 事件不记录。首个失败尚未收到 response.created 的默认/命名 lane 保留兼容处理。只有没有 lane/response identity 的事件进入 connection scope，即使默认 lane 仍活跃也保留真正全连接 429。 |
| `gateway_request_route.go:27` | OAuth 已选定出口后，当前账户代理绑定改为另一个代理，或 direct 改 required，仍返回请求缓存中的旧出口/直连。 | 保持同一 admission 出口，但若该出口不再属于当前账户绑定则拒绝；不自动改到另一代理。仍被允许的出口保留，新请求可采用新配置。取消的路由请求也在选择前返回 context 错误。 |

## CLI 一致性与明确差异

| CLI 0.155.0 证据 | 网关行为 |
| --- | --- |
| `model-provider-info/src/lib.rs:374` `retry_429: false` | 网关收到真实 HTTP 429 不透明重放 inference；记录后原样交给调用方。与 transport 层 429 不自动重放一致。 |
| `codex-client/src/retry.rs:23` 按 HTTP status、transport error、attempt limit 决定 retry；`:97` 根据 backoff 重试 | CLI 有自己的 5xx/transport 重试；网关此专项不额外叠加自动 inference 重放。多账户 priority/weight/concurrency 是网关功能，CLI 单登录状态没有对应池调度。 |
| `codex-api/src/endpoint/responses_websocket.rs:599` wrapped error；`:602` status_code alias；`:675` scalar JSON header conversion | 修复了 alias 和非字符串 header 导致漏记的差异。网关只将 Retry-After 的数字/字符串用于 delay；布尔 Retry-After 作为无效值回退，符合 HTTP 整数秒/日期规则。 |
| `codex-api/src/sse/responses.rs:685` 对 rate_limit_exceeded 的 message 做正则，解析 “try again in …” | **保留差异：** 网关不从自由文本错误 message 推断重试时间，只信结构化 rate-limit code/status + Retry-After。缺失/无效 header 默认 4 秒，最大一小时。CLI 自己仍收到原始 payload 并可实施自己的 retry。 |
| CLI 基础响应身份与单连接状态 | 网关 named lane、跨请求 model cooldown、Redis 共享冷却、busy 429 都是网关扩展，不宣称与 CLI 内部状态机逐字段一致。 |

## 多边界检查

新增 `gateway_routing_cooldown_audit_test.go` 10 个测试函数及表驱动矩阵。明确检查了以下边界：

1. Priority 始终优先于更低级账户的 MaxInt weight；高优先级 busy/cooling 时回退。
2. 两/三个 MaxInt weight 分别覆盖负/正溢出；0/负权重使用默认值；空候选安全。
3. 原有 80 goroutine 并发 admission 上限、重名/凭据轮换后的稳定 admission key、release 幂等回归通过。
4. busy 不创建 cooldown，也不继承其他账户的一小时 Retry-After；释放后立即可选。
5. 取消发生在读取前、OAuth 已占 admission 时、收到 429 后，分别检查错误来源、无泄漏及已收到限流的持久记录。
6. 秒值、HTTP date、0、过去日期、非法/巨大值沿用 `gateway_retry_after_test.go` 与 `gateway_cooldown_test.go`；新增内部 MaxDuration 和 Retry-After 向上取整极值。
7. 12 类 WS scope 情况：未知 lane、unknown lane 携带其他 active ID、ID mismatch、unknown ID、completed ID、正常 named ID、首个 named failure、无 lane 的已知 ID、首个 default failure、default mismatch、真正 connection error、默认 lane 活跃的 connection error。
8. numeric quota、numeric Retry-After、boolean quota、嵌套 header、null、status_code alias、status 优先级；仅 message 含 429 不制造冷却。
9. 模型冷却不影响其他模型；connection cooldown 影响全部模型；models catalog 保持独立；credential generation/token 变化释放旧冷却，rename 不释放。
10. 本地/Redis 容量上限不驱逐活跃 deadline；短的后续 429 不缩短先前长冷却；原有共享读故障 typed 503/fail closed。
11. 新增真实 Redis server TIME 与 ZSET deadline 比较；最大冷却后写入短冷却，确认 key PTTL 仍保留最大 deadline，短记录不改其他 member。
12. warm scheduling snapshot 之后，当前 Enabled/Groups/concurrency/credentials/owner 仍由现有权威读取决定；撤权、存储损坏、当前并发调整回归通过。
13. OAuth/inference 同一代理，配置 fingerprint/version ABA 改动拒绝、管理请求采用新配置、无可用绑定代理禁止直连回归通过。
14. 新增 direct→required、required→direct、required→other、仍允许原代理、仍为 direct 五组请求内绑定变化；仍允许原代理时不切换。

## 保留行为与限制

- `accountsSnapshot` 约 5 秒缓存用于排序及 Enabled/Groups 预过滤。新授权/重新启用可能最多延迟一个缓存窗口，ranking 的 priority/weight 也可能短暂旧值。命中后的当前行/当前 key 再检查防止撤权继续放行。没有为每次空候选强制全表读，避免无权限请求变成存储放大。
- 文件模式 admission、cooldown 和代理 round-robin 是进程内状态；Redis 只共享已有实现覆盖的 cooldown/session。没有把进程 admission 改造成跨节点并发总预算，CLI 也没有该对应能力。
- Redis deadline 使用 server TIME；本地 deadline 使用进程单调时钟，按相对 TTL 协同。不会把 Redis score 直接当本机绝对时间。写入后请求取消不能撤回已经到达 Redis 的命令。
- 默认 lane 尚无 response.created 时，新 ID 的首个失败无法仅凭 ID 判断是否是从未见过的旧响应；保留现有首个失败兼容性，completed-ID 集合仍负责已知旧响应排除。

## 验证与复跑

目标回归主包通过 128 个测试/子测试，耗时约 0.85 秒；日志 `.local/gateway-audit-20260924/a12-routing-cooldown.jsonl`。随后新增的 priority fallback 与 admission 中取消两项测试单独通过，耗时约 0.062 秒。此本机未配置 TEST_REDIS_URL，两个选中的 Redis 测试跳过，不能据此声称已验证 Redis；已交 root 最终 Linux / PostgreSQL / Redis / race 冻结套件运行。

```powershell
go test ./... -run 'TestGateway(RoutingAudit|Cooldown|RetryAfter|WSResponseIdentity|ExpiryRefresh|RequestPinned|ConfiguredUnavailable|SelectionAuthorization|AtomicAdmission|Admission|WebSocketRecoveryDoesNotSwitchProxyVersion)|TestCanceledClientStillPersistsRefreshRotation' -count=1
```
