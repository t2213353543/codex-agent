# 使用记录接口与导出数据字典

当前源码 CSV v4 共77列，保留上一版75列的名称和顺序，末尾追加 `final_upstream_error_code`、`final_upstream_error_source`。链路观测增加可选实际 HTTP 状态，OAuth 刷新与生成派发分别记录。详 [0.4.13 观测契约](OAUTH-ATTEMPT-OBSERVATION-20260928.md)；源码候选不表示已上线。

更新于 2026-09-18。用户明确移除全部计费，当前契约以 [无计费范围](MANAGEMENT-NO-BILLING-SCOPE.md) 和本文件为准；其他旧文档中的价格路由、金额与价格快照描述已退役。源码候选不代表已经上线。

## 导出版本

`X-Export-Schema: codex-usage-v4`。CSV 首行为字段名，UTF-8、RFC 4180；有公式前缀的文本前加单引号。JSONL 每行一个管理日志对象，无金额对象。按字段名读取，不依赖固定列号。v4在原75列末尾追加两个最终生成错误字段，历史记录留空；v3曾移除v2全部价格、成本、倍率列及 `usage_amount`、`rollout_budget_units`，这些字段仍不返回。下表旧列数说明只描述历史追加位置，以当前77列为准。

`GET /api/logs/export` 默认 `redacted=true`；显式 false 才含身份管理字段。默认移除名称/IP/UA/自由错误，以稳定摘要替代资源及请求标识；数字数据库记录 ID 是本地序号，不是上游请求 ID。`X-Export-Count` 是生成文件的记录数，不是当前页数；`X-Usage-Scope` 始终为 `retained_request_details`。

| 字段 | 类型、单位与语义 |
| --- | --- |
| id / event_id | 本地数据库或文件行 ID / 本地事件 ID。 |
| time | Unix 秒；CSV 不隐式转换到设备时区。 |
| account_id / account / account_name_snapshot | 稳定账号 ID 与历史名称，不代表当前账号仍存在。 |
| key_id / key_name | KEY 资源 ID 与历史名称，不含可用密钥。 |
| groups | JSON 字符串数组（CSV 单个格）；一个请求可多组，不能把各组汇总相加当全局总数。 |
| model / upstream_model / upstream_model_source | 请求模型 / 上游明确观察的模型 / header或response来源；缺失不意味着一致。 |
| endpoint / request_type / stream | 端点、传输类型、是否流式；历史缺值不猜测。 |
| status / http_code | 业务结果 ok/error/aborted 与 HTTP 状态；HTTP 200 也可能业务失败。 |
| terminal_status / incomplete_reason | 实际终态及不完整原因；没有终态不伪造 completed。 |
| input_tokens / output_tokens / cached_tokens | 输入总量、输出、缓存读取；输入包含缓存，输出包含 reasoning。CSV 用量未知留空；JSON 数值需联合 usage_known 判读。 |
| usage_known | true=已知用量，false=明确无已知量，null/空=历史未知。真实零必须 true。 |
| reasoning_tokens / cache_write_tokens | 上游提供的可选子集；缺失/null 与 0 不同。 |
| usage_total_tokens | 上游原 total 字段；本地列表总量仍为 input+output。 |
| latency_ms / ttft_ms / upstream_header_ms | 总耗时 / 首次实际非空输出 / 响应头耗时，毫秒；后两者未知为空，不能互相替代。 |
| first_output_kind | 实际首输出事件种类；不存文本、工具参数或加密内容。 |
| service_tier / reasoning_effort | 兼容展示值；结合 source 判断请求还是响应，不作价格计算。 |
| requested_service_tier / requested_reasoning_effort | 请求值，不代表上游执行确认。 |
| service_tier_source / reasoning_effort_source | request/response；缺失为来源未知。 |
| native_compaction | 上游协议可证明的原生压缩触发语义，nullable bool；普通 compact 不自动等同 true。 |
| websocket_warmup | 已持久化的 WebSocket response.create 预热观测，nullable bool；true 来自 generate=false，false 来自明确普通生成或协议缺省，缺失为未知。CSV 在 v3 末尾追加此列，共 59 列。 |
| error_severity / error_severity_source | 失败记录的P1/P2/P3/unknown管理派生等级与规则来源，不是上游原生等级；成功省略。随后追加为第60/61列，当前CSVv3共61列，详 [错误分级契约](MANAGEMENT-ERROR-SEVERITY-API.md)。 |
| request_id / response_id | 上游标识，与 event_id 分开；request_id 筛选同时支持这两项和本地 ID。 |
| proxy_name / ip / user_agent | 原记录网络上下文；默认脱敏导出省略。 |
| error_category / error / error_source / error_phase | 分类、限长脱敏错误、来源与相位。历史未持久化的错误无法恢复；默认导出不含自由错误。 |
| sent_model / returned_model / inbound_path / upstream_path | 实际发送/返回模型及标准路径，来源独立，不以请求值回填。 |
| request_preparation_ms / account_selection_ms / proxy_selection_ms | nullable 阶段毫秒，区间可重叠，不能相加为总耗时。 |
| attempts / attempts_truncated | 最多 4 条实际派发/刷新/握手摘要（CSV JSON 一格）及明确截断状态；派发不等于上游接受。 |
| attempts[].status_code / attempts[].observed_http_status_code | 协议逻辑结果 / 实际响应头状态；例如流内限流可分别为429/200，无响应头时后者省略。 |
| final_upstream_error_code / final_upstream_error_source | 最终选中生成响应中实际观察到的已识别失败，与后续 OAuth 刷新失败分开；不证明响应已交付，也不覆盖后续本地失败的 error_source/error_phase。无已识别生成错误时为空。 |
| observation_schema_version / usage_schema_source / error_truncated | 实际采集版本/用量来源/错误截断；历史缺失保持空，不伪造版本或 false。 |

