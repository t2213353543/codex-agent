# App Server、官方 CLI 与本项目直连的对比

本次使用固定官方 Codex 0.155.0，捕获隔离测试进程的 HTTP 请求、响应及 WebSocket 消息，并核对同版本源码。原始捕获留在服务器私有目录，脱敏报告在 `.local/appserver-parity/reports`。生产设置在捕获前后保持不变。

## 已经明确的结论

| 对比对象 | 实际行为 | 本次证据 |
| --- | --- | --- |
| 官方 `codex app-server` | 官方运行时管理线程，通过 Responses HTTP/WSS 访问 Codex | 原生 pilot/full 捕获 |
| 官方 `codex exec` | 同版本官方 core 构造请求；客户端自身管理线程、工具 | 同账号、令牌、模型、档位、输入对照 |
| 本项目原 `official_codex_cli` | 普通 Responses 接口直接经 Rust 网络辅助进程转发；没有运行 `codex exec` | 旧模式 pilot/full 捕获及 handler 源码 |
| 新轻量 App Server CLI | 根据真实模型目录构造请求；客户端负责历史和工具执行 | 独立 writer oracle、工具序列化捕获、真实请求与回归测试 |

前三者访问的普通推理地址均为 `https://chatgpt.com/backend-api/codex/responses`。相同 URL 不足以证明完整请求或服务端执行相同。

真正官方 App Server 与官方 CLI 的受控首轮测试，展开预热和增量输入后都是 7 项上下文。明确归一测试目录后，上下文文本相同；续聊输入都为 10 项。两者的模型目录、模型选择和 `xhigh` 一致。真实差异包括 originator/user-agent、动态身份与时间、CLI 的 `thread_source=user`、恢复会话时连接是否复用。这不是“所有出站逐字相同”的结论，也没有证明一个使用高质量模型、另一个使用低质量模型。

两种官方进程都遇到过 WebSocket 403 并回退 HTTP。403 正文为空，没有错误码证明它由配额、认证、风控或模型路由中的哪一项造成。

## 已修复的轻量通道问题

1. 按实际目录处理 Lite、默认推理/文本参数、工具前缀和请求元数据；区分用户明确的空 instructions 与网关自动补出的空值。
2. 保留客户端自定义 grammar、函数、命名空间、schema 和显式参数；不追加客户端无法执行的服务器内置工具。
3. 预热后仅在前缀验证通过时发送增量；握手不可用且尚未生成时可以回退 HTTP，生成已经发送后不自动重放。
4. 上游完整错误事件正常送达客户端；不再把已收到的参数错误误变成连接 EOF。
5. 不擅自添加上游拒绝的 `tool_choice.namespace`。
6. 账号认证、额度或模型不可用时，按原权重、优先级和并发准入选择其它账号；输出已经可见后不重放，成功前以 CAS 更新会话归属。
7. 标准内置搜索使用顶层 `web_search` 的 HTTP 格式。实际捕获证明，把它放进 Lite `additional_tools` 并不等于获得搜索能力。
8. 补齐原生客户端独立搜索的 `/v1/alpha/search` 透明转发；中转服务器不运行搜索选择或工具执行循环。
9. 保留 `output_item.done` 中的实际输出及最终返回模型、input/output/cached/reasoning usage，避免末尾 `output: []` 导致误判工具没调用。

## 性能和完整性边界

真实 `execve` 审计确认轻量通道未启动官方 Codex 会话运行时，只使用网络传输辅助进程。它不在服务器保存完整对话或为每条对话创建官方 agent。

本轮也观察到上游处理阶段的长等待：握手约 0.2–0.9 秒，响应创建后约 12–32 秒才出现首次推理内容/文字。原始出站捕获已含这段等待，因此不能将它归因为网关缓冲；目前无法仅据此区分上游排队和推理耗时，不能承诺固定首字延迟。

最新修订已跨 HTTP 复用连接，以账号、key、代理和线程隔离；只保存固定摘要与响应引用。每轮重新核对完整历史前缀，并在响应交付和 owner CAS 完成后归还连接。已经构造好的原生 Lite 请求只同步实测身份/元数据差异，不叠加上下文。仅携带 `previous_response_id` 的增量请求缺少跨账号重建所需的完整历史，因此保留原账号失败，不清除 ID 后伪装续聊成功。

Guardian、记忆、实时、插件安装、云任务、遥测等条件接口在出站清单中分别标注源码与捕获状态；没有为了制造“相同流量”而自动触发这些客户端操作。完整清单见 `APP_SERVER_EGRESS_INVENTORY.md`。这个实现不能称为整个官方运行时的完美替身。

## 启用与复跑

代码通过 `CODEX_APP_SERVER_CLI=1` 启用轻量适配，并保留现有 `official_codex_cli` 模式值。管理界面会显示“App Server CLI（轻量）”；旧直连显示“Codex 直连”，避免误认为每次请求都在运行官方 CLI。

```powershell
python -B tools/appserver_parity_capture.py --route official_codex_cli --lightweight --native-helper /opt/codex-agent-integration/checks/native-unbounded-20260927T121556Z/work/target/release/codex-native-transport --native-helper-sha256 361b0df32ce0e74d5a07bad697abf49cb59e295eb1795797ae4c2a18548948ee
```

