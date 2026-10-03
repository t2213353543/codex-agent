# 增删改查与自然流量错误协作

本文件供共同工作的独立模型平等协作；没有主副关系。以源码、实际进程和证据为准，先在此声明正在修改的文件，避免相互覆盖。共享目录不是 Git 仓库，发布必须冻结产物并绑定哈希。

## 当前任务

1. 其他增删改查：单资源全量加载、删除错误误报、无用写入；复核已上线修复并继续查漏。
2. WS：持续观察自然流量，区分入站 WS、上游 WS、101、握手不可用后的 HTTP 回退、上游失败和客户端取消；修复有证据的网关缺陷。
3. HTTP 流/非流式：持续观察自然流量，按 error_source/error_phase/终态分类，不能把 HTTP200 当作成功终态。

## 用户约束

- 少量真实模型验证可以执行；不允许为测试反复切换生产全局 WS。
- 用户随后明确允许“少量验证没问题后再打开”：已完成真实 SSE101+终态、created后cancel控制、JSON在404后HTTP/2一次回退完整成功；2026-09-30 03:51附近开启 `prefer_websocket=true`，settings_version32。后续自然观测不改开关。
- 不重放已经发送的生成；不删除用户账号/日志，不改变用户分组、配额或停用设置来让测试通过。
- 不绕过账号/Key授权或加密历史身份绑定来减少错误。

## 当前生产状态（必须重新读现场核对）

- 项目：Codex Agent，主机43.225.196.170，公开18879→稳定ingress18878→私有世代。
- 已上线 release：`b3b052f7038304caca8185f2f35943381e66d3253bb8925eb4de1ca073e04f13`。
- 已上线 binary SHA：`dfee3451acea69b9608c74b5a5b9e7397866a91c073e5ea30df78288662f29d0`。
- `/var/lib/codex-agent-release/active-runtime.json` 是运行真值，`/var/lib/codex-agent-ingress/target.json` 是入口真值。
- 生产schema000–024共25项已应用。024精简统计投影已分232批回填完成，remaining0，其读取代码已随5b415发布。
- 独立PG容器`codex-agent-postgres`（25433/codex_agent）已无重启调整为4CPU/2GB，compose在`/opt/codex-agent-db/compose.json`，runtime连接上限32/max_connections40。
- SSH与脱敏调用复用`tools/integration_env.py`；凭据只在服务器内存处理，不输出或写文档。

## 已上线及待发布差异

已上线：021解除账号删除导致历史日志级联重写；20k/269MB历史删除3.58ms。WS typed-nil panic已修，BPS/官方Responses优先WS及发送前不可用回退；402账号级30分钟停调/持久状态/发送前复检；单Sub2资源查询按匹配行返回；删除status保真；删无关group不重写gateway_keys。CRUD真实PG已过。

旧候选 `6914bc9d…` 已撤下，快照计划差异已定位并修复为单请求generic计划，随5b415上线。当前下一包仅已通过的mixed资源导入优化与监控分类工具；冻结目录 `.local/mixed-import-20260930/source`。窄统计查询与serial预算因生产/70万反证未发布。D的WS availability仍为独立在途变更，不随本包。

## 当前文件修改窗口

- 本会话 `/root`：`tools/performance_rollout.py`、`tools/performance_hotfix_verify.py`、发布验证及此文档；管理统计候选收尾，实际切换前在此写状态。
- 子任务 `/root/route_inventory_perf`：`admin_usage_*`统计与options、`stats_rollup*`、`admin_usage_analytics*`、024迁移（已经上线后不可再修改其checksum）。正在只读抓snapshot慢查询差异；业务文件当前冻结。
- 子任务 `/root/resource_pipeline_extreme`：已认领 `gateway_bps_natural_error_diagnostic.go` 与对应测试，以及 `gateway_bps.go` 的 `OnUpstreamTerminal` 局部诊断 hook；默认关闭、限频、脱敏，只为自然未知失败取证，不改授权/重试/WS全局设置。
- 子任务 `/root/exports_sql_opt`：HTTP SSE/JSON自然流量只读分类与复现；尚未认领新的运行文件。
- 新加入的独立模型：请在此追加自己的标识、认领文件和证据路径；可独立检查CRUD遗漏或认领一类错误修复。发布/迁移/全局设置变更前先核对当前锁与运行身份，避免两个操作者同时切换。
- 独立会话 D 已认领 `admin_compat_references.go`、`admin_compat_cpa.go`、`admin_compat_store.go` 及 `admin_compat_bounded_crud_test.go`，详见 `COLLAB-D-CRUD-TRAFFIC-20260930.md`。root不覆盖这些文件；已反馈文件模式DB=nil回归panic，请D修复并冻结。

## 证据入口

- `.local/hotfix-20260930/combined-pg/report.json`：WS/CRUD/402联合回归。
- `.local/hotfix-20260930/payment-final-pg/report.json`：402跨代际和兼容可调度状态。
- `.local/hotfix-20260930/analytics-pg2/report.json`：17项投影/回填/并发/删除/TRUNCATE/统计回归。
- `.local/hotfix-20260930/analytics-snapshot-http-capture.json`：真实HTTP15s失败，SQL前1023bytes采样。
- `.local/hotfix-20260930/analytics-snapshot-lateral-compare.json`：只读精确SQL plans，变更后资源下2s左右；不能替代HTTP失败。
- `.local/bps-ws-preference/ws-preference-validation-summary.json`：最终少量真实WS/取消/安全回退证据。
- `.local/hotfix-20260930/evidence/ws-enabled-after-validation.json`：用户条件满足后的全局开启记录。

## 协作更新

- 2026-09-30：按用户新的三项任务启动自然流量分路观测；监控只读，新增模型流量为0。

### WS 自然流量取证（resource_pipeline_extreme，2026-09-30 04:01–04:05）

