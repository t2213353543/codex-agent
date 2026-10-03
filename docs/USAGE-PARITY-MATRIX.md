# Codex 使用记录功能对齐矩阵

**最新被动采集已授权并实现：** 图像输入/输出Token及精确来源、已有客户端关联ID、真实授权/响应传递耗时和独立准入前错误持久链路已接管理展示/统计/筛选/导出；不发送任何主动探测或真实上游测试，旧数据不补造。Logs54/Accounts55/Node161、131管理race及同源码440网关+管理race通过，详 [被动采集验收](MANAGEMENT-PASSIVE-OBSERVATION-VERIFICATION-2026-09-18.md)。尺寸/张数和未采网络阶段仍不可推算，参考运行行为配置不在本授权中。

**最新错误管理分级：** `error-severity-20260918` 已接来源明确的P1/P2/P3/unknown、同条件列表/统计/详情/导出/清理和独立错误工作区筛选；成功不分级、HTTP200事实不改、原记录不写回。Logs46/Accounts55/Node154、108真实管理race通过，见 [本轮验收](MANAGEMENT-ERROR-SEVERITY-VERIFICATION-2026-09-18.md)。下方旧“severity未接”已被本轮取代，仍保留真实被动采集及网关运行行为边界。

**当前 account-status 增量：** 已消费独立任务实际采集的 `websocket_warmup`，管理筛选/详情/导出三态一致；修复长请求ID被截到500字符、新链路筛选清理预览被错误拒绝。Logs42/Accounts55/Node150、104真实管理race通过，见 [本轮验收](MANAGEMENT-STATUS-WARMUP-2026-09-18.md)。参考错误工作区 severity 分级仍未接，真实多模态及部分网络阶段观测仍缺来源，不将这些缺口伪装成历史数据可回填。

**非计费最新增量：** operational-fixes 修复数字偏移/别名时区查询、未知用量误显为零、历史候选伪资源 ID，并让默认时间排序使用现有索引；Logs38、Accounts53、Node146、94真实管理race通过。见 [最新验收](MANAGEMENT-OPERATIONAL-FIXES-2026-09-18.md)。采集来源边界不由 UI 合成测试代替。

**最新范围变更（2026-09-18）：用户明确不需要任何计费。** 当前运行界面已移除参考价、采购成本、销售策略及全部金额统计；此前计费对齐记录仅为历史阶段证据，不再是当前需求或待办。当前以 [日志无计费范围](LOGS-NO-BILLING-SCOPE-2026-09-18.md) 和 `no-billing-20260918` 候选报告为准，保留请求、Token、缓存、耗时、观测、错误处理、清理与导出。纯计费测试已退役，不计入本轮通过数。

审计日期：2026-09-18。范围：现有 Codex 账号可用的使用记录功能；参考本机 Sub2API 源码。前半部分保留实施前审计基线，后面的「逐项实施状态」是当前事实。状态分别列出后端源码、真实 PostgreSQL、旧壳浏览器合成接口及被动采集来源，不能互相替代。

**基线结论（实施前）：** Logs 只有基础日志工作区。现在已补同筛选全量统计、参考估价、可选观测字段、全量脱敏导出、持久清理 UI、资源候选、时区粒度和排行；尚不能声明整个矩阵完成，尤其第二批采集、完整模型链、阶段计时及失败链不因界面有字段就视为已实现；历史未知只报告覆盖，不当作可恢复数据。

## 口径与适用性

- `A`：现有 Codex 资源和记录即可支撑的管理功能，必须完成。
- `B`：Codex 协议或响应支持，但当前未采集、未持久化或缺定价来源，必须补齐真实来源与能力状态，不能因为当前无字段而删掉功能。
- `C`：参考系统的多租户/商业管理能力；需要明确映射为本项目账号、KEY、分组或声明未建立的业务实体，不能虚构用户余额/实际扣费。
- `X`：当前 Codex 端点不具备的其他平台专属能力，单独列出适用性；不冒充已对齐。
- “未知”与真实 `0` 分开；统计返回覆盖数。`HTTP 200` 不保证业务成功，`response.failed`、中断与缺少结束事件需要独立状态。
- 当前 Codex `input_tokens` 包含缓存读取；总 Token 为输入 + 输出，缓存和 reasoning 是子集。Sub2API OpenAI 入库时会先扣除 cache-read/cache-write 得到互斥输入桶，不能照抄其表格求和到当前字段。
- 所有列表、统计、图表、导出、清理预览使用同一个规范化筛选快照；必须标记数据时间/覆盖范围/是否截断/是否精确总数。
- 用户禁止改 Codex 网关。下述数据采集缺口要求先定位现有被动观测点，只追加不影响请求内容、指纹、认证、选路、重试或流式字节的管理观测。需要触及冻结代码时应明确边界并交由主任务协调；不能以此把 Codex 能力默默排除。

## 证据索引

以下行号以审计时源码为准，代码并行修改后应按符号查找。

| 编号 | 源文件与关键位置 |
| --- | --- |
| S1 | [Sub2API UsageView.vue](../../sub2api/current-source/frontend/src/views/admin/UsageView.vue)：5 统计、8 图表、86 工具栏、122 服务端排序、141 分页、143 错误、159 排行、242 breakdownFilters、373 列表参数、391 列表竞态、401 统计、435 模型分布、489 图表、534 刷新、575 全量导出、640 列配置、818 错误查询。 |
| S2 | [Sub2API usage.ts](../../sub2api/current-source/frontend/src/api/admin/usage.ts)：12 统计字段、41 清理筛选、54 清理任务、83 查询参数、119 stats、146 搜索用户、178–203 清理 API。 |
| S3 | [Sub2API UsageFilters.vue](../../sub2api/current-source/frontend/src/components/admin/usage/UsageFilters.vue)：11 用户、48 KEY、84 模型、91 账号、127 请求类型、132 compaction、138 结算来源、144 计费方式、150 模型不一致、156 错误筛选、174 分组、274–326 选项、342/366/426 远程搜索。 |
| S4 | [Sub2API UsageTable.vue](../../sub2api/current-source/frontend/src/components/admin/usage/UsageTable.vue)：29 用户关联、55 模型链、93 reasoning、107 端点、148 Token、208 成本、235 延迟、258 请求 ID、276 上游 ID、294 UA、299 IP、396 费用解释。 |
| S5 | [Sub2API UsageStatsCards.vue](../../sub2api/current-source/frontend/src/components/admin/usage/UsageStatsCards.vue)：9 请求、17 Token、68 实际费用、77 标准费用、86 平均时长、109 账号费用。 |
| S6 | [Sub2API UsageCleanupDialog.vue](../../sub2api/current-source/frontend/src/components/admin/usage/UsageCleanupDialog.vue)：173 每 10 秒轮询、195 状态、278 取消条件、287 请求快照；[cleanup service](../../sub2api/current-source/backend/internal/service/usage_cleanup_service.go)：125 创建、189 分批执行、308 取消；[cleanup repository](../../sub2api/current-source/backend/internal/repository/usage_cleanup_repo.go)：119 领取、285 批量删、326 聚合失效。 |
| S7 | [Sub2API usage_handler.go](../../sub2api/current-source/backend/internal/handler/admin/usage_handler.go)：62 列表、164 时区范围、188 排序、225 stats、393 用户搜索、469 清理任务；[usage_log_repo_query.go](../../sub2api/current-source/backend/internal/repository/usage_log_repo_query.go)：99 筛选、171 exactTotal、197 快速总数、226 排序白名单。 |
| S8 | [Sub2API dashboard.ts](../../sub2api/current-source/frontend/src/api/admin/dashboard.ts)：71 趋势、108 模型、158 分组、173 端点、195 用户 breakdown、205 snapshot-v2；[UserTokenRanking.vue](../../sub2api/current-source/frontend/src/components/admin/usage/UserTokenRanking.vue)：115 排序项、124 Top 20/50/100/200、159 同筛选请求。 |
| S9 | [Sub2API 类型](../../sub2api/current-source/frontend/src/types/index.ts)：1690 请求类型、1694 UsageLog、1768 AdminUsageLog、2153 UsageQueryParams；[ops.ts](../../sub2api/current-source/frontend/src/api/admin/ops.ts)：893 错误列表、941 错误详情、1122 获取详情；[OpsErrorDetailModal.vue](../../sub2api/current-source/frontend/src/views/admin/ops/components/OpsErrorDetailModal.vue)。 |
| S10 | [Sub2API OpenAI usage](../../sub2api/current-source/backend/internal/service/openai_gateway_usage.go)：181 输入桶、209 定价时刻、234 服务层、299 平台/上游费率区分、369 UsageLog、432 TTFT；[gateway_usage_billing.go](../../sub2api/current-source/backend/internal/service/gateway_usage_billing.go)：1068 定价入口。 |
| C1 | [当前 Logs.vue](../frontend/src/views/Logs.vue)：filters/appliedFilters、load/search/nextPage/exportPage、pageStats/pageMetrics、详情抽屉。 |
| C2 | [logQueryState.js](../frontend/src/utils/logQueryState.js)：1 七个筛选、签名/分页/日期、validLogPage；[logMetrics.js](../frontend/src/utils/logMetrics.js)：未知值/当前页统计；[logExport.js](../frontend/src/utils/logExport.js)：当前页字段白名单；[logFailure.js](../frontend/src/utils/logFailure.js)：展示用错误分类。 |
| C3 | [stats.go](../stats.go)：21 RequestRecord、45 tokens、83 保留范围、397 Series、679 LogQuery、703 文件日志、767 匹配、844 usageFromResponse；[stats_pg.go](../stats_pg.go)：91 错误脱敏、198 持久化、315 聚合、580 游标、603 logsPG、745 Prune、762 ExportRecords。 |
| C4 | [admin.go](../admin.go)：147 overview、255 响应范围、1072 handleLogs、1333 routes；[api.js](../frontend/src/api.js)：overview/logs API。 |
| C5 | [002_metrics.sql](../migrations/002_metrics.sql)：usage_events、request_logs、minute/day 聚合和旧快照基线；[migration_cli.go](../migration_cli.go)：120 重建、138 清理、574 存储导出。 |
| C6 | [response_events.go](../response_events.go)：48 terminalResponse、73 recordResponseOutcome；[gateway.go](../gateway.go)：461 请求记录、754 总时长；[gateway_websocket.go](../gateway_websocket.go)：443 每轮记录；[proxy.go](../proxy.go)：122 请求 ID 头、165 响应 ID 头、375 被动流观察。仅读取，未修改。 |
| O1 | [本地官方 Codex 0.154 responses.rs](../.local/codex-source-0.154.0/codex-rs/codex-api/src/sse/responses.rs)：35 openai-model/x-request-id、57 upstream ID、119 completed.id、129 usage、143 cached、144 cache-write、148 reasoning、151 total、153 rollout units、898 单测。 |
| O2 | [本地官方 Codex common.rs](../.local/codex-source-0.154.0/codex-rs/codex-api/src/common.rs)：service_tier 请求字段；[openai_models.rs](../.local/codex-source-0.154.0/codex-rs/protocol/src/openai_models.rs)：service_tiers/default_service_tier。 |

