# Codex 管理后端契约与数据边界

> 当前修订：用户明确不需要任何计费。本文旧价格、采购、售价接口和金额统计均已退役，路由返回 404；当前范围以 [无计费契约](MANAGEMENT-NO-BILLING-SCOPE.md) 及 [v3 用量导出](MANAGEMENT-PARITY-USAGE-API.md) 为准。

更新：2026-09-18。范围为现有 Codex OAuth 账号与使用记录。账号详细契约见 `MANAGEMENT-ACCOUNT-API.md`；逐项适用性与验证状态见 `ACCOUNT-PARITY-MATRIX.md`、`USAGE-PARITY-MATRIX.md`。这些源码仍需联合冻结和发布，不代表已上线。

## 公共规则

所有下列路由在现有管理鉴权与 `Cache-Control: no-store` 下。管理界面继续加载旧版蓝色壳，使用 `node frontend/scripts/build-management-parity.mjs` 构建 Accounts/Logs 局部模块，并继承已验证 Settings 模块。不要直接部署当前源码的完整前端构建。

时间查询为 Unix 秒，`from <= time < to`。账号、KEY、分组用稳定 ID；名称是展示快照，重命名不能扩大操作对象。未知字段、未定价费用与真实 0 分开。`HTTP 200` 仍可能是业务失败；`status` 保留 ok/error/aborted。

## 使用记录接口

| 接口 | 行为 |
| --- | --- |
| `POST /api/logs/snapshot` | 固定同筛选精确 ID 集合与参考价版本；5 分钟有效，绑定管理员会话，后续查询传 as_of。 |
| `GET /api/logs/coverage` | 读取实际保留明细数量、起止时间；不从仪表盘滚动天数猜测明细留存保证。 |
| `GET /api/logs` | 服务端过滤、排序、分页；`exact_total=true` 才要求 PG 精确计数。返回 items、total_kind、has_more、next_cursor、scope。 |
| `GET /api/logs/{id}` | 根据事件 ID 或数字记录 ID 获取实际保留详情；删除后 404。 |
| `GET /api/logs/errors` | 同筛选错误工作区，可按 resolved 过滤当前人工处理状态；游标绑定处理集合，变化后 409 提示回到首页。 |
| `GET /api/logs/{id}/review`、`PUT /api/logs/{id}/review` | 持久处理标记、备注与版本历史，version CAS 与审计同事务；不改原请求结果，详见 [错误处理契约](MANAGEMENT-ERROR-REVIEW-API.md)。 |
| `POST /api/logs/ip-geo` | 管理员手动查询 1–50 个 IP，固定 GeoJS 来源、去重与 24 小时缓存；非公网 IP 不外发，无自动背景查询。 |
| `GET /api/logs/cost-rules`、`POST /api/logs/cost-rules` | 独立人工采购成本，稳定账号或明确组、模型/服务层、不可变版本、倍率和来源；CAS 与审计同事务，停用也是新增版本。 |
| `POST /api/logs/cost-rules/preview` | `{record_id}`，使用真实留存请求和当前已保存采购规则只读计算；未保存草稿不生效，已清理返回 404。 |
| `GET /api/logs/sale-policy`、`POST /api/logs/sale-policy` | 渠道、组模型价/倍率/订阅高峰、KEY 唯一计价归属的整目录不可变版本，原子 CAS 与审计。 |
| `POST /api/logs/sale-policy/preview` | 使用真实留存请求和当前已保存目录计算独立配置售价，不查询上游、不扣款；未保存草稿不生效。 |
| `GET /api/logs/cost-policy`、`POST /api/logs/cost-policy` | 有序多目标采购策略的完整不可变版本；规则/模型顺序、显式渠道跟随、账号倍率与 CAS/审计。 |
| `POST /api/logs/cost-policy/preview` | `{record_id}`，按真实留存请求的时间和当前保存策略预览；未生效/停用策略沿用旧采购模式。 |
| `GET /api/logs/stats` | 同筛选全量汇总、趋势、模型/端点/错误类别分布、账号/KEY/分组排行；不是当前页求和。 |
| `GET /api/logs/options?field=account|key|model|group&q=&limit=50` | 从同筛选历史数据查候选，返回稳定值、展示名、删除状态及 has_more；支持自由文本继续精确查询。 |
| `GET /api/logs/export?format=csv|jsonl&redacted=true` | 导出整个筛选范围，继承服务端排序；默认脱敏。先完整生成私有临时文件，失败不返回伪成功的半文件。 |
| `GET /api/logs/pricing`、`POST /api/logs/pricing` | 管理有来源、不可变、带生效时刻的美元参考价版本；没有默认猜测价格。 |
| `POST /api/accounts/usage-batch` | `{ids,from,to}`，最多 100 个账号、90 天；一次同筛选聚合，包含无请求账号的真实零请求数。 |
| `GET /api/accounts/{id}/runtime?model=` | 只读观察当前实例并发、连接/模型冷却、OAuth 刷新冷却及当前全局有效策略；不清除冷却，不触发刷新或推理。 |
| `POST /api/accounts/last-used` | `{ids}`，最多 100 个稳定账号 ID；PG 读取持久 usage_events 的真实最近时间，文件模式读取留存请求。无记录为 null，错误不伪装为未使用。 |
| `POST /api/accounts/import-costs`、`GET /api/accounts/import-costs/receipts/{receipt}` | 实际账号导入后的独立成本预览/确认与回执；不覆盖已有本地规则，显式倍率更新另确认，部分/未知不自动重放。 |
| `POST /api/logs/cleanup/preview` | `{filters}`，要求显式过去时间区间；固定本次可见的精确日志 ID，5 分钟有效，绑定管理员会话。 |
| `POST /api/logs/cleanup/jobs` | `{preview_id,confirmation_token,request_id}`，幂等创建持久清理任务。 |
| `GET /api/logs/cleanup/jobs`、`POST /api/logs/cleanup/jobs/{id}/cancel` | 查看持久状态和真实进度；取消保留已完成事实。 |