- 只读确认 active binary 为 `32ebbb315c581366d75c22434eb96a34679d2ddabcbd051b9a095a3b2eca36f0`，与 registry 一致。最近 15 分钟查询以时间索引限 5000 行，已触顶，不能称覆盖完整窗口。
- 04:00:20 的 5000 行中：3402 ok、1598 error、0 aborted；2475 行有真实 WS101，1276 行 completed、1199 行失败。其中 1110 行是 WS101 后 `response.failed`，持久日志未保留具体 code。原始统计在 `.local/bps-ws-preference/natural-watch/20260929T200022Z.json`。
- 04:01:36 的开关后最新 5000 行细分：账号哈希 `f97ddfffe134db03f7973b6af0111680c46107df3bb18b7c1e587fdb62afd50b` 有 2009 条 WS101 后未知终态失败，平均 577ms，同时有 973 条成功。04:00 一分钟出现 1431 条未知失败；不能归因为客户端取消或全部账号不可用。证据 `.local/bps-ws-preference/natural-watch/detail-20260929T2004.json`。
- 按明确授权，对该哈希对应、仍启用且未过期的账号仅发 1 次隔离 SSE 短请求，运行文件逐 SHA 与 deployed 2a 一致；真实 101、create 帧 1、完整 `response.completed`、2.211s、无 HTTP 回退，未复现自然错误。生产 `prefer_websocket=true/settings_version=32` 前后不变。证据 `.local/bps-ws-preference/natural-failure-capture-source/.local/reliability-modes/live/20260929T200232.012736Z/live.log`，test binary `4d51763f8d66f2e72e72c7d57b2cd8026873b910cf8d888123a110cd5569b891`。
- 结论：未知终态根因仍缺原始错误分类。不得用短请求成功覆盖自然失败，也不得以未知错误触发生成重放。下一步优先对自然失败做有界、脱敏、限频的 terminal code/type 采证。当前 agent 未修改 WS 运行文件，不再主动发送模型请求；持续监控统一复用 root 的 `tools/natural_error_monitor.py`。
- 观察中“同一记录有 WS101 和后续 HTTP”不单独证明不安全重放：记录可包含附件、显式上游 429 后跨账号尝试等；须结合有序 attempts 和发送/终态边界逐例判断。

### 自然失败私有采样补丁（resource_pipeline_extreme）

- `gateway_bps_natural_error_diagnostic.go` 与测试已完成；`gateway_bps.go` 只在 `OnUpstreamTerminal` 添加旁路观察，HTTP/SSE/JSON/WS 共用。为避免不同分类器白名单隐藏 camelCase 错误，开关启用时采全部失败终态，不采成功。
- 仅 `CODEX_NATURAL_FAILURE_DIAGNOSTICS=1` 启用。文件模式目录为 `Store.Path` 父目录下 `diagnostics`；生产 PG 模式使用 systemd `STATE_DIRECTORY/diagnostics`，不回退到 release 当前目录。目录 0700，固定 `natural-failure-00.json` 至 `31.json`，文件 0600。
- 每账号 SHA256 每 60 秒最多 1 条；内存 deadline 表最多 32 条、已在窗口内 O(1) 返回。存储失败也限频。跨进程非阻塞的 sibling lock 序列化磁盘检查/轮转；全部槽位仍在 60 秒窗口时不驱逐。崩溃遗留 `diagnostics.natural-failure-lock` 会安全停止采样，需操作者确认无采样进程后移除该目录。
- 只保留捕获时间、账号 SHA、事件 type、status 与 error.code/type/param/status/message；字符串最多 400 个字符。原 AT、刷新后请求 AT、RT、客户端 key、账号标识及常见凭据格式均替换；不写原事件、请求内容、input/output 或 headers。
- 四项专项测试及相邻 BPS/WS/终态语义测试通过（相邻合计 0.678s），`go vet ./internal/server` 通过。该 agent 未部署、未改生产开关、未增加模型请求。

### HTTP 与 CRUD 复核

- `crud-production-validation.json` 证明线上3个CRUD修复文件与release source hash一致；生产单group/proxy GET分别5.168ms/4.169ms，均200。错误状态保真、无关keys不写由 `combined-pg`/本地全测试证明，不删除生产资源做试验。
- HTTP只读结果：51个missing-bound错误均不透明/文件历史绑定的owner已删除或替换，按现有身份语义拒绝正确；不应放宽授权跨账号迁移。
- 5个上游400明确缺少 `input[5].name`，没有证据表明网关删字段；`http-natural-analysis.json` 含对应本地复现命令。
- 根会话正运行 `python tools/natural_error_monitor.py --watch-seconds 600`，每30秒只读最近15分钟最多5000条，保存 `.local/natural-errors-20260930/`。窗口触顶时只代表最近5000条；无推理、无账号或设置写入。
- `/root/resource_pipeline_extreme` 新认领 `gateway_bps_natural_error_diagnostic.go`/测试，以及 `gateway_bps.go` 的失败终态被动采样hook。默认关闭、每账号60秒最多1条、固定32个私有脱敏文件，不记录请求输入/headers。用于定位集中未知上游失败；其他模型暂避开此hook区域。

### 04:15 故障缓解与待发补丁

- 最近5000自然流量的未知WS终态失败增至约4500条，root已在UTC20:14:55（日志ID261292之后）临时关闭WS优先，当前settings_version33、prefer_websocket=false、websocket_enabled=true。这是生产故障缓解，不是测试开关。短期HTTP错误主要明确上游429；没有panic/aborted爆发。
- 精确camelCase结构化 `rateLimitExceeded` / `usageLimitExceeded` 原未被终态冷却/日志分类识别，3文件最小修复已通过synthetic红绿测试，不声称已证明自然未知错误就是这两个code。
- 快照超时已证实为新preview_id尚未统计的custom估计错误；全新快照force_custom超过30s，force_generic约1.75s。单请求READ ONLY+SET LOCAL force_generic修复6项PG通过，不改全局数据库配置。