## 查询、列表与状态

| ID / 适用 | 功能 / 参考证据 | 当前差距 | 接口或数据要求 | 可验收条件 |
| --- | --- | --- | --- | --- |
| Q01 A | 日期区间/时区，S1:285、S7:164 | 有秒级 from/to、本地日期输入与快捷范围；未统一服务统计时区 | `from <= time < to`，明确 timezone 与显示时区 | 跨日、DST、空边界、非法范围；列表/图表/导出相同边界 |
| Q02 A | KEY 精确过滤与搜索，S3:48/366 | 仅 key_name，重名/改名不稳定 | `key_id` + 名称快照；可搜索选项；已删状态 | 两个同名 KEY 只匹配选中 ID；已删日志仍可查 |
| Q03 A | 账号搜索与 ID 过滤，S3:91/426 | 有名称/ID 手填，无联想选择 | `account_id` + display snapshot；候选分页 | 改名/删除后可追溯，不用 email 作为唯一键 |
| Q04 A | 分组过滤，S3:174 | 日志 Groups 是快照，未暴露查询筛选；单请求可能多个 group | `group_id` 明确请求路由分组或所属组口径 | 分组改名/成员迁移不改历史；聚合不重复计数 |
| Q05 A | 模型搜索候选，S1:254/S3:84 | 只有自由文本精确匹配 | 模型候选来自当前范围全量结果；requested/observed 分开 | 长名/无数据/旧模型可查询，不限当前页模型 |
| Q06 A/B | sync/stream/ws_v2/compact 类型，S3:274 | 只有 stream bool；WS 被混为流式，compact靠端点 | `request_type`、`stream`、`endpoint` 独立；历史 unknown | SSE 与 WS 不混；compact不被误当普通生成 |
| Q07 B | native_compaction_v2，S3:132 | 未捕获 stream+compaction_trigger 语义 | 被动提取布尔/unknown；与 transport正交 | 仅确实存在且触发语义成立时 true；无请求正文持久化 |
| Q08 A | 状态、错误相位/类别、HTTP，S3:156/S9 | 状态筛选有；类别仅客户端文案，HTTP无法筛 | `status/http_code/error_category/error_phase`；分类规则版本 | 200+failed归失败；429与aborted分开；无类别为unknown |
| Q09 A/B | 请求 ID、IP、端点查找，S4:107/258/299 | 值部分存在但无筛选；协议 ID 未存 | `request_id/upstream_request_id/event_id/ip/endpoint`精确语义 | 外部req ID不冒充DB行ID；端点白名单不会丢models/compact |
| Q10 B | requested/upstream/returned model与不一致，S3:150/S4:55 | 仅请求 Model | 三种模型、映射链、mismatch nullable | 模型别名/版本变体有解释；上游无返回不能判一致 |
| Q11 A | 服务端排序，S1:122/553/S7:226 | 固定time desc | `sort_by/sort_order` 白名单，稳定第二排序ID，绑定游标 | 正反排序与同值翻页无重复遗漏；排序切换重置页 |
| Q12 A | 精确总数与分页，S1:141/S7:171 | PG total=-1；文件total可能扫描窗口 | `total_kind:exact/not_computed/scanned_window`、exact_total、has_more、next_cursor | 不能把估计/下界当总数；0条与未知总数区分 |
| Q13 A | 文件与PG一致，C2/C3 | **风险：file HasMore无NextCursor，validLogPage拒绝**；file忽略cursor | 明确pagination_mode或两存储相同游标契约 | file 51条/20页大小与PG多页都通过；受扫描上限时明示 |
| Q14 A | URL/收藏筛选，S1:320，C1 | 当前支持七字段URL和20个收藏；新增字段未覆盖 | 单一规范化QuerySnapshot，sort/size入URL；cursor只绑定signature | 刷新/后退/跨页保留；旧收藏迁移；无凭据进入URL/存储 |
| Q15 A | 慢请求防覆盖、刷新，S1:391/401/534 | 列表已有abort+revision；无并行stats/chart状态 | 各资源revision+abort，共用applied signature | slow→fast；筛选改未查询不复用旧cursor；旧数据显式标注 |
| Q16 A | 加载/错误/空态/焦点，C1 | 列表/抽屉已覆盖基础；新操作须延续 | 独立加载错误、不将失败清零为真空 | 列表成功stats失败可重试；键盘焦点返回原详情按钮 |
| Q17 A | 列配置和持久化，S1:632/674 | 固定7主要列，仅密度设置 | 字段白名单、必显主身份、偏好版本迁移 | 按需启用TTFT/成本/ID；移动端卡片有相同信息和操作 |
| Q18 A | 关联账号/KEY/组，S4:29/47/51 | 只有账号跳转，删除历史关联弱 | resource_id + name snapshot + deleted/missing | 存在时定位资源；删除时保留快照而不跳错误资源 |
| Q19 A/B | 服务端完整详情，S9:941 | 打开当前行快照；无独立detail API | `GET detail(id/event_id)` + provenance + nullable metrics | ID隔离鉴权；详情404/删除反馈；显示完整脱敏错误 |
| Q20 A | 手动/自动刷新，S1:534/S6:173 | 手动刷新存在；没有周期刷新 | 可选auto-refresh与页面可见性；清理任务轮询独立 | 自动刷新不打断输入/详情/翻页；离开页面取消 |

## 全量统计、图表与排行

| ID / 适用 | 功能 / 参考证据 | 当前差距 | 接口或数据要求 | 可验收条件 |
| --- | --- | --- | --- | --- |
| A01 A | 同筛选总请求/成功/错误，S2:12/S5 | 当前页reduce；overview仅按时间全局 | `GET logs/stats`共享全部filter，requests/success/errors | 50条分页+1001条全量时显示1001；不随翻页变化 |
| A02 A/B | 全量Token/缓存分解，S5:17/45 | overview有3字段但不支持所有filter | input/output/cache_read/cache_write/reasoning/total与known_count | 子集不重复相加；未采集记录计覆盖数不伪0 |
| A03 A/B | 平均耗时/TTFT，S5:86/S4:235 | overview平均耗时仅时间；TTFT无 | avg_duration_ms/avg_first_token_ms及分母 | 延迟0有效；未知TTFT不进入均值 |
| A04 B | 标准/用户/账号成本汇总，S5:68/77/109 | 没有定价及账务数据 | estimated/reference/charged/account_cost分开、货币、pricing coverage | 未定价不返回0；与逐行成本相加同口径 |
| A05 A/B | Token趋势及hour/day粒度，S1:489/S8:71 | overview趋势只有time，非共享筛选 | 同filter按时区bucket聚合，输入/输出/缓存/成本 | 有空bucket、边界正确；切filter全部同步；不以页数据画图 |
| A06 A/B | 模型分布三来源和Token/成本切换，S1:8/435 | 未提供 | requested/upstream/returned/mapping维度，metric选择 | 三来源标签准确；未知单独分桶；成本未知不装零 |
| A07 A | 分组分布，S1:8/S8:158 | 未提供；仅账号top10 | group统计须明确每请求单归属/多归属口径 | 总计能对账；若多标签统计不等于总量必须提示 |
| A08 A/B | 入站/上游/路径端点分布，S1:401/S8:173 | 仅记录一端点，无分布 | inbound_endpoint/upstream_endpoint/path + metric | responses/compact/models与WS路径准确；无协议改写 |
| A09 A/C | 用户Token/成本排行与点选，S1:159/270/S8 | 无用户域；overview只账号Top10 | Codex管理实现账号、KEY、分组排行；请求/各Token/成本排序，limit20/50/100/200 | 点击排行设置同维度筛选；不能叫“用户”却列上游账号 |
| A10 A | 缓存命中率与样本覆盖，C2/C4 | 当前页已有真实覆盖，overview有has_samples | sum(cache_read)/sum(input)；count与异常值计数 | 全0/unknown显示无样本；不平均单行百分比、不强造100% |
| A11 A | 缓存失效/刷新一致性，S1:401/429/534 | 各页统计无统一cache invalidation | stats version/generated_at/query signature，force refresh | 刷新后列表/分布同filter；删除后缓存与图表同步失效 |
| A12 A | 保留范围与全历史口径，C3/C5 | aggregates92d/file扫描10万/PG详情可prune且usageevents保留 | `retention/range/coverage/truncated/details_available` | 详情删后不声称历史总计也删；baseline不伪造为逐条请求 |