筛选包括 from/to、account/account_id、key_name/key_id、group_id、model/upstream_model、endpoint、status、request_id、error_category/error_phase、ip、http_code、stream、request_type、service_tier、reasoning_effort、model_mismatch、native_compaction。

新增真实链路条件 sent_model/returned_model/inbound_path/upstream_path、sent_model_mismatch/returned_model_mismatch、error_truncated；未知字段不匹配明确 false。统计返回对应四种独立分布及三种阶段样本/平均值，详情显示最多四项实际派发摘要与截断/schema来源，契约详 [观测接口](MANAGEMENT-OBSERVATIONS-API.md)。原兼容上游模型及旧model_mismatch语义保持。

排序白名单：time、model、tokens、input_tokens、output_tokens、cached_tokens、latency；asc/desc，第二排序键为稳定记录 ID。每页 1–500 条；offset 页码最多 100，深翻页使用 cursor。cursor 绑定规范化筛选、排序、存储类型及初始 ID/文件字节上界。文件模式扫描整个保留日志，不再静默限制到最后 100000 行；导出使用有界外部归并排序。

`timezone` 为 IANA 名称，默认 UTC；`step` 支持 60、300、3600、86400、604800、2592000。无请求桶补真实零请求/零费用，有请求但未定价桶保持费用 null，图线在未知处断开。日、周、月按当地日历切分（周一开始，月不是固定 30 天）；小时及更细按 UTC 秒切桶。过多桶返回明确错误，不截断成假完整图。统计会返回 checked_at、first_record_at、last_record_at、scope 和各指标覆盖数。

## 统计、价格与导出语义

输入总量包含缓存读取/写入；总 Token 为 input+output，reasoning 是输出子集，不能再相加。参考费用将输入拆成互斥的普通/缓存读/缓存写桶，用十进制定价；cached/write 超出输入、usage 未记录、实际返回模型未定价等情况均为 unknown。cache-write 数量未知时，只有缓存写价格与普通输入价格相同才能得到确定费用。