### 04:43 联合修复已上线（请同伴以此新版本为基线）

- active release：`5b415cd1728c7839758a49f85ef6ae1d1d377007ef3b8d01417dca35e41355ff`，binary `d15edd01962daa206e34b12a173938d78e9dddd1bf58d5157e3b7da0261c1b4c`，PID645802，私有18891；仍需实时registry复核。
- 包含D的3个CRUD运行文件及root补的文件模式nil保护，5个D新PG回归及全量Go172.324s通过；不含D新开发中的`gateway_websocket_availability.go`与其后网关接入。D的工作保留在共享树，未覆盖。
- 包含402状态/代际与compat显示、精简统计表（024已应用并完整回填）、所有class统计只读事务generic计划、camelCase quota别名、限频自然失败私有诊断。
- fresh HTTP candidate验证9个形态全部200：普通stats约68ms，all/error/class snapshot分别约4.14s/2.52s/3.58s，currentclass3.58s。不再15s500，但未达到全部<1s目标。
- `CODEX_NATURAL_FAILURE_DIAGNOSTICS=1` 在该世代启用，STATE_DIRECTORY已核验；当前HTTP自然错误主要头部429，近2分钟没有失败终态，因此私有终态样本暂为0（不是“没有错误”）。
- 当前设置保持 `prefer_websocket=false`，`rate_limit_cooldown_enabled=false`；root已向用户询问是否启用429冷却，未收到答复前不视作同意。当前5000条/60s约4千条upstream429，用户冷却关闭时多账号fallback会扩大到约219上游attempt/s，不是共享Redis丢状态。
- 包路径由 `.local/hotfix-20260930/natural-final-release-path.txt` 指向；冻结源码在 `.local/hotfix-20260930/natural-release-source/`，避免把同伴在途变更混入部署。

### 持续自然观测已安装

- `codex-agent-natural-monitor.timer` 已启用，每分钟执行独立只读服务，未重启应用/数据库；原observer不改。
- 脚本 `/opt/codex-agent-integration/natural-monitor/observe.py`，最新与30个固定滚动报告在 `/var/lib/codex-agent-natural-monitor/`，0600私有。查询只读15分钟内最多5000行，10sSQL期限；模型请求0，业务写入0，CPUQuota20%/MemoryMax128M。
- 输出区分客户端request_type与上游attempt transport/101/不可用回退，包含status、终态、source/phase、已识别code、panic与持久化错误数量；不存原始请求或令牌。
- 定时器只持续取证，**不自动修改账号或开关，也不宣称自动修复所有错误**。两模型基于共同证据决定修复，再冻结测试与发布。部署证据 `.local/natural-errors-20260930/persistent-monitor.json`。

### D 状态（2026-09-30 04:42）

- CRUD 四文件（admin_compat_references/cpa/store/admin_compat.go + 测试）已完成并冻结；root 的 compatAuthIndexTargets DBnil 回退保留。入口审计结论见 COLLAB-D 文档：root 补丁后文件模式全部安全。
- D 新增 WS 账号可用性熔断（评审四点已落实：account×proxy 维度 key、1024 上限惰性清扫、到期探测语义、并发/复位测试 12 项通过）。仍在修改的运行文件清单见 COLLAB-D 回复第 4 条；均避开 resource_pipeline_extreme 的诊断 hook 区域与 admin_usage_*/stats_rollup*。
- D 侧自然流量证据（18:34Z 起）：`.local/collab-d-20260930/natural-errors-*.json`。要点：WS 404 为账号级确定性（ghaniafghan427 404:2998/101:58；jmshydrhmty43 404:2808/101:78；habdullahsas56 101:20438），熔断可消除每次回退前 ~200ms 浪费拨号；HTTP 错误主体为上游 429/response.failed 容量类，绑定失效 503 与 oauth-invalid 账号删除链路自洽，structured-mixed 为既有刻意校验（34/175k）。
- 发布：同意 root 从 peer-crud-pg/source 冻结本包；WS availability 待下一包评审。

### 后续文件认领（root团队，继续轮次）

- `/root/exports_sql_opt`：`store_key_snapshot.go`，`admin.go` 仅 overview/groups 的APIKey计数读取，新增 `store_key_management_test.go`。保留PG权威版本验证，消除只求计数时复制全部密钥和重建索引；不改D兼容文件。
- `/root`：`gateway_bps_natural_error_diagnostic.go` 与专用测试、`gateway_bps.go` 仅 bpsWriteUpstreamError 旁路采样（不改D的WS选择逻辑）。增加自然HTTP非200错误的限频脱敏错误码及Retry-After诊断；不改变429开关和请求重试语义。
- `/root/resource_pipeline_extreme`：只读复核D可用性熔断，结果写D协作文件，不直接编辑D认领文件。
- `/root/route_inventory_perf`：`admin_usage_cleanup.go` 的过期临时成员清理分批化，新增 `admin_usage_cleanup_prune_test.go`；不改快照语义或schema，保留活动清理任务。

### 05:15 数据库周期性 OOM 已止血

- PostgreSQL 日志确定 OOM backend 的 SQL 为旧 observer 的 `WITH source AS (SELECT l.id, ...`，每分钟约 05:xx:58 被杀，连带数据库进入恢复；并非新应用 panic。
- root 于 05:15:49 停止 `codex-agent-observer.timer`/service，保留新 bounded `codex-agent-natural-monitor.timer`。route_inventory_perf 认领旧 `tools/codex_agent_live_monitor.py` 查询修复，resource_pipeline_extreme 独立观察数据库稳定性。请 D 暂不启动旧 observer、不调整全局 WS。
- 本轮冻结源的全量 Go 已通过 157.229s，vet 通过；发布先等数据库稳定核验。exports_sql_opt 整理绑定冻结源的 PG/功能证据，root 统一执行切换，避免并发候选冲突。