## 单条真实数据、Token、费用和性能

| ID / 适用 | 功能 / 参考证据 | 当前差距与来源 | 字段要求 | 可验收条件 |
| --- | --- | --- | --- | --- |
| D01 A | 稳定行标识/幂等，C3:198/C5 | EventID存在但不是协议请求ID；文件ID由行位置补 | event_id/db_id/client_request_id/upstream_request_id/response_id分开 | 重试落库不重复计费；协议ID可关联不冲突 |
| D02 B | 请求/上游ID复制，S4:258/276，O1:57/119 | 头可观测，response.id可解析但未保存 | 来源字段、长度限制、脱敏策略 | 多轮WS每轮响应ID分开；缺失为—；剪贴板失败反馈 |
| D03 B | 实际返回模型，S4:55，O1:35 | 仅请求body Model，openai-model头及response模型可观测 | requested_model/upstream_model/response_model +来源 | 上游无模型时unknown；绝不从请求模型推断已验证 |
| D04 B | reasoning强度/映射，S4:93/S10:381 | 未捕获请求reasoning.effort/上游值 | requested_reasoning_effort/upstream_reasoning_effort | 原值与映射区分；没有改写时不虚构链路 |
| D05 B | service_tier，S9:1694/S10:234/O2 | Codex支持但未存；参考类型/计费有，当前参考表无独立主列 | requested_service_tier/observed_service_tier，nullable | priority/fast/default保持真实返回语义；估价按实际已知层 |
| D06 A/B | 输入/输出/总Token，S9/O1:129 | usageFromResponse支持input/output但丢usage存在性与上游total | usage_present、input/output/total、usage_source/version | 真0与缺字段分开；上游total和计算total不同时可解释 |
| D07 B | 缓存读取与缓存写入，S4:148/S10:181/O1:143 | cached已读，**cache_write_tokens官方已支持而当前丢弃** | cached_tokens/cache_write_tokens，输入子集语义 | input100/cache_read40/cache_write60 不被算为200输入 |
| D08 B | reasoning输出Token，O1:148 | output_tokens_details.reasoning_tokens未读 | reasoning_tokens、output子集 | output50/reasoning20的总输出仍50 |
| D09 B | 上游usage元数据/rollout预算，O1:123/153 | 只保存3数字；usage_metadata/rollout units没读 | 明确白名单versioned usage metadata，原始金额/单位不能混 | 可回溯新增计量；不储存完整response/工具输出/会话正文 |
| D10 B | TTFT，S4:235/S10:432 | 仅总latency；观察到首个有效内容事件时刻可计量 | first_token_ms nullable、first_output_kind、timing_source | 不能把HTTP头/心跳/response.created当首Token；非流式未知不等于总时长 |
| D11 B | 分阶段时延，S9:941 | 未存auth/routing/upstream/response latency | 被动计时与阶段定义，不修改调度 | 多轮WS每轮独立；取消/超时显示已有观测，未知不补0 |
| D12 B | 每Token项价格和成本明细，S4:396/S10 | 无价格表，不能从usage推真实ChatGPT账单 | pricing catalog、model/tier/effective_at/version/source、各桶cost、unknown reason | fixture按价格版本可复算；旧记录不随最新价无声改动 |
| D13 B/C | 标准费用/倍率/平台扣费/账号费用，S10:299 | 本项目无用户余额/倍率账务；Codex订阅并非逐TokenAPI账单 | reference_estimated_cost与configured charged_cost分列；实扣unknown | 无账务系统显示“参考估算”，不称OpenAI实际收费；不可全填0 |
| D14 B | 不认识的模型/价格缺失，S10:264 | 无处理 | price_status:known/unknown/not_applicable，pricing_model/source | gpt-5.6-sol无确认价时仍保留请求/Token，费用—并计未定价条数 |
| D15 A/B | 零值/历史数据兼容，C3:21/C2 | omitempty省略0；UI正确视缺失未知，无法历史恢复 | 新记录显式nullable/known标志；迁移不得把省略值全当0 | failed无usage与usage:{0,0}显示不同；历史unknown保留 |
| D16 A | 错误摘要/完整详情，S9/C1 | 有分类解释和完整脱敏error，暂无独立phase/source | status/http_code/error_category/error_phase/error_source、truncated | HTML/XSS只文本；HTTP200 failed不显示成功；长错误可展开 |
| D17 B | 上游失败链/错误详情，S9:941 | 当前只终态error，重试链/分阶段未持久化 | 有限、脱敏attempt summaries与上游status/message | 不保存认证头/请求正文；缺失链路不伪造某次重试 |
| D18 A/B | UA、IP及地域，S4:294/299 | IP有、UA未存、geo无 | user_agent白名单裁剪、IP访问范围、可选缓存geo resolver | 无geo来源显示未知；批量去重；导出默认移除IP/UA |
| D19 A | 成功/失败/中断与结束状态，C6 | terminal outcome已有；记录不含原response status/incomplete原因结构 | terminal_status/incomplete_reason，error含usage可计量 | completed/failed/incomplete/cancelled及断流逐项真实fixture |

## 导出、清理与操作闭环

| ID / 适用 | 功能 / 参考证据 | 当前差距 | 接口或数据要求 | 可验收条件 |
| --- | --- | --- | --- | --- |
| E01 A | 全量筛选导出，S1:575 | **仅当前页脱敏JSONL**；CLI存储导出不替代UI | 完整筛选+排序快照、csv/jsonl流式读取，row_count/coverage | 超过500/10万条测试；导出与精确筛选总数一致或明确截断 |
| E02 A | 导出进度/取消/失败，S1:236/563/575 | 当前页同步文件，无全量进度 | AbortSignal、接收行/字节进度、完成标识 | 取消不保存半截成功文件；中途401/500/断流不可提示完成 |
| E03 A/B | 导出字段与数据字典，S1:595 | 当前安全白名单刻意移除名字/模型/错误等 | 默认脱敏；可辨析匿名ID/模型/Token/成本/timing，格式schema版本 | 不出现OAuth、refresh、API secret、认证头；CSV公式注入安全 |
| E04 A | 导出query一致性与错误旧数据，C1/C2 | 当前页dirty/error禁止已有；新全量需快照隔离 | immutable applied snapshot、服务端fingerprint/as_of | 导出时改draft不改变任务；错误旧页绝不冒作新结果 |
| E05 A | 清理范围预览与确认，S6 | 只有CLI按before删verbose logs；无UI | 同filter计数+日期+内容范围(detail/accounting)预览，确认token | “取消”0写入；筛选变更后旧确认失效；明确保留账务边界 |
| E06 A | 持久异步清理任务，S2:54/S6 | 缺接口、表、worker与UI | id/status/filter_snapshot/created_by/progress/deleted_rows/error/times | pending/running/succeeded/failed/canceled全状态，刷新页面可恢复 |
| E07 A | 批次执行/取消/恢复，S6服务/仓库 | CLI单次批量无任务生命周期 | 限量批次、任务领取锁、超时租约、取消检查、幂等 | 大数据不阻塞HTTP；重启继续/明确失败；重复取消安全 |
| E08 A | 删除后统计/图表一致性，S6:326/C5 | Prune仅删request_logs保留usage_events；语义不同于参考 | 先确定清理“详情”或“用量记录”，按范围事务更新/重建聚合 | 页面清楚说明；重建不会复活被承诺删除的usage；数值对账 |
| E09 A | 任务列表/分页/轮询/错误，S6:173 | 缺 | task list带total、10秒或自适应poll、取消权限 | 关闭dialog停止轮询；失败保留详情，不吞错/假成功 |
| E10 A | 服务端鉴权/限额/审计，C4/S6 | 现有admin session需复用，新增功能未有 | cookie session+CSRF策略，输入/范围限制，任务操作者，导出限并发 | 未登录不能下载；枚举ID不越权；不向日志/URL写secret |
| E11 A | 构建与旧壳兼容 | 用户已恢复旧UI，src完整壳不是线上 | 局部Logs build、旧依赖共享、无全局style/nav修改 | 原44资源hash保持；1440/390及320不溢出；其他业务页回归 |

## 参考中的业务/平台特性适用性

