.22 的 B 组审查持续进行。实际发布目录已从共享工作树切换到 `C:/Users/Administrator/Desktop/项目/codex-agent-hot-release-20260929`。当前不能宣称新应用完成上线；稳定入口接入与应用蓝绿切换是两件事，最终部署仍由主会话唯一执行。

原共享工作树首轮冻结 b4fc4568 的 PG/race 回执已保留：PG 21 父项、34 含子项全通过，但 wrapper 因源码漂移报告 fail；race 1332 PASS/20 SKIP、三包通过，同时记录 source_changed_after_freeze=true。两者不用于认证变更后的整个工作树。真实模型首轮的计费 fixture 版本冲突由 C 组单独保留，不能用其余 11 组成功掩盖该失败。

隔离目录首冻结 f2a76a18 的当前证据（2026-09-29 06:08 +08）：

| 范围 | 已核事实 | 尚不能证明 |
| --- | --- | --- |
| 隔离来源 | 759 后端文件全部匹配冻结；38 个声明覆盖文件和 3 个 mixed 文件完整，其余逐 SHA 等 .21 的 65fb 基线。 | 非本次功能的并行统计/迁移修改不属于这个隔离版本。 |
| 计费设置保留 | 3 个 mixed 文件的精确 diff 仅新增默认 true、指针持久化 false、partial update 和 runtime 字段；原新 Go 4 父项、23 含子项各唯一通过。 | 不能替代实际 PostgreSQL、Redis、模型和生产切流验证。 |
| 新 PG | 215712.769957：21 父项、34 含子项全通过，exit 0、无漂移；binary a8eadadc，759 执行源等当前后端。 | 其后变更的 deploy/tools 文件不在 PG 验证范围。 |
| 新 race | 215712.777876：1140 PASS/30 SKIP、exit 0、原快照无漂移。 | 启动未加 with-redis/include-official，6 个共享 Redis 父项跳过、官方测试未选中，不满足完整验收。须主会话补齐实际完整范围。 |
| 旧停服保护 | 共享和隔离树已同为 base57e3/reliability52ca；守卫函数 AST 等 B 候选，原 7 项纯 Fake 验证通过。 | 历史不可变包内旧 helper 不会被追溯修改；Path.exists 不是原子互斥。 |
| 制品允许集 | 新 validator 允许 ingress/controller 两个精确制品，校验 SHA、库存及 Linux amd64 ELF，保留六迁移限制；原 owner 单项回归在私有副本通过。 | 使用的是临时假包与假 ELF，不是最终真实包已验。 |

旧停服保护修复的具体路径包括 pending-state preflight 的隐式 rollback、直接 rollback、底层执行器独立入口和真实 stop_service 边界。stage、clean preflight 及无 registry 的原模式仍保留。采用记录分两次读取：22:00:49 UTC 隔离树尚旧，22:01:54 保存时已同步；不要继续引用早先“未同步”作为当前状态。

应用后端、前端和六迁移目前保留初 f2a 的字节身份，部署脚本另有更新；最终封包必须按最新 fullsource 重新登记。入口健康检查排空采用主会话的 same-held CAS 方案，由 C 核实际连接行为，B 只核它是否保留精确恢复合同。不要把仍未采用的 C Out.Close 候选算成已部署入口代码。

B 的原件在 `C:/Users/Administrator/Desktop/项目/codex-agent/.local/cross-session-20260928/01a0e7ed/v0422-release-audit/`：`pg-review.*`、`race-review.*` 为首共享轮；`isolated-pg-review.*`、`isolated-race-review.*` 为真实隔离轮；`isolation-20260928T220309749796Z/` 留三文件 diff、759 源验证与所选 Go 原终态；`legacy-guard-adoption/` 留采用与纯 Fake 结果；`artifact-20260928T220550310939Z/` 留单项原回归、8 源摘要与原日志。B 未新增生产 SSH、SQL、Redis 或模型请求，未改 owner 发布源码。

后续更新（06:14）：C 已封隔离 12live/20 必需子项全部通过，新计费 cold SSE 将 actual read0/write49026 仅在公开出口变为 read49026/write0，内部记录保留原值；关闭 JSON 的该次 write0 样本不单独证明正 creation 的关闭差异。本轮模型无需因 race 缺选项再跑。主已启动新的 race 220909.245719，B 仅跟其实际原句柄与终态，未把旧缺项轮改成通过。

adapter 两树已同步为 347004a0，相对保存 d1d63 仅新增 parse_lifecycle_timestamp 并改 lifecycle 调用；精确目标、durable、prune、activate、reconcile、retire、hold/resume 九个函数 AST 全同。`adapter-final-source-binding.json` 仅扩展恢复合同的源码绑定，不把旧五组 Mock 说成时间解析或实际排空已测。