### 05:24 发布窗口已占用（root）

- 数据库已完成 >2 周期只读稳定核验：旧 monitor 停止后 oom_kill 无增长、无新恢复、近 2 分钟应用持久化错误 0。
- 本轮包 `7cbcae38c19407ab3493ebda363cf017fbed54fc81ff812112567bf318f8f9be`，binary `3a309d9c3a8925df2bbba04d7809c71f7fd75e337ba260dfa6b21c5f5e76249c`，source `f73b2c14eedef5f795d64f0177eb9cbcc600fb266192a4450c4d478a15c3e308`。上传→stage→候选验证→activate 由 root 顺序执行；其他协作者此窗口请勿创建/激活候选。
- 本包仅 Key 计数、分批清理、HTTP 被动头部诊断。无新 schema。旧 observer 修复由脚本独立发布，有界 SQL 验证通过前不启动 timer。
- exports_sql_opt 新认领主树 store.go/store_pg.go/store_noop_update_test.go 做 native 同值更新优化；该在途工作不包含在本包。

### 05:30 本轮上线完成

- `7cbcae38…` active PID893975/private18890，入口 durable=true；Key计数/独立分批临时清理/HTTP头部诊断已生效。12项健康+UI+管理读取全部200，管理读取74–750ms，panic与持久化错误0。
- 候选复杂快照9形态均200，但 all snapshot stats5.91s/errors3.32s/rate-limit4.71s，数据增长到约46万，仍未达总体性能目标。
- 自然HTTP诊断已捕获多账号 HTTP/2.0 429，type=server_error，message=429: Rate limit exceeded，无code/Retry-After。原终态samples另有tokens限流和context_length_exceeded；不能据此倒推以前WS未知簇。
- 新natural定时器持续读最新5000；旧observer仍停，正完成有界修复SQL验收。429冷却与WS全局设置保持不变。

### 05:35 旧 observer 修复已部署恢复

- 最终工具SHA `f9835fe41ebd69baca8832d98c83c5358586c16e471118393d4c8e92af2f2bd7`，10项真实SQL合成回归通过；6002宽错误样本仅取5000，数据库backend峰值约89MiB。
- 生产单次只读验收1.445s，PG cgroup anon由289.6MB至峰值301.9MB后回落，oom_kill始终26；scope和new_errors_complete=false正确标记。
- 旧observer timer现每5分钟、已05:34:28成功一轮、下次05:39:28；新natural-monitor仍每分钟。未改业务设置或重启应用。证据 performance-continuation/evidence/observer-production-once.json 与observer-restored.json。
- native同值更新下一包暂不发布：独立review发现跳过List后旧缓存可能未刷新，exports_sql_opt与resource_pipeline_extreme正协作复现/修正；只修DB无用写入不能造成运行缓存过期。

### 05:48 唯一隔离 WS 请求发现实际漏分类

- 当前7cb部署源码精确冻结，生产prefer=false/version33前后未变。仅/sse一请求：握手101@285ms，response.create帧1，总341ms，上游code=`basispoints_route_rate_limit_exceeded`，message=`Rate limit exceeded`，无Retry-After。HTTP generation/attempt0，cancel0，无重放。
- 该精确code目前不在已知终态/冷却/最终日志分类中，FinalUpstreamErrorCode为空。这证明当下route限流漏分类，不证明旧所有未知失败同因。
- resource_pipeline_extreme 认领 internal/bps/stream.go、gateway_cooldown.go、gateway_appserver_failover.go 与新专用测试、observer精确code白名单；不碰D网关WSavailability文件，不改全局开关或持久账号失效状态。
- native no-op最终全Go168.829s/PG3通过；包d9b49e已本地构建但尚未上传/候选/切换，等上述实际错误最小修复合并后生成新包，旧包不激活。

### 06:03 路由限流修复已冻结，联合回归中

- `basispoints_route_rate_limit_exceeded` 精确识别为逻辑429；wire/日志安全原码保留，final/attempt canonical遵既有rateLimitExceeded。WS observed101保持、JSON429、SSE已提交200不回写；显式status/status_code保留。
- 18个上游HTTP/WS × JSON/SSE/WS客户端 × 三种terminal envelope组合通过；opaque/visible-output双账号不重放（WS帧1，HTTP0）、默认4s可选model cooldown、不改持久账号状态均已验证。
- 6运行+2测试SHA在 `performance-continuation/evidence/route-rate-limit-fix.json`。与native no-op3文件合并后全Go/vet进行；无D在途gateway_bps.go/availability修改。
- 旧observer白名单工具已原子更新为 `fd0060190f1cf2c812b7c5a22088abfba742bbd5748a5a8e95adff37445ccbba`，SQL与有界采样无变化，cadence仍5min；新增4Python边界测试通过。

### 06:05 root占用联合发布窗口

- 真实WS路由限流与native no-op、monitor code联合包 `9042e53a21fcc12634d43f1f0017a688cd6d70e0cd2f851c699bf290cecb1061`，binary `a8237bce1dbd2906d9e8403e17b498b7cf056d493482a817b010835e158e3152`，source `96f9f23e4e4608fe6db6745db2c07c4db706670705740f549308b813559ebb27`。全Go163.961s、vet通过。正在上传/stage/候选12读接口/切入口，协作者请勿同时创建或激活候选。
- 本地中间包d9b49e不激活。DB schema仍25项无新migration。

### 06:08 联合版本9042已上线，发布窗口释放

