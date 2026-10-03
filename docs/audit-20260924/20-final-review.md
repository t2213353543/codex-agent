# 20 — 独立最终代码复审

日期：2026-09-24。以 `.local/gateway-audit-20260924/baseline-source.zip` 重新生成生产差异，不依赖可能陈旧的粗 diff；同时阅读专项 01–17、`INTEGRATION.md` 和只读 Codex `rust-v0.155.0` 源码。本专项未访问生产环境。

完成时核对了 36 个既有 Go/Rust/Cargo 生产文件的变化及新增 `gateway_sse_idle.go`。发现并修复一个管理恢复路径回归，另对 root 在完整 Linux race 中定位的 SSE splitter 性能修复进行了独立算法复核。没有为追求表面一致而更改默认 native 生命周期、放宽 TLS 校验、增加自动重放或取消资源限额。

## 独立发现并修复

### 坏设置不能通过管理 API 修复；旧文件缓存还会覆盖未修改字段

新增持久化读取校验正确阻止坏策略进入 runtime，但 `handleSettingsPost` 在应用修正字段之前调用 `managementSettings → Latest`。持久 timeout/mode 已损坏时，提交合法修正仍提前返回 400；旧“可恢复”测试直接调用 `SettingsStore.Set`，没有经过该 handler，未覆盖此问题。

文件模式还有同一根因的另一表现：`Latest` 返回旧缓存。持久文档版本和其他字段已变化时，POST 即便明确带当前版本，也会把旧缓存中的 AdminAPI/提示词/其他策略重新写回；原本没有修正的坏字段可被旧值无声覆盖。

修复仅涉及 `admin.go` 和 `store.go`：

- POST 已经过独立的管理认证，再调用私有 `SettingsStore.readForUpdate`，始终读取当前持久文档。
- 该读取只提供合并素材，不调用 runtime 发布逻辑，不修改 `cached/loaded/loadedAt`。
- 缺失旧字段继续正常迁移；显式 `admin_api:false` 和未提交的当前字段保持。负持久版本直接拒绝，最大版本仍由 Set 拒绝递增。
- 应用用户提交的字段后，对完整结果执行原有 `validateSettings`；未修正的其他坏字段仍返回 400。`Set` 保留同一版本 CAS，过期请求返回 409。
- runtime `Get/Latest`、readiness 和启动校验仍拒绝坏策略。若启动前文档已损坏，仍需修正持久文档再启动；本修复针对运行中的管理恢复入口。

新增 `gateway_final_review_test.go`。修改前实际执行失败：冷缓存路径在应进入 CAS 的合法合并前返回 400；热缓存路径在仍有另一坏字段时错误返回 200。修复后，实际认证路由验证以下边界：

1. 文件冷/热缓存和 PG 冷/热缓存各自覆盖同一场景；PG 测试由 root 隔离 Linux gate 执行，本机无 PG 时明确 skip。
2. 未认证修复返回 401，不能借恢复入口绕过认证。
3. 只修 timeout、遗留坏 prompt mode 仍返回 400；持久内容和缓存保持不变。
4. 完整合法修复但版本过期返回 409；不覆盖其他人的文档。
5. 完整合法修复返回 200，保留当前 `AdminAPI=false`、当前提示词，版本恰好加一，后续 GET 和 runtime 读取恢复。
6. 负版本和 `MaxInt64` 不能通过修复请求被重置或溢出。

## 交叉边界复核