| ID / 适用 | 参考功能与证据 | Codex处理结论 / 不静默省略的边界 |
| --- | --- | --- |
| X01 C | 用户email搜索、deleted用户、KEY依附用户，S2/S3 | 现有系统没有终端用户域。账号指上游Codex身份，不能冒充终端用户。当前范围以KEY/组归因；未来建立用户域时接入同查询契约。 |
| X02 C | 用户余额历史弹窗，S1:258 | 本项目无余额账簿，不建立假余额；资源关联详情与真实用量需完成。 |
| X03 C | balance/subscription结算来源，S3:288 | ChatGPT订阅类型是上游账号属性，不等于Sub2API终端用户订阅扣费。显示账号计划可另行对齐，不能拿来填billing_type。 |
| X04 C/B | token/per_request/image/video计费，S3:318 | 现有Codex支持Token参考估价必须做；按次价格仅可来自明确配置。没有启用的图像/视频专用端点不虚构次数/费用。 |
| X05 X/B | cache_creation_5m/1h、TTL override，S4:148/S9 | Claude TTL桶当前不适用；**Codex cache_write_tokens适用，单独D07，不能随TTL一起排除**。 |
| X06 X | Gemini Live、其他平台专有transport，S3:274 | 当前未启用相关平台；仅保留可扩展类型，不显示无法产生的选项。Codex WS必须实现。 |
| X07 B/C | cyber分类，S3:274/S10:74 | Codex可有cyber/policy上游拒绝，应保留真实错误分类/usage；参考的专属cyber账务标记未必有相同本地业务，不能直接移植扣费。 |
| X08 B/X | image input/output Token与尺寸明细，S9/S10:189 | Codex多模态返回的真实Token细分可被动采集，若上游未提供不可按图片推算；独立图片生成尺寸计费、视频时长属于未启用业务。 |
| X09 C/B | account/group倍率、渠道定价、长上下文阶梯、高峰价，S10 | 没有商业配置不应凭空收取；定价计算能力可支持明确版本规则，成本解释必须表明配置来源。Codex模型/服务层费用不可因此排除。 |
| X10 A/B | 错误页resolved、owner/source/severity，S9:893 | 错误持久分类/来源适用；resolved需要真实管理状态存储和审计，不能仅本地隐藏。参考Usage只展示错误列表+详情，不据此声称它有重放/重试操作。 |

## 参考实现必须保留的事实边界

1. `UsageView.exportToExcel` 是浏览器按100条分页读取、首次精确总数、最后生成Excel，**不是服务端异步导出任务**。可用服务端流式导出实现更可靠的同等结果，但不能声称参考已具备任务队列导出。
2. `usage_log_repo_query.go` 普通列表可能使用 `offset + limit + 1` 下界而非精确总数；仅 `exact_total` 或特定强筛选走精确count。Codex应明确 `total_kind`。
3. 参考使用记录表只提供模型/创建时间服务端排序；排行另有请求、Token、成本排序。按TTFT/成本排序不是已证实的参考表功能，可作为本项目增强。
4. 清理payload仅日期、用户、KEY、账号、分组、模型、request_type/stream、billing_type；没有继承native_compaction、billing_mode、model mismatch等全部条件。Codex清理须提示实际支持范围并拒绝静默丢筛选。
5. 图表各API组装filters并非完全一处：breakdownFilters未包含所有模型条件，用户排行另传model。Codex应统一规范化filter，不照搬差异。
6. 参考默认“近24小时”先取日期字符串，后端按本地完整日期区间解析，并不是精确滚动24小时；当前Codex快捷范围采用秒级滚动区间应保留真实说明。
7. 参考Usage页有手动统一刷新、清理任务10秒轮询，未看到Usage主列表常驻自动刷新；自动刷新可作为增强，不当作已证明的参考必备项。
8. 参考模型service_tier有存储和计费字段，但UsageTable主列没有独立service tier列；仍属于Codex相关数据能力，应补解释与详情。

## 实施分组与共享查询契约

第一组不是总验收：统一过滤/排序/精确总数、同filter全量stats+分布、独立详情、完整导出；随后补真实观测/成本、趋势排行、清理任务，并按上表逐项落测试证据。

```js
// 规范化只接受白名单；值不得持有任何密钥/请求正文。
QuerySnapshot = {
  filters: { from, to, account_id, account, key_id, key_name, group_id,
    model, endpoint, stream, status, http_code, error_category, request_id, ip },
  sort: { by: 'time', order: 'desc' },
  pageSize: 20,
  exactTotal: true,
  signature: 'stable canonical representation'
}
// draft只编辑；applied每次提交生成新不可变快照。
// 列表、stats、图表分别有AbortController/revision，但提交结果必须匹配signature。
// page/cursor属于该signature；任何filter/sort/size改变均丢弃cursor/history。
// export/cleanup preview显式捕获applied快照；下载错误不回退导出旧列表。
```

建议管理接口：`GET /api/logs`、`GET /api/logs/stats`、`GET /api/logs/export`、`GET /api/logs/{id}`；图表沿stats分布或独立trend/排名接口。所有接口复用一个解析器与匹配函数/SQL构造，不重复维护筛选语义。清理任务独立持久资源；nullable metrics与pricing provenance必须进入API/schema，不能仅在前端临时猜测。

## 最低验收数据与证据

- 两个存储模式；0/1/超过页大小/超过500/超过扫描上限；同时间戳多行；ASC/DESC；稳定游标与错误游标；跨查询游标拒绝。
- 同名KEY与账号、改名/删除/组变更、长邮箱与模型、多个端点、WS与SSE、HTTP200失败/400/429/502/取消/缺结束事件。
- 无usage、明确零usage、缓存命中0/100%/非法缓存大于input、cache_write、reasoning、未知服务层/模型价格、定价版本变化、TTFT缺失/真实0。
- 全量列表/统计/趋势/排行/导出逐项对账；列表一页只20条而全量1001条时全部页外数据必须计入；不得把当前页合计改标题伪装全量。
- slow→fast、查询失败保留旧数据、draft未提交点next/refresh、导出期间换筛选、取消导出、HTTP错误下载、取消清理、任务中断重启、重复操作。
- 旧UI壳/依赖hash检查与真实浏览器1440/390/320截图，键盘/焦点/读屏标签；完整错误XSS与CSV公式注入；导出扫描不得出现凭据和原始请求内容。

完成定义：上表每个A/B项都有实现来源、接口字段、自动/浏览器证据和数据口径；C/X有清晰适用性说明。单次构建通过、当前页指标上线或第一组接口完成均不代表“使用记录全部对齐”。

## 当前证据层级（2026-09-18）

- **U：前端纯逻辑测试。** `node --test frontend/tests/log-management.test.js frontend/tests/log-cleanup.test.js frontend/tests/log-pricing.test.js frontend/tests/log-related-resource.test.js frontend/tests/log-query-state.test.js frontend/tests/log-metrics.test.js frontend/tests/logs.test.js`，30 项通过。包含快照/游标、真零与未知、下载文件验证/取消/401世代、日历粒度/DST显示、费用未知断点与小数坐标、清理确认/进度、定价与排行。
- **V：旧壳真实浏览器、合成接口。** `node frontend/tests/run-log-management.mjs .local/management-parity/dist`；[报告](../.local/logs-management/browser/report.json)与同目录1440/390/320截图。fixture运行真实Vue/Element/路由/下载，网络用独立本地mock；这不是服务端SQL、真实账单或真实上游采集证明。最新20组全部通过：10组查询/统计/详情/导出/候选/日历/手机/固定快照/请求与上游模型独立分布，3组清理，5组费率/排行/字段列/关联资源，2组真实旧runtime会话竞态。当前401确实停留登录页；迟到401不清除新登录session。
- **B：后端实现与专项测试源码。** [查询](../admin_usage_query.go)、[统计/导出/详情](../admin_usage_management.go)、[费率](../admin_usage_pricing.go)、[清理](../admin_usage_cleanup.go)、[候选](../admin_usage_options.go)；专项测试同名`_test.go`。root负责运行后端测试，不能把这些文件存在写成生产已生效。
- **P：真实PostgreSQL证据。** root已报告使用记录PG、费率PG、清理PG及候选/日历PG验证；本子任务已核对[PG查询测试日志](../.local/management-parity/usage-pg-test.log)、[PG候选/日历详细日志](../.local/management-parity/usage-options-calendar-pg.log)。后者明确执行`TestPGManagedUsageOptionsCalendar`，覆盖改名/删除候选、纽约23小时日、日历月、分组去重/重叠及匿名导出。费率/清理各自运行证据由root最终报告补齐，不以V替代。新增[快照PG成功日志](../.local/management-parity/management-pg-snapshot-lease-tests.log)明确通过TestPGManagedUsageSnapshot，覆盖晚提交旧ID/新增记录隔离与冻结费率；早期usage-snapshot-pg-tests.log是已修fixture外键错误，不作为最终证据。[日历复验日志](../.local/management-parity/management-pg-auto-calendar-tests.log)也通过真实PG。
- **O：真实采集交接。** [CODEX-SOURCE-COORDINATION.md](CODEX-SOURCE-COORDINATION.md)记录源码owner首批observer已接HTTP/SSE/JSON/compact/models/WS并通过专项；第二批来源/请求与观测拆分等仍由源码owner交付。管理子任务未改网关。旧历史字段缺失无法补回，不将当前UI或合成fixture当作所有历史记录已采集。
- **R：兼容构建。** `node frontend/scripts/build-management-parity.mjs`通过，54文件、48基线资源原哈希不变；原44旧资源和Settings overlay都保留，只替换Accounts/Logs局部模块，共享同一旧Vue/Element/API会话，无第二运行时。未部署候选，其他业务页联合回归由root/audit负责。

## 逐项实施状态：查询与使用体验

本节覆盖全部62个A/B管理与数据项目，前文参考证据/字段要求/验收条件仍有效；十个C/X边界另在后文逐项确认。“部分”表示仍有真实缺口，不以三批功能完成代替总目标。

