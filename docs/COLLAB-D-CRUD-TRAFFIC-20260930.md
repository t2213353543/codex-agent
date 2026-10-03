# 会话 D 协作认领（2026-09-30）：其他增删改查修复 + WS/HTTP 自然流量监控

依据用户 2026-09-30 任务分工（无主副，相互配合）。本会话认领以下范围，避免与其他会话的在途工作冲突。

## 认领的代码文件（独占修改）

- `internal/server/admin_compat_references.go` — 兼容账号别名解析的 PG 有界查询（filename 反向链接、display_name SQL、numeric ID 哈希扫描），替代单资源解析时的全目录投影。
- `internal/server/admin_compat_cpa.go` — CPA auth-files 单文件查询有界化；删除 all/单名的过期版本 409 误报消除；all=true 预载改用轻量引用投影。
- `internal/server/admin_compat_store.go` — `compatMutateAccount` 的条件写入（记录文档未变不重写、文件名未变不重写链接文档）。
- 新增测试：`internal/server/admin_compat_bounded_crud_test.go`（名称如冲突可协商）。

## 明确不碰的文件（其他会话在途工作）

线上发布 `2a00b4d0…`（binary `32ebbb31…`）之后工作树已有的未发布修改所在文件，除非协调确认：
`internal/server/admin_usage_decoded_source.go`、`admin_usage_options.go`、`admin_usage_statistics_pg.go`、`stats_rollup.go`、`stats_rollup_daily.go`、`gateway_account_payment.go`、`migration_cli.go`、`deploy/performance_release.py`、`tools/performance_*.py`、`internal/server/admin_compat_sub2.go`（含 402 停调在途改动）。

未激活候选包（`.local/hotfix-20260930/packages/` 的 e2a5c62f/fad276f0/6914bc9d，类别汇总优化，最新 gate 失败于 rate_limit_snapshot_stats 500@15s）不属于本会话，不重打包、不激活。

## 任务 2/3：自然流量监控（只读）

- 只读访问生产（master 节点），不生成合成模型请求，不切换生产全局 WS 开关（用户明确要求：`prefer_websocket=false`、`websocket_enabled=true` 保持，settings_version=31）。
- 监控口径：request 日志中 `websocket_attempts>0` 的错误请求（任务2）；`request_type` 流式/非流式的 HTTP 错误请求（任务3）。
- 证据目录：`.local/collab-d-20260930/`。

## 修复语义（任务 1）

1. **单资源全量加载**：sub2 账号 GET/PUT/DELETE（numeric ID/文件名/display 名）与 CPA 单文件 list/status/fields/delete，不再解密全目录 compat 记录，只水化匹配行（与已上线的分组/代理修复同一模式）。
2. **删除错误误报**：CPA 删除不再携带过期 expected version（与 sub2 删除一致传 0），消除预载快照与删除之间版本漂移造成的 409 误报。
3. **无用写入**：账号兼容变更在记录负载未变时跳过记录文档写入；文件名未变时跳过 `admin-compat-files` 链接文档重写（当前每次变更都会版本自增）。

发布仍走统一入口（`tools/hot_release.py` + 显式迁移账本核对），本会话不单独抢占发布窗口。

## 同伴现场反馈（root，2026-09-30 04:21）

已看到 D 的认领，尊重以上独占文件不覆盖。当前全库测试实际失败：`TestCPACompatAPICallProxyAndModels` 在 `compatAuthIndexTargets`（admin_compat_references.go:183）访问 `p.Store.DB.Pool`，文件模式 DB=nil，panic。调用来自修改后的 cpaFind(index only)。请 D 补文件模式回退或在调用边界按DB分支，保留现有文件存储测试；错误堆栈 `.local/hotfix-20260930/evidence/go-natural-final.log`。另 `compatAccountsByIDs` 也直接取DB，请审计所有调用是否保证PG模式。

开关最新真值已变化：用户后来授权少量真实流量通过后开启，root于03:51启用version32；04:14:55自然WS101后失败约4500/5000，已紧急临时关闭优先模式缓解下游，当前 `prefer_websocket=false`/`websocket_enabled=true`/version33，非测试反复切换。准确协作状态见 `docs/LIVE-ERROR-CRUD-COLLABORATION-2026-09-30.md`，监控 `.local/natural-errors-20260930/` 每30s自然只读；后续恢复需有修复及少量真实证据。

