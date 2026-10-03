# 10：OAuth、401 恢复与请求重放

日期：2026-09-24。源码基线：`.local/codex-source-0.155.0/codex-rs/login/src/auth/manager.rs`，下文源码行号均指该本地版本。本专项只修改 `gateway_auth.go`、`gateway_upstream.go` 的 `doAccountRequest`、`proxy.go` 的 `refreshAccountWithSettings`，新增 `gateway_auth_refresh_audit_test.go`。只读审查了冷却、恢复后授权、文件与 PostgreSQL 刷新入口及相关测试；没有生产操作。

## 已修复

1. **完整响应边界**：原来 `LimitReader(2 MiB)` 加一次 `Decode` 会接受有效 JSON 后的第二个对象、垃圾尾随，甚至 JSON 后附着的超限空白；读取完成前发生的网络错误也可能被有效 JSON 前缀掩盖。现在最多读取 2 MiB + 1 字节，验证长度，再对完整字节使用 `Unmarshal`。2 MiB 整响应可接受，多一个字节即拒绝。截断、尾随、读取错误不修改原凭据。位置：`proxy.go:501`。
2. **expires_in 不得丢失轮换凭据**：该字段在 CLI 的 `RefreshResponse` 中根本未定义。原来 Go `int64` 强类型会因字符串/过大数值等非标准附加值拒绝整次已成功轮换的响应，正整数加当前时间也会溢出。现在保留原始 JSON，仅接受加法不溢出的正 `int64`，否则回退 JWT `exp` 或 opaque token 的未知过期时间 0，保存 AT/RT。位置：`proxy.go:497`、`proxy.go:530`。
3. **成功状态对齐**：从仅接受 HTTP 200 改为 200–299，与 CLI `status.is_success()` 对齐。204 空响应仍不能作为有效凭据响应接受。位置：`proxy.go:490`；源码：`manager.rs:1620`。
4. **配置/错误脱敏与 nil client**：无效 URL 的解析错误不再原样返回，避免 endpoint userinfo/query 泄露；拒绝无 host、非 HTTP(S)、userinfo、fragment、opaque URL；nil client 返回固定连接错误。既有网络、OAuth body 错误脱敏保持。位置：`proxy.go:459`。
5. **RT-only 并发刷新合并**：Store 仅比较旧 AT，OAuth 只轮换 RT 时，16 个使用相同旧快照的 401 请求原来会逐个再交换新 RT。现在在 owner/enabled 检查后识别 RT 变化，复用已提交快照。实际并发回归验证只进行 1 次 OAuth 请求，并保留旧 AT、新 RT。位置：`gateway_auth.go:47`。
6. **异常 transport 响应**：首发和重试共用检查，保留上一专项的 `(nil, nil)` 错误保护；缺失 body 且声明正长度时失败，合法空 body 则统一为 `http.NoBody`，防止恢复路径 `Close` panic。位置：`gateway_upstream.go:38`。
7. **重放 body 生命周期**：`http.NoBody` 无 `GetBody` 的空请求允许恢复；缺失授权回调不重放；`GetBody` 返回 nil、与正长度矛盾的 `NoBody`、或 body+error 时保留原 401，释放失败克隆的 body。恢复后及克隆后再次检查取消，取消时释放原响应及克隆，不执行重试。位置：`gateway_upstream.go:56`、`gateway_upstream.go:73`。

## 一致性与有意差异