| ID | 已落实现与证据 | 尚未完成或证据边界 |
| --- | --- | --- |
| Q01 | 绝对秒级范围、设备本地日期输入、URL `timezone`，列表/详情/趋势按指定IANA显示；step支持分钟/5分/小时/日/周/月。U、B、P日历/DST，V接口一致性。 | 日期输入明确使用设备时区，不伪装指定IANA墙上时间；范围查询仍是同一绝对时刻。新增as_of固定记录ID与费率快照，详情被并发清理后不可恢复，不能声称永久历史快照。 |
| Q02 | `key_id/key_name`、全留存范围远程候选、同名按ID、已删除标记、模糊检索最多50项并提示缩小条件。B/P候选，V。 | 候选限制明确，不声称前50项是全部；资源详情跳转见Q18。 |
| Q03 | `account_id`优先、远程候选使用当前资源名称与历史ID，改名/删除可追溯；选中清名称避免重复约束。B/P、V。 | 不是按当前页推候选；当前无终端用户实体。 |
| Q04 | `group_id`全链查询，候选和统计按请求当时Groups快照；重复组成员在单条内去重，多组可重叠。B/P、V。 | 历史只存ID时显示ID，不能伪造历史组名；资源页联动待Q18。 |
| Q05 | 模型远程候选来自同filter全部留存记录，手动精确模型仍可用；空态/加载/取消/已截取提示。B/P、V。 | 请求模型候选已做，返回模型的独立来源候选需完整模型链字段见Q10。 |
| Q06 | 类型/stream/endpoint独立筛选，HTTP流式/非流式/WS/compact/models明确，历史unknown不推断成新类型。B、V、O首批。 | 每种真实客户端覆盖最终由源码owner观测专项确认。 |
| Q07 | `native_compaction=true/false`可选布尔筛选共享列表/stats/export/cleanup，详情未知保留—。U、B、V。 | 第二批observer对真实compaction_trigger语义采集证据待O；缺失不能匹配false。 |
| Q08 | 执行状态/HTTP/错误类别/错误阶段共享查询，HTTP200 failed独立标记，友好分类保留完整已存脱敏错误。B、U/V。 | 错误阶段/来源的真实分支填充等待O第二批，分类版本字段尚未对外输出。 |
| Q09 | 本地event/db ID与上游request/response ID查询，IP、端点完整值筛选；详情复制协议ID。B、U/V、O首批。 | 入站/上游/路径三种端点未分开，见A08。 |
| Q10 | 入站Model、最后实际发送SentModel、真实响应ReturnedModel已由被动observer独立持久化；UI详情/可选列/精确筛选和请求-发送、发送-返回两种差异筛选已接。旧UpstreamModel兼容字段及旧model_mismatch保留。B真实HTTP401刷新链至record/admin的E2E、PG与U/V。 | 差异只在模型对两端都有观测时判断；兼容header值不充当返回模型。历史缺失不回填，512字节合法模型不被前端500字符旧限制截短。 |
| Q11 | time/model/总Token/输入/输出/缓存/latency ASC/DESC，稳定游标；改变排序重置第一页。B/P查询、U/V。 | 成本/TTFT列表排序是额外增强，参考主表只证实time/model，不虚构为必缺项。 |
| Q12 | exact_total开关，total_kind与当前页条数分开，游标上一页/下一页/首页及URL恢复；1001全量对10当前行。B/P、U/V。 | 新增as_of冻结记录ID集与费率；并发新增不会纳入，已删除明细可能减少，过期409提示刷新。 |
| Q13 | 文件全扫描及有效游标，PG稳定游标；旧十万尾部扫描限制已由管理查询替代。B专项文件10万+、P查询。 | 前端V不代替两个真实存储的排序/分页测试；文件模式不承诺清理worker。 |
| Q14 | 所有已支持过滤/排序/step/timezone统一白名单、immutable snapshot、URL与20组收藏、游标签名。U/V。 | 收藏保存绝对时间，文案明确；不保存凭据、正文或导出身份选择。 |
| Q15 | list/stats/detail/options分别Abort+revision，draft只编辑；错误标旧结果；draft刷新清游标，翻页不重算全量stats。U/V。 | as_of绑定fixed IDs/pricing，变更筛选/刷新更新快照，翻页仍复用；不进URL/收藏。 |
| Q16 | 列表、统计、详情、候选、导出、清理、定价独立loading/error/empty/retry；详情焦点返回、键盘操作、手机卡片。V、R。 | 全套键盘/屏幕阅读器人工审计尚未证明；已修查找按钮外层label多控件语义。 |
| Q17 | 八项显示列白名单（身份/模型/Token/耗时费用、ID/IP/UA/分组），时间/结果/详情固定；取消不改、应用持久、手机同步，默认四组保持；舒适/紧凑。U、V已通过。 | 独立可选字段已补；不通过新增列扩大任何导出权限。 |
| Q18 | 账号按稳定ID跳账号管理；KEY/组从请求详情与可选列打开当前资源弹窗，现有API按稳定ID查找，缺失保留历史且不跳同名，允许筛资源日志或进入真实管理列表。纯helper允许字段测试，V已通过。 | 已核对旧Keys/Groups不读ID路由，故精确定位在资源弹窗完成，管理列表入口不谎称过滤定位。弹窗不保留完整/掩码KEY及任意凭据字段。 |
| Q19 | 独立GET详情，加载/404/重试，列表快照与服务端记录标记，完整已存字段/脱敏错误，不替换成错误新数据。B、V。 | 原始未持久化/被截断内容无法恢复；不承诺完整上游原文。 |
| Q20 | 手动刷新统一已应用查询，输入改动则提交新查询；清理任务10秒可见性轮询，关闭取消。U/V。 | 主列表自动刷新为可选增强；参考Usage主列表未证明有自动刷新，不能列作未对齐必需项。 |

## 逐项实施状态：全量统计、图表和排行

| ID | 已落实现与证据 | 尚未完成或证据边界 |
| --- | --- | --- |
| A01 | 同filter服务端total/success/errors/aborted，1001匹配不当作10行本页；统计独立失败重试。B/P、V。 | 只统计留存明细，不冒充生命周期累计。 |
| A02 | input/output/cached/total全量，reasoning/cache_write含样本覆盖；未知不归真0，子集不重复相加。B/P、U/V、O首批。 | 无法回填旧记录遗漏桶；真实采集覆盖由样本数体现。 |
| A03 | 同filter平均总耗时、nullable平均TTFT和采样数，TTFT真0有效。B、U/V、O首批。 | TTFT实测含义必须由observer首次有效输出定义，不能用HTTP头到达代替。 |
| A04 | 参考USD、独立采购account_cost_usd、配置售价sale_cost_usd三金额域及各自覆盖数；汇总、五维分布、趋势与账号/KEY/成员组排行可切换，未知断点/末位、真零保留。服务端同snapshot冻结三套规则。B费率专项、U/V。 | 配置售价只依显式KEY计价组，不拿账号成员组推断；成员组图不是计价组图。三金额均非ChatGPT实际扣款，实际账务不伪造。 |
| A05 | 请求/总Token/输入/输出/缓存/费用趋势，step/timezone共享查询；月为日历月、周一周桶，前端不按固定86400填DST日。B/P、U/V。 | root已新增fillSeries按日历补空桶（无请求费用0，有请求未定价null），源码落盘、日历PG已通过；前端费用未知断点单测通过。 |
| A06 | models/upstream_models保留，新增sent_models/returned_models独立服务端分布，与Token和三cost切换一致，可点精确filter；仅非空实际观测计入。B真实observer持久化/PG、U/V。 | 分布合计可少于全量请求，不补未知；原header兼容模型仍独立，不伪造请求/发送/响应相同。 |
| A07 | `groups`全量统计、分组分布和排行；名称/ID可点选，显示一请求多组的重叠口径。B/P、V。 | 合计不等于去重全局总数是明确设计，不伪造占比总和100%。 |
| A08 | 原endpoint分布保留，新增标准化inbound_path/upstream_path精确filter及inbound_paths/upstream_paths服务端分布；独立于旧端点，支持Token/三cost和点击筛选。B真实采集/PG、U/V。 | 只保存路由白名单标准路径，不记录域名/query；未知路径不复制另一侧。标准路径不等于完整原始URL。 |
| A09 | 账号/KEY/分组排行20/50/100/200，请求/总Token/输入/输出/缓存/参考成本及额外成功率排序；点击稳定ID过滤。B、U/V。 | 终端用户排行无对应业务实体；成功率属本项目增强，不声称参考已有。各资源零分母显示未知。 |
| A10 | 总cache/input加权比例、全未知/0输入无可算样本，原始Token覆盖数明确。B、U/V。 | 服务端异常桶计数未单独提供；不能只以已知比率掩盖异常记录。 |
| A11 | POST snapshot先冻结PG可见IDs/文件边界与参考/采购价格版本，五分钟as_of后list/stats/options/export/detail共享；详情捕获其已显示列表成功响应的原scope，不混用加载中的新scope，409保留原值并提示刷新。B/P真实快照专项与V新增详情组通过。 | 并发清理的详情不可恢复，非成员404；独立错误工作区详情明确用当前费率而非主列表snapshot。不是max(id)近似快照，不进URL/收藏。 |
| A12 | scope全链明确，按需GETcoverage查询整体明细数量/实际首尾/存储/手动清理规则/累计事件保证/schema。B、V已通过。 | 未保证留存天数，不把dashboard92天常量冒充明细留存；最早记录不证明历史完整，删除明细不从累计量伪重建。 |

## 逐项实施状态：真实观测、费用和错误

