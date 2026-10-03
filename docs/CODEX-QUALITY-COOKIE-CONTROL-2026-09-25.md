# GET /models 与 helper 生命周期对照：执行前缀结果

**后续独立实验：** [v7 当前网关源码／设置与直连的八格同账号对照](CODEX-QUALITY-GATEWAY-DIRECT-2026-09-25.md)已完整执行；新选账号两路均为 21，g7 两路均为 29。该批选择近期无可见网关流量的账号，没有恢复 q0 或补齐本实验中断格。本报告的七格前缀与停止结论保持。

2026-09-25，v6。**预登记12格实际执行7次模型POST、5次目录GET，随后因q0被外部停用而停止。5份完整模型答案均为29，另2次q0 POST返回429；未得到答案21，也没有完成三模式镜像设计。** 不能将这组前缀解释为cookie效果的完整对照，更不能外推Go网关质量恢复。

登记时间为UTC 07:28:51，批次停止并清理于07:33:24（北京时间15:28:51–15:33:24）。审核者没有发模型请求、恢复账号、补跑缺失格或操作部署。

## 设计和实际执行边界

两个账号q0/g7，三模式每账号各2次，计划最多12个POST、8个目录GET。正式登记前原拟g2的usage GET返回401，因此换为当前allowed的g7；选择理由与库存hash在[selection.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cookie-controls-v6/selection.json)中预先保存。没有刷新401账号或根据本轮模型结果换样本。

| 模式 | 登记干预 |
|---|---|
| cold | fresh helper → POST |
| same_helper | fresh helper →完整GET/models→同helper POST |
| separate_helper | fresh helper G→完整GET/models→清理G→fresh helper P→POST |

每次请求使用独立IPC socket；同helper和分离helper的scope、PID、存活、清理状态分别记录。各格不共享helper，前后quota读取另起helper，不预热实验helper。GET固定为 `/backend-api/codex/models?client_version=0.155.0`，真实CLI新捕获确认UA/originator/version与POST一致、Accept为`*/*`；没有复制POST的Lite/turn/session headers或手工注入Cookie。

GET必须完整200并收到IPC END，完整models schema有效且目标`gpt-5.6-sol`唯一存在，才发送固定POST。目录内容不改写POST。无refresh、重试、fallback或换账号补发。

| 固定对象 | SHA-256 |
|---|---|
| 原题input | `cd771ba38f8f75696ba703c7d3e91e5273588c91cb8c4145e3ce4b077c075c3f` |
| 官方CLI 0.155.0 | `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501` |
| 实际native helper | `9110b869e65db6bf7664e39fcfe3b2ec17fa7a0207ddbeccf28d73dbc12ccce8` |
| 新捕获原始POST body | `f8147e587f877b1a518e2f0e5651d3b1c860e6c63950c83f4c2c884dd3256f3d` |
| 新捕获POST head | `11ba9f5e0fb343f88bb0c862d856c3b90b71890423e7f1adf8049be983abf2a1` |

模型为`gpt-5.6-sol / medium`。原题答案21只参与离线评分，没有进入请求。原始CLI请求在任何Go policy之前截取，11个顶层字段，不补空instructions；全部已发送POST的实际body hash相同。预检记录的Go binary为`a4777428…`，但本轮模型路径是native helper直接官方HTTPS，Go推理转发不在路径中。

## 逐格结果与停止

`—`是无模型答案/usage，不是零。5个已执行目录GET均200、目标唯一、target model完整摘要hash一致：`0c26d83c081feafbe593e3958071ea5301ac29a156077b569c74da730435d079`。