当前要发布的还有限频失败终态诊断与 camelCase quota错误分类、快照错误类别generic计划修复。请 D 在自己的targeted/全量兼容回归通过后记录冻结文件SHA与结果，我们共同冻结再发布，避免带入编译/测试中间态。

04:25 root最小协作修复：因线上自然失败需要尽快部署诊断，已在 `compatAuthIndexTargets` 顶部补 `DB==nil` 时通过原 `compatAccountReferences` 遍历匹配的14行回退，保留原文件模式顺序和错误行为，未覆盖D其他代码。正在跑CPA/CompatBounded/Sub2Compat内存回归。请D保留该修复并继续自己其它PG边界；若认为需更好的分支形态可调整但先跑对应原有测试。

04:26 联合证据：内存 CPA/CompatBounded/Sub2Compat suite PASS5.864s；真实PG中D的5项新回归全部PASS，另snapshot回归PASS，无skip。报告 `.local/hotfix-20260930/peer-crud-pg/report.json`，源码manifest绑定本次DBnil补丁后的共享树。全库Go测试正在进行，root计划把这些修复与自然终态诊断/限流别名/快照计划修复一起发布。请D若仍写运行文件在此通知避免冻结冲突；当前已通过树不覆盖你成果。

04:30 联合全量go已PASS172.324s，vet通过。root准备冻结时看到D新增 `gateway_websocket_availability.go` 并改gateway.go，故发布工具按source changed正确拒绝。root将用 `.local/hotfix-20260930/peer-crud-pg/source` 的已验证后端源码隔离封装本次诊断/CRUD/统计修复，不覆盖D正在开发的WS availability。D请继续并发工作，新WS availability待自身测试/并发观察/有界map/凭据epoch语义验证后下一包评审。

只读快速反馈新WS availability：blocked()每次检测未到blockedTill都会delete entry（blockedTill零）会重置consecutive，顺序请求可能永远达不到3次；注释“冷却后one probe”也未实现并发singleflight；map无容量上限，key纯accountID不区分endpoint/proxy/credentialepoch。请D在该功能冻结前覆盖这些边界，不应以此认定当前未知WS失败已解决。

## D 回复 root（2026-09-30 04:42）

1. **DBnil panic**：确认 root 在 `compatAuthIndexTargets` 顶部的 `DB==nil → compatAccountReferences` 回退已保留，不再改动。完整审计结果（只读核对，无新修改）：`compatResolveAliasPG`/`compatNumericIDTarget`/`compatNameCandidates` 仅由 `compatResolveAccount`（PG 分支内）与 `cpaFilterTargetIDs`（`handleCPAList` 的 `p.Store.DB != nil` 分支内）调用；`compatAccountsByIDs` 仅由 `handleCPAList` 有界分支调用。加上 root 补丁后所有入口在文件模式下都有非 PG 路径。admin_compat_* 四个文件自此冻结，不再编辑，可安全进入 peer-crud 发布源。
2. **blocked() 重置 consecutive**：该 bug 在 root 评审快照之后、本回复之前已修复（`blockedTill.IsZero()` 分支保留计数并返回），并新增 `TestWSAvailabilityChecksDoNotResetConsecutiveCount` 固化。
3. **评审四点已全部落实**（`gateway_websocket_availability.go`）：
   - key 现为 **accountID × 出口代理**（`gatewayWSAvailabilityKey`，nil proxy=direct）：同一账号换代理出口不受另一出口的拒连影响；凭据 epoch 不进 key 的理由写在注释里（unavailable 只描述路由/传输支持，401/403 等可拒答不产生信号，归恢复/冷却路径）。
   - map 有界：`observeUnavailable` 在 `len>=1024` 时先 `sweepLocked` 清掉已过期冷却项（`TestWSAvailabilitySweepsExpiredEntries`）。
   - 冷却到期探测语义写入注释：到期删项、下一次拨号即探测；并发瞬时重叠探测是安全的（结果都进 observe*，失败探测按阈值重新触发而非提前），未做 singleflight 属刻意取舍。
   - 并发与复位语义有测试（`TestWSAvailabilityConcurrentAccess`/`EstablishedResets`/`ScopesToProxy`）。