| ID | 已落实现与证据 | 尚未完成或证据边界 |
| --- | --- | --- |
| D01 | event/db/request/response ID分开，查询和详情保留稳定ID，后端现有event幂等。B/P、V。 | 新字段上游与本地ID来源由O确认；UI不证明并发落库幂等。 |
| D02 | request/response ID显示与复制、复制失败提示；首批HTTP/WSobserver已交接。V、O。 | 历史缺失仍—；不同WS轮次真实隔离由源码测试确认。 |
| D03 | sent_model与returned_model真实观测已接：发送字段来自准备后实际最后发送，返回字段只取真实response对象/事件；UI三段链及差异明确。B真实source/PG与V。 | UpstreamModel可能来自header或response的兼容语义不改变；没有返回时保持未知，不把请求值或header作为返回证据。 |
| D04 | 请求推理强度、观测推理强度、来源分开展示，筛选实际兼容字段。B、V。 | 第二批请求/响应来源分离待O证据，不把请求值标成上游确认。 |
| D05 | requested/observed service_tier+source，费率按实际有据字段匹配model/tier/version。B、V。 | 第二批source填充与真实priority/default响应证据待O；未知费率为—。 |
| D06 | usage_known明确，真0/历史unknown区分，展示上游usage_total_tokens及计算input+output。B、U/V、O首批。 | 上游total/source新增采集待O第二批，usage schema/source版本字段未建立。 |
| D07 | cache_read/cache_write分桶、nullable+样本数、细节提示是input子集；参考价先扣缓存读写。B价格算术、U/V、O。 | Claude 5m/1h TTL不适用，不因此遗漏Codex缓存写入。 |
| D08 | reasoning_tokens为output子集，行/汇总有覆盖，未采集—。B、U/V、O。 | 已支持字段不能推测历史reasoning占比。 |
| D09 | 白名单usage_total/rollout_budget_units/usage_amount保留，新增observation_schema_version及usage_schema_source随真实observer写入并在详情显示。B真实E2E/PG、U/V。 | schema来源不是完整上游body；rollout/usage_amount未知单位不转USD，不能凭本轮版本字段宣称多模态usage或所有历史元数据已齐全。 |
| D10 | nullable TTFT/first_output_kind，真实0可显示，聚合按样本数；首批observer声明只首次有效输出。B、U/V、O。 | 第一输出种类与阶段来源新字段待O第二批；不将非流式总时长填TTFT。 |
| D11 | 新增request_preparation_ms/account_selection_ms/proxy_selection_ms三nullable阶段，详情和全量均值/样本数实际接入；真实0计样本、nil不补0。B真实observer/PG、U/V。 | 三阶段可能重叠不可累加；账号选择可包含等待/OAuth。仍未独立观测的auth、DNS、connect、TLS不伪补；WS首轮握手可长于当前turn时间。 |
| D12 | 不可改写费率版本新增/列表，模型/层/生效时间/来源，input/cache/cache_write/output明细；长上下文与分时IANA/工作日/时段倍率组合，服务端quote显示实际命中与原因，无默认假价。B分时/DST/版本专项、U/V。 | 未来版本不静默覆盖已有生效段；未命中分时按1，旧规则不补默认时段；配置费率不等于订阅账单。 |
| D13 | 独立采购规则已具备真实管理实体：账号/分组目标、模型/服务层、人工四桶单价、倍率、供应方/来源、生效与启停版本；日志详情与汇总单独显示采购估算及已估/未知覆盖。B、U/V及账号子任务验证。 | 采购估算不代表上游实际扣费，也不建立用户余额扣账；未配置/归属冲突保持未知，不复制参考售价冒充采购成本。 |
| D14 | unknown reason友好解释，缺价/缺usage/独立写入价但无写入量均保留—；真实0有明确格式。B、U/V。 | 未知模型仍保留请求/Token，不要求凭空补价格。 |
| D15 | 新日志usage_known，历史非0证据可known，历史全0且无标志为unknown；各零值指标不使用truthy判断。B、U/V。 | 旧省略数据不可逆恢复，明确不做伪回填。 |
| D16 | 分类/HTTP/status、已保存错误与phase/source保留，新增error_truncated三态和精确筛选；详情明确保存片段及未知完整性，XSS仅文本。B真实截断/脱敏后计量回归、PG、U/V。 | false只证明记录诊断未截断，nil不证明完整；有限WS握手诊断不称完整上游正文。历史缺标志保持未知。 |
| D17 | 最多4条GatewayAttemptSummary已由真实HTTP/WS/OAuth派发分支持久化，含序号、phase、标准endpoint、nullable status/duration/header与有限错误码；UI按开始顺序展示并说明attempts_truncated。B真实HTTP401→OAuth→重派发E2E/PG、U/V。 | 派发不代表上游接收或接受；刷新缓存/锁等待不伪造发送。WS握手仅首准入轮，后续nil不复制；cap后未记录尝试不会凭空重建。 |
| D18 | IP/UA详情和默认脱敏导出保持；本轮增加明确手动GeoJS查询、服务端24小时缓存/非公网拦截/逐项错误、详情来源与时间/刷新。已有来源证据为参考 `src/utils/ipGeoLookup.ts`。单IP和本页批量旧壳浏览器共5组通过：先预览再显式查询、公网去重/非公网排除、每50顺序批次、取消和逐项失败；默认无外发。 | GeoJS是外部近似地址来源，必须明确点击后才外发公网IP；不能推断精确位置。批量当前页去重与取消已通过本轮报告；它不查询未显示的历史页面。 |
| D19 | ok/error/aborted与HTTP区分、terminal_status/incomplete_reason/first_output_kind可选详情。B、V源码、O第一批终态。 | 第二批完整终态原因真实HTTP/WS采集fixture待O，不以HTTP200推completed。 |

## 逐项实施状态：导出、清理和兼容

| ID | 已落实现与证据 | 尚未完成或证据边界 |
| --- | --- | --- |
| E01 | 服务端完整CSV/JSONL、samefilter/sort；X-Export-Count校验；1001全量对10本页浏览器，文件10万+后端测试。B/P、U/V。 | 浏览器mock1001不替代后端大数据验证；当前流式接收最终Blob需浏览器内存，非持久导出任务。 |
| E02 | 服务端准备/接收字节进度、Abort、长度/类型/附件/行数验证；JSON错误/断流/取消拒绝保存。U/V。 | 当前登录401与旧401新登录竞态实际旧runtime验证见V会话组，不仅toast。 |
| E03 | 默认redacted=true移除名称/代理/IP/UA/原错误，匿名稳定标识保留；显式身份字段开关不持久化，每次打开重置安全默认。B/P匿名测试、U/V。 | root已新增X-Export-Schema: codex-usage-v2；完整字段字典仍需文档化；凭据永不包含，完整管理错误仍脱敏。 |
| E04 | immutable filter+as_of固定ID/价格快照，draft不改任务、错误不导旧数据、身份选项开始后禁改。U、V已通过。 | 导出过程中并发被清理详情不可恢复；明确实际X-Export-Count，不伪补已删行。 |
| E05 | 明确from/to过去范围→签名预览5分钟→确认checkbox→创建，取消零写；改范围废旧预览。B、U/V。 | 删除范围仅request_logs详情，账务保留明确；不把预览按钮当删除。 |
| E06 | 持久job资源pending/running/succeeded/failed/canceled，列表恢复及操作者/快照/进度/错误。B迁移+worker、U/V。 | 真实PG运行由root验证，不支持文件模式删除，界面显示服务端能力错误。 |
| E07 | 分批进度、取消二次确认、已删不恢复、UUID创建幂等。B测试含1205条、1000/205 checkpoint、新实例恢复、老ID晚提交排除、取消/事务回滚。 | PG完成日志由root整体报告补充；V只证明管理交互，不证明崩溃恢复。 |
| E08 | 清理保持usage_events和累计额度/Token，detail stats/图表清理后刷新。B生命周期对账1207 events/2 retained logs、V。 | 与Sub2API删除usage语义不同而明确告知，未获删除账务授权。 |
| E09 | 分页任务列表、10秒dialog可见轮询、关闭取消、独立error/retry，失败不虚假100%。U/V、B。 | 多管理员任务权限范围由后端admin域控制，无不存在的终端用户权限假设。 |
| E10 | 复用同源HttpOnly session/API鉴权；手写下载capture authGeneration，当前401 clearSession+跳登录，旧/取消401不清新会话。B鉴权专项、U/V。 | 全量导出并发/资源限额及审计覆盖需root终验，浏览器不替代服务端安全边界。 |
| E11 | 旧壳局部build、原48基线资源哈希未变；1440/390/320无横向溢出，原生表单不依赖缺失Vue directive。R、V。 | 联合其他业务页四主题回归和部署验收由root/audit；本子任务未部署。 |

## C/X适用性复核

| ID | 当前处理 |
| --- | --- |
| X01 | 无终端用户实体，不把上游账号叫用户；账号/KEY/分组全量归因已接。 |
| X02 | 没有余额账簿，不显示假历史余额；资源关联不足继续列Q18。 |
| X03 | 上游ChatGPT订阅计划不冒充本地下游billing_type，当前无balance/subscription账务过滤。 |
| X04 | Codex Token参考价已接；按次/图片/视频价格没有配置和启用业务不计算假费用。 |
| X05 | Claude TTL不适用；Codex cache_write已单独做D07并保留真实unknown。 |
| X06 | Gemini Live等未启用；Codex WS明确类型与O采集来源。 |
| X07 | 真实上游策略拒绝保留错误类别/原脱敏错误；未建立cyber专属扣费标记。 |
| X08 | **B待来源：**Codex返回的image Token细分尚无持久字段/fixture，不能猜图像用量；独立视频/尺寸计费为X。 |
| X09 | 参考价长上下文/分时、完整销售目录、采购阶梯已完成；completion新增完整版本化有序采购策略、多账号/显式计价组OR、跨规则顺序、同规则精确模型优先后通配列表顺序、显式渠道follow_sale_base及账号倍率覆盖。自定义命中未知不绕过；停用策略版本恢复旧规则mode，不改历史。三domain同snapshot/stat/export。 | 所有金额保持管理估算，不冒称上游实扣。自定义采购不增加参考不支持的分时规则；跟随使用未舍入售价基数而非反除最终金额，真实后端精度/PG由root专项证明。 |
| X10 | 新增持久review状态及审计：独立错误列表继承已应用filter、未处理/已处理/全部、分页集合变化409；详情CAS版本保存、备注脱敏、操作者会话指纹、历史50条与真实累计。原HTTP/status/费用不改，不重放请求。旧壳5组浏览器/3项逻辑与root真实file/PG专项通过。错误owner/severity分类仍需真实来源，不把管理员会话指纹当终端用户实体。 |

