# 使用记录链路观测契约

> 当前修订：项目不包含计费。原始采集行为不改，但金额/预算标量不再从管理 API 或导出返回；旧三费用域聚合描述已退役。仅保留请求、Token 与运行观测，详 [无计费契约](MANAGEMENT-NO-BILLING-SCOPE.md)。

2026-09-18。本功能只读取已持久化观测，不更改网关发送、选路、重试、会话或累计用量。真实采集由独立源码任务维护，基于 [严格生命周期与观测说明](CODEX-STRICT-LIFECYCLE-OBSERVATION-2026-09-18.md)；该文档不能作为所有历史请求已拥有这些字段的证明。

## 独立来源

- model 是入站请求模型；sent_model 是最后实际发送时观测到的模型，未发送不从入站模型补值；returned_model 来自真实响应对象/事件，不从模型头或发送值猜测。
- 原 upstream_model/upstream_model_source 保持原兼容语义，可能来自响应头或响应对象。不能用这个单一值代替独立发送/返回模型。
- inbound_path/upstream_path 是标准路径白名单，不含 host/query。当前可观测 `/responses`、`/responses/compact`、`/models`、`/oauth/token`、`/guardian`、`/guardian-classifier`。路径归一化不意味着原始入站URL逐字相同。
- request_preparation_ms、account_selection_ms、proxy_selection_ms 是实际管理记录中的独立毫秒区间，缺失为 unknown，0 为实际测到零。账号选择可能包含锁/OAuth等待，阶段之间可能重叠，不能相加或反推 auth/DNS/TLS 耗时。

## 查询与统计

列表、同筛选统计、固定快照、选项、清理预览、详情、错误工作区与导出共同支持四个精确文本条件 sent_model/returned_model/inbound_path/upstream_path。不存在的来源不补值。

布尔条件：sent_model_mismatch 比较请求与发送、returned_model_mismatch 比较发送与返回；只有对应两端都非空时才参与 true/false 匹配，未知不匹配 false。原 model_mismatch 继续比较请求与兼容 upstream_model，不改变已存收藏含义。error_truncated 也只匹配明确 true/false，缺失不冒充未截断。

新条件进入游标/快照过滤签名，变更后不能沿用旧游标。UI高级筛选、URL/收藏白名单、服务端图表点击筛选均沿该契约。详情沿已显示列表的快照，不取后来新增价格或记录。

全量分布增加 sent_models、returned_models、inbound_paths、upstream_paths，结构与 models/upstream_models 相同，各含请求/用量/三费用/覆盖信息。仅非空真实值进入，因此维度合计可能小于总请求数；不以请求模型补齐成100%覆盖。三个费用域在相同记录集与只读事务里进行数据库聚合，文件实现保持等价。

所有 totals/维度/趋势返回 request_preparation_samples/average_request_preparation_ms、account_selection_samples/average_account_selection_ms、proxy_selection_samples/average_proxy_selection_ms。平均值分母只含该字段非负样本，无样本为 null；实际0计入样本。负值fixture不参与均值，不影响请求总数。

## 尝试摘要与截断

attempts 是最多4项的派发摘要：attempt、phase、endpoint、status_code、duration_ms、header_ms、error_source、error_code。phase 区分 request/refresh/websocket，记录实际调用发送/拨号函数，**不表示上游已收到、接受或成功执行**。HTTP通常到响应头结束派发区间，OAuth刷新可能包含解析；不能当完整生成耗时。

401→刷新→重发可以产生三项实际尝试；缓存刷新不伪造网络派发。WS只在首个准入轮记握手，后续轮不复制握手时间或次数。字段缺失或空attempt列表表示未记录，不代表实际0次发送；attempts_truncated=true 表示后续尝试未包含，false是明确观测未截断，缺失仍未知。

error_truncated 是原始错误保存/解析/脱敏截断链路的观测，不代表当前表格缩略文案。true时UI明确错误并非完整原文，false与未知分别显示。observation_schema_version 只展示正整数实际值，usage_schema_source 展示真实来源；没有这些字段的历史记录不回填schema1。

## 导出与隐私

CSV尾部追加12项：四个模型/路径字段、三个阶段毫秒、attempts（JSON单格）、attempts_truncated、observation_schema_version、usage_schema_source、error_truncated。未知可选数值/布尔/版本为空，不转换成0/false；JSONL保持原可选字段。

所有新增字符串继续秘密样式脱敏，尝试数组复制后过滤，不就地改原记录；默认脱敏身份字段的原规则继续。attempt本身只有有限分类/状态/路径/耗时，不包含任意错误正文、提示词、工具参数、header或account身份。页面按文本显示，异常响应长度仅展示前4条并明确限制。

## 证据范围

`TestManagedObservationQueryFile` / `TestPGManagedObservationQuery` 验证新增条件 true/false/unknown、游标与快照、四维分布、真实0/负值样本及三费用口径和CSV。

`TestManagedObservationRuntimeToQuery` / `TestPGManagedObservationRuntimeToQuery` 不手工构造记录：通过已有真实网关handler向本地假上游走HTTP401→OAuth→重发，检查返回字节不变、实际三项尝试，再从管理查询/详情/统计/导出读出已存观测。提示词、查询参数和刷新token不入记录。该fixture证明本地路径的数据合同，不是生产上游成功率测量。

其他来源分支由源码owner的 HTTP/WS/native/取消/截断 专项证明，不能用单个HTTP样本泛化所有协议。新字段可能随独立源码任务演进，最终管理候选只绑定固定源快照，未知多模态细桶不凭空编造。
