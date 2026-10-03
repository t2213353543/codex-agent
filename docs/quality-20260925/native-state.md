# 原生调度中的账号状态竞态修复

日期：2026-09-25。范围：native HTTP/WS 的账号作用域、冷启动后的权威授权状态、取消/复用边界；未调用真实模型、未 SSH、未修改生产服务器。

## 新发现与失败前证据

当前实现已经按账号 owner/revision 隔离 helper，也已经在账号选择时读取当前状态。但账号被选定后，HTTP 正文读取、native helper 冷启动、IPC 连接都可能等待；这些步骤完成后，原来的代码只重查代理快照，没有再确认该账号和调用方授权是否仍有效。

新增 `gateway_research_native_state_test.go` 使用完整 HTTP 网关和现有 self-exec native IPC fixture，建立确定性顺序：

1. 原账号已经通过业务选择和 admission，session 绑定已建立。
2. 在 native helper 命令创建处暂停；此时 helper 尚未启动，不可能已收到 IPC 请求。
3. 主线程在权威 Store 中完成账号状态变更。
4. 放行启动，检查实际 helper 是否收到并回显本次正文。

修复前执行 `TestGatewayResearchNativeColdStartupOwnerRevocation`，以下四项均失败：同一管理 ID/同 workspace 换 user、owner A→B→A、删除账号、停用账号。每项都得到 fixture 的 HTTP 202，且回显原请求 sentinel，直接证明状态变更完成后仍发送了旧 owner 的正文。原始日志：`.local/research-20260925/native-state-red.log`。相同 owner 的 AT/RT 刷新和未变更两个对照通过。

这与旧 cookie 测试不同：旧测试改变 owner 后再发起新请求；本次改变发生在已经选定账号、尚未发送的同一个请求内部。失败不是 cookie 推测、哈希推测，也没有以糖果题答案代替调度证据。

## 最终实现

业务 HTTP `/responses`、compact、models 在 route context 中挂一个私有的授权工厂，保存调用方 token/key ID，真正发起每次原请求或 401 恢复请求时绑定该次 Account 快照。native HTTP 在完成正文缓冲、helper 获取、IPC dial 后，写 IPC head 之前执行一次权威授权检查。native WS 在首握手 IPC head 写入之前执行相同检查。

检查复用既有 `authorizeGatewayRetry` 的账户/调用方判定，但不触发 OAuth、不重试、不切账号、不替换 bearer：

- owner reference/revision 改变时拒绝，包括同 workspace 换 user 和 A→B→A。
- 账号删除、停用、key 撤销、key group 或账号 group 不再重叠时拒绝。
- 同 owner 的合法 AT/RT 刷新允许继续，发送本请求原先准备的凭据，保持 helper scope 连续；后续 401 仍按原有受控恢复路径处理。
- 权威存储读取失败时按既有错误分类拒绝，不采用调度缓存绕过。

派生 context 是这个边界的一部分。最初补丁复用旧 callback 时会捕获 HTTP 请求的父 context，Q13 只读复核指出 `http.Client.Timeout` 的更早 deadline 可能无法取消授权读库。最终实现将真正的 native dispatch context 传给授权工厂，WS 同样使用握手 context。专门测试先温热相同 origin/scope/timeout 的 helper，再将 client timeout 设为 300ms，确保授权阻塞在派生 deadline 到期时退出，而父 context 仍可用；温热避免 Linux race helper 启动速度引入假失败。

权威检查可能涉及读库，因此最终的代理指纹检查放在它之后。WS 也补上 IPC dial 完成后的代理复核。HTTP 与 WS 两个测试都在授权回调中改变代理密码，确认返回 `errGatewayProxyUnavailable`，而不是写入过时代理对应的 IPC。

首次 native 调度拒绝使用 `before native dispatch` 错误文字，不误写为 `after authentication recovery`。只有这个明确发生在 IPC head 写入之前的拒绝才附加可 unwrap 的 unsent 标记。观测层据此移除尚未发送的候选 attempt，同时保留业务错误记录和原 owner/authorization HTTP 状态；既有网络错误计数不作广泛重解释。被移除的 handle 保留内部索引，其他并发 observation handle 不会移位或覆盖。