## 剩余A/B清单与下一阶段验收

1. **模型及路径链（Q10/D03/A06/A08）本轮已接：**inbound Model、SentModel、ReturnedModel、标准入站/上游路径由实际发送与响应observer采集，管理筛选/详情/分布独立支持。真实E2E及PG证明来源，不再列作未持久化。
2. **观察数据边界（Q07/D04–D06/D09–D11/D16/D19/X08）：**本轮已接schema版本/usage来源、准备/账号选择/代理选择阶段以及截断标志。仍不声称多模态Token或未独立观测的auth/DNS/connect/TLS阶段已齐全；旧历史缺失不回填。未覆盖来源继续以源码owner独立证明为准。
3. **错误链（D17/X10）本轮已接：**最多4条实际派发尝试及截断状态已有真实HTTP401刷新链至record/admin的PG E2E；管理处理状态继续独立CAS/审计。派发不等于上游接受，WS握手不复制到后续轮，不能把有限摘要说成完整无限重试链。
4. **共同快照与覆盖验收（A11/E04/A12）：**as_of固定ID+价格、过期刷新、按需coverage已落，V及PG快照已通过。cache/input只在有效输入范围计算；单独异常桶计数未见参考必需项证据，属于后续观测增强，不把它作为本轮管理A项阻塞。
5. **信息入口验收（Q17/Q18）：**已补独立ID/IP/UA/分组列与稳定ID资源详情、删除/重名保护，V已通过；旧管理路由不支持ID定位时不伪造跳转。
6. **图表边界验收（A05）：**root已补日历空桶源码，其真实PG已复验通过；前端已修未知费用伪0/微额坐标问题，分布/排行标注各项定价覆盖。该项已通过后端日历专项与前端费用断点验证。
7. **导出文档与联合验收（E03/E10）：**schema版本响应头已落，root正在同步正式接口/字段字典；同源鉴权、默认脱敏、明确身份选项、限时准备与session竞态已完成。并发任务队列导出未在参考中证明是必需项，不将其当对齐阻塞。
8. **可配置管理项（D18/X09）：**GeoJS和当前页批量查询已验收；Codex长上下文、分时参考价以及账号/组采购归属和倍率现均有真实配置接口与UI。人工采购/参考价不作为实际账单；没有的用户账本和网关渠道路由不伪造。

9. **commercial 后四项进展：**completion已实现采购跟随售价倍率前金额、有序多目标成本规则、租期bundle迁移及账号活跃日/费用日趋势；分别有Logs51、lease3与账号日统计专项证据，由root统一冻结验证。渠道参考没有单独价格目录导入/导出入口的证据，不自行扩大为必缺。被动观测来源B项仍需独立核验，不能因管理四项完成声称历史采集数据齐全。

资源候选、step/timezone、分组分布、各Token与成功率排行、默认脱敏身份开关已从剩余清单转为本轮实现，但仍须以上述最新V/P证据判定。整个矩阵仍有来源相关B项与文档/联合验收边界，**不能把前端管理闭环完成表述为所有观测字段已有真实来源，也不能声明所有历史数据已补齐。**

## 最后交接状态

2026-09-18，Logs 源码冻结给联合构建。旧运行时产物 `logs-managed-CQbXepWE.js`，20组浏览器/30项纯逻辑测试通过，无pageerror；固定快照、覆盖范围、字段列、关联资源和上游声明模型独立分布已纳入最后一轮。截图为合成数据，不是生产账号实测。可运行入口为 `frontend/tests/run-log-management.mjs`，不要使用旧单Logs构建覆盖Settings/Accounts。

本轮可独立于网关完成的管理A功能已落实上述界面与接口；root仍负责统一数据字典、全后端与联合发布验收。剩余B观察来源按源码owner交接推进，尤其完整模型/端点链、阶段计时、上游attempt摘要和多模态usage。地域/实际商业账务依赖不可捏造。

最终联合构建保留 `accounts-managed-AJ1IN3_r.js`，48个基线资源哈希不变，共54文件；[构建manifest](../.local/management-parity/build-manifest.json) SHA-256 `d7e2fd3de502ac63aeaa9f994a5d6293f3000a101009be4caad0325145aa948d`，前端文件集合SHA-256 `a0d44dcc6bc577905a25d5cc47c22b09d25bf7dbc111a1fb946b192d664c3d26`。

## 后续管理补齐（2026-09-18，独立候选）

上一冻结20组报告与8878b6候选保留。本轮 `--iteration followup-20260918` 输出独立目录，首次旧壳30组浏览器通过、0页面异常，Logs纯逻辑35项通过；最新证据在 [本轮浏览器报告](../.local/management-parity/followup-20260918/logs-browser/report.json)。首轮覆盖长上下文配置/阈值quote2组、错误管理5组、单IP GeoJS手动查询3组。网络为独立本地合成mock，真实Go/PG由root和management_backend专项另证；不能把浏览器地名fixture当真实地理位置。当前页批量IP查询最终两组已通过。

### 本轮最终冻结证据

- **旧运行时浏览器：32组通过，页面异常0。** 命令：`node frontend/tests/run-log-management.mjs --dist .local/management-parity/followup-20260918/dist --output .local/management-parity/followup-20260918/logs-browser`。包含原20组、长上下文配置与quote2组、错误处理5组、单IP/批量Geo5组；不把mock页面当真实GeoJS响应或真实Codex计费样本。
- **逻辑测试：Logs35项 + Geo7项通过。** 原30项之外，费率阈值/倍率/quote新增2项，review管理字段/CAS/独立filter新增3项；Geo7项含规范化、非公网过滤、来源校验、显式请求、50批次顺序和取消。root另运行统一前端全套。
- **视觉：64张四主题截图通过，并额外覆盖320px。** `logs-shell`、`long-context-form/rules/quote`、`error-review-list/panel`、`geo-detail/batch`分别light/dark/ocean/mint ×1440/390；报价及阈值截图直接定位实际内容。root/resource_frontend与management_audit交叉目视review、pricing、Geo。Geo局部样式改用旧壳主题变量，暗色文字和边框已复查。
- **真实后端：** root报告本轮Linux PostgreSQL管理race 73个顶层、124测试事件全部通过，含review/长上下文/Geo与账号运行状态；对应执行证据由root归档。未改任何Codex网关实现。
- **最终产物：** `accounts-managed-DeD8Vq7Q.js`、`logs-managed-CyzmCKOe.js`，54文件，48基线资源哈希不变。[本轮构建manifest](../.local/management-parity/followup-20260918/build-manifest.json) SHA-256 `8d9faa86fca4f023c939cc6a38fda26741bc35b44b717897822ae9a29e1eb5db`；前端文件集合SHA-256 `b9a485f1c655f13b1dde04899feb2470be8a8da85fa1d049c56632a20a529fed`。上一冻结候选保留，未覆盖。

## 分时与采购管理补齐（独立 pricing-next 候选）

2026-09-18，新候选位于 `.local/management-parity/pricing-next-20260918`，不覆盖上节冻结产物。源码未修改网关转发、协议或调度。

本节“未修改网关”指管理任务的文件归属；同一工作区源码 owner 的独立网关改动也会进入统一 Go 归档，不能声明整个共享源码与上轮网关逐字相同。

- **分时参考价：** `LogTimePricingEditor.vue` 支持显式启用、IANA命名时区、工作日、1–48时段增删及倍率；数字UTC偏移、Local、非法/重叠/跨午夜时段和超范围精度会阻止提交。规则真实POST至既有immutable费率接口，取消零写、409保留草稿、提交中锁定。`LogTimePricingQuote.vue` 显示服务端本地RFC3339时间、命中/周末/时段外/无效配置原因及实际倍率，与长上下文相乘但不自行推断命中。
- **独立采购成本：** Logs入口接`AccountCostDialog`，详情用独立`AccountCostQuote`，原参考价保留。汇总/请求与上游模型、端点、组、错误维度/趋势/排行支持`account_cost_usd`及`costed_requests/uncosted_requests`；真实零与未定价分别显示，采购成本不拿参考价补齐。采购规则管理操作由账号子任务独立验证，本日志子任务验证集成与金额口径。
- **详情一致性修复：** 详情使用其已显示列表成功响应的完整filter+as_of，避免加载中的新筛选或后来新增费率改变旧详情。快照409保留打开时记录并要求显式刷新，非成员后端404；独立错误工作区明确按当前费率读取且不带主列表as_of。新增费用后原unknown不被详情偷偷改为known。
- **验证：** 旧壳38组浏览器、49项Logs/Geo纯逻辑通过，页面异常0。分时新增3组、采购集成2组、详情快照新增1组。新增区域及旧关键区域共128张四主题1440/390截图，另含320表单/手机边界；resource_frontend和management_audit交叉目视，无未处理视觉问题。真实Go/PG与race最终结果由root单独归档，不将前端合成费用当真实账单证据。
- **证据与产物：** [浏览器报告](../.local/management-parity/pricing-next-20260918/logs-browser/report.json)、[机器验证](../.local/management-parity/pricing-next-20260918/logs-verification.json)、[构建manifest](../.local/management-parity/pricing-next-20260918/build-manifest.json)。最终采购首版必须启用的UI校验修复后，Accounts为`accounts-managed-D_F84EDE.js`、Logs为`logs-managed-Dac1Oqqj.js`；此最终构建重新通过38组日志行为及128张四主题截图，未复用旧JS行为证明。共54文件/48基线资源哈希不变。Manifest SHA-256为`39bf89b3f14ac76511cf94a01ec61f42c6e99540654ab693ccc09b69d55b25ee`，前端集合SHA-256为`01595549e741c1e113e2f70da1bf75ca5d64e2a99cf9deef56c685a7cc015fa7`。

