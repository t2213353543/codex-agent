# 网关研究：多轮上下文保真与原生派发竞态

2026-09-25。本轮从未覆盖的多轮状态和并发窗口重新构造实验，确认并修复两类生产逻辑缺陷。它们不是根据答题结果猜出的改动：修改前有确定性失败，修改后在相同场景验证。原题稳定答 21 的目标没有被这两项修复自动宣称完成。

## 多轮 JSON/compact 丢失已完成上下文

固定 CLI 0.155.0 将 `response.output_item.done` 的完整 item 独立加入历史；终态主要提供 response ID、usage 等信息。网关 JSON 聚合却会用相同 ID 的非空 terminal.output item 完全替换已收集的 done item。

新实验通过真实 HTTP handler 做两轮请求：上游先给完整 reasoning、function_call、message，再给只剩 ID/type 等字段的瘦 terminal；客户端把第一轮输出真正送回第二轮。修改前 JSON 路径丢失 `encrypted_content`、`encrypted_function_args`、`namespace`、`phase` 及未知字段；SSE 对照保持完整。compact 同样可能丢失已完成的加密摘要，错误返回 502。

修复仅在全部 terminal item 都能以 ID 关联已完成 item 时，补回缺失顶层字段。明确 null、空串、空数组、空对象继续保留；不深合并、不解密、不猜未知 item。完整 terminal 顺序保持；工具目标、角色、call ID 等身份冲突拒绝拼接，旧加密参数不会被接到新 plaintext arguments 上。聚合仍受 32 MiB 限制，raw SSE/WS 不改写。

修改前两个顶级研究用例失败；修复后 36 项关联回归、98 个含子项测试通过。详细触发条件、源码比较与 red/green 证据见[上下文研究](quality-20260925/context.md)。这说明网关曾存在能够损坏下一轮上下文的路径，不代表历史单轮糖果题必然由此导致；使用 raw SSE 的原生 CLI 在该控制实验中原先就通过。

## 账号撤销期间仍派发旧身份

第二组实验在真实 HTTP 账号选择完成后，阻塞 native helper 冷启动。在等待期间，管理存储完成同 workspace 换 user、owner A→B→A、删除或禁用；释放启动后，旧实现仍将旧 owner 的请求头和正文送入 IPC。四种初始失败已实际复现。

修复将权威账号/授权检查带到 HTTP/WS 的原生 IPC head 写入前。每个首次/恢复尝试绑定自己的账号快照，核对 owner revision、启用状态及当前 key/group 权限，不自动换账号、不偷偷替换 bearer；相同 owner 的正常令牌刷新继续保持已准备请求。被明确拦在发送前的请求不记录为已发上游 attempt，同时保留具体授权/身份错误。

交叉审查进一步检查了新校验本身：HTTP 授权读库使用 native transport 派生的请求 context，遵守当前请求 deadline；不能沿用较长寿命的父 context。最终账号检查完成后再次核对代理版本，WS 同样覆盖本地 IPC dial 后的最后发送窗口，防止检查等待期间代理被改掉。

新增用例涵盖 HTTP 9 个状态场景、WS 8 个场景、正常刷新保留原 bearer、同 helper 并发取消互不干扰、派生超时、检查期间代理变化和未发送 attempt 的观测边界。细节见[原生状态研究](quality-20260925/native-state.md)。检查后到实际发送之间仍不是跨数据库和 socket 的原子事务；本修复堵住已经完成修改、但被长等待掩盖的窗口，不承诺分布式瞬时撤销。

## 出口假设的独立控制

同时核实 q4 所在 28765 的既有出口为 `account-warp` SOCKS5，而现网网关直连；两个无凭据 trace 请求确认公网 IP 摘要不同，同为 LAX/US，分别 warp=off/on。该信息仅验证出口不同，不证明执行集群不同。

原 q4 身份和分配保持，但上游已自然更新 AT/OAuth 时间戳；本轮锁定新凭据窗口，不混入旧基线。预先固定“直连→来源 WARP→直连”的三次实验，保持原题、instructions、model、medium、helper 和 native 生命周期相同，遇非完整响应就停止。

第一次直连实际 IPC 正文、身份和进程检查通过，上游 HTTP 200 后出现 created/in_progress/error，12.023 秒结束，没有 completed 或最终答案。因此按计划停止，实际只有一次模型调用，没有执行 WARP 和第三次直连，不给出口影响或数学正确率结论。固定旧探针没有保留该 SSE error 的结构化错误码，不能再推断它是容量或限流。AT 临时快照已删除，helper 均正常回收。

证据位于 `.local/research-20260925/egress-q4/` 与 `egress-report.json`。这项未完成对照不阻止两个已复现源码缺陷的修复，也不充当它们的因果验证。

## 集成验收与发布记录

两项修复已经上线。最终 Linux 网关 SHA256 为 `2ed703ba3e696ec90cca97990c5e1209dcac7d41813ca7f5e20e6dbfef9173c6`；Rust helper 保持 `f72e5c0ff8798c02f430f7aa36f74009dad40fade6111dad722390891239b556`。无在途请求时切换后端，readiness 恢复正常；账号 metadata 摘要、应用环境（排除 systemd 每次生成的进程标识）、监听和前端路径保持一致，没有数据库 migration。

| 最终验证 | 结果 |
|---|---|
| Linux 全套 race + PostgreSQL/Redis + 真实 Rust helper | **854 个顶级通过、0 失败、21 个明确 opt-in 跳过**；2410 个含子测试终态，287.906 秒，零 DATA RACE，vet 通过 |
| 固定官方 CLI 工具/compact/恢复/WS 独立批次 | **7 个顶级通过、0 失败、0 跳过**，13 个含子项终态，15.607 秒；CLI 实际执行工具，上游模型回答为合成 fixture |
| Windows 最终全套 | **732 通过、0 失败、144 个明确外部环境跳过**，41.111 秒；vet 与 Linux 交叉构建通过 |
| 快照对应 | 两个 Linux 冻结快照均与当前 Go/SQL/module 文件逐项一致；108 个构建输入（生产 Go 与 module 文件）摘要与候选一致 |
| 发布后实际 HTTP | 认证 `/v1/models?client_version=0.155.0` 返回 200、9 个模型 |
| 发布后实际 WS | 101 握手及 accept 校验通过，正常关闭后 in-flight=0；只预连接、不发模型回合 |

首版 852 项 Linux／730 项 Windows 结果早于派生 context 和最终代理检查的修正，保留为中间证据，不代替上表最终结果。完整 Linux 中的 CLI opt-in 跳过与独立 CLI 批次分别记录；没有把未重跑的真实数学调用算作通过。

最终原始证据：`.local/gateway-audit-20260924/go-research-20260925-final/`、`go-cli-research-20260925/`；发布与运行证据：`.local/research-20260925/build-manifest.json`、`activation-before.json`、`activation-result.json`、`runtime-smoke.json`。本次服务覆盖文件只修改 ExecStart；回退可移除本轮 `research-20260925.conf` 并重载/重启，既有质量修复覆盖和账号数据无需回退。

重复运行有判别力的新回归，不调用真实模型：

```powershell
go test . -run '^(TestResearchContext|TestGatewayResearchNative)' -count=1 -timeout=90s
```

结论限定为：瘦终态导致的多轮上下文丢失与冷启动等待期间的旧身份派发已修复并验证；未确认它们解释此前单轮糖果题错答，原题稳定答 21 的目标继续保留。