| 边界 | 独立复核结果 |
| --- | --- |
| WS scoped error、FIFO 和历史 ID | named lane 只完成 FIFO 头，未知 lane 不计账；历史完成 ID 的迟到 error 不关闭后续 turn；无 scope error 仍先取消/关双方再 abort。未通过 named error 引入自动重连或重放。 |
| WS idle 与准入释放 | watcher 与 observe/start/abort 共用 mutex；只计算各 lane 的头，晋升重置计时；空连接无响应期限；取消触发 owner 关闭两端，随后幂等 abort/release。Go 1.26 timer 停止/重置语义符合实现，不需要旧版本的 channel drain 模式。 |
| SSE 计量与完整事件 idle | scanner 和 O(1) tracker 都支持首 BOM、CR/LF/CRLF、空 data、多行 data；注释/id-only 不续时；invalid UTF-8 不成为合法进度。observer 本地容量失败不设置 protocol terminal，故不截断透传。 |
| 大 SSE line 与 Scanner | root 新增 `sseLineSplitter.scanned`，无换行前缀只扫描一次；尾 CR 回退到该 CR 等 lookahead；advance 后清零；EOF 返回完整 token。Scanner 在 advance=0 时保留同一内容前缀，缓冲搬移不会使 offset 失效。该修复回应本轮 Linux race 下长行反复重扫的实际 CPU 问题；专项 19 补性能/分片回归。 |
| HTTP 429 和冷却归属 | 捕获已到达的 429 后，使用有界脱离 caller cancel 的写入；未知 named lane/冲突 response ID 不升级成连接全局冷却；busy 可用账户不被更长 cooldown 掩盖。没有把 429 加入重放触发。 |
| OAuth/401 重放和 token rotation | 仅显式 401 且可克隆 body、可重新授权才恢复；403/429/网络失败/SSE内部错误不重放；RT-only 变化合并并发旧快照；body clone 失败释放新 body，取消不丢已交换持久凭据。 |
| native HTTP IPC/timeout/size | 请求体 close-once、短写检查、剩余 deadline 向上取整、完整 header pair 校验、Content-Length 和无 body status 处理互相一致；frame读取保留 timeout原因；收到 typed timeout 不误映射为普通协议成功。 |
| native WS IPC/ACK/限额 | UTF-8/UTF-16 metadata、accepted/phase、序号、单 outstanding ACK、读预算和动态降限均有真实断点回归；partial IPC command 与 close 等待有期限；只在背压时分配队列 timer。确认 frame16MiB/message32MiB 是不同限额。 |
| HTTP 与 WS TLS/CA | HTTP default backend 与 WS ExplicitCodexTls 分开；custom CA 优先级复用官方；cli_request 每次新 connector，pooled 明确额外复用。报告中的相同 hash/feature tree不能单独代表握手成功；真实 HTTP/WSS证据由专项17和root分别关联最终快照。 |
| proxy 半关闭与取消 | CONNECT EOF 向另一侧 CloseWrite，错误/bridge关闭则关双方；hijacked socket受bridge context约束；SOCKS握手保留错误链且拒绝畸形address，取消已归属请求不会留下独立dial；保留远端DNS，无直连fallback。 |
| settings 当前读取和恢复 | 此专项发现并修复上述真实handler缺口；current raw不发布到runtime；全对象验证、管理员独立认证、版本CAS及未指定字段保留。 |
| response output 合并与 call_id | 调用仅在相同工具类型的call_id内去重，call output不与调用合并；已知不同item ID不互相覆盖；terminal只含全部已知ID子集时补回done项，未知/匿名terminal保持原有权威；跨响应ID拒绝拼接。 |
| usage 明确零及溢出 | present原生零优先于兼容字段；非法计数与input+output溢出不累计；usage:null未知。HTTP/WS冲突terminal状态遵循失败优先，成功观测不撤销下游aborted。 |
| models读取限额和下游写 | 8MiB+1读完再提交成功头，读错/超限返回502；304与空body仍合法；Write/Flush/short-write记录客户端失败，上游attempt200保持独立。 |
| header canonical/scalar/未来兼容 | 大小写alias确定性合并，已知scalar首值、未知x-codex多值；Connection提名字段双向剥离；不修改src切片。UA/thread originator可不同，身份改写时清opaque attestation。 |
| resource close与nil边界 | base direct client补CloseIdleConnections；native reaper不回收starting/closing/有waiter child；启动listener所有权有defer；nil配置输入、nil响应、无body正长度失败明确，不依赖panic。 |

## 保留差异与证据边界

没有确认尚未修复的高影响新增回归，也不能据此声称整个网关与 CLI 完全相同。以下差异实际仍存在，已经与专项证据交叉检查：