- release `9042e53a21fcc12634d43f1f0017a688cd6d70e0cd2f851c699bf290cecb1061`，binary `a8237bce1dbd2906d9e8403e17b498b7cf056d493482a817b010835e158e3152`，PID1134890/private18892；candidate/pending均null，入口切换durable。
- native Group/Proxy/Key同值跳过资源写+版本递增，保留审计/CAS；版本0规范、跨实例缓存单项发布、不倒退新缓存/Key.LastUsed、密码未改不重复加密均通过。
- 精确BPSroute quota分类与JSON/WS状态修复、attempt逻辑429/observed101已上线；未合入D的availability在途补丁。
- 全Go163.961s/vet/12top PG3/18协议组合与no-replay通过；候选和上线后12项检查全200，上线服务器loopback管理读1.68–68.93ms。没有再次做宽snapshot全扫，其最后5.9s问题继续排期。
- postdeploy检查：panic0/persistence0/PG40min新OOM0；settings_version33、prefer_websocket=false、websocket_enabled=true、rate_limit_cooldown_enabled=false，诊断开关启用。旧observer5min+natural1min定时器均active。
- 发布路径 `.local/crud-noop-20260930/release-path.txt`；最终源在同目录source，验证证据source/.local/verification。下轮优先 `performance-continuation/evidence/snapshot-next-review.md` 的窄join/fallback边界；WS需继续自然错误分类与Davailability复核，不能把本次一个route限流样本等同全部旧未知根因。

### 下一性能轮次（06:20）

- 已重新核对 /proc/exe、active runtime 均9042/a823，无候选；上轮属已部署且验证的进展，目标保持active。
- route_inventory_perf认领 analytics.go/stats_rollup.go/stats_rollup_class.go及2测试，窄join/惰性PK回退正在独立PG100k同数据old/new对照；保留全部快照与缺投影语义。
- exports_sql_opt认领admin_account_bundle_resources_pipeline.go及专用测试，修混入1条invalid/mapped/existing导致1000proxy整批回退：baseline真实HTTP2.63–2.80s、DBacquire约2001次。两重PG实验先后执行，无资源竞争。
- root发布冻结基线 `.local/snapshot-narrow-20260930/source`；完整Go/vet正验证analytics patch，不覆盖D在途WS文件。root新增 tools/performance_coverage.py 对127路由和184报告索引，区分9current样本/38历史/80缺对应HTTP测量，不冒称全域达标。
- 新自然巡检5000样本无WS/无panic/无persist error，剩余有明确429容量/owner绑定/请求与structured校验；没有证据放宽拒绝。仅monitor typed分类补齐gateway_capacity/structured_validation，工具SHA51dce6ff已独立部署，SQL/频率/业务设置无变。

### root占用窄查询发布窗口

- 已准备release `78bbc78bcd3371a52b669417a02f341d407891c9248705756804f718983b390a`，binary `98dbe188b04b98f162ffdbf70d8d0b046e37559df8ef86316460bf10e156fd4b`，source `49f90d625bc1459707cdcca05b37e5861142be464b821b318386f2d9007bba3e`。只含3个窄查询运行文件+2测试与已独立部署monitor分类、coverage工具，不含exports在途mixed-import或D的WSavailability。
- 16PG+全Go163.861s/vet通过；root将上传、stage、一次freshsnapshot9形态验证后激活。其他协作者请勿同时创建/激活候选。

### 06:32 候选78bbc验证失败，已撤下

- 未激活。production candidate9shape：all649379 snapshotstats6.209s/errors5307825.120s/rate_snapshot500@1.031s/currentrate10.613s。PG报3个parallel worker DSM段申请16MB失败，容器/dev/shm总64MB。没有PG重启/OOM新证据。
- root已在发布锁内验证候选无入口流量/连接，drain生命周期active_handlers/refresh/persistence全0后移入draining并通过既有retire控制器停止；candidate=null、active仍9042/a823。失败证据保留 `snapshot-narrow/evidence/candidate-failure.json` / shared-memory-facts.json / withdraw-failed-candidate.json。
- route_inventory_perf优先回到查询，分析真实数据并行plan与100k隔离分布差异，修单事务并行预算并复测；snapshot成员压缩实验暂停。exports继续独立mixed导入实验，无新发布抢占。不可将隔离SQL提速当作生产已验证。

### 07:05 70万代表性测试反证，查询不发布

- 700000记录、560000 rate_limit/13类别分布已成功构造，new narrow_serial当前统计15.041s SQLSTATE57014，仅24行部分结果，不通过。没有为过测试提高应用timeout或生产shared memory；该方案保持未发布。新5项周边PG功能通过不能代替此性能失败。
- exactscope证据 `.local/snapshot-narrow-20260930/serial-dense-13class-pg`，隔离fixture自动drop；下一实验改为保留可复用隔离fixture并先100k验证enable_parallel_hash=off保留scan并行。只有在代表性数据通过后才重新安排查询候选。
- root单独提取已证实mixed导入3files并从9042干净复制，发布源 `.local/mixed-import-20260930/source`，不含任何narrow/scopedstats改动；全Go/vet进行，exports负责该独立包证据。主工作树的在途query代码继续保留，不覆盖。

### root独立mixed发布窗口占用

- release `b63165848cd18498bf3492faa76e826fed958cf931bcae2688d499a2b63d8889`，binary `cf20f6190a0f1ec87259665b5db4b6f796993c9ae43fe9ee872823faabe342d1`。source `601d0b8eeba1ce12d45ba94fffe2aca0d331bc68b58093eceee88d8e0f317b4f`精确9042+7files，唯一app runtime delta为mixed resource pipeline。stats/admin_usage/migrations均同线上，失败优化完全排除。
- 17PG/12HTTP/216+4语义review/全Go185.362s/vet通过；root正在upload→stage→12只读接口→activate。其他协作者此时不要创建/激活候选。

### 07:15 mixed导入独立版本上线，发布窗口释放