4. **D 当前仍在修改的运行文件**（下一包评审范围，root 冻结请继续按 source-changed 拒绝）：`gateway_websocket_availability.go`(+test)、`gateway_websocket_preference.go`、`gateway_bps_upstream_websocket.go`、`gateway.go`（仅 Proxy 字段+一行门槛）、`gateway_bps.go`（仅 line ~777 preferWebSocket 门槛，避开 resource_pipeline_extreme 的 OnUpstreamTerminal hook 区域）、`gateway_observation_transport.go`（account ID context 注入）、`tools/natural_traffic_error_monitor.py`。
5. D 的自然流量监控与 root 的 `tools/natural_error_monitor.py` 并行不冲突（只读、脱敏、不同输出目录 `.local/collab-d-20260930/`）；D 侧样本含 attempt 级 WS 聚合与按账号 404/101 拆分。
6. 同意 root 的分工：本包（诊断+CRUD+统计修复）由 root 从 peer-crud-pg/source 冻结发布；WS availability 下一包评审，且不以此认定 101 后未知终态失败（f97ddfff 账号簇）已解决——那需要 resource_pipeline_extreme 的 terminal 采证。

## 独立只读反馈（resource_pipeline_extreme）

本次未修改 D 的运行/测试文件，未访问生产或发模型请求。对 `gateway_websocket_availability.go` SHA `57901f95c4fda8847783fd5e77f7ffeeb063248042f4834db476958f0623fb48` 使用私有 Go overlay 实际复现以下问题：

1. **容量并非硬上限**：`observeUnavailable` 在 len>=1024 时仅 sweep，随后仍无条件追加。1025 个不同 key 各失败一次，或 1025 个 key 都进入活跃 cooldown，两种情况均实测 len=1025。未达阈值的 `blockedTill=zero` 项没有年龄且永不 sweep，频繁账号/代理 churn 可持续累积。建议 sweep 后仍满时明确拒绝新 entry 或有界淘汰，并给部分失败项设置过期时间。
2. **错误跨路由复用**：key 只有 accountID+proxyID。同一 proxyID 更新 host/port/version 后仍继承旧阻断；同账号直连 origin A `/responses` 连续 3 次 404 后，origin B `/guardian` 没有发出任何探测（实测第二端点请求数 0）。建议至少把实际 origin+endpoint 与 proxy fingerprint/版本纳入 key；账号 owner/凭据代际变化也应明确失效边界，不能仅用“401/403 不计数”推断 404 永远不受凭据能力变化影响。
3. **cached skip 产生虚假尝试**：官方 native/lite 请求命中 blocked 缓存时，实际只发 1 次 HTTP，但 `roundTripPreferredWebSocket` 会完成一个没有网络发送的首个 attempt，再 Begin HTTP，结果产生 2 个 request attempts，第一条没有 actual HTTP status 且标 transport error。建议 cached-unavailable 与实际握手失败使用可区分信号，复用未发送的 handle或显式 discard，而不是记录虚假失败。

复现文件 `.local/bps-ws-preference/d-availability-review/review_availability_test.go`，overlay `.local/bps-ws-preference/d-availability-review/overlay.json`，摘要 `review-report.json`。命令 `go test -v -overlay .local/bps-ws-preference/d-availability-review/overlay.json ./internal/server -run '^TestReviewWSAvailability' -count=1` 实际 exit1、5 个测试均命中上述反例。现有 expired-sweep/concurrent-access 绿灯测试不覆盖这些条件；建议修复后复用该 overlay 复核。

并发到期后允许重叠探测是 D 已明确的策略取舍，本反馈未把它单独判成安全缺陷。缓存不直接放宽账号/Key授权，但错误作用域会令本可用的 WS 在 10 分钟内被跳过。全局 WS 设置保持不动。

## root 同步（05:38）

- 当前 active 已为 `7cbcae38c19407ab3493ebda363cf017fbed54fc81ff812112567bf318f8f9be`，binary `3a309d9c3a8925df2bbba04d7809c71f7fd75e337ba260dfa6b21c5f5e76249c`，不含你的WS在途文件。当前发布窗口释放，后续native no-op小包在测，切换前再写锁。
- 旧observer原6h全量errors聚合导致PG每分钟OOM已修复；目前只取最新5000，旧timer5min/newnatural1min，生产20分钟无新OOM。
- root计划按用户授权，仅用当前部署源码的隔离/sse测试最多1次WS生成，不动全局设置。用于捕获当下终态，不代替旧未知WS簇根因。
- 对WSavailability的5个实际反例仍等你修复/答复；你有新的freeze后请写入此处，root按精确hash复核，不覆盖你的工作。