| 格次 | 账号 | 模式 | GET | POST | 答案 | input | cached | output | reasoning | 格耗时秒 |
|---:|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| 01 | q0 | cold | 不执行 | 200/completed | 29 | 6879 | 5888 | 972 | 758 | 62.358 |
| 02 | g7 | same_helper | 200 | 200/completed | 29 | 6879 | 5888 | 709 | 516 | 59.327 |
| 03 | q0 | separate_helper | 200 | 429 | — | — | — | — | — | 0.882 |
| 04 | g7 | cold | 不执行 | 200/completed | 29¹ | 6879 | 5888 | 766 | 516 | 53.664 |
| 05 | q0 | same_helper | 200 | 429 | — | — | — | — | — | 0.753 |
| 06 | g7 | separate_helper | 200 | 200/completed | 29 | 6879 | 5888 | 532 | 377 | 38.061 |
| 07 | g7 | separate_helper | 200 | 200/completed | 29¹ | 6879 | 5888 | 703 | 516 | 50.108 |
| 08 | q0 | same_helper | 未发 | 未发 | 前置检查停止 | — | — | — | — | — |
| 09 | g7 | cold | 未执行 | 未执行 | — | — | — | — | — | — |
| 10 | q0 | separate_helper | 未执行 | 未执行 | — | — | — | — | — | — |
| 11 | g7 | same_helper | 未执行 | 未执行 | — | — | — | — | — | — |
| 12 | q0 | cold | 未执行 | 未执行 | — | — | — | — | — | — |

¹ 冻结自动评分保留`answer=null / no_unambiguous_final_answer`，两份公开final均有boxed 29及后接的保证结论。人工注解独立保存，不修改原report/评分器：04的final SHA为`05900a3b9a135a1b54b83c504d99c69f130f79ec11a8a6707094dd8a8be30100`，07为`dae107482f22ff76345f3381ce3525331d3e34f8f528d61d8b4fd3a8796a1dbe`。其余3份由原评分器直接识别29。按用户21判据，实际完整答案0正确、5错误；2份429和5个未发送格不计错答。

运行收据为`finished=true / status=stopped / stop_reason=epoch_observation_failed`。08在第一道read_epoch失败，没有发本格目录GET或模型POST。主任务随后只读观察到q0记录仍存在，但`enabled=false / version=21`，user、identity、token、workspace四个hash与登记全部相同；g7仍enabled且四hash不变。因此这是**外部账号启用配置变化触发的停止**，不是已证实的credential rotation。没有恢复或绕过disabled状态，后五格不补齐。

停止后只有q0 cold一次、same一次、separate一次；g7 cold一次、same一次、separate两次。不能再使用原12格镜像平衡或每条件两重复的结论。

## 429和并发负载证据

03、05的HTTP429安全错误体均为32字节、SHA `96c534cc320264e5091e907e67dc8667bfadc8b0cd6172031bfa91c7c5d8f82d`。独立离线对公开固定文本 `{"detail":"Rate limit exceeded"}` 计算精确UTF-8 hash及字节数，与保留值一致；字段名`detail`的hash也与唯一字符串字段摘要相符。该识别另存[已知常量匹配证据](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cookie-controls-v6/review/observed-error-reconstruction.json)，没有改原report、恢复私有响应或新增请求。

所以可以确认响应明确描述Rate limit exceeded；新工具记录03的Retry-After为33秒、05为38秒。它不说明限流的具体策略、容量分配、账户标签或质量机制。v5两份429有相同错误体hash，但其Retry-After数值未观测，不猜测。

7格前后usage GET均allowed。q0的primary用量在01前后92→93%，03为95→95%，05为98→98%；GET allowed不保证模型POST在当时不会受请求级限制，也不能把98%直接改写成“总额度耗尽”。

另外，主任务在只读数据库事务中按同一账号记录统计与每格时间窗重叠的已完成网关请求，发现q0有实际生产并发流量，而g7各实验窗的可见记录为0：

| q0实验格 | 重叠已完成网关请求 | 其中同gpt-5.6-sol | 其中HTTP429 |
|---|---:|---:|---:|
| 01-c-q0 | 40 | 22 | 4 |
| 03-d-q0 | 12 | 6 | 2 |
| 05-s-q0 | 7 | 2 | 1 |

独立审核了[聚合脚本](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cookie-controls-v6/concurrent_traffic_remote.py)的口径：`BEGIN READ ONLY`、20秒statement timeout；`created_at`由网关`time.Unix(rec.Time,0)`写入，对应请求开始，结束由`record.latency_ms`推算，窗口加±1秒余量。直接实验绕过网关，本身不在这些日志里。计数是**各窗口的重叠请求数，可能重复，不能把40+12+7相加成不同请求总量**；也不是峰值并发数或纯模型POST数。只覆盖查询时已持久化的请求，未完成、外部渠道以及早于查询向前15分钟截取的长请求可能缺失。