- active b63165848cd18498bf3492faa76e826fed958cf931bcae2688d499a2b63d8889 / binary cf20f6190a0f1ec87259665b5db4b6f796993c9ae43fe9ee872823faabe342d1，PID1534723/private18891，candidate/pending=null。
- 17PG、12真实隔离HTTP、216+4独立语义检查、全Go185.362s/vet通过。混合1000导入2.63–2.80s→142–156ms；10000仍1.43–1.93s，未SLO达标。上线后12读接口全200，管理2.44–94.69ms。
- postdeploy panic0/persistence0/PG40min新kill0，schema25；设置version33 preferWS=false/wsenabled=true/429cooldown=false均不变，monitor51分类正常/自然timeractive。
- 失败统计query未发布。所有stats/admin_usage应用运行文件与9042字节一致。主树保留实验narrow/scopedstats文件，**不能直接从共享主树打包上线**；当前生产源由 `.local/mixed-import-20260930/release-path.txt` 指定包及同目录source为准。
- 新反证：100k13class/80k匹配时narrow各模式无实际Gather且decoded1.11–1.33s，oldwide在Gather内并行decode0.33–0.36s。related scalar fallback限制主解析并行；只禁parallelhash未带来明显收益。下一轮先用disjoint UNION ALL分离present projection普通join和missing projection fallback，再同数据验证，并保留700k失败门禁。

### 本轮最终实验状态与下轮入口

- 100k13class retained fixture 的privateHash矩阵已完成，无显著收益；保留数据便于下轮不重seed（具体schema/重用命令见routeagent证据）。当前主树查询实验**不可直接上线**，生产仍以mixed-import/source为真值。
- snapshot成员分块原型100k已PASS6.8s整套，无生产触碰且实验schema已清理。capture packed321ms vsrow499ms，WAL363KB vs21.9MB；batchjoin93.91ms vs94.20ms无提速，语义/并发PASS。仅设计证据，不是应用集成或10M验证。tools/performance_snapshot_members_probe.py / docs/SNAPSHOT-MEMBER-PACKING-PROBE-2026-09-30.md。
- 覆盖索引标签最终澄清：12current binary读样本，78已有测量但需逐项source/dependency绑定，40缺相应HTTP测量；不同sourcehash可能因harness/清单范围不同，不直接等于旧实现。root已将coverage工具标签改为source_binding_pending，发布冻结源未改。

### 新继续轮次：方案验证与备份引用优化

- 上轮b631部署与representative反证属progress，继续全目标，不标记完成。当前root认领admin_accounts_management.go仅bundle import引用预读、新admin_account_bundle_bindings.go/test：PG仅查询实际映射后IDs，不导入无关分组/代理目录；文件模式原行为保留。冻结源 `.local/bundle-bindings-20260930/source` 基于b631，只含root3files+coverage标签工具，没有失败stats实验。
- routeagent继续present/missing disjoint UNION，named PREPARE下100k无收益；下一验证把typeddecode放各自分支，未部署、未扩700k。
- exports10k真实phase证明DB写+commit+audit占89–91%，不关闭fsync/synchronous_commit、不改逐项事务；本地准备3个剩余HTTP写路由测试。
- resource公平capture6轮100k：exact原始INSERT中位411.4ms→packed217.8ms（约1.9x），fresh未ANALYZE批量读取反而退化，尚不能集成。仅后续1M边界试验，不改schema或production。
- integration排程：route88972已terminal→root40439 bindingPG→resource1M→exports剩余写路由→route typeddecode；按实际handle释放，禁止并行性能干扰。

### root备份引用小包发布窗口

- release `ad7795e64ef74bc1d03d92a95bcf87c5e77d7f11165552846975e755577f152e`，binary `4643210e6c6ef51c0fb2988d2d465fc7906e2d3a7f6d7c03498ed2c0f9dd562d`，source `85b2ee0d5ac0582b647853582d26fb14af7aad1cdfc5ab8efd5666c4e971dc6f`。仅bundle实际引用读取2runtime+1test与coverage标签工具；统计/WS/资源pipeline均当前b631，不混typed/UNION实验。
- 全Go191.756s/vet/6PG+9真实HTTP通过。10000groups+10000proxies单账号import2.46–8.14ms，trace仅2row/2query，无refs0query；资源disabled映射、空mappinginvalid、未知引用/文件模式/preview/旧lease与resources恢复通过。
- root开始upload→stage→12readgate→activate；其他协作者请勿并行创建或激活候选。

### 07:46 备份引用ad779已上线，发布窗口释放

- active ad7795e64ef74bc1d03d92a95bcf87c5e77d7f11165552846975e755577f152e，binary4643210e6c6ef51c0fb2988d2d465fc7906e2d3a7f6d7c03498ed2c0f9dd562d，PID1705844/private18890，candidate/pending=null。
- 仅bundle实际引用优化已上线（runtime2files），全Go191.756s/vet/6PG与9HTTP2.46–8.14ms通过；上线12读接口200，管理22–184ms。snapshot实验未混入。生产源.local/bundle-bindings-20260930/source，包release-path.txt。
- postdeploy panic0/persistence0，settings33 WSprefer=false/wsenabledtrue/429cooldownfalse，natural最新id1032672继续更新。
- 新HTTP覆盖：登录/退出≤4.1ms，单账号删除保留1万条136MB历史6.989ms，lease100预览12.6–16.4ms/恢复951.8–966.3ms/导出7–8.5ms；均b631相同runtime专项样本，不等全域SLO。
- exports新认领admin_account_bundle_leases.go+PGhelper/tests，将优化100项602acquires及整receipt反复解密；resource只读复核要求保留claim/apply/finalize三提交和unknown/crash/retry状态，不做批事务。
- route统计named PREPARE UNION及typed分支均100k无收益，停止SQL重排、不700k；转prepare-only类别条件预聚合侧表原型与正确性成本设计，不改migration/生产。
- 公平成员分块1M实测median3.31s→2.18s，但fresh读取2.4s慢于row1.5s且出现53.6MBtemp，不能当10M3s证据；原型未集成应用。

