# 会话绑定选择观测合同

2026-09-29 01:49 +08 核对：**0.4.16已部署并现场验收通过**，releasee145f1ba…/PID1911991/binary70b42e59…，Go/vet、PG16、单轮race及完整9live证据均闭合。[发布复核](COLLAB-B-V0416-RELEASE-REVIEW-20260929.md)区分实际正例与现场普通smoke；自然错误改善仍需后续数据，旧六条根因不会被追溯补造。

`bound_selection` 是可选诊断对象，仅在已有会话绑定无法匹配本次授权候选集的精确尾分支出现。它解释**当时选择使用的快照与后续处理结果**，不修改账号选择、授权、重放、HTTP错误、用量或费用，也不证明旧六条503的根因。

## 六字段

| 字段 | 合法值 / 含义 |
| --- | --- |
| `bound_snapshot_state` | `exact_disabled`：同一快照中精确owner被禁用；`exact_group_denied`：精确owner启用但不满足本次key组授权；`missing_or_replaced`：该快照无法匹配，不能据此区分删除/替换；`unclassified`：其它未分类状态。均非当前权威账号状态。 |
| `eligible_candidate_count` | 同次启用状态及key授权组过滤后的非负数量，0明确有效。**不等于ready账号、空闲槽位、可成功迁移数或真实派发次数**；后续仍有refresh、并发、冷却、代理和授权复核。 |
| `snapshot_origin` | `fresh_read`、`cache_hit`、`stale_on_read_error`。最后一项表示读取失败后使用既存快照，不表示本次额外查询失败。 |
| `snapshot_age_ms` | 可选非负毫秒数，在原snapshot锁内用其加载时间取值；新读为0，缺可信时间则省略。它是快照年龄，**不是会话年龄、请求耗时或Store读取耗时**；沿用现有List开始时刻的时间约定。 |
| `replay_class` | `full_plaintext`、`previous_response`、`opaque_or_file`、`unobserved`。与原迁移判断共用首次请求解析，history展开前保存；完整明文不等于已获迁移许可。未知不伪装为完整明文。 |
| `recovery_outcome` | `blocked_input`、`blocked_context`、`blocked_delegation`、`blocked_other_guard`、`candidates_exhausted`、`replacement_failed`、`recovered`。在最终record选择、owner commit及交付处理之后记录；仅选中替代账号不足以称recovered。无法可靠判断时省略。 |

不存在此事件的普通请求、旧记录和未观察路径省略整个对象。未知可选字段不补0；已知0不省略。对象仅含这些枚举和计数，不保存owner/key/session/组名、凭据、正文、图像、工具参数或内容hash。

## 同次快照与最终记录

原accounts/key组数据只在选择调用栈暂存，不进入context或持久化。只有精确尾分支才将同次snapshot转为有限标量；不失败后重读Store/Redis，也不另扫body解释过去。

逻辑请求首次tail的快照被独立复制，后续重试不覆盖。最终附到被实际选中的record，不能把旧失败Error、HTTPCode、Account、ToolPolicy、attempt或usage复制到替代成功记录。因此 **`status=ok` 可以与旧tail快照及 `recovery_outcome=recovered` 同时存在**；这不是“成功被标失败”。下游交付失败即使owner已commit也不是recovered。异常退出保留可确定的快照事实，`recovery_outcome`省略，不补造完成结果。

## 消费与持久化

- 详情UI使用有限枚举映射和非负安全整数，已知0显示为0，未知显示“未记录”，不回显未知私密文本。折叠详情明确快照非当前状态、授权候选非可立即使用。UI不按status=error限制对象，成功恢复也可查看。见[实际UI静态语义审查](../.local/cross-session-20260928/01a0e7ed/bound-selection-ui-review/REPORT.md)；该审查没有自行重跑浏览器。
- JSON/JSONL沿RequestRecord可选对象保存；CSV继续 **`codex-usage-v4`、77列**，不为bound新增列，不变更observation schema2。统一存储门禁 **`TestPGBPSBoundAndAttachmentObservationRoundTrip`** 已在PG173634.831282实际通过，与本轮PG16全部PASS分别可核原始日志。[正式Go/PG复核](../.local/cross-session-20260928/01a0e7ed/v0416-release-audit/gate-coverage/completed-pg-go/README.md)绑定当前717项执行源；早先[准备回执](../.local/transport-reliability-release-20260929/pg-test/report.json)的编译SKIP仍按原样保留，不冒充此次真实数据库执行。
- monitor只在非ok错误行投影并清理六字段，不新增统计窗口或分组。成功recovered不会进入errors/new_errors；缺少监控错误不代表没有成功恢复。缺省/非法enum、bool冒充count、负数、溢出会被过滤；原cursor/persist与缓存分母保持。[实际合入AST核对](../.local/cross-session-20260928/01a0e7ed/bound-selection-monitor/integrated-review.md)证明与36项本地pure/mock候选一致；后续主会话[server回执](../.local/transport-reliability-release-20260929/monitor-server-tested-source.json)已绑定同76bc7957…代码和Python3.9的25项测试/只读SQL通过，安装与生产效果需另认现场回执。

## 已有实测范围

- [0.4.15重基聚焦](../.local/cross-session-20260928/01a0e7ed/bound-selection-rebase-0415/evidence.json)：20顶层、76含子项PASS，含HTTP选择/保存、原始replay、first-tail克隆、普通成功省略及early-param/warmup/summary/compaction邻接。不是全库/生产测试。
- [新基线独立对照](../.local/cross-session-20260928/01a0e7ed/bound-selection-independent/rebase-0415/result.json)：9个前后业务结果完全一致，比较wire、调用数、错误、账号、attempt双HTTP、usage和ToolPolicy。
- [真实loopback WS](../.local/cross-session-20260928/01a0e7ed/bound-selection-ws-review/result.json)：dispatcher→inner POST→outer final实际链路，opaque/previous仍503且不迁移，普通warmup无对象，完整明文替代已commit后才标recovered；两侧3顶层/4leaf通过。这里“真实WS”指本地协议连接，未调用真实provider。
- [owner额外3边界审查](../.local/transport-reliability-release-20260929/bound-review/REVIEW.md)：捕获后账号/key改变不污染旧snapshot；commit后真实writer失败保持aborted；prior generation401加后续OAuth400耗尽仍保存正确priorRecord和网络账。私有受测源稳定，wrapper曾因并行canonical变动退出1，报告明确没有把它包装成整个现树通过。

## 旧版本读取与回写

**源码推断，未新增混版harness：** 不含BoundSelection字段的旧Go类型通常能解码带新对象的JSON，但通过旧typed结构重新序列化、typed导出或迁移会丢掉该未知可选对象。原JSONB只读本身不会改写原文；需区分读原数据库和经过旧类型再写回。

旧记录没有这些时点字段，不能从503、空account、零tokens、attempt数或当前配置倒推六字段；本功能没有历史回填。正式上线和观察效果只认对应版本的冻结、门禁、运行及自然流量回执。