06:23 更新：完整 flags 的 race 220909.245719 已独立通过，1332 PASS/20 SKIP、12 必需项均实际 PASS；shared 七父项、Redis4、official95、front 全部覆盖，无 DATA RACE，759 后端源与当前一致。此前缺参数的运行仍保留，未改写其覆盖不足结论。

实际 .22 不可变包审已通过，release `4c2a72e245d14dc649a81d0efff8940589e799a31fe8d1cf9788eb264237bd49`，应用 binary `8e4078b46a1050c1150c641be37acb54690864475619cd7ca34e0e906d6ce05b`，最终 fullsource `216a2b4e3bbba7e634777cbacafd0d3d9e9d9d79da0cc8edb43b52eda195f13a`。1345 源与隔离 current 相同；98 制品、35 证据和 99 个归档文件的库存/内容摘要完全对应。包内原 validator 的纯验证以及 PG、billing_live、shared_race、front 四份明确审查绑定均通过，见 `package-4c2a72e2-audit.json` 与 `review-index-4c2a72e2.json`。

race 原 JSONL 未随包封入，审计明确按包外本地原件处理：实际 SHA 与固定独立 review 相同，review 同时核 raw/report 映射一致并绑定包内 report/source。原件与 review 在检查结束再次核 SHA，不声称它在归档中。首轮包检查器因 Windows TEMP 的 ADMINI~1 短路径和 resolve 后长路径不同而误拒，在纯 validator 执行前失败；该结果已独立保留，修正私有临时目录规范化后，同一个未修改的包实际通过。没有降低包路径或源码守卫。

这次结论是包内容与审查来源通过，生产 activation、连接切换与现场计费仍须主会话真实回执；B 未执行部署。

06:32 后续：两次实际激活均返回等待并恢复旧 .20 接流量，尚无新应用 active 证据。第二次 45 秒窗口保留 78 次观察，首样本 backend/front 为 8/8，末样本为 4/4、waiting 为 2，没有观察到归零。计数不能相加，也不足以判定 HTTP/SSE/WS 或始终同四个请求。`drain-socket-boundary.*` 已固定包内函数：它统计旧 PID 拥有的指定本地端口、非监听 TCP socket，包含入口转发对端、idle 及仍持有 FD 的半关闭连接，并非“非入口旧直连数”；四项 Fake 控制只验证这一定义，无现场 /proc 查询。

旧版本切前基线已找到原 snapshot、逐 SHA 与摘要对齐：21:59:34 UTC 的 20 分钟窗口共 1874 个自然候选请求、11 个错误，实际 read 113907016 / 同样本 input123600686 = 92.1573%，creation 8739170 单列，11 个未知不补零。已知发布 smoke 在窗口外，未标记自然候选不等排除了所有自动化。完整证据见 `precutover-baseline-review.*`；不能算 .22 的效果。

新的共享 `tools/hot_release.py` 为 8c2de982，已经保存最后一次 held 状态下的三计数，恢复旧流量后仍按历史观测透传。隔离目录仍为 5d0fcb22；真正包内库存没有该 tools 文件，只有 source-manifest 的旧 pin，故实际 45 秒执行来源由 A 组另核，B 不冒其由已封 wrapper 执行。B 发现 owner 已改同范围后停止竞争补丁，仅保两源和字段合同于 `drain-diagnostic-candidate/`。

06:43 更新：.22 已真实激活，新 active PID3045419、binary8e4078、release4c2a、版本/单位/18890 地址与已审包相同，入口 held=false/durable=true；旧 .20 PID2479613 保留无重启。.23 预留方案已取消，私有候选未作为生产发布。B `check_deployment.py` 仅读原件，`deployment-review.json` 20/20 通过：22:38:55 两生成 SSE2749ms/JSON2806ms 完成，真实 request attempts 共2、无refresh；top_p本地拒绝5ms/HTTP400/0派发；3CSVv4及ToolPolicy原谓词通过；settings27/api_key/cooldownfalse、计费booltrue、账号配置保留。22:40:52运行回执表明新unit enabled/running、0restart、资源限制infinity。

这次成功窗口的94次health全部ready、同client keepalive跨两个generation、active_changed=true、最大3.24ms，与此前两次waiting分别保存。它是实际健康连接切换证据，不等于逐条在途模型SSE/WS都已观察。post摘要未保留公开usage和原CSV全文，`cached_tokens=null`与`known=true`不能由B改写成具体零值或unknown；数值投影由C按实际验收predicate另核。B没有新增模型/SSH/SQL或部署操作。

原同session失效owner并发迁移仍有独立可用性红测，.22的成功上线不等于该问题解决。下一步严格合同与函数/source依据已另交 `session-migration-next/`，仍待真正实现及跨进程验证。