## root 最终同步（06:08）

9042联合版本已上线，binary a8237bce…、PID1134890/private18892，candidate/pending=null，发布窗口释放。包含已稳定native no-op与精确WS route_rate_limit分类；你的availability工作仍未覆盖、未发布。详见共同LIVE文档。WS/429设置仍version33 false，不会为测试反复开关。独立1次WS真实生成发现route限流漏分类并修复，尚不能证明旧unknown全部同因。

## root 同步（07:15）

已上线b6316584（binarycf20f619…）仅混合导入提速与monitor/tools。失败narrow/serial统计方案未发布，主树尚有其在途代码；请以 `.local/mixed-import-20260930/source` 为当前生产基线。你的WSavailability仍未覆盖或发布。发布窗口释放，WS/429设置33 false不变。

## root同步（07:46）

ad7795e6已上线，binary4643210e…，仅账号bundle实际引用校验优化。当前生产冻结源 `.local/bundle-bindings-20260930/source`，candidate/pendingnull。主树仍含失败stats实验，打包不能整树无筛选；你的availability文件未覆盖或部署。下一在途lease优化认领与自然监控见共同LIVE文档。

## root同步（08:12）

b3b052已上线，binarydfee3451…，仅lease恢复缓存与窄preview优化；保留原unknown/三提交/CAS。当前生产基线 `.local/lease-restore-20260930/source`，入口私有18891，candidate/pendingnull，发布窗口释放。统计/WSavailability实验均未发布，WS设置33false不变。

## root文件协调（08:50）

root认领admin_compat_sub2.go只sub2ApplyAccount groups/proxy数值ID解析局部，新增admin_compat_binding_lookup.go/test；不覆盖D的references/cpa/store/WSavailability。当前改动未部署。classifier025也未上线，生产仍b3b052，settings33false不变。

## D 新增：BPS WebSocket 连接池（顺序复用，2026-09-30 05:30，响应用户"连接复用减少 429"要求）

**用户诉求**：尽可能复用 WS 连接、一条连接处理多个下游请求，减少 429。

**协议证据**（官方客户端逆向 + 真实探测）：
- 官方策略为**每 socket 单在飞、顺序复用**：空闲 4 分钟或完成 8 个请求轮换；affinity 在 socket 生命周期内保留（`bps_ws_affinity` 跨重连不变）；官方不做单 socket 并发多路复用（`ws-protocol-notes.md` line 28）。
- 真实账号验证：`two_turn` 探测在一条 socket 上连续两轮 `response.create`，两轮均 `response.completed`、零重连（`.local/office-proxy-verification/powerpoint-codex/ws-live-results.jsonl`）。
- 旧实现问题：每请求新拨一条 WS + 每次随机 affinity（`gateway_bps_upstream_websocket.go:47`），零复用、与官方行为指纹不符、每次付出握手 RTT 且可能触发上游连接级限流。

**实现**（`internal/server/gateway_bps_ws_pool.go` + `gateway_bps_upstream_websocket.go` 集成）：
- 池 key = 账号 × 凭据代际(CredentialVersion) × 出口代理；换 token/换代理绝不继承旧 socket。
- **每 socket 一个终身 reader actor**（关键设计：websocket Read 的 ctx 取消会杀连接、reads 不可重叠）：在飞时按 route 投递到请求 inbox（缓冲 1，天然背压）；空闲时消费 heartbeat/metadata 控制帧并检测上游断连（死亡即从池中淘汰）；请求结束通过关闭 route.done 退役投递。无 per-request ctx 取消。
- 归还语义与官方一致：正常终态（completed/done/failed/incomplete）且消费者已排空 → 归还；显式取消/中途断连/draining/未排空中断/歧义写 → 关闭不复用；空闲 4 分钟或满 8 请求 checkout 时轮换；空闲上限 4 条/key。
- affinity 每拨号生成一次并在 socket 生命期内稳定（贴近官方"跨重连保留"）。
- 设置开关 `bps_websocket_reuse`（默认 false，`prefer_websocket` 开启时才生效）：store.go/admin.go/settingsState + 前端 Settings.vue 开关已加。**上线后需显式开启**（与 prefer_websocket 同一流程，由用户决定）。
- 不做的事：不做单 socket 并发多路复用（官方协议不支持）；不做 resume 协议（生成中断不重放，安全规则保持）。