参考费用不等于 ChatGPT 订阅实际扣费、用户余额或账号购买成本。版本按实际记录时间选取，同模型追加版本不得倒改已生效历史规则；第一条规则可以声明历史生效日。统计同时返回 priced_requests/unpriced_requests，部分可估算不能称全量账单。来源由管理员填写并保存；默认脱敏导出省略其自由文本备注。

可配置长上下文阈值与输入/输出倍率，严格使用 input_tokens > threshold 选择档位。输入总量已含缓存；进入高档时对整个请求的相应 Token 桶应用倍率，列表、统计、排行、导出和冻结价格快照口径一致。配置与边界详见 [参考定价契约](MANAGEMENT-PRICING-API.md)，返回字段和新增 CSV 列见 [数据字典](MANAGEMENT-PARITY-USAGE-API.md)。

参考价可追加显式 IANA 时区的工作日/每天分时倍率，以记录请求起点判断左闭右开时段，与长上下文倍率组合，不伪装官方高峰价。采购成本独立存储和计算，输出 account_cost/account_cost_usd 及 costed_requests/uncosted_requests，未配置不回退为售价，详情见 [采购成本契约](MANAGEMENT-PROCUREMENT-API.md)。PG 参考价与采购价统计共享只读 REPEATABLE READ 事务；明细详情携带 as_of 与原过滤时也复用固定价格和记录成员，不能在固定列表里默默改用新价格。

采购可配置 `(min,max]` 的整次 Token 档位绝对四桶价，基数=input+output，未命中回基础价，不重复加缓存。配置售价则独立 `sale_cost` 域，明确 KEY 计价组、组价→启用渠道价→基础目录、KEY 覆盖组倍率及订阅高峰因子；三费用域均进入相同快照和只读统计事务，详 [采购阶梯](MANAGEMENT-PROCUREMENT-TIERS.md) 与 [渠道售价](MANAGEMENT-SALE-POLICY-API.md)。账号成员组分布不等于售价唯一计价组，不能重写历史 Groups。

有序采购策略是显式新版本，不升级重排旧账；先范围+模型顺序匹配，未命中才考虑明确的跟随渠道。跟随售价基数保留精确金额，排除销售KEY/组/高峰倍率后另乘采购倍率，详 [有序采购合同](MANAGEMENT-COST-POLICY-API.md)。租期备份回执的新路由和双确认独立于价格，详 [租期往返](MANAGEMENT-LEASE-BACKUP-API.md)。

普通原始导入可显式 `include_external_leases` 适配 Sub2API 外层账号租期，默认不读取/应用，令牌到期始终独立；自家凭据导出可显式 `include_costs` 带账号成本历史和当前显式倍率，后续独立回执恢复。细节分别见 [外部租期](MANAGEMENT-EXTERNAL-LEASE-IMPORT.md) 与 [账号成本备份](MANAGEMENT-ACCOUNT-COST-BACKUP.md)。

原始 Sub2API `rate_multiplier` 另以 `include_external_costs` 显式识别，保持原 JSON number 精度并以十进制字符串返回，null/absent不伪造1倍。只映射为独立待确认账号采购覆盖，不映射Weight或自动创建有序策略；源不是采购policy时不伪造源版本，见 [外部倍率契约](MANAGEMENT-EXTERNAL-COST-IMPORT.md)。

账号运行状态和最近使用的来源、毫秒/秒单位及只读约束详见 [账号运行观察](ACCOUNT-RUNTIME-OBSERVATIONS-2026-09-18.md)。共享 Redis 不可读时会明确报告 unavailable，不将缺少读取结果当作无冷却或可调度。

脱敏导出清空账号名、KEY 名、代理、IP、UA、自由错误文本，用稳定摘要替代账号/KEY/分组/请求等标识，保留结构化错误类别和可比较的数值。显式 `redacted=false` 才包含管理身份字段；所有导出继续进行密钥文本脱敏及 CSV 公式转义。响应提供 X-Export-Count、X-Export-Redacted、X-Export-Schema。

