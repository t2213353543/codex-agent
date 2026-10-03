# 专项 11：会话归属、授权与缓存

日期：2026-09-24。源码基线：本地只读 `.local/codex-source-0.155.0/codex-rs`。本专项不修改生产环境、不修改账户存储实现、调度主文件或 WebSocket 实现。

## 结果

修复 `gateway_sessions.go` 的五个明确边界，新增 `gateway_session_ownership_audit_test.go` 九个测试函数（包含多组表驱动用例）。没有改变用户/工作区归属规则、未知身份保守处理规则、TTL 默认值或授权模型。

1. **路由键与实际上游头不一致。** 原先直接 `Header.Get`，会忽略非规范 Go Header map 大小写，也会使用被 `Connection` 点名剥除的 session header。现在复用 `copyGatewayHeaders` 的大小写、canonical scalar 优先级、首值与 hop-by-hop 过滤规则，再按 `session-id → session_id → thread-id → conversation_id → prompt_cache_key` 取值。修复前测试实际出现 6 个失败子例，包括原始小写/大写、legacy underscore、Connection-nominated 和非规范冲突。
2. **取消后仍写会话。** 文件分支原先完全忽略 context，会创建/续期已经取消请求的绑定。现在在入口、settings 读取后和本地锁取得后检查 context；取消的 Redis 请求也不再进入命令派发。修复前 read/claim × 已存在/不存在 4 个子例失败。
3. **Redis 取消被错误归类。** 入口 context 在 Redis 查询/claim 期间取消，原先统一返回 `session routing unavailable`；现在保留调用方的 `context.Canceled` / `context.DeadlineExceeded`。网关自身的 3 秒存储超时仍按 routing unavailable 处理。通过 Redis ProcessHook 确定性取消真实调用链，修复前 read/claim 两例失败。已经提交到 Redis 的命令不能因为随后发生取消而回滚；修复保证取消前不发命令，取消结果不被掩盖，不声称跨网络事务撤销。
4. **首次绑定的 key identity 未复查。** `pickAccount` 返回当前 key ID，但首次 session claim 原先丢弃它，可能用入口旧 key ID 建绑定。现在核对当前 ID，不同则释放 admission 并拒绝，不建立旧命名空间绑定。修复前不同 ID 复现实例成功建立会话，修复后失败关闭。
5. **TTL 在校验前换算溢出。** 持久化 settings 不一定经过当前管理 API 的校验。`18446744134` 秒先换成 `time.Duration` 会溢出成 `60.290448384` 秒并被旧范围检查接受。现在先以秒验证 `60..2592000` 再换算。整合专项 14 后，SettingsStore 读取时首先校验完整持久化配置，异常 TTL 直接拒绝请求且不创建绑定；sessionAccount 的换算前检查保留为纵深防御。磁盘 settings fixture 已同步覆盖有效上下限、负数、边界外和溢出值的失败关闭行为。

## CLI 对照

| CLI 0.155.0 源码证据 | 网关状态 / 差异 |
| --- | --- |
| `codex-api/src/requests/headers.rs:5` 的 `build_session_headers` 分别写 `session-id`、`thread-id` | session 优先于 thread，保留根会话及子 agent 的账户归属。下划线 header 为网关兼容扩展。 |
| `core/src/agent/control.rs:123` 说明 session ID 等于 root thread ID | 根与子 agent 共享同一绑定；不会按子 thread 单独切换账户。 |
| `core/src/client.rs:497` 的 `prompt_cache_key` 优先 override，内部任务可用 `source:parent_thread`，通常返回 session ID | session header 优先于缓存 key；原生缓存 key 保持不透明，不能把内部任务缓存 key 当成另一账户的会话。已有 `TestGatewayNativeCacheKeyAndControlsRemainOpaqueAcrossSubagents` 回归。 |
| `core/src/responses_metadata.rs:306` 附近的 `client_metadata` 同时携带 session/thread 等信息 | 网关本专项未从任意 metadata 发明新优先级；原生 HTTP/WS 握手 session header 与 body prompt cache fallback 沿用现有协议。 |
| `login/src/auth/change_state.rs:15` 的 `same_owner` 要求 auth mode、非空 user、非空 workspace 相同 | 网关 `gatewayCompleteOwner` 要求 AccountID + UserID；user/workspace 变化不复用旧会话。网关处理 ChatGPT 账户池，没有 CLI 全部 auth mode 状态机，不能宣称该层面完全相同。 |
| `login/src/auth/manager.rs:2606` 计算 credential/owner generation；`change_state_tests.rs` 覆盖轮换与 coalesced owner changes | 网关持久化随机 `OwnerRevision`，完整身份同 owner 凭据轮换保持版本；unknown owner 的 credential 变化推进归属。持久版本及旧绑定 fail closed 是为多进程网关增加的机制。 |
| `core/src/client.rs:516`、`:1403` 依据 owner generation 重置 WS state | 网关通过绑定 owner ref、当前账户读取和 WS 每轮授权拒绝变更 owner；网关拒绝旧 thread 与 CLI 本地状态重置是不同职责。 |

## 已检查的边界