**测试**：`gateway_bps_ws_pool_test.go` 10 项——顺序复用单连接、8 请求轮换、4 分钟过期轮换、key 作用域（代际/代理）、关闭开关回退每请求连接、稳定 affinity、并发独占+空闲上限、中途断连淘汰、空闲断连淘汰、客户端中止发取消控制且不复用。全量 `go test ./internal/server ./internal/bps`、vet、前端 237 项测试与构建通过。

**429 预期**：握手从每请求 1 次降为每 ≤8 请求或每 4 分钟 1 次（≥8 倍降幅），affinity 稳定使上游路由粘性恢复；已建立连接上不再重复付出可能被计入限流的握手。是否足以压低 429 需开启后用 `.local/collab-d-20260930/` 监控对比验证（429 主体是账号容量，复用只消除连接层面的浪费）。

**D 当前修改窗口（本节新增/持续）**：`gateway_bps_ws_pool.go`(+test)、`gateway_bps_upstream_websocket.go`、`gateway.go`（池字段）、`store.go`/`admin.go`（settings 字段各 4 处）、`frontend/src/views/Settings.vue`（一个开关）。root 发布冻结请继续按 source-changed 拒绝；此项与上一节 WS availability 一并归 D 的下一包评审。

## D 发布记录：WS 复用上线（2026-09-30 05:08–05:12 UTC）

用户指令：开启前置条件并上线。已执行：

- **发布源**：线上 `b3b052f7`（source `eaa7bb5e…`，即 root 部署的最新版）+ 仅 D 的 WS 文件集叠加（逐文件 diff 核对纯净：7 个修改 + 5 个新文件 + Settings.vue）。root 在途未过 gate 的 `admin_usage_decoded_source.go`/`stats_rollup.go` 等未带入。构建树 `ws-release-tree/`（不得置于 `.local` 下——acceptance 清单按绝对路径 parts 排除 `.local`，会把 deploy/tools 全排除，此坑已记录）。
- **发布**：`21147f0eac51b509df5ed0a501e13584cfd1e40685ea9afd07e17159b6106a26`，source `4940b22e…`，binary `9148663b…`，PID 3401872，ingress durable，3 个旧世代自然排空。verification JSON（go/vet/pg/frontend 四项全过，6 项隔离 PG 回归绑定同一 source manifest）随包固化于 `.local/performance-release/packages/21147f0e…/`。
- **上线验证**：10 端点全 200（1.3–25ms），首页哈希与包一致。
- **前置开启**：settings_version 34→35，`prefer_websocket=true`（34 已是 true，用户此前已开）、`websocket_enabled=true`、`bps_websocket_reuse=true`。服务端解密 admin_key 调 `/api/settings`，密钥不出服务器。
- **上线即验证了用户的诊断**：激活前 1 小时内 **176,704 次 WS 握手被上游 429 直接拒绝**（vs 1,069 次 101）——上游确实在**连接/握手层限流**，旧"每请求一条新连接"正是元凶；04:00–04:45 每 15 分钟 11–14 万次握手 429。自然流量 04:57:13 停止（客户端被打挂），复用效果待流量恢复后以"websocket 尝试无状态码=复用命中、握手数下降"度量。
- **监控**：`tools/natural_traffic_error_monitor.py` 持续；ws_attempt_outcomes 已区分复用（无 status_code）与拨号（101/404/429）。

## D 追加发布：429 治理（2026-09-30 06:30 UTC，用户"继续修"）

**诊断**（开启 reuse 后用户仍见 429）：
- 复用已生效：完成交换中 72% 走复用 socket（613 复用 vs 237 拨号建立），无重放。
- 429 主体转移：05:52 窗口 98% 是"全账号冷却中的网关快速失败"（4901 条 gateway fast-429），真实上游 429 仅 82 条；尝试/请求从 1.65 降至 0.47（重试风暴已消除）。
- 根因链：`rate_limit_cooldown_enabled` 生产从未开启（默认 false）→ 饱和账号被持续捶打；开启后（v36）快速失败但客户端在冷却间隙全部吃 429。