页面先 POST /api/logs/snapshot 固定当时可见的精确记录 ID 和参考价版本，再由列表、统计、候选、导出共用 as_of。PG 使用 management_cleanup_targets 的 usage_ 命名空间，只存 ID，不复制请求内容；文件模式固定字节边界。快照绑定会话及筛选，5 分钟后返回 409 提示刷新，不纳入后续新增或晚提交旧 ID。并发明确清理掉的明细不会为快照额外保留，因此数据仍可能减少，不能承诺已删除内容仍可读。清理修改日期后另行预览，不沿用不匹配快照。不带 as_of 的外部兼容查询保留原独立查询行为。

## 持久管理状态与清理

`admin_management_state.go` 复用现有加密 settings 文档，独立 `management/<namespace>/<key>` 命名空间，不改变系统 Settings 字段。PG 使用事务级 advisory lock 与 version CAS；文件模式使用进程锁、跨进程目录锁和私有原子文件。OAuth verifier/临时令牌等状态有 TTL，取消/完成清空材料，后台定期物理清除过期材料。

唯一新增迁移 `003_management_cleanup.sql` 存储清理预览的精确目标 ID。000–002 的校验和不变；发布前须显式执行新增迁移，不能用旧的 schema-preserving 发布流程跳过它。

清理每事务处理最多 1000 条，删除目标、检查点和完成审计同事务提交。重启续跑、并发 worker、取消和审计失败回滚均有真实 PG 测试。只删除 `request_logs` 明细，不删除 `usage_events` 或累计计量；清理后的模型/KEY/分组等明细统计自然不再包含被删记录，界面明确 `retained_request_details` 范围，不能把它称生命周期账单。文件模式不提供持久清理任务，明确返回 postgres_required。

管理 worker 在 serveManaged 生命周期中启动并等待取消；清理和账号计划分开执行，慢上游检查不阻塞清理。配置动作采用 UpdateAudited 将修改和审计放在同一事务；后台心跳/检查点不反复写配置审计。

## 网关与真实来源边界

本管理任务不改变请求转换、指纹、选路、重试、WebSocket/SSE 字节或累计计量。RequestRecord 只新增可选观测字段；具体被动采集由另一个网关源码任务维护并独立验收。请求 service_tier/effort 与响应确认值有 source 区分；历史缺字段继续显示未记录，不能回填猜测。TTFT 仅真实首次非空输出，不能以响应头延迟或总时长代替。

网关级模型映射、每账号指纹覆盖、Spark 父子账号、调度阈值等不能只加无效界面开关；逐项边界记录在矩阵。自动重置券是显式启用的独立管理策略：阈值/间隔/凭据版本绑定、实时查询券、优先即将到期的有效 codex_rate_limits 券、持久 claim 与幂等，未知写结果暂停并要求核对后确认。成功后必须观测到原先触发重置的每个配额窗口真实降至阈值以下才允许下一轮；该窗口缺失或过期不能用另一个窗口的低用量代替，避免连续消费多张券。旧策略没有触发窗口记录时保守要求两个窗口均已知且恢复。默认关闭，不改变网关选路。管理推理测试、OAuth、上游额度、隐私与人工重置操作均独立于业务流量，页面 GET 仅读取已有状态，不偷偷发送推理。

## 验证证据

真实隔离 PG 日志位于 `.local/management-parity/management-pg-*.log` 和 `usage-options-calendar-pg.log`。文件模式、价格/未知值、完整导出、筛选绑定、清理与账号批量语义有对应 Go 测试。Accounts/Logs 的旧运行时浏览器报告分别在 `.local/accounts-parity/browser/report.json` 与 `.local/logs-management/browser/report.json`，浏览器使用合成 API 数据，不代表上游生产服务验证。

最终联合验收需绑定不可变源码、完整 Go/Node 检查、最新旧壳浏览器和实际 PG 测试。其他会话较早快照的网关 race 成功不能替代这批新管理代码；本轮没有执行生产压力测试。