- Go transport 的 TLS/HTTP2 栈不等于 native Rust，应用 User-Agent profile 不能改变 TLS 指纹；macOS/Linux profile 中 OS版本为确定参考值，并非运行设备探测。
- 多账号授权/路由、固定显式代理、不走环境代理绕过、准入、session lease、named WS lane、有限完成ID历史、后台配置是网关能力，CLI没有相同多租户层。
- CLI默认WS connect为15秒，网关握手为90秒；native WS写/IPC保护仍最多60秒，provider stream idle默认300秒；timeout接口和背压排除范围不完全相同。
- native默认cli_request追随新client/connector生命周期；pooled额外复用。HTTP helper禁重定向和内部重试，由Go集中控制一次401恢复，没有照搬CLI全部reload/refresh/retry状态机。
- CLI fork默认message64MiB/frame16MiB，网关message32MiB/frame16MiB；SSE事件/聚合32MiB、8192个输出项、models8MiB、OAuth2MiB均为网关资源限额。
- `response.done`、无response ID和宽松usage是兼容扩展；nonstream collector保留EOF无空行兼容；首terminal决定结果，而CLI部分failed/incomplete分支可继续读；nonstream partial JSON与CLI stream错误不是同一输出接口。
- legacy JSON compact适配与CLI0.155原生`/responses` compact不同；prompt/compat normalization是显式网关策略，不能称请求逐字节不变。
- 单条usage已检查int64溢出，跨长期聚合或PG bigint总量极限未在本次改变。OAuth重复JSON字段/大小写接纳仍比Rust serde宽松，id_token/last_refresh和永久refresh失败策略仍存在数据模型差异。
- 动态证据只覆盖已运行平台/证书/路由/fixture。Windows/macOS native、真实生产账户/域名、HTTPS/SOCKS代理同平台CLI抓包、HelloRetryRequest、raw header顺序、精确压缩wire bytes和TCP/IP指纹没有因本轮通过自动获得证明。

## 本专项执行与最终 gate

本机 Windows 实际执行：

```powershell
go test . -run 'Test(AuditFinalSettingsRepair|GatewayRuntimeAuditPersistedSettingsValidation|GatewayRuntimeAuditSettingsVersionsDoNotOverflow|GatewaySettings|Settings|WebAPIRequiresBearer)' -count=1
```

修复后 PASS，0.439秒；此前新回归在未修生产实现时失败，保留了真实缺陷的前后证据。

```powershell
go test . -run 'Test(Audit|GatewayNativeAudit|GatewayNativeWSAudit|GatewayWSSessionAudit|GatewayHeadersAudit|GatewayRuntimeAudit|GatewaySettings|Settings|AuthRefreshAudit|GatewayCooldown|GatewayRouting|SOCKS)' -count=1 -timeout=120s
go vet ./...
```

广泛专项回归 PASS，12.911秒；vet PASS。该命令的源码快照早于root刚补的stateful SSE splitter，splitter由root另跑SSE/collector边界并通过，本专项没有把前一个快照的动态结果冒称为后一个快照结果。

PG管理恢复最终必须包含 `TestPGGatewayAuditFinalSettingsRepairThroughHandler`，由root在真实合成PG环境加race执行。root还负责包含本修复、最终SSE splitter和最后WSS owner/same-child断言的完整Linux/Windows冻结快照验证；本报告不将待执行项写成已经通过。最终状态与hash以`INTEGRATION.md`和主报告为准。

本专项生产文件冻结时SHA256：

| 文件 | SHA256 |
| --- | --- |
| `admin.go` | `c0aa28c298c632738a6b1acfb31aa816f3a521d0fcf39f1a7b082e8b0d2db357` |
| `store.go` | `a222681dceac785103d6abfcf37feb93d3667961f4c7b667d31b29cf4d6c40ab` |
| `gateway_final_review_test.go` | `84073a8679cce6dbfc8c867f87ddecd176fcc347f34e59a54d237663cfed4caa` |

单条复跑命令（有PG环境时同时验证文件和PG真实管理路由）：

```sh
go test . -run 'Test(AuditFinalSettingsRepair|PGGatewayAuditFinalSettingsRepair)' -count=1
```

## 主任务最终验收回填

最终固定CLI批次13顶级测试通过，包含本专项新增/强化用例；最终完整Linux race+PG/Redis+native helper为821顶级通过、0失败、0 DATA RACE（19外部opt-in逐项跳过，CLI和network-none OAuth已独立补跑）。WSS最终报告的同helper进程、late-alert归属与全部四场景checks均为true；PG管理修复冷/热缓存已在完整最终批次通过。当前所有Go生产源码与这两个最终快照一致。证据见`.local/gateway-audit-20260924/go-full-final/`、`go-cli-final/`、`cli-reports/cli-final-codex_native_ws_tls_report.json`。
