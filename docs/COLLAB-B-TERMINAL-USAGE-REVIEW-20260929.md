# B 组终态计量与0.4.14门禁复核

2026-09-29 00:07（Asia/Shanghai）状态：独立边界测试已完成；主会话0.4.14源码冻结，正式Go/PG/race/live仍在运行，未据此宣称发布。

## 独立计量边界

主会话修复合法上游终态携带真实usage、但output工具校验失败时丢计量的问题。失败仍保留，拒绝的工具输出不会被放行或作为成功历史保存。只在收到终态且该输出校验失败的分支运行新的metadata回调，不继续读取已提前拒绝的流来补造用量。

B额外核对真实`recordBPSTerminalUsage`计量入口：

- 负数、null、bool、int64溢出及相加溢出按既有parser保持未知；合法0/read0/create0明确保留。
- 六种terminal别名中，前后call_id冲突仍为身份错误且metadata callback为0次；普通policy拒绝可采样1次，但Result.Response/Output保持空，Completed不变成true。
- 旧私有helper33a43f4e…的兼容usage细节有两项失败：丢失prompt_tokens_details.cached_tokens=2，以及非法prompt详情被丢弃后让usage误成known。旧失败保留。
- 主会话最终helper`0e44fc326ab798a216552d1c22a47f87619f09e1f6eb8967e773cfe3e5801805`已修复，B只替换helper复验相同断言，**3顶层/31含层级/22leaf全部PASS**。stream617c334a…与测试494bb396…保持相同，受测源码无漂移。

证据：`.local/cross-session-20260928/01a0e7ed/terminal-metadata-boundary-review/REPORT.md`；最终`owner-after/result.json`与tests.txt。原33a失败不能作为当前0e版本未解问题。

## 消费者合同

五个重点统计入口均将状态与计量分开：失败记录保留真实usage会补回token、缓存读分母、日指标、TPM及可估算参考费用，仍计失败而非成功。

input100/output7/read80/create20代表107token；create不另加到token或缓存读分子。已知用量必须在Metrics.Record与费用冻结之前恢复，不能事后只补近期显示；SentModel定价优先、未知不推0、旧费用快照不重算。success-only分组和全状态overall本来是不同切片，不能要求相同。

这是源码及已有测试合同审查，无额外SQL或费用操作。详细五入口、函数位置及判据见 `terminal-usage-accounting-review/REPORT.md`。

## 正式门禁状态

0.4.14最终三份执行manifest均697项、完全一致，mapping SHA为`2661ac9ded734cc55fabb76838ff66a6987b5c27894b90ab6aed7ca804a2c2f7`，包含最终terminal helper。ToolPolicy helper13/gateway3与新terminal bps3/server2都被实际race选择规则覆盖；PG新`TestPGBPSToolPolicyObservationRoundTrip`已显式纳入本轮14项wrapper。

16:06:38 UTC读取时，以下进程仍运行，report未完成：

- race PID29056，`.local/powerpoint-migration/race/20260928T160442.069959Z`。
- PG PID17736，`.local/cache-observation-pg-linux/20260928T160442.016102Z`。
- live PID28852，`.local/reliability-modes/live/20260928T160442.068411Z`。
- fullGo/vet PID39356，`.local/tool-policy-release-20260928/`。

选择匹配和源码一致不等于实际PASS。B后续读取同一运行的raw终态与新包回执，不重复发模型、不重启这些进程。旧0.4.13正式门禁不能代替新源码证据。

精确名称和快照：`tool-policy-gate-coverage/20260928T160638.263358Z/`。当前所有B工作为独立overlay/私有产物/只读审查，未与主会话竞争修改冻结运行源码。