| 边界 | CLI 源码证据 | 网关结论 |
| --- | --- | --- |
| OAuth 请求方法与 JSON 字段 | `manager.rs:1599`、`:1710`：POST JSON，client_id/grant_type/refresh_token | 一致；不额外加 scope。支持导入账号原 client_id，默认常量与源码 `:1724` 相同；CLI 另有进程环境覆盖。 |
| 标准请求头及路由客户端 | `default_client.rs:324`、`:335`：auth 路由客户端，originator/User-Agent，可选 residency | 使用选定出口客户端与已配置 Codex 指纹；既有指纹测试通过。CLI 进程级 residency 状态不由本专项新增。 |
| HTTP 成功状态 | `manager.rs:1620` | 修复后均为 2xx；204 无 JSON 失败。 |
| AT/RT 可选 | `manager.rs:1717`、`:1572` | 支持 AT-only、RT-only；RT-only 保留旧 AT/expiry。网关拒绝 AT/RT 都缺失或空的响应；CLI 可接受 `{}` 并更新 last_refresh。 |
| expires_in | `manager.rs:1717` 未定义该字段 | 网关为了账号到期调度支持此扩展；无效/溢出值现在不阻止轮换持久化。 |
| id_token、last_refresh | `manager.rs:1572`、`:3047` | 仍有差异：CLI 更新 ID token claims 和 last_refresh；网关 Account 无原始 ID token/last_refresh 字段，当前刷新只更新 AT/RT/expiry，不将新 ID token 的身份直接应用到已有会话。 |
| JSON 完整性 | `manager.rs:1621` 的 response.json | 修复后拒绝尾随和截断；网关额外限制 2 MiB。Go encoding/json 对重复已知字段、大小写字段仍比 serde 宽松，留作最终一致性复核；本专项没有引入自定义 JSON 解析器。 |
| 主动刷新时机 | `manager.rs:191`、`:2959`：8 天 last_refresh 判断 | 网关按 token 到期时间及运行设置调度，有意保留多账号网关策略。 |
| 401 恢复次数 | `manager.rs:1856`、`:1983`：先 reload，再 refresh，逐个 401 推进 | 网关合并读取/刷新，最多重放一次。保持现有明确策略，未增加 reload 后的额外请求。 |
| 不重放已接受请求 | 401 恢复状态机入口为未接受 HTTP/握手错误 | 网关只在明确 HTTP 401 时进入恢复；403/429/5xx、网络错误、HTTP 200 SSE 内部 authentication error 均不触发重放。 |
| 账号所有权一致性 | `manager.rs:2458`：reload 必须匹配账号 | 网关在 store callback 及返回快照后均验证 owner/ref；重放前再检查实时账号、key、groups；unknown owner revision 另有更严格会话保护。 |
| 同账号并发 | `manager.rs:2803`、`:2842` 的 refresh_lock | 文件模式锁与 PG advisory/CAS 已存在；AT 变化复用已有快照，本次补齐 RT-only 变化合并。不同 PG 账号独立锁由现有测试覆盖。 |
| 永久/瞬时失败 | `manager.rs:1628`、`:2530`、`:2852`：识别 invalid_grant 等并按 auth snapshot 缓存永久失败 | 网关仍只保留固定错误类型/状态，失败冷却默认 4 秒、Retry-After 最大 60 秒，1024 条本地 LRU，可共享 Redis。未复制 CLI 永久失败状态机；避免持久缓存原始响应或 URL 秘密。 |
| 调用者取消 | CLI 是本地 auth storage 流程 | 网关 preflight 取消不开始刷新；已开始交换使用独立 45 秒预算，PG 已存在独立提交预算，使 RT 轮换可在调用者离开后落库。重放使用原始 caller context，授权/取消只阻止重放，不丢已提交凭据。 |
| 压缩、观测 context | OAuth JSON 与 Responses 业务请求不同 | 清除 OAuth 的 Responses 请求压缩标记，保留 attempt observation；重放保持原 body/GetBody/headers/context，既有 exact-wire 压缩回归通过。 |

## 冷却、授权与存储审查

- `gateway_refresh_cooldown.go:76`：key 是账号身份、AT、RT、client_id、endpoint、CredentialVersion 的 SHA-256，元数据改名不会绕过失败缓存；重新导入凭据会形成新 key。Redis 不可用时保留本地抑制，并继续依赖 store 权威锁/CAS。
- `gateway_refresh_cooldown.go:166`：交换在 store 锁内，失败在释放锁前发布；上下文取消、deadline、出口失效不作为常规 OAuth 错误进入冷却。显式 Retry-After=0/过去时间不被默认冷却覆盖。
- `gateway_recovery_authorization.go:20`：重新读取账号和授权，not-found/revoked 返回 403，授权存储不可用返回 503，owner 变化停止重放。现有 HTTP、WS、原生 transport 测试分别覆盖凭据持久化与请求拒绝。
- `accounts_refresh_context.go:24`、`accounts_pg.go:1000`：PG 准入等待可取消；回调开始后提交与 caller 取消分离。文件模式仍保持既有锁语义，不能宣称具备 PG 那样的独立按账号并发准入。
- RT-only 复用通过现有 Store callback 返回，因此现有 merge 仍可增加元数据/credential revision；没有扩大 Store API 或引入新 CAS 语义。
- 注入 `do` 若违背正常 `http.Client.Do` 契约，返回 response+error 且保留未关闭 body，仍由 transport 契约负责释放；标准 Go error+response 的 redirect 分支 body 已关闭。本专项没有为假想 transport 契约扩大双重 Close 行为。

## 验证

新增 8 个测试组位于 `gateway_auth_refresh_audit_test.go`：2xx/204、单 JSON、2 MiB 临界、截断/读取错误、22 组 JWT/opaque 过期扩展、URL/transport 错误脱敏、45 秒独立 context、7 类重放 body/授权/取消、nil transport 返回、16 并发 RT-only。

执行命令：

```powershell
go test . -run 'Test(AuthRefreshAudit|SourceRefresh|Source401|SourceGateway401|SourceGatewayDoesNotReplay|GatewayRefresh|GatewayRecoveryAuthorization|GatewayRequestCompression|RefreshAccount|ConcurrentRequestsRefreshOnce|CanceledClientStillPersistsRefreshRotation|GatewayUnknownOwner.*Refresh|GatewayUnknownOwner401|GatewayUnknownOwnerWebSocket401|GatewayAttempt.*Refresh|GatewayCooldownAfter401|GatewayRequestRoute|GatewayExpiryRefresh|GatewayWebSocket.*401)' -count=1
```

结果：`ok codex-agent 2.857s`。较早一轮定向测试同样通过，`ok codex-agent 2.490s`。`gofmt -d proxy.go` 无输出。

该命令验证本机单元与本地 HTTP/WS 回归，不把带环境门控的原生二进制/PG/race 测试视为已完成。Linux race、PG/Redis、整体原生 transport 集成由主任务统一执行；本专项没有访问真实 OAuth 服务或使用生产凭据。