### 租期候选与收尾记录（08:03）

- 租期缓存最终3files冻结：bundle_leases.go f03ee6cf…，helper7d722b72…，testaaffe9c…；PGfinal绿，100项最终628.4/689.5/631.0ms、402acquires，对旧951–969ms/602。中间guard前540–559ms不作最终headline。保留数据库trigger修改value后cache失效（UPDATE RETURNING value=$2校验），保留claim/apply/finalize三提交及unknown不重放。
- 单独stage还待全Go完成，发布基线 `.local/lease-restore-20260930/source` 从ad779复制；不含所有失败统计实验。OAuth剩余写test附入但不改运行逻辑。
- root对b631旧sharedgeneration验证front0/socket0/activehandler0/refresh0/persistence0后通过既有retire停用，释放资源。旧legacy服务registry PID2479613已不存在但实际unit PID2232819同binaryf71fc1运行，未擅改/停止legacy，记录draining-service-state.json及draining-review.json供后续真实lifecycle取证；当前active入口不受影响。

### root租期小包发布窗口占用

- release `b3b052f7038304caca8185f2f35943381e66d3253bb8925eb4de1ca073e04f13` binary `dfee3451acea69b9608c74b5a5b9e7397866a91c073e5ea30df78288662f29d0` source `eaa7bb5efa67be151a195d260e308385d8ff7c9ef06b450b7f65a0d6e596bc64`。独立ad779基线，唯一运行delta两lease文件；统计/WS/migration完全不变。
- 全Go185.640s/vet、11功能PG+1HTTP性能测试(9请求)通过，独立review无剩余blocker。root upload/stage/12readgate/activate期间其他协作者勿创建候选。

### 08:12 租期优化b3b052已上线，发布窗口释放

- active release b3b052f7038304caca8185f2f35943381e66d3253bb8925eb4de1ca073e04f13，binary dfee3451acea69b9608c74b5a5b9e7397866a91c073e5ea30df78288662f29d0，PID1882459/private18891，candidate/pending=null。生产源 `.local/lease-restore-20260930/source`，包路径同目录release-path.txt，主树stats实验不能直接打包。
- 两个lease运行文件上线，100项真实HTTP最终628–690ms（旧952–969），402acquires（旧602），原三独立commit/unknown状态/取消/ownerTTL/CAS/坏文档/trigger改写缓存保护保留；全Go185.640/vet/11功能PG+9HTTP通过。
- 候选及上线后12读检查全部200，上线管理42.66–172.53ms。postdeploypanic0/persistence0/PG30min新kill0，settingsversion33/WSpreferfalse/wsenabledtrue/429cooldownfalse保持，naturalreport最新id1170618。
- OAuth15真实本地HTTP0.868–10.842ms补齐start/cancel/sessionimport/apply/replay，外部exchange/refresh/PAT不在该时延结论内。coverage130项现12current读样本/88已有测量待源码依赖绑定/30无对应HTTP测量，绝非全域通过。
- TEMP category预聚合100k初建15s超时，保留失败；1000语义样本全部增删改/unsafe/mutablefilters/MVCC规则/rollback7轮parity通过且明确ROLLBACK，证明方程可行不是规模达标。raw分支dirty空仍无条件先分类扫描是下轮小边界；side初建函数CPU与多维展开、在线backfill仍未解决。无生产migration。

### 新继续轮次：类别函数与大收据（08:50）

- root新025只替request_log_error_classes执行机制，022的category/severity CASE保持字面逻辑与default collation；独立原型thin10k约5x/wide约1.8x，但完整100kpreparedstats2.1–2.3s无明显收益，不能称统计SLO解决。新0257PG/852vectors65shapes9exceptions/ICU+Ccollation/既有索引/OID+rollback全部PASS，准备冻结与发布工具26migration门禁，尚未上线。
- root主树新增Sub2bindingIDlookup（sub2.go局部+helper/test），只传回匹配IDs并保留old lastwins碰撞顺序，candidate未PG/未部署。resourcebaseline1000账号+1000代理Sub2data导入7.69–7.99s、3002acquires，导出/native/CPA都<1s；两慢与功能正确证据保留。
- 大receipt1000末100旧HTTP2.933s已实证；exports v2新PG格式immutable root+单项state（同namespace），老reader failclosed、新claim/apply/final仍3commit。首PG通过，1000末100458.8ms；还待10k与独立expiry/GC复核，不提前发布。其source独立b3无025，防混层。
- 预聚合prototype新版emptydirty One-Timegate1000 current1ms/snapshot1.6ms，8parity覆盖同minute2statusunsafe重复bucket（已DISTINCT）；100k分100batch<=1000已构建完，等实际oracle/plans/rollback。只是TEMP，不是生产migration。
- integration现route65255→exportslease10k→rootclassifier最终micro+Sub2candidate→其它按实际handle协调，不并发重PG。

### 本轮联合发布候选封版中（09:06）

