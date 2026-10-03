# OAuth 撤销与 HTTP/WS 恢复：独立源码审计及修复候选

2026-09-25。确认并用合成 loopback 复现一项生产差异：**网关把 OAuth grant 已明确永久拒绝的同一凭据，仅缓存为短暂冷却；冷却到期后后续请求会再次使用同一 RT 交换。** 固定官方 CLI 0.155.0 在当前 AuthManager 内存中记住该凭据的永久 grant 失败，凭据未变时快速返回。当前四文件候选修复了这一差异，尚不在本文提前宣称部署完成。

审阅/测试没有真实模型调用、登录或凭据刷新。生产相关八个源文件均与 `a4777428…20136a` 对应发布 archive 的逐文件摘要匹配，不是仅相信共享工作区路径；archive SHA 为 `818f756f…0312`。[修复前源证明](../../.local/research-20260925/auth-revocation-review/source-proof-before.json)

## 先区分 AT 撤销与 RT grant 永久拒绝

v9 只读 usage GET 在 g7 的 v8 新 AT 摘要 `79dd4bf6…283ab` 下得到 HTTP401、固定 `token_revoked`，消息为 `Encountered invalidated oauth token for user, failing request`；前后 identity、token、Version=2 保持。该次 GET 没有模型、refresh 或账号修改。它证明本次 AT 请求被服务端拒绝，**不能单凭该响应证明 RT 也永久失效、刷新导致撤销或模型能力变化**。[真实脱敏诊断](../../.local/research-20260925/cache-key-controls-v9/auth-diagnostic.json)

原 new2/g7 v9 缓存键组合已在整体资格门槛前停止：模型0、refresh0，raw删除，capture进程退出。认证失败没有被记作原题错答。[归档回执](../../.local/research-20260925/cache-key-controls-v9/experiment.json)

## HTTP 与 WebSocket 现有恢复边界

| 路径 | 当前生产逻辑 | 固定官方 CLI 对照 |
|---|---|---|
| HTTP Responses 首次401 | `doAccountRequest` 只有显式401、可重放body和非空authorizeRetry才调用恢复；相同账号/代理，重新授权后最多一次retry | provider默认把HTTP401交给UnauthorizedRecovery；managed模式最多reload再refresh，随后仍401就报错 |
| HTTP accepted SSE内error、网络错误 | 不触发认证重放 | 不把已接受的流任意当作新turn重放 |
| WS upgrade401 | 握手还未接受时恢复一次，再握手一次；已accepted socket上的frames不因断线或auth错误重放 | CLI websocket建立/请求中的recoverable transport401走相同有限恢复状态机 |
| reload观察到不同owner或账号disabled | gatewayAccountRef/授权检查阻断旧history，成功旋转凭据先持久化 | CLI reload要求当前workspace符合预期，owner变化独立处理 |
| refresh失败 | HTTP保留原401状态与诊断body；WS保留原upgrade失败；不会将其伪装为模型completed | CLI区分永久/暂时refresh错误，并向上报失败 |

源位置：[gateway_upstream.go](../../gateway_upstream.go:39)、[gateway_auth.go](../../gateway_auth.go:12)、[gateway_websocket.go](../../gateway_websocket.go:229)。未发现单个HTTP或WS请求内部无限refresh循环；网关一次恢复把reload/current检查与必要grant合并，CLI managed可能先用未变化reload状态再尝试一次，因此不声称两者逐次网络序列完全一致。RT-only响应可能保留原AT且只retry一次，属于可选token响应边界，不是无限循环。

## 官方永久失败的精确范围

固定源码 [login/src/auth/manager.rs](../../.local/codex-source-0.155.0/codex-rs/login/src/auth/manager.rs:1598) 的 `try_refresh_token()` 只对 **token endpoint grant响应**执行以下分类：

- grant HTTP401：永久，即使响应code未知或正文不可解析。
- HTTP400 且结构化code为 `invalid_grant`：永久。
- 任一非成功status且code为 `refresh_token_expired`、`refresh_token_reused`、`refresh_token_invalidated`：永久。
- 其他HTTP错误或传输错误：暂时失败；`token_revoked`文案本身不被借用为RT状态。

code读取优先级为 error字符串、error.code字符串、顶层code字符串，ASCII大小写不敏感；已存在的空字符串不回退到顶层code。正文消息中碰巧出现known文字不算code。源位置：[1630–1698附近](../../.local/codex-source-0.155.0/codex-rs/login/src/auth/manager.rs:1630)。