这提供了正面的并发负载混杂证据，限制对q0各模式429差异的归因；不能据此断言并发是服务端限流的唯一原因，更不是错答原因证明。

## Cookie对照能证明到哪一层

正式工具先通过相同helper/相同合成凭据的真实HTTPS测试：cold无cookie；same_helper保留允许且Path匹配的基础设施cookie；separate_helper清理后POST不继承。非允许session cookie和Path不匹配cookie被排除，5次独立TLS连接确认`cli_request`每请求重建client而进程cookie jar可保留。这只验证测试环境中的实现行为，不等于捕获了本批真实POST的Cookie。

本批5个模型目录GET都收到`__oailb`、`__cf_bm`、`__cflb`三种允许名称的Set-Cookie；02/05 same_helper的POST未返回新的Set-Cookie，分离模式的POST可返回新的Set-Cookie。报告记录这些响应的安全摘要，但**没有直接抓取实际POST发出的Cookie值**，不能据响应有无Set-Cookie断言发送或命中。

same_helper保留进程状态，不止cookie；先行GET也可能改变服务端状态。实际前缀中q0 cold完成、same和separate都429，g7三模式均完成但均29。没有出现答案21；又存在账号被停用、样本未镜像完整、q0生产并发等混杂，不能宣称某种cookie处理改善或降低质量。

## 独立一致性核验与清理

独立结果审核对7份report、5次目录GET、10个helper scope及第08格停止证据通过，`findings=[]`。这里`passed=true`只指已执行证据一致，另明确`full_predeclared_design_completed=false / passed_full_experiment=false`；不能把“脚本核验通过”当成“实验完整成功”。检查包括：

- 原始POST body固定、请求模型/effort一致，GET目标唯一有效目录，不使用GET内容改POST；
- same_helper同scope/PID、GET和POST不同IPC，separate旧scope正常清理后新scope，跨格无helper共享；
- 每阶段认证/workspace与本格epoch一致，前后身份/usage、报告和execution/control sidecar一致；
- completed/final/usage、最终文本hash与冻结自动评分重算；人工漏解析单独绑定hash；
- 实际7POST/5GET/10helper，没有补跑、重试或refresh；08无请求记录，09–12未开始。

启动前独立故障注入13项通过；主任务完整工具包146项运行，145通过、1项因缺openssl跳过，另用精确相同源码/测试hash和helper的TLS套件23项通过、0跳过补足。详见[控制审核记录](C:/Users/Administrator/Desktop/项目/codex-agent/docs/quality-20260925/cookie-control-review.md)。

UTC 07:37:48的服务器[清理核验](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cookie-controls-v6/cleanup-verified.json)确认inventory、capture、control三个unit均`MainPID=0 / inactive / dead / exit=0`，私有凭据/raw文件均不存在。服务器execution及7份report原始字节SHA与本地逐份重算一致；不是下载器重序列化后自行声称匹配。全部已启动helper正常回收。

## 证据和离线复跑

- [预登记计划](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cookie-controls-v6/plan.json)与[实际执行收据](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cookie-controls-v6/execution.json)
- [停止边界只读观察](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cookie-controls-v6/stop-boundary-observation.json)
- [生产并发聚合证据](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cookie-controls-v6/concurrent-traffic.json)
- [独立逐格结果](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cookie-controls-v6/review/results-final.json)与[摘要](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cookie-controls-v6/review/summary.json)
- [人工答案判据及final哈希](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cookie-controls-v6/review/human-annotations.json)

```powershell
python .local/research-20260925/cookie-controls-v6/review/review_results.py --require-complete --output .local/research-20260925/cookie-controls-v6/review/results-final.json
```

复跑只读本地证据，不发模型或网络请求。`--require-complete`要求已存在终态及清理证据，不会补齐未执行的计划格。