| 边界 | 验证 / 结果 |
| --- | --- |
| 原生 root session、子 thread、legacy headers、body fallback | 新 audit header 矩阵、既有 `TestGatewaySessionIdentity`。 |
| 非规范大小写、canonical 冲突、多值 scalar、空首值 | 新矩阵核对转发前后 `gatewaySessionKey` 一致。 |
| `Connection` 指定 session/thread 字段 | 新矩阵：被剥除的字段不再参与 sticky 身份。 |
| key/session 隔离、并发首次 claim | 既有 `TestGatewaySessionAtomicClaimAndKeyIsolation`，40 个并发提议只会得到一个 winner。 |
| 首次选账户后 key identity 改变 | 新 audit：禁止写入过期 key ID 命名空间，admission 归零。 |
| 文件已有/缺失绑定的 read/claim 在取消后行为 | 新 audit 4 子例；不建、不续，保留 cancel。 |
| Redis in-flight cancellation 与预取消 | 新 Redis hook 测试；不静默回退本地、不 mask 调用方取消。 |
| TTL 有效上下限、异常持久值、溢出、续期 | 新磁盘 fixture + 既有 `TestStickySessionSettingsAndExpiry`。 |
| 本地缓存容量上限、活跃记录保护、过期清理 | 新 audit：满容量拒绝新 claim；只清理过期记录，不逐出活跃加密历史。 |
| 运行中的租约续期、冲突、缺失 Redis、关闭 | 既有 `gateway_session_lease_test.go`；新增 parent canceled + 8 次并发 stop，终止后无残留绑定。 |
| complete user/workspace、已知身份 token rotation | `TestGatewayOwnerIdentityRefKnownUserAndStableCredentials`、`TestGatewayOwnerIdentitySameUserRotationKeepsSessionAndWSAuthorization`。 |
| unknown owner token/refresh token/client ID 变化、metadata 无关变化 | `gateway_unknown_owner_test.go`，unknown rotation 旧会话拒绝，刷新凭据仍保存。 |
| user/workspace 变更、删除、ABA 回到旧身份 | `gateway_owner_identity_test.go`、`gateway_owner_revision_test.go`、`gateway_owner_cache_test.go`。 |
| 当前账户读取、ID 与另一个 account 名称冲突 | `TestGatewayOwnerRevisionReloadUsesExactID` 通过，精确 immutable ID 优先。 |
| key disable/delete/rotate/groups、跨实例管理 index 陈旧 | `gateway_key_authorization_test.go` 与 session/selection authorization 测试通过；授权绕过旧管理 index。 |
| key 重新启用、负授权结果 | 新 audit：独立 writer disable 后读取拒绝，再 enable 无须刷新 reader index 即允许。 |
| OAuth 刷新前后授权和失败释放 | `gateway_session_authorization_refresh_test.go`、`gateway_selection_authorization_refresh_test.go`、`gateway_recovery_authorization_boundary_test.go` 通过。 |
| 持久连接下一轮授权撤销 | 既有 WebSocket authorization/owner 测试在本专项回归中通过，未修改 WS 实现。 |
| 出口代理顺序/进程稳定、key 隔离、禁用局部迁移、关闭 stickiness | `TestSessionProxyStableAcrossOrderAndProcesses`；complete owner 的 token rotation 不改变 rendezvous 输入，unknown owner 换凭据会改变。 |

## 已知差异与保留行为

- **Redis / 文件模式不等价：** Redis 通过 GETEX/Lua 原子 claim 跨进程并有原生 TTL；文件兼容模式的 sessionRoutes 是进程内内存，重启丢失，不与另一个 Proxy 实例共享，并且最多 16384 项。两者均不在存储失败时静默换账户。不能把文件模式描述为生产多实例共享 sticky。
- **账户调度负缓存：** `candidatesForContext` 用最多约 5 秒旧的 enabled/groups 预过滤；刚重新启用或新授权的账户可能短暂不可选。正授权命中后仍重读当前账户和当前 key，因此撤销不会因为该缓存放行。这是可用性延迟；已交 root / 专项 12，按协调要求未在本专项扩大调度逻辑。
- **设置缓存：** 文件 SettingsStore 依赖当前实例 Set 更新，PG settings 约 2 秒刷新；本专项未改设置一致性协议。
- **proxy store：** pickSessionProxy 只考虑当前 ProxyStore index 的 enabled 条目，并以 key/session/owner 做 rendezvous；跨进程配置 index 刷新属于 proxy store 生命周期，不在此修改。
- **已经建立的 HTTP 工作：** lease 验证 session binding 并在冲突/存储失败时取消；不会撤销已经发给上游的请求，下一次 selection/WS turn 才走各自新的授权边界。owner 变化绝不把当前加密历史改绑到另一账户。
- **错误状态差异：** 旧缓存中匹配到 owner、随后权威读取发现变化可产生 owner conflict；刷新候选列表后找不到旧 ref 可能是 unavailable。两者均禁止迁移，状态码统一属于整体网关策略，未擅自改动已有用例定义。

## 验证

修复前跑新增 audit suite，获得实际失败输出（header、file cancel、Redis cancel、initial key identity）。修复后下列本机命令通过，最后一轮主包耗时约 1.93 秒：

```powershell
go test ./... -run 'Test(Gateway(Session|Owner|Unknown|KeyAuthorization|SelectionAuthorization|RecoveryAuthorization|Redis)|StickySession|SessionProxy|AuditSession|GatewayNativeCacheKeyAndControlsRemainOpaqueAcrossSubagents)' -count=1
```

本专项未配置 `TEST_REDIS_URL` / PG 服务，依赖外部 Redis 的既有测试按 fixture 跳过；新增 Redis ProcessHook 测试已实际执行，不能替代 root 的 Redis/PG 集成和 race 总回归。没有声称生产验证或 CLI 字节级全面一致。