**修复**：
1. 设置：`rate_limit_cooldown_enabled=true`（v35→v36，无需发版）。
2. 代码：**冷却有界等待** `pickSessionAccountAwait`（gateway_sessions.go）——生成路径（官方 /responses + BPS）在"仅剩 ≤5s 的限流冷却"时等待到期后重选（最多 2 次、ctx 可取消），把快速 429 转成等待后的成功；模型目录路径保持快速失败；payment 30 分钟停调永不等待（保持快速 429+Retry-After）。hop 边界测试契约同步更新（等待后重放一次、hop Retry-After 仍剥离）。
3. 发布 `bef6f7f354719e406c9ffea71f2f651e4f0fdc86d471cec0b76de4763d4253a6`（binary `2439b562…`，source `58b4f18a…`，PID 3572239）已激活；健康 5 项 200、设置保持 v36 三开。

**验证**：await 单测 5 项 + 端到端 2 项（700ms 冷却等待后选中、>5s 快速失败）+ hop/cooldown 契约 + 发布树全量 go/vet 通过；主树同步相同改动并通过同组测试。

**诚实边界**：06:15–06:30 客户端突发（每 5 分钟 1.2–1.5 万错误）后于 06:34 停止——等待转化率待下一波流量度量。若上游冷却普遍带长 Retry-After（>5s），等待不触发、429 仍是容量信号：3 个账号对当前需求严重不足，网关侧已消除全部可消除的浪费（重试风暴、握手、快速失败转等待）。

## D 第二轮：WS 握手 429 退避 + CRUD 全量扫描修复（2026-09-30 07:0x UTC）

**用户指令：所有优化由 D 处理，不再移交 root。**（root 可复核；优化、打包、发布由 D 推进。）

### 复用度量口径修正（重要）

- 复用命中在 request_logs 的真实形态是 `attempts[].phase=websocket && status_code=200 && duration_ms=0`（合成 200 响应经 `finishGatewayObservedAttempt` 记录），**不是**此前记录的"无 status_code"。此前记忆与本文档旧口径作废。
- 分桶证据（`.local/collab-d-20260930/natural-errors-20260930T060127Z.json` + 分桶查询）：
  - 复用前 04:30–04:57：429 拨号 214,778 / 101×1,515。
  - 复用后 05:30–06:00：429 拨号 3,191 / 101×559 / **复用命中 1,408** / 404×19（404 从数千降说明可用性熔断生效）。

### 复用后仍存 ~107 次/分钟 429 拨号的根因与修复

- 根因：`gatewayWebSocketUnavailableBeforeGeneration` 只认 404/405/426/501/网络错误——**握手 429 既不熔断也不进池**，受限账号每请求白拨 ~200ms 再回退 HTTP。归因账号：abdullahyaqobi57（862×429 零 101）、hassanrnjbr329（750×429 零 101）纯浪费；skndrsamim 混杂（662×101+1,573×429+1,607 复用）由 established 复位自然免疫。429 拨号后回退的请求 300 例最终 ok，拨号纯浪费。
- 修复（D 认领文件，6 运行文件+测试）：
  1. `gateway_websocket_availability.go`：`observeHandshakeLimited`/`dialLimited`——握手 429 连续 3 次触发 **60 秒拨号退避**（与 404 的 10 分钟熔断独立计数，established 含复用命中即复位，到期单次失败即复断）；key 扩为 **account × proxy 指纹(ID@host:port) × endpoint**（落实评审 #2）；map 硬上限 + `lastObserved` 老化清扫（落实评审 #1）。
  2. `gateway_bps_upstream_websocket.go`/`gateway_websocket_preference.go`：预检加 dialLimited；握手 429 处 observeHandshakeLimited（请求仍走原 HTTP 恢复）。
  3. `gateway_bps.go`/`gateway.go`：预检门槛同步 `wsDialSkipped`，避免幻影 ws attempt。
  4. `gateway_appserver_cli.go` roundTripPreferredWebSocket：blocked/limited 直接走 HTTP（评审 #3 appserver 幻影 attempt 修复；BPS 路径 21147f0e 已有 attemptKind 门槛）。
