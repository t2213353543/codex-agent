# 流协议研究：错误归属、WS 字段一致性与出口对照

本轮继续在上一轮已上线的 `2ed703ba…173c6` 基础上研究可判别的新边界，确认两类缺陷：外国响应错误污染 HTTP 账号冷却，以及 WS 对协议字段的大小写折叠导致授权和回合状态不一致。同时完成一次两出口原题对照。原题通过标准仍为 21，没有通过修改题目、提示词或选择答案来宣布恢复。

## 外国错误事件不应暂停当前账号

真实 TCP 上下游实验先发 HTTP 200 与 `response.created(id=A)`，随后发带 `id=B` 的 rate-limit 错误。旧指标观察器拒绝把外国 usage 合到 A，但另一个冷却观察器没有核对响应归属，仍给当前账号添加 60 秒冷却。下一条独立请求实际没有发往上游，直接被本地 429 拒绝。

JSON/SSE 各三个冲突/外国 ID 场景，共六个失败前复现；同 ID 与匿名 error 对照继续限流。修复为每条 HTTP 流保留精确响应 ID 的摘要，拒绝与当前 ID 不符或顶层/嵌套 ID 相互矛盾的事件安装冷却；不截断 ID、不把日志脱敏字符串作为路由依据。缺少 ID 的合法兼容错误仍生效，第一次就失败、尚无 created 的请求也能正常记录冷却。原 SSE 字节不改，JSON 对失败的 502 兼容行为保持。

共享冷却解析还明确按精确 JSON 字段读取事件类型、lane、response ID 和 response/error 容器，防止未知 `Type`、`STREAM_ID`、`Response` 等字段覆盖规范字段。HTTP header 字典内的 Retry-After 名称继续不分大小写，不能将 JSON 成员规则错误套到 HTTP header 规则。error/code 的既有兼容读取没有全项目泛改。

详细失败前日志、合法控制与真实 native 回归见[错误语义研究](quality-20260925/failure-semantics.md)。该缺陷会造成误冷却及额外本地 429，不代表它已被证明导致数学错答。

## WS 状态必须按精确协议字段判断

规范控制用实际多轮 WS 形态验证了 `generate=false` 预热、`previous_response_id` 搭配空 input 的续接、opaque reasoning/tool-result 增量、多 lane 以及迟到旧终态。这些规范形态原先就通过，未改写上游 payload。

新的反例来自 Go `encoding/json` 向 struct 解码时不区分字段名大小写：

- 规范 `type=response.create` 搭配未知 `Type=response.cancel` 时，网关把它当作非 create 直接透传，跳过下一轮授权；账号已停用仍被上游执行。规范请求控制正确返回 403。
- 规范 `stream_id=main` 搭配未知 `STREAM_ID=shadow` 时，网关把并发槽记在 shadow，上游按 main 返回完成；槽未释放，合法空增量续接被本地 429 拦截。

修复仅改变网关用于控制、归属和观测的字段读取：按确切 JSON 成员取值，原消息和未知扩展继续透传，不把协议大小写规则混同 HTTP header 规则；WS 终态复用相同的完成契约，避免独立实现分叉。规范 CLI 多轮保持原样。官方 CLI 并不会因为此报告就被认为会自行生成这些大小写遮蔽字段；该项是网关协议和授权健壮性问题，不作为账号集群假设的证明。

详细复现与限制见[WS 多轮语义研究](quality-20260925/ws-semantics.md)。

## 两出口原题对照

上一轮固定三次实验的首条请求收到 SSE error 后按计划停止，没有评分或继续。此轮增强了 test-only probe：HTTP 200 后发生的 `error`/`response.failed` 会保存有限 code/type/reset 字段和错误 payload 摘要，不保留任意诊断正文；流截断/EOF 不被猜成上游错误码。离线测试覆盖顶层、嵌套失败和 EOF。