测试会冻结源码和可执行文件，使用隔离目录，不改变生产模式。正式使用时客户端继续向原 `/v1` API 发送会话、压缩及工具请求。

## 最终验收记录

- 真实轻量测试：`20260927T105836.078019Z`，14 项全部通过；模型 `gpt-6-astra`，`xhigh`。报告：`.local/appserver-parity/reports/final-functional.json`。
- 普通回复、两种记忆恢复、函数回传、自定义语法输入与回传、命名空间及结构化对象均逐项检查内容。
- 标准网页搜索：1 次真实 `web_search_call`、1 个来源引用；此次返回 `cached_tokens=3584`。这只是该次命中证据，不表示每次短请求都应命中缓存。
- 独立 `/alpha/search`：HTTP 200，12 条结果，保留 `output` 与 `encrypted_output`；该接口没有标准 usage，不伪造 token 数。
- Linux race：49 项顶层测试全部通过，0 skip、0 missing。报告：`.local/appserver-parity/checks/20260927T110118.103613Z-d7e2f7/report.json`。
- 真实捕获与 race 的 548 个共有源文件哈希全部一致；race 额外包含 4 份捕获 fixture。捕获可执行文件 SHA256 为 `dbb5d0664b8a35644a0e791087aeb0b6da93050910df16e2d47baea3dfe7e4a6`。
- `go test ./...`、`go vet ./...`、前端运行模式显示测试及 Vite 构建通过。
- 本次没有部署或切换生产模式；现有生产运行时与这份已验证的候选代码分开记录。

## 连接复用与辅助协议修订

新捕获 `20260927T114002.409532Z` 的首轮和续聊仅建立一次 WebSocket 握手。完整复测 `20260927T114305.414073Z` 的 14 项通过，包含一次握手 403 后的生成前 HTTP 回退。该版本的 128 项 Linux race 测试全部通过，0 skip、0 missing；捕获与 race 的 562 个共有源文件哈希相同。

应用身份在准备阶段通过官方 binary 的离线 `initialize` 获取一次，网络命名空间禁止外部连接，不登录、不发模型请求。User-Agent 与 originator 与原 App Server 捕获逐字符串一致。生成阶段仍为零官方 Codex exec，网络辅助进程负责传输。分别记录初始化与生成计数，不能把生成阶段零进程扩大为从未运行过初始化捕获。

新增图像生成、图像编辑和 memory JSON 辅助转发；两种官方 frontend 的图像请求已在纯本地 fixture 捕获。Guardian 的两个 WebSocket 路径已补齐，并通过实际 Rust native helper 到隔离本地上游的测试；没有把模拟响应称为远端图片或审核成功。

尚未据此宣布整个目标完成。完成审计发现，轻量入口仍经过旧 direct 通道的 32 MiB 请求/消息限制，切换生产前必须贯穿入口、JSON 准备、Rust IPC、WS/SSE 一并处理，才能保持此前要求取消会话请求上限的行为。Realtime 等尚未验证的条件接口也在继续单独捕获。

## 无总载荷限制与 Realtime 最终候选

上述 32 MiB 回归已经贯穿修复：HTTP plain/zstd、JSON 准备与账号缓存改写、Rust HTTP 与分块 WebSocket v2、SSE 单事件和聚合输出均已移除轻量 profile 的本地总量限制。账号并发仍按原配置执行。独立 Realtime call-create 与 sideband 已实现并核验官方七种请求格式；Realtime 使用独立的无压缩 WS 配置，保留 Responses 原有 deflate。

最终候选 Go artifact 为 `af327b3a0ec3295db7fa3d7e93149fa8ca0738a8b617ec7a6807d35d33ae22f9`，Rust helper 为 `361b0df32ce0e74d5a07bad697abf49cb59e295eb1795797ae4c2a18548948ee`。

- 模型真实 run `20260927T123407.756914Z`：14 项内容核验通过，0 官方生成运行时进程；独立离线身份准备运行一次 initialize。搜索产生 2 次真实调用、1 次引用，实际 `cached_tokens=3584`。
- 最新 Go/Rust 组合：13 项全部通过、0 skip，包括真实 33 MiB HTTP/zstd/TLS WebSocket、Realtime、Guardian、96 并发和同账号认证恢复。
- 当前源码 Linux race：168 项全部通过，0 skip、0 missing；源码哈希 `77911745048138ecb5e8375dea78aa4a7ea0fdccfffbce84db21c8075b843415`，冻结后源码未变。
- 所有 575 个真实捕获共有源文件与当前源码、race 快照一致。`go test ./...`、`go vet ./...`、前端构建和相关显示/导出测试通过。

统一证据索引：`.local/appserver-parity/reports/latest-evidence.json`。真实模型内容审计：`functional-unbounded-realtime-reviewed.json`。早期审计仅因合法的 `npm i` 缩写被写死的字符串检查拒绝而失败；确认 npm 内置帮助的 alias 后修正审核规则，原始捕获保留且没有重发该模型请求。

Realtime 用量仅采集实际 sideband 消息，缺少缓存字段时显示未知；账号详情和 CSV/JSON 导出也保留此区别。已验证的本地 Realtime fixture 不等于真实音频 ICE/媒体可用性。其它客户端登录 gate、插件/账户生命周期和全部条件差异仍在完成审计中；候选尚未部署，不能宣布整个目标已完成。