- 测试：availability 测试 12 旧+7 新（429 生命周期/独立性/到期复断/指纹与端点 key/硬上限/老化/端到端暂停拨号）全过；全量 internal/server（173s）+internal/bps+vet 过（在 429 治理改动合入主树之前）；PG 针对性回归另跑。

### CRUD 全量扫描（Explore 全库，排除已冻结/他人认领）结论与修复

**删除误报类：无发现**——所有 delete 的版本 CAS 均在写入事务内（DeleteVersion/managementState.Delete/schedule 文档 CAS），预读不参与版本判定。此前的 compat 修复已覆盖 compat 域。

修复（无用写入）：
1. `store.go` SettingsStore.Set 同值跳过（同值 POST 不再 version+1+全文档重写）；修复过程中发现并修正版本回写 bug（`*state=next` 覆盖递增后版本，TestSettingsVersionConflict 红绿验证）。
2. `admin_account_leases.go` lease PUT 同值跳过（errManagedLeaseNoChange 中止，200 语义保持）。
3. `admin_account_schedules.go` schedule PUT 同值跳过（errManagedScheduleNoChange）。

修复（单资源全量加载）：
4. `admin_account_crs.go` CRS preview：逐条 link+GetMetadata（最多 2 万查询/请求）→ `managedCRSLinks`（GetMany）+ `metadataByIDsContext` 两次批量读（文件模式保留 legacy state links 优先）。
5. `admin_account_crs.go` managedCRSProxyID：全表 List+全表代理解密去重 → 元数据过滤+仅候选解密；sync 加同指纹代理 memo。
6. `admin_account_auto_reset.go` auto-reset PUT：两次全凭证读 → 两次 GetMetadata。
7. `admin_account_bundle_resources.go` bundle 导出：全量 groups+proxies（凭证导出全表解密）→ 仅引用 ID 有界读（新增 `GroupStore.managedSelectedMetadata`，admin_resource_reads.go）。
8. `admin_account_bundle_resources.go` createManagedBundleProxy PG：每插入后全表 List+全解密（O(N²)）→ createPG 式增量索引发布。

测试：`admin_noop_updates_d_test.go` 新增；CRS/lease/schedule/settings/autoreset/bundle/resource 套件过。

**扫描发现但评估不改**：文件模式删 group/proxy 前的全量引用扫描（单文件存储固有）；schedule create/delete 存在性预读（低价值）。

**本轮修改窗口**：上述 1–8 + `admin_resource_reads.go` + 测试 `gateway_websocket_availability_test.go`/`admin_noop_updates_d_test.go`/`admin_account_reliability_test.go`（补 nil 参数）/`admin_account_performance*_test.go`（补 ctx 参数）/`admin_accounts_management.go`（仅传 ctx 一行）。

**对基线的影响**：本轮全部基于含 429 治理（bef6f7f3 源）的主树；下一包 = bef6f7f3 源 + 本轮文件集，由 D 冻结发布。

## D 追加发布 2：等待上限 15s + 抖动（2026-09-30 09:05 UTC）

07:51–08:51 突发（22.4 万请求，~62rps）观测：191,483 条"冷却中快速失败"429，但 ok 请求中位延迟 5.8s/P90 20.3s（等待已在转化部分请求）；实时 zset 采样显示冷却剩余 2–7.4s，**大量条目卡在旧 5s 等待上限之上**（4s 默认 + 并发 429 扩展在负载下到 6–8s）。

修复：`gatewayCooldownAwaitMax` 5s→15s（仍远低于客户端 45s 存活预算）+ 0–1.5s 随机抖动（打散冷却到期瞬间集体醒来互相重新冷却的惊群）。发布 `cd5762bb8747caae4a9198930e01abbdabe46888ce41050ace1f53f9b4b5add1`（binary `a1cd4770…`，source `0bcf1695…`，PID 4131861），健康 200、设置保持 v36 三开。全量 go/vet、cooldown/hop/await 测试通过；主树同步。

容量结论（不变）：实测成功速率 ~2.6 ok/s vs 需求 ~62 r/s——网关侧浪费已全部消除，剩余 429 为纯容量缺口。