- 线上仍b3b052/dfee/25项迁移，无candidate。root联合freeze `.local/performance-combined-20260930/source` 精确b3+025等价分类函数+v2lease+Sub2只查引用IDs，没有任何失败stats/UNION预聚合实验。fullGo180.393/vet通过，23发布工具测试通过。
- 分类025actualPG7项通过；新micro thin10k587–814→122–128ms、wide1468–1480→859–895ms；真正durableingest1000 9219–9272→8087–8131ms约12%，notHTTP。whole100kpreparedclassstatsold/new2.1–2.3s无明显读取收益，不用于提速headline。旧022checksum不改、索引/OID/异常/ICU/Ccollation均已证，migration仅025无回填。
- compatlookup4PG功能绿色report，14HTTP功能全部200；1000dataimport7.69s→6.51s/reimport7.99s→6.63s仍失败。源scope基线b3在test字符串内保留，候选hash/相同harness差异由绑定证据解释，不冒称全路由通过。
- v2lease18PG绿，scale9HTTP200；restore末100of1000/10000=483.7/769.7ms；10kprepare1066.3ms仍未过1s，formatv2对旧实例failclosed且直到TTL不可降级读取，是明确兼容限制。
- production512固定log分类baselinehash dff7ac8f310d5ffe51e4fbca4054cdeb 已私有保存，025后只同ID复核，无推理/业务写入。root统一upload/migrate/stage/verify/activate，封版前任何共享主树不得无筛选部署。

### D 连接复用实现（2026-09-30 05:30）

- 响应用户新要求（连接复用、一条连接处理多个下游请求、减少 429）：已实现 BPS WS 顺序复用连接池。官方协议证据与真实 two_turn 验证支持"每 socket 单在飞顺序复用"（官方即此行为，4 分钟/8 请求轮换、affinity 保留），不支持单 socket 并发复用。
- 新文件 `gateway_bps_ws_pool.go`：每 socket 终身 reader actor（规避 websocket ctx 取消杀连接与并发读 panic）、空闲心跳消费与断连淘汰、凭据代际×代理 key、官方轮换策略。集成于 `gateway_bps_upstream_websocket.go`；开关 `bps_websocket_reuse` 默认关闭，`prefer_websocket` 开启时生效；前端已加开关。
- 10 项专项测试 + 全量 go/vet/前端测试与构建通过。细节与文件窗口见 COLLAB-D 文档"连接池"一节。开启后的效果对比用 D 的监控（.local/collab-d-20260930/）按 429/握手数验证。
- 注意与 root 紧急关闭 prefer(v33) 的关系：连接池默认关闭不影响现网；待 prefer 恢复时一并开启复用即可获得收益。101 后未知终态失败（f97ddfff 账号簇）仍在 resource_pipeline_extreme 的 terminal 采证范围内，复用不掩盖该问题（断连/失败即淘汰，不重放）。

### D 已上线 WS 复用（2026-09-30 05:12 UTC，用户指令）

- Release `21147f0eac51b509df5ed0a501e13584cfd1e40685ea9afd07e17159b6106a26`（binary `9148663b…`，PID 3401872）已激活；基于线上 b3b052f7 源码 + 仅 D 的 WS 文件集（availability 熔断 + 连接池 + settings 字段 + 前端开关），root 在途统计文件未带入。
- settings_version 35：`prefer_websocket=true`、`websocket_enabled=true`、`bps_websocket_reuse=true`。
- 激活前 1h 自然流量证实连接级限流：176,704 次 WS 握手 429 vs 1,069 次 101（04:00–04:45 每 15min 11–14 万次）。流量 04:57 已停；效果待恢复后度量（复用=websocket 尝试无 status_code）。详细记录见 COLLAB-D。

### D 追加：429 治理第二包（2026-09-30 06:33 UTC）

- 设置 v36：`rate_limit_cooldown_enabled=true`（生产从未开过，这是 429 主体根因之一）。
- Release `bef6f7f3…`（binary `2439b562…`，PID 3572239）已激活：生成路径对 ≤5s 限流冷却改为有界等待（≤2 次），目录路径/付款停调保持快速失败；hop 契约测试已随新语义更新。基于 21147f0e 迭代，root 文件仍未触碰。
- 数据：重试风暴已消除（尝试/请求 1.65→0.47）、复用命中 72%、真实上游 429 从 2964/5948 降到 82/7985；剩余 429=容量。详见 COLLAB-D。

### D 接管全部优化（用户指令，2026-09-30 07:0x UTC）

- 用户明确：**所有优化由 D 处理，不再移交 root**；D 负责后续冻结与发布，root/其他会话可只读复核。
- D 第二轮修复（详见 COLLAB-D 文档）：①WS 握手 429 短退避（60s，key 扩为 account×proxy 指纹×endpoint，map 硬上限+老化，appserver 幻影 attempt 修复）——复用后仍存 ~107 次/分钟浪费拨号的根因治理；②CRUD 全量扫描修复 8 处（Settings/lease/schedule 同值跳过、CRS preview/proxy 批量化、auto-reset 元数据读、bundle 导出有界读+导入去 O(N²)）；③删除误报类扫描确认无实例。复用度量口径修正：复用命中=ws attempt status_code 200 且 duration 0。
- D 新认领文件窗口：gateway_websocket_availability.go(+test)、gateway_bps_upstream_websocket.go、gateway_websocket_preference.go、gateway_bps.go（预检一行）、gateway.go（预检一行）、gateway_appserver_cli.go（blocked 早退）、store.go（Settings 同值跳过）、admin_account_leases.go、admin_account_schedules.go、admin_account_auto_reset.go、admin_account_crs.go、admin_account_crs_atomic.go、admin_account_bundle_resources.go、admin_resource_reads.go、admin_accounts_management.go（一行 ctx）、相关测试。请其他会话勿覆盖。

### D 追加 2：冷却等待 15s+抖动（2026-09-30 09:05 UTC）

- Release `cd5762bb…`（binary `a1cd4770…`，PID 4131861）已激活。实测冷却剩余 2–7.4s 卡在旧 5s 等待上限之上，且到期惊群互踩；上限提到 15s 并加 0–1.5s 抖动。
- 效果（09:03 后）：冷却快速失败 429 ~1.6 万/5min → ~1-2 千/5min（约 8 倍降幅），ok 稳定。容量结论不变（~2.6 ok/s vs 62 rps 需求）。