详细链路来源见 [观测合同](MANAGEMENT-OBSERVATIONS-API.md)，其中旧金额章节不再有效。错误人工处理通过独立 [错误处理接口](MANAGEMENT-ERROR-REVIEW-API.md) 维护，不改变原请求结果或累计用量。

## 全量统计与快照

`error_severity=P1/P2/P3/unknown` 支持相同范围筛选；统计 `error_severities` 仅汇总失败/中断，顶层来源为 `management_rules_v1`。处理状态不会改变等级，等级不会改原始HTTP或Token。

`websocket_warmup=true/false` 精确匹配已观测布尔值，false 不包含历史未知；不传则不限。列表、统计、快照、导出及清理使用相同语义。清理支持实际发送/返回模型、入站/上游路径、模型差异、错误截断和预热条件，仍要求明确过去时间范围，未知筛选字段明确拒绝。

summary、models/upstream_models/sent_models/returned_models、endpoints/inbound_paths/upstream_paths/error_categories、accounts/keys/groups 与 series 使用相同请求计数、输入/输出/缓存和时延口径，不读取价格目录。模型及路径维度仅统计对应非空观测，不回填，因此合计可能少于总请求数。身份优先稳定 ID；历史缺 ID 不按名称猜测为当前账号。

覆盖字段包括 usage_known_requests / usage_unknown_requests、ttft_samples、reasoning_samples、cache_write_samples，以及各阶段的 samples/nullable 平均值。不存在 priced_requests/unpriced_requests 或金额合计。

series 按请求的 IANA 时区和日历粒度分桶；缺请求的桶是零请求、零 Token。日历月不是 30 天，夏令时日可能为 23/25 小时。`as_of` 固定记录 ID 范围；后续新增不混入，但并发删除的明细不可恢复。快照不保存任何价格版本。

## 可用范围

`GET /api/logs/coverage` 返回 detail_count、first_record_at、last_record_at、checked_at、storage。retention_days=null 不代表有保留保证；管理员清理与 CLI prune 可缩小明细范围。PG 累计 usage_events 保留，但已删明细的模型/分组/IP等维度无法重建。日期没有请求不等于证实从未删除。

浏览器使用合成 API fixture；文件与本地隔离 PostgreSQL 测试验证过滤、快照、导出、日期和持久行为，不替代真实上游各采集分支的独立验收。
