# 错误管理分级

## 数据与来源

`error_severity` 是管理层基于已经保存的请求结果、错误类别和 HTTP 状态派生的分级，不是上游返回的字段，不代表更改请求优先级、网关调度、恢复策略或重试。`error_severity_source` 当前固定为 `management_rules_v1`。成功请求不返回这两个字段；未取得有效 HTTP 状态的失败请求返回 `unknown`，不伪装成低级别错误。

参考 Sub2API 的实际链路为 `ops_error_logger.go` 中 `inferStreamFailureStatus` / `normalizeOpsErrorType` -> `classifyOpsSeverity`，并由 `OpsErrorLogTable.vue` 展示。参考存有规范化原始错误类型，本站的留存字段不同，不能声称两个系统对所有输入完全同值。本站映射规则如下，按顺序匹配：

| 条件 | 管理级别 |
| --- | --- |
| 请求成功 | 不适用，不分桶 |
| HTTP 状态缺失，或不在 100..599 | unknown |
| 已保存错误匹配参数不兼容、system 角色、OAuth 失效、模型不支持、加密历史、认证类别 | P3 |
| 已保存错误匹配限流、过载、上游错误、服务端错误、超时类别 | P1 |
| 其他 HTTP 5xx 或 429 | P1 |
| 其他 HTTP 4xx | P2 |
| 其他失败记录，有有效 HTTP 状态 | P3 |

`managedFailureCategory` 的既有分类优先级继续有效，不能把消息中的任意片段越过已确定类别重新解释。413/请求过大不被无依据重命名为参考的 `invalid_request_error`；策略拒绝也不被强行当作 `permission_error`。

例如 HTTP200 的流内限流或过载仍可分为 P1，但原 `http_code=200` 与 `status=error` 保留不变。普通200业务失败没有更多证据时按剩余规则分级，并不因此宣称它已成功。管理标记“已处理”不降低等级、不改变原请求结果或累计用量。

## 查询与输出

- `error_severity=P1/P2/P3/unknown`，区分大小写；不传则不限，非法或重复参数拒绝。筛选复用于列表、统计、详情快照、导出、清理和错误处理列表。
- 管理列表、详情、JSONL 和错误处理 `record` 对象返回同一派生级别与来源；不回写原始 RequestRecord 或历史数据库文档。
- CSV 在 `codex-usage-v3` 末尾追加 `error_severity` 与 `error_severity_source`，目前共61列。成功记录两个格为空；消费者按字段名读取。
- 统计返回 `error_severities` 维度和顶层 `error_severity_source`，仅包含失败/中断；空或纯成功范围返回空数组。各桶提供与既有维度相同的请求、Token与时延统计，未知用量不伪零。
- 前端只展示可信规则来源的已知级别；缺失或不认识的来源明确显示不可判定，不在客户端猜测或重新计算等级。

## 独立验证

除了列表、PG/file、统计、快照、错误复核和清理生命周期集成测试，`admin_usage_severity_contract_test.go` 用798种请求状态、HTTP边界与典型错误组合独立校验 Go/SQL 等价及原HTTP/业务结果未被改写。测试只用合成请求，不向真实上游发起推理。

管理分级不包含收费、价格或余额逻辑。原始请求隐私、脱敏导出、留存范围及未知字段约定继续有效。