## 验证与限制

新增测试包含：

| 测试 | 关键边界 |
| --- | --- |
| HTTP 冷启动 9 场景 | 四个原始 red 场景，key 停用/key groups/账号 groups，same-owner refresh，未变更；拒绝项不得回显正文、attempt 数为 0。 |
| WS 冷启动 8 场景 | 同 workspace 换 user、删除、停用、key 停用/key groups/账号 groups，以及两个允许对照。合成 helper 收到实际握手 head 才写私有 marker；拒绝项必须没有 marker。 |
| WS 凭据保持 | same-owner refresh 后允许的握手实际 Authorization 哈希仍等于本请求原快照，未被偷偷换成新 bearer。 |
| 取消与并发复用 | 一个 response 保持打开，另一个同 owner 请求取消，再发独立请求；它们仍使用同一 PID，取消不破坏兄弟连接，活动计数最终归零。 |
| unsent observation | 移除中间未发请求，不改变前后 request/refresh handle 的状态和编号；收到过响应头的请求不能被抹掉。 |
| 派生 deadline | helper 已温热，授权使用 HTTP client 的派生 deadline 并及时结束，父 context 继续存活。 |
| 代理变更 | 授权回调期间改配代理，HTTP/WS 都在最终写 IPC 之前拒绝。 |

新增专项最终生产源码版本全通过，最后的 deadline 预热修正单项通过。随后在 Windows Go 1.26.7 对 native/auth/owner/refresh/observation 相关现有测试执行针对性回归：**165 个顶层测试通过、32 个明确跳过、0 失败，11.546 秒**；计入子项为 480 pass、53 skip。日志 `.local/research-20260925/native-state-green-final.jsonl`。跳过项主要要求 Linux、真实 Rust helper、官方 CLI 或数据库环境，不将它们算作通过。该回归使用最终生产补丁；deadline 测试随后仅增加 helper 预热以消除慢启动干扰，单独重跑通过。根任务负责冻结最终测试文件后执行 Linux race/PG/Redis/真实 helper 完整回归及部署验证，本报告不提前代替这些结果。

复跑命令：

```powershell
go test . -run '^TestGatewayResearchNative' -count=1 -timeout=90s -v
```

这次修复增加每次 native 业务发送前的一次当前账号和 key 权威校验，存在额外存储读取成本；没有增加模型请求或重放。检查不与远端发送形成跨系统事务，不能保证检查完成之后发生的任何状态修改都阻止已经开始的网络发送。对已经接受的响应，不强杀其他活动连接或改写生成内容。

本次 guard 限于带业务 admission 路由的 native HTTP/WS 请求。Go fallback、无业务 admission 的管理/OAuth 请求没有扩展新 guard；它们原有的刷新/管理授权和 CAS 逻辑保持原范围，不能用本次测试宣称这些路径也获得了相同的最终发送检查。

本次证据证明一个会在特定状态变化时触发的本地调度缺陷及其修复；不能由此证明此前单个糖果题错误答案的原因、提供方隐藏集群、账号算力标记或模型质量已经恢复。

## 修改文件

生产文件：`gateway_native_dispatch_guard.go`（新增）、`gateway.go`、`gateway_upstream.go`、`gateway_native_http.go`、`gateway_native_ws.go`、`gateway_websocket.go`、`gateway_recovery_authorization.go`、`gateway_attempt_observation.go`、`gateway_observation_transport.go`。

测试文件：`gateway_research_native_state_test.go`。仅该测试文件新增合成 helper 进程入口与 IPC marker，不将观测钩子放入生产 helper。

主任务最终已完成 Linux race/PG/Redis/native 全套 854 项通过、官方 CLI 独立 7 项通过，并部署 `2ed703ba…173c6`；新进程的 HTTP models 200、WS 101，关闭后 in-flight=0。构建输入与最终冻结快照一致，具体日志和范围见[集成上线记录](../CODEX-GATEWAY-RESEARCH-2026-09-25.md)。
