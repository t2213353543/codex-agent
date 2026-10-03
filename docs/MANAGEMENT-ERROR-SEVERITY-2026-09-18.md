# 错误严重级别

错误严重级别是管理端对已留存事实的只读派生，不是上游返回的等级，不是人工优先级，也不改变 HTTP 状态、请求结果、账号状态、调度或网关重试。原始 `RequestRecord` 与数据库日志不会增加该字段，不涉及计费。

## 规则与来源

成功记录 `status=ok` 省略 `error_severity` 和 `error_severity_source`。其他记录的来源统一为 `management_rules_v1`，按以下顺序分类：

1. HTTP 状态缺失、为 0 或不在 100 至 599 时为 `unknown`。不依据文本将未知 HTTP 自动补为 200 或 500。
2. 本站类别 `unsupported_parameter`、`system_role`、`invalid_oauth`、`unsupported_model`、`encrypted_history`、`authentication` 为 P3，优先于 HTTP 分档。
3. `rate_limit`、`overloaded`、`upstream_error`、`server_error`、`timeout` 为 P1，包括通过 HTTP 200 返回的明确业务失败。
4. 其余 HTTP 大于等于 500 或等于 429 为 P1；大于等于 400 为 P2；其余失败为 P3。

`request_too_large`、`upstream_refusal` 不被猜测成参考系统的 invalid-request 或 permission 类型，而是按实际 HTTP 分档。等级始终基于本站 `managedFailureCategory` 的既有分类优先级，未新增错误文本规则。

本机参考 Sub2API `backend/internal/handler/ops_error_logger.go` 的 `classifyOpsSeverity` 以原始错误类型优先确定 P3，再按状态分档；其 `inferStreamFailureStatus` 还会先把明确流式失败推为 429/503 等。本站没有完整保留相同的原始 `errType/code`，因此采用上述可解释的管理规则并单独标识来源，不声称与参考分类逐字等价。本站保留 HTTP 200 事实，只有派生等级反映已记录的限流或过载类别。

## API 契约

- 列表、详情、错误处理视图、JSONL 的失败记录新增 `error_severity: "P1" | "P2" | "P3" | "unknown"` 与 `error_severity_source: "management_rules_v1"`。成功记录省略两项。
- 查询参数 `error_severity` 接受上述四个大小写严格的枚举，空值表示不筛选。分页游标与固定快照绑定此参数，改变等级后不能复用旧游标或快照。
- 统计新增 `error_severities`，复用既有维度的 `name` 和请求/Token/耗时汇总，固定按 P1、P2、P3、unknown 排序，未出现的等级不生成空行，不包含成功请求。顶层 `error_severity_source` 始终为该规则版本；仅成功或空结果时维度为 `[]`。
- CSV 保持 `codex-usage-v3`，原 59 列之后追加 `error_severity` 和 `error_severity_source`，总 61 列；成功记录的两列为空。脱敏导出保留管理等级与来源，不据脱敏后空错误重新推断等级。
- 清理预览与任务允许该筛选，但仍沿用明确时间范围、固定 ID 集合、独立确认和逐批事务。严重级别本身不会触发删除或自动处理。

## 验证

本地与实际 PostgreSQL 16.15 六项专项通过，包含严重级别完整 API、798 条独立分类边界矩阵、WebSocket 预热、固定快照、无计费合同和清理观测筛选。最终 PG 为 6 顶层、10 项含子测试、0 跳过、0 失败，临时 tmpfs 容器已删除；结果在 `.local/management-parity/error-severity-20260918/severity-pg-final-summary.json`。

覆盖缺失/非法 HTTP、HTTP 200 限流和过载、P3 类型优先、四等级分页/统计/错误处理交集、参数拒绝、游标与快照隔离、JSONL/CSV 原事实保留，以及清理只删除快照内的三条 P1 而不删后来记录、不改十二条持久用量事件。读取前后确认原日志中没有派生等级字段。

首轮实际 PG 的唯一失败来自旧预热测试把其 CSV 字段写死为最后一列，已改为按表头名定位且继续核验 nullable 布尔值。该首轮报告保留，最终通过报告没有覆盖失败证据。最终全库、race、构建与前端验收另由固定归档绑定。
