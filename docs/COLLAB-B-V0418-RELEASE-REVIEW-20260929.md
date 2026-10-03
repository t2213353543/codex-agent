# B组0.4.18发布复核

2026-09-29 02:58 +08：主会话已部署0.4.18，18:56:28 UTC现场验收及B独立核对通过。

- release：`65a408448e06ae8de6770e88af0e4e79570cda993693079a0a54454cf4080d88`
- PID：2186899
- binary：`133f242c81365a09cc766f93dd084deeb417bbfee59401ccc5eca95b461e424e`
- fullsource：`36ec9457ddada16de68f3457b362a072d76dbc97c2d4a48370484a80d1ce86a7`

## 变化与证据边界

本版新增有限ErrorType诊断，独立于Code并遵守原错误对象优先级；仅允许四个固定类型，不从自由message或事件type推断。安全日志保留usage_limit_reached原码；失败根/response字符串诊断按原清理规则处理。结构化失败投影不再泄露内部formatter名称，成功内容及其它允许字段保持。

error→response.failed归一化本就已在0.4.17，本版没有新增该转换。固定CLI额外纯loopback实验包含WS fallback断言失败，原结果单独保留，不能冒称新协议三variant全部通过或用来代本版正式9live。

新增error_type=rate_limit_error可能触发现有管理字符串分类，SSE200显示由unknown改为rate_limit；monitor仍优先有效final generation码，不把错误类型升格为错误码。此为统计诊断标签边界，不是新增重试/冷却或计费行为。

## 正式门禁及补验汇总问题

Go/vet exit0/no drift，4785含子PASS/462环境SKIP/0FAIL；新13top/58含子实际全PASS。PG17全部实际PASS，沿既有存储用例验证本版源，不假称新增专门ErrorType PG字段。

主race新增11top/48含子通过，两项server Independent名称被筛选遗漏，主会话用完全相同728源码/固定镜像精确补跑2top/8子，实际exit0/包PASS、无race/skip/fail。主协作20、Redis4、official95逐项全PASS；两批不重叠组合430top/1154含子PASS及16明确skip。

补验汇总脚本错误截取测试名前缀，原report因此写statusfail，实际测试并未失败。B与owner分别从原raw检查唯一RUN/PASS和源；原报告、原runner和raw保持不变，独立verification附录明确该特定错误并绑定四原件SHA。发布包同时包含原fail与通过的离线附录，**没有把历史报告改成PASS，也没有为修汇总重复执行测试**。

最终完整9live本轮exit0，原selector9组按原判据通过。前端235/55制品与monitor新25项server/只读SQL证据各自绑定。包内validate通过，89制品/28证据/90归档文件名称与SHA集合一致；727 backend scope加既有embedded JSON组成728执行源，无源漂移。

## 现场验收

manifest/activate/postdeploy同version/PID/binary。SSE gpt-5.5→gpt-5.6-sol与JSON codex-auto→gpt-6-astra均completed，max→xhigh、schema2、双HTTP200、transport与真实usage合同正确；2request/0refresh。top_p=1本地3ms400精确param且零派发；普通请求bound/mixed省略，ToolPolicy已知0/false保留，三次精确CSVv4导出通过。

settings27、api_key缓存、cooldown=false、全部账号配置和六迁移保持。B仅仅读保存回执，未额外发生成/导出/SQL。现场不故意制造formatter泄露或失败终态，相关正例依正式本地、race和网关用例；自然流量改善尚需后续窗口。

证据目录：`.local/cross-session-20260928/01a0e7ed/v0418-release-audit/`，重点`deployment-audit.json`、`package-65a40844-audit.json`、`additional-race-raw-audit.json`、`supplement-verifier-review/`及`gate-coverage/completed-*`。

为防再漏选，B已交只读`gate-selection-preflight/preflight.py`：按明确required名单和实际race flags核名称选择，不运行Go/网络、不自动改名，不能把静态匹配当实际PASS。当前仅私有工具，供后续启动门禁前复用。

只读复跑：

```powershell
python -B .local/cross-session-20260928/01a0e7ed/v0418-release-audit/check_deployment.py .local/reliability-release/65a408448e06ae8de6770e88af0e4e79570cda993693079a0a54454cf4080d88
```