## 显式销售目录补齐（独立 commercial 候选）

2026-09-18，`commercial-20260918` 完成渠道/组/KEY显式配置售价管理，保留既有蓝色旧壳。售价并非实际收款，不参与网关转发、账号选择或额度账务。

- **完整目录管理：** 渠道启停/模型价格CRUD；计价组启停、渠道关联、模型覆盖、默认倍率、仅订阅模式高峰；KEY按稳定ID唯一绑定计价组与倍率覆盖。条目及模型先编辑子草稿，再一次原子CAS发布完整不可变目录。取消零写，关联删除先解除引用，版本冲突保留草稿且禁止盲重试，历史目录直接查看真实价格/分时/长上下文/高峰。
- **计价解释：** 组模型覆盖优先于渠道，组价不叠渠道分时；KEY倍率替代组倍率，高峰再乘。四桶价格真0、KEY倍率0和高峰0都保留。长上下文草稿显式关闭后再次编辑不会重新开启；只读规则用实际0回退1语义。quote显示策略版本、唯一计价组、所选价格来源（可能基础参考目录回退）、倍率来源和时区，预览只针对已保存目录与真实记录ID。
- **三种金额域：** 参考价/采购成本/配置售价独立汇总、覆盖数、趋势、分布及排行。sale_cost_usd缺失不由另外两种金额补齐，未知时间桶断点、未知排行置后。图表中的组仍是请求时账号成员分组，明确不是KEY唯一计价组。
- **验证：** Logs44组旧运行时浏览器、56项辖内纯逻辑全部通过，pageerror0；新增销售6组覆盖创建/时段与高峰/唯一绑定/敏感字段/完整发布/冲突/历史/预览404/独立金额。192张四主题1440/390图及320边界通过；长表单另拍订阅高峰0与应用按钮底部。排行DOM实测每行clientWidth=scrollWidth=1138、末成本cell=135且rightOverflow=0，未将截图库边界误当产品溢出。Accounts46组和采购区间11项helper另由audit验证；真实Go/PG/race由root单独归档。
- **最终证据：** [Logs报告](../.local/management-parity/commercial-20260918/logs-browser/report.json)、[机器验证](../.local/management-parity/commercial-20260918/logs-verification.json)、[构建manifest](../.local/management-parity/commercial-20260918/build-manifest.json)。Accounts `accounts-managed-BGYtKyiw.js`、Logs `logs-managed-C_J6Btx1.js`，54文件/48基线原资源保持。Manifest SHA-256 `fa845eab012f20603703d31488415b26b9adf9eb594c5d5318cd95c1324ddcb9`，前端集合SHA-256 `e93f5b75148d01d5ec366b03d2628657f0763bfe45ed45cc000449e3db170bd4`。旧冻结候选未覆盖。

本轮不将无证据的渠道独立导入导出列为必缺；继续推进的已确认项为采购跟随售价倍率前金额mode、顺序多目标成本规则、账号租期bundle迁移、活跃日与费用日趋势，以及此前明确的真实观测来源B项。

## 有序采购与管理补齐（独立 completion 候选）

2026-09-18，completion新增R1/R2独立采购策略管理并修复真实空库发现的售价目录空选择占位。旧商业冻结产物与本机固定预览保持不变。

- **完整策略：** `LogCostPolicyDialog`、`LogCostPolicyPrices`、`logCostPolicyState` 和独立API提供不可变整体CAS版本、历史查看、规则及模型价格上下重排、多账号/显式计价组OR、四桶及Token阶梯、默认与账号覆盖倍率、显式渠道跟随。目标名称仅作候选，payload只稳定ID；不复制任何凭据，也不把账号成员分组当计价组。
- **真实匹配口径：** 跨规则按数组优先；规则范围命中但模型未命中继续下一条；同规则精确模型先于通配，通配按价格列表顺序。规则命中但用量未知不会跳后续价格。没有custom匹配时仅列出的活跃关联渠道可follow_sale_base，排除KEY/组/高峰倍率；未知源保持未知，倍率0不反除售价。策略停用是显式后续版本，恢复旧单目标规则mode。
- **出处显示：** AccountCostQuote与LogCostPolicyQuote展示legacy/ordered_rule/follow_sale_base、策略/命中规则顺序/跟随销售版本、格式化基数与组合采购倍率。它继续使用`account_cost`金额域，没有新增第四套成本。策略预览只读取已保存目录与留存请求，不发送上游；未发布草稿不混入预览。
- **验收：** Logs51组真实旧运行时/本地独立mock全部通过，pageerror0；62项辖内纯逻辑通过。新策略7组覆盖OR/稳定ID、规则与价格重排、取消/删除取消、wholeCAS忙状态、冲突保稿、历史、follow/unknown/404预览、显式恢复旧mode、取消迟到响应。空售价目录另断言下拉值为`''`与“尚无目录”option一致。256张关键区域四主题1440/390图和320边界完成；双方目视新策略目标、价格、跟随、来源与顺序，无未处理视觉问题。
- **界限：** 上述浏览器使用合成API，不能代替真实PG规则选取、精度、冻结快照/导出验证。root负责这些后端证明；租期bundle与账号每日统计由对应owner独立验收。账号统计跳日志是新查询，保留ID/from/to/timezone/step而不承诺沿用统计原as_of。
- **固定构建：** [浏览器报告](../.local/management-parity/completion-20260918/logs-browser/report.json)、[机器验证](../.local/management-parity/completion-20260918/logs-verification.json)、[manifest](../.local/management-parity/completion-20260918/build-manifest.json)。Accounts `accounts-managed-CuBOfqxs.js`，Logs `logs-managed-E0IrKgCL.js`；54文件及48旧基线资源字节未变。Manifest SHA-256 `ebd3d90fbeae6debe7ba78ea3f80f20575982646b2d5daa4c192ddd6a7b19ebb`，前端集合SHA-256 `cb1045b21245a667786588bb7b7e92be777a44429cfea2bdbe43795f19267e85`。

## 生命周期观测管理接入（独立 observations 候选）

2026-09-18。管理UI接入源码owner实际新增的observer字段，没有在本管理子任务改写网关。共享源码归档包含owner独立网关改动，不能声称整个共享源码与旧网关逐字不变。来源语义见 [生命周期观测合同](CODEX-STRICT-LIFECYCLE-OBSERVATION-2026-09-18.md)。

- **查询与展示：** 新增sent_model/returned_model/inbound_path/upstream_path精确筛选，sent_model_mismatch/returned_model_mismatch/error_truncated三态筛选，完整贯穿URL/收藏/snapshot/list/stats/detail/export/cleanup。四类模型可保留合法512字节值，超出接口1024UTF8字节明确报错。旧兼容upstream模型和旧mismatch不改口径。
- **详情与可选列：** 新增“链路观测”可选列且手机同步；LogObservationDetails分开模型三段/兼容来源、标准路径、nullable阶段、按开始顺序最多4次派发、截断与schema。真0不当未知、缺状态/headers不补0，WS101只归首轮观测，后续缺链不复制握手。“已保存错误”标记是否截断及未知完整性。
- **统计：** sent_models/returned_models/inbound_paths/upstream_paths只汇总非空真实值，与Token和三费用域一致；点击维度精确筛选。准备/账号选择/代理选择平均值按各自真实样本数计算，0有采样而nil未知，阶段可能重叠不能累加。
- **证据层级：** root报告本轮真实管理/网关观测E2E及PostgreSQL/Redis隔离race 125顶层、302事件全部通过，包含实际HTTP401→OAuth→发送链→持久记录→管理API。UI本轮57组旧运行时浏览器使用独立mock网络，验证交互与查询合同，不冒充生产上游推理或所有历史数据已有采集；68项日志相关纯逻辑通过，页面异常0。
- **视觉与固定构建：** 原关键区与新模型/阶段/无响应头尝试/schema/分布共304张四主题1440/390截图，另含390链路列四主题和320列，共309张列举证据；已由resource_frontend与audit交叉目视关键区，无行动项。Accounts `accounts-managed-BqsbixQG.js`、Logs `logs-managed-h8bkz_3y.js`。54产物文件与48基线资源一致性已核；[manifest](../.local/management-parity/observations-20260918/build-manifest.json) SHA-256 `449c449b6f55278521dfac20fe0bc558bea56122bfb036b85c732eb6b44be24d`，前端集合SHA-256 `d83b8b444f6e29ebba324599a5225e6752222efc0ade1e41fad1f2cf313a26e6`。[浏览器报告](../.local/management-parity/observations-20260918/logs-browser/report.json)、[机器证据](../.local/management-parity/observations-20260918/logs-verification.json)。

本轮更新当前实施行Q10/A06/A08/D03/D09/D11/D16/D17，保留开发前基线的历史描述；不以阶段验收宣称全部管理/多模态/网络观测或历史数据已完全补齐。