新批次预先固定两个条件，各只调用一次：先使用 q4 所属 28765 项目的既有 WARP 出口，再使用网关的直连出口。固定同一个 user/workspace、新 AT 窗口、原题、instructions、`gpt-5.6-sol/medium` 和 native helper。HTTP 401/403/429 或明确认证/限流错误会停止；容量或其他无答案响应作为可用性观测保留，不按答案重试。本次两条都正常完成，没有触发停止条件。

| 条件 | 原题结果 | HTTP/终态 | 时长 |
|---|---:|---|---:|
| 28765 既有 WARP | 29 | 200 / completed | 12.057 秒 |
| 直连 | 29 | 200 / completed | 36.210 秒 |

两个无凭据 trace 确认出口公网 IP 摘要不同；均为 LAX/US，分别 WARP on/off。两条实际 IPC 正文均为 `763f3380…df7f5`，相同 AT 摘要、workspace、model/effort 和所选应用头均核对一致；每条恰好一次 dispatch，helper 正常回收，临时 AT 快照删除。模型返回均为 `gpt-5.6-sol/medium`。输入 token 同为 226，但时长和输出 token 有差异，不能据此推导算力或内部集群。

此配对的结论是：本次切换回来源出口没有使原题答对。每条件一次、有固定顺序，不能估计总体正确率、排除时变因素或证明 IP 永远无影响；旧无答案样本保持原分类。证据位于 `.local/research-20260925/egress-errors-q4/` 与 `egress-errors-report.json`。

## 最终集成与发布

两类补丁已上线：后端 SHA256 `ac25b0f020baf0211725e94ae779a2f8e046cbebce0b704ba0629c0e875d3d82`。无在途请求时替换 ExecStart；原生 helper 仍为 `f72e5c0f…b556`，应用环境、监听、前端及账号 metadata 摘要保持一致，没有数据库 migration。

| 最终冻结验证 | 结果 |
|---|---|
| Linux race + PostgreSQL/Redis + 真实 native helper | **864 顶级通过、0 失败、21 项明确 opt-in 跳过**；2467 个含子项终态，280.495 秒，零 DATA RACE，vet 通过 |
| 固定官方 CLI 工具/compact/恢复/WS 批次 | **7 顶级通过、0 失败、0 跳过**；13 个含子项终态，15.876 秒；使用实际 CLI 和合成上游响应 |
| Windows 全套 | **742 通过、0 失败、144 个外部环境跳过**，42.896 秒；vet 与 Linux 交叉构建通过 |
| 发布后真实传输验收 | HTTP models 200、9 个模型；WS 101 与 accept 校验通过，正常关闭后 in-flight=0，readiness=ready |
| 源码关联 | 两个冻结 Go/SQL/module 快照与当前工作区逐文件一致，构建输入与候选摘要一致 |

新增真实 native 冷却分支在 Linux 全套中实际执行，并非将 Windows 的 Go loopback 结果代替 Rust。发布后的 WS 只做握手和关闭，未发送模型回合；不能由此宣称新数学题通过。两出口模型实验的二进制基于上一版已部署 `2ed703ba…173c6` 加 test-only 结构化错误采集，与本次补丁分开，不混写成升级后的质量验收。

最终证据：`.local/gateway-audit-20260924/go-stream-20260925-final/`、`go-cli-stream-20260925/`，以及 `.local/research-20260925/stream-continuation/{build-manifest,activation-before,activation-result,runtime-smoke}.json`。新增服务覆盖仅为 `stream-20260925.conf`，移除此覆盖并重载/重启可回到上一个已验证后端，不需要回退账号数据。

本轮确切进展是修复误冷却、跨轮授权遗漏、并发槽未释放和观测不一致；原题两出口均答 29，尚未证明异常账号恢复或账号标记/内部集群假设。仍保留原目标，不将这些协议修复替代原题通过要求。

一条命令复现本轮边界，不调用真实模型：

```powershell
go test . -run '^TestResearch(Failure|WSSemantics)' -count=1 -timeout=90s
```
