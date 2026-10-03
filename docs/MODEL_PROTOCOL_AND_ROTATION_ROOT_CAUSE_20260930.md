# 模型目录协议与额度轮换调查

## 已上线的模型协议修复

普通 OpenAI /v1/models 原本返回 Codex 专用的 models/slug 清单，Sub2API 固定上游目录后会按 data/id 解析并得到502。现按明确协议入口区分：/v1/models且没有非空client_version使用标准object=list/data；原生/models或带client_version保留Codex清单。已用同一上游AccessCatalog顺序验证10种路径/查询/UA组合以及异常目录，避免两种缓存串用。

service_tier原先比较原始JSON字节，合法的Unicode转义auto/default会被误拒；现按解码后的字符串值判断，仍拒绝真正不支持的priority/flex和非法类型，没有静默降级请求。

工具失败新增有限证据，只在首个真实拒绝时记录：工具名SHA256、同名其他类型声明、入站和实际初始出站声明匹配布尔值、固定update_plan枚举、合法上游tools/prompt SHA。成功请求不额外解析，也不存正文、schema或参数。正常HTTP/SSE/usage/错误行为的16条契约逐项一致，真实PG存储/详情/导出回归通过。

部署release：4b35e35e8d71da7bedc55c9aab7bac4a6bd20e5a70d1f73448ee9e1851027cef。
binary：671deaedf2d0a224bdd16b9ca70cf8987fb2c88b44103374bf66122f4094cc8c。
两节点已切换。12项健康和管理API检查通过；当前上游账号为空，真实客户端目录仍因无供给返回失败，不能把本地协议回归当成线上模型可用证明。

## 监控修正

request_logs.created_at是请求开始时间，不是落库时间。旧最近两分钟时间过滤会漏掉长请求的完成错误。监控改为读取最新20,000个主键行并保留重叠ID集合，只计新观测到的完成记录，兼容较小ID迟提交。首轮只建立基线，不把历史重报；完全失去重叠时报告gap_detected，不声称完整。最多60份聚合与20,000个ID，没有请求正文。

5项去重/迟完成/乱序/429分离/缺口测试通过，真实隔离PG临时表证实两个30分钟/1小时前开始但新提交的错误均被捕获。脚本和timer已更新，保留文件约0.3MB。证据.local/root-cause-20260930/monitor-completion-pg.json。

## 持续账号中断的实际来源

运行中的Loophub 28765按照官方wham/usage的secondary_used_percent=98阈值执行quota轮换，不使用缓存率85%的对外显示值。原先先self_leave撤团队身份，再删除Codex下游账号。

12:36:10 UTC删除前已开始的29个请求，在删除后完成并失去本地授权；旧账号与新导入账号处于同workspace但user不同，不能把它们当同一个owner或跨账号重放加密状态。metadata-only编辑没有改变credential_version。真正管理调用链已与Loophub PG quota_removal/child_self_leave/destination_synced逐时间对应。

13:08 UTC现场：3个启用母号均处于held recovery round1/waiting，原恢复成员占advanced席位；各自官方available=0，无active/pending生成子号。最早21:49:39 CST仅进入恢复流程下一步，其余时间10月1日00:36:02、02:53:27，不保证届时必然补号。worker持续运行，无证据支持“卡住后重启”或恢复停用母号。

用户随后明确保持母号恢复任务不处理，只继续修已知代码错误。本轮不变更6/9小时策略、不启用停用母号、不购买席位、不复用已用身份，也不新增模型请求等待或处理429。

## 账号排空协议：服务端已部署，调用端待切换

Codex Agent 两节点已于22:00 CST前后切到release `3a283b2a3fe4664dc89fede6f561a9321f625d01014eb4ce7c27066b2adb4547`，真实运行binary为 `1dc3f79dc87219c98ab2231be08364ca3e55c3e969dc925b114a23d81688b168`。仅增加027迁移；未带入根目录另一任务的025迁移。两节点健康、共享设置与存储身份一致；Docker容器网络访问 `http://172.18.0.1:18879` 健康和管理员capabilities均200，返回真实account_drain协议。主节点12项页面/管理/健康检查全通过，原缓存率85%与关闭限流冷却设置未改。

自动quota退出与Codex共享准入租约对接：停止旧账号的新分配，让已入场模型请求正常完成，再允许self_leave与带身份凭证的DELETE。只承诺in_flight_requests，不把client-owned未来工具回调数量null伪造成0。PG持久排空记录防止Redis键丢失后重新准入；连续零租约观察跨过旧租约有效期后才能删除。该观察只控制后台回收，不让推理请求排队。

测试覆盖跨节点租约、迟到Redis命令、丢失租约、身份替换、提交失败、普通编辑锁兼容、管理stream/compact及CPA兼容生成入口。精确候选全量go test及go vet通过，真实隔离PG/Redis 64项含子测试通过；Loophub 111项回归通过；真实最新Python镜像直接调用Go API及隔离PG/Redis的联合测试通过。独立审查无未解决P1/P2。

Loophub候选镜像 `loophub-ipv6-app:20260930-account-drain-ae00e4cbcf92693d`，实际ID `sha256:cfad3e30965f6ba1d033d9ce0871229651190bc9ff19ddc2a38d9dcecc40158b`。持久化操作及目标身份，逐轮单次观察，部分删除/回复丢失/重启可恢复；排空未完成不触发立即唤醒忙循环。held恢复与429分支保持不变。

22:06现场尚有两个旧代各3条WebSocket，常规退休工具保留它们。启用自动排空调用端前，必须确保全部接流量的旧代也已退出。Loophub另有100个自动注册worker运行，部署保护拒绝直接重启；pause只停止新任务派发、在途可自然完成，但新进程无法自动取得原启动参数。用户已收到是否允许排空上线后手动恢复注册的选择，答复前不改注册状态。不能把候选通过当作调用端已经上线或轮换事故已经闭环。

证据：`.local/root-cause-20260930/account-churn-final-review.json`、`joint-overlay/verification.json`、`drain-schema-installed.json`、`.local/error-repair-20260930/drain-peer-verified-140559.json`。

22:09资源复查：Codex Redis使用约71MiB、RSS约95MiB；主机总125GiB、可用约95GiB（按MemTotal-MemAvailable约30GiB已用），根盘可用约618GiB。未因监控重新累积请求正文；监控文件约0.3MB。当前没有可用生成账号，仍不能验证恢复后的真实成功率。

22:41更新：旧671dea和1dc3f代均在已验证空池窗口正常退休，双节点无draining代。最新release为 `2c88147bd8a97015a48ab3e72f236635a79315168cc96360ae954514a0209e8c`，binary为 `6ccb5155b4394eb550d32dbab2892ab9674c9084eac0f5181139087bdf3563ae`，新增已入队WS终止帧防丢失与取消别名处理。详见 `docs/WEBSOCKET_TERMINAL_REPAIR_20260930.md`。Loophub注册维护选择仍待用户答复，不影响母号held恢复任务。
