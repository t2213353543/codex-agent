# OAuth 尝试观察与 schema 2 兼容合同

**0.4.13 已于2026-09-28 15:39:56 UTC / 北京时间23:39:56完成现场验收。** 不可变release `dd343bda80dbafd2fe593b2ba4dbde443d6c1ffdcb6b69b57799e88e0c76de35`，运行PID1422598、binary `948c483bcabc7fb130501a7f22bcd43dc3f448840c64928f81ab2fa209d42479`。两条标记SSE/JSON生成、一次本地参数负例、三条精确事件CSV v4只读导出全部通过，设置和账号配置保持。见[独立现场核对](../.local/collab-a-20260928/oauth-0413-release-review/postdeploy-review.json)；此验收不代表所有自然错误已消除，也没有在现场主动触发OAuth失败。

## 状态、实际 HTTP 与最终生成错误

| 字段 | 含义与示例 |
| --- | --- |
| `attempts[].status_code` | 保留既有逻辑/协议结果。上游HTTP200的SSE最终报限流时可为429；不能当作抓到的HTTP响应头状态。 |
| `attempts[].observed_http_status_code` | 仅收到实际响应头时记录其HTTP状态。上述SSE例为200；头部前超时、取消、断连接保持缺省，不补0或逻辑429。 |
| `attempts[].phase` | 分开记录实际 `request` 与 `refresh`。OAuth刷新失败属于它自己的条目，不伪装成已发出的生成请求；命中已有刷新失败状态不新增网络条目。 |
| `final_upstream_error_code` / `final_upstream_error_source` | 独立标识最终选定生成的可信机器错误。后续OAuth条目不替换先前真正返回给客户的生成失败；不靠任意错误文本或数组最后一项猜测。 |

例如生成429后另一次OAuth400，尝试历史可以同时保留两条，但最终生成来源仍是原生成限流。未发生成、客户取消、下游写失败或普通传输错误，不据此虚构最终上游机器错误；原producer的`transport`、`deadline`、`canceled`等观察码仍保留在实际尝试中。成功记录没有残留的final失败字段。

若生成失败终态已经被观察到，之后在首次输出前发生会话绑定冲突，真实的 `final_upstream_error_*` 诊断可以保留，即使客户收到的输出字节为零。整请求的最终来源、相位和原因仍由 `error_source`、`error_phase`、`error` 表达；不能仅凭这些上游诊断把明确的本地失败归为上游限流。管理 Go/SQL 分类仅在 `status=error` 且 `error_source` 为空或为 `upstream` 时优先采用受支持的 final 机器码。

`observation_schema_version=2` 表示 **BPS外层已采用实际网络尝试账本**，不再重复把整个handler算作一次生成。其他路径可以继续沿用schema1。

**新增HTTP字段不只可能出现在schema2。** `ObservedHTTPStatusCode` 位于通用`Headers()`观察方法，通用`applyGatewayAttemptSnapshot()`仍写schema1，所以schema1也可能带实际HTTP字段。消费者应按字段是否存在解释，不能用version=2作为显示/读取该字段的前提。

## 截断与未知

尝试列表最多保留4项，并以`attempts_truncated`标记还有未展示条目。截断时不得把“最后保留项”当成最终生成，更不能用最终成功覆盖较早的401；独立final字段用于保留选定生成的归属。完整schema2空列表可以表示本请求没有实际网络派发；旧记录、缺省和截断信息不足时保持unknown，不虚构一次请求。

字段缺省仍是未观察/不适用。即使记录的version为2，也不保证每条attempt都有HTTP状态或一定存在final错误字段。

## 导出与存储

导出合同升级为 **`codex-usage-v4`**，通过`X-Export-Schema`标识。CSV原75列的位置和含义不变，仅尾部追加第76、77列：

1. `final_upstream_error_code`
2. `final_upstream_error_source`

实际HTTP状态位于既有`attempts`列中的JSON对象，不新增独立CSV列。JSON请求记录的`observation_schema_version`与CSV导出schema是不同版本体系，不能混用。

新字段使用现有`request_logs.record`等JSONB/JSON详情保存。当前集成没有修改SQL迁移集合；不需要DDL或把历史缺失观察回填成已知。usage计数、费用来源与账号调度不是此次观察字段变更的含义。

## 混合版本与回滚迁移

使用新旧实际`RequestRecord`编解码和日志恢复函数的[本地双版本验证](../.local/collab-a-20260928/oauth-schema2-compat/REVIEW.md)已确认：

- 旧0.4.12 reader可以读取schema2，不因version数字报错。普通PG读取不重写原JSONB；旧版恢复并保存日志聚合时，原`logs.jsonl`字节保持不变。
- 旧类型重新编码、旧`export-legacy`或旧工具向新数据库导入时，会丢失不认识的三个新增字段：`attempts[].observed_http_status_code`、`final_upstream_error_code`、`final_upstream_error_source`。版本数字2仍可能保留；新版再次读取也无法恢复这些观察，不能据逻辑状态反推实际HTTP状态。
- 同库相同event的持久化去重不覆盖原request_logs；usage修复采用`record || patch`局部更新，现有helper不替换新增观察字段；聚合重建不重写request_logs详情。

因此，普通切回旧binary读取原数据库与“通过旧typed导出/迁移后重写”是两种不同边界。需要完整保留新增观察时，使用支持这些字段的迁移工具或保留原始数据库备份。混合writer期间按每条记录的版本及字段存在性解释；latest-account视图被较新的schema1请求替换，不等于旧历史event被改写。

## 本地集成核对

本轮只读比对[主任务集成清单](../.local/oauth-observation-review-root/integration-files.json)与[A重基证据](../.local/collab-a-20260928/oauth-rebase-0412/REPORT.md)：7个后端文件均匹配记录的after摘要；与A重基的唯一约定差异是导出schema从v3升v4。root补充的producer传输错误保留分支、截断保护、整请求固定`bpsModelRoutes`快照均在；`store.go`与已部署0.4.12一致，冷却默认false未改变。

monitor在集成清单之后又增加一条精确namespace工具错误分类，15:18:57的源码SHA为`c88f8080…`，因此与清单原after摘要不同；这是单列的在途工具变化，未把早先消费者结果冒充该新文件已验收。具体摘要、75→77列前缀检查和保护项见[集成只读核对](../.local/collab-a-20260928/oauth-schema2-compat/integration-review.json)。本页编写期间未重跑Go、PG、live，未改运行文件或数据库。