永久状态不是磁盘封禁：`CachedAuth.permanent_refresh_failure`是AuthManager内存单slot；`refresh_failure_for_auth()`只匹配当前auth快照；`record_permanent_refresh_failure_if_unchanged()`仅当前仍等于attempted时发布；`set_cached_auth()`在auth/credential变化时清除；grant开始前查该slot直接报错。进程重启可以再次尝试。相关位置：[1803](../../.local/codex-source-0.155.0/codex-rs/login/src/auth/manager.rs:1803)、[2357](../../.local/codex-source-0.155.0/codex-rs/login/src/auth/manager.rs:2357)、[2493](../../.local/codex-source-0.155.0/codex-rs/login/src/auth/manager.rs:2493)、[2530](../../.local/codex-source-0.155.0/codex-rs/login/src/auth/manager.rs:2530)、[2603](../../.local/codex-source-0.155.0/codex-rs/login/src/auth/manager.rs:2603)、[2853](../../.local/codex-source-0.155.0/codex-rs/login/src/auth/manager.rs:2853)。

## 复现与最小修复

修复前 `proxy.go` 非2xx只保留status；`gateway_refresh_cooldown.go`无永久分类，所有失败采用默认4秒、Retry-After最多1分钟的暂时缓存。新测试真实调用loopback OAuth server：400 invalid_grant、401 refresh_token_invalidated、401未知grant错误分别触发一次拒绝，再将暂时缓存时钟推进到期，均观察 **同RT被交换2次，期望1次**。三项红原始记录保留。[permanent-red.log](../../.local/research-20260925/auth-revocation-review/permanent-red.log)

候选仅涉及：

| 文件 | 改变 |
|---|---|
| [proxy.go](../../proxy.go:503) | grant401由status直接分类；其他错误body最多64KiB并检查完整JSON code，原文不留日志/缓存，超限/不可读不能靠正文认永久 |
| [gateway_refresh_cooldown.go](../../gateway_refresh_cooldown.go:74) | 每账号一个进程内拒绝slot，存当前AT/RT/client_id/token-endpoint组合SHA和固定status；无时间自动解除，不把它写入Redis或账户持久schema；新组合解除旧slot |
| [gateway.go](../../gateway.go:103) | 真实Store.List成功后清理已经不存在的账号slot；取列表前记录代次，期间新发布/覆盖的slot不被旧列表删除；不按列表旧credential清除仍存在owner |
| [gateway_refresh_permanent_test.go](../../gateway_refresh_permanent_test.go) | 原缺陷红绿、严格分类、元数据与凭据变化、并发/stale401、取消、删除清理、进程重建scope |

元数据Version、改名/notes或本地owner revision变动不会使相同拒绝凭据重新grant。AT、RT、client_id或endpoint真实改变则允许新attempt；这是有明确输入变化的恢复，不是到期解禁。生产恢复路径仍由Store序列化/CAS读取当前snapshot，旧请求发现已换AT会先返回当前凭据，不调用旧snapshot去清除新失败slot。

清理的代次门槛避免并发列表遗漏新账号导致错误解除；只在fresh成功List运行，缓存命中/读取失败都不剪除。没有永久slot时直接返回，避免平时每次列表刷新都做账号hash工作。内存规模随仍存在的失败账号，每账号至多一项，删除账号在下次成功fresh列表观察后清掉。

该修复按CLI的进程scope工作：新Proxy/服务重启、其他实例可以各自尝试，未发明共享持久禁用。既有Redis只继续服务暂时backoff。错误提示优先永久分类，即使known永久code出现在429/5xx，也提示重新导入而不误写retry later。客户端在grant开始后离开不会擦除已经观察到的永久失败；preflight取消仍不启动grant。

## 验证与已知范围

Windows合成loopback及现有认证回归 **46个顶层测试、含子项210项全部通过，0失败、0跳过**；覆盖HTTP401最多一次retry、WS握手恢复、成功旋转持久化、同owner连续性、身份变更阻断、context取消和错误脱敏。`go vet ./...`通过。最后prune无slot优化后专项删除/水位测试再次通过。[最终JSONL](../../.local/research-20260925/auth-revocation-review/auth-targeted-final.jsonl)、[prune最终记录](../../.local/research-20260925/auth-revocation-review/prune-final.log)、[候选manifest](../../.local/research-20260925/auth-revocation-review/candidate-manifest.json)

正文错误解析有64KiB/既有grant45秒界限；这比CLI读取完整错误text更有界，大于该上限的非401响应不会仅凭不完整正文认定永久。暂时失败仍按原短backoff恢复。不会因本次usage `token_revoked`预先写永久slot，更没有实际测试g7当前RT是否可用。

独立reviewer已经用自己的3项overlay验证：旧AT调用不清除current拒绝slot；invalid_grant后真实import同owner新AT/RT可以成功刷新；Store读取失败不prune。其3项与本专项合计33测试项全部通过、0失败/跳过，四文件冻结hash相同，未发现阻断问题。[独立overlay证据目录](../../.local/research-20260925/auth-revocation-review/cache-agent-overlay/)。完整Linux/race及生产发布验收由主任务在a477源快照只合四文件完成。本文未将专项通过扩写成已上线或模型质量修复。

复跑专项：

```powershell
go test . -run '^TestGatewayRefresh(Permanent|Cooldown)' -count=1 -v
```
