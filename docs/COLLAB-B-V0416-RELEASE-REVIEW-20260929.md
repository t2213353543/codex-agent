# B组0.4.16发布复核

2026-09-29 01:49（Asia/Shanghai）：0.4.16已由主会话部署，现场验收与B独立复核通过。

- release：`e145f1ba35394071be855149b9f4931dee4511a3d60ece0b59aa4190663fda65`
- PID：1911991
- binary：`70b42e593ba0e86d02c10ab625978988c356603a4f2256e5cb0f44ca8e609de4`
- fullsource：`72853cd01090c7a6b25b2e1bcf72c6c8beef701219e437cadb61d66fc38b6b4c`

## 本轮变化与范围

B的BoundSelection有限观测及详情/monitor已上线，仅解释选择时快照和恢复结果，不改变原选号/授权/重放守卫。附件实际路径观察、已转义路径保真及`top_p`精确本地错误帮助区分错误来源；没有把真实拒绝参数假装支持。

附件共享上传恢复仅在另一个上传任务返回context类取消/超时错误、等待方自身仍健康时允许再竞选一次。HTTP401/429/503不因此重试；C实测也覆盖leader父ctx健康但上游headers超时的情况，故不能窄称只有用户主动取消才触发。它不是HTTP429频发的已证解决方案。

## 正式证据

Go/vet实际通过，4702含子PASS/461环境SKIP/0FAIL；本版14新顶层与其子项共70均PASS。PG173634.831282的16项全部实际PASS，包括统一`TestPGBPSBoundAndAttachmentObservationRoundTrip`，B原单bound准备fixture未重复合入。

race173634.893147本轮完整单run PASS：新14、协作20、Redis4、official95逐项通过，无DATA RACE/OOM/源码漂移；server286选择项都有终态，291.717秒，采用明确600秒合成测试包预算。没有使用上版timeout组合回执。

C独立核完整9组live173634.900159唯一RUN/PASS、20必需子项全部通过，WS压缩/摘要链路均属于本次源。前端235单元与3标准浏览器通过，额外组合UI补证单列。monitor Python3.9的25项检查及只读SQL通过。

717执行源逐项匹配fullfreeze/current，包内716 backend scope加既有embedded JSON保持完整。不可变包92制品/32证据/归档93文件经包内validate和逐SHA核对通过；Windows本机内容检查不替代主会话Linux preflight/activate。

## 17:48:01 UTC现场验收

manifest、activate和postdeploy的version/PID/binary一致。两条真实生成SSE/JSON各一次，均200/completed、schema2双HTTP200、实际`/responses`路径、model静态映射与max→xhigh正确，响应与记录的input/output/read/create逐项通过既有判据。两个实际request、没有refresh。

`top_p=1`负例本地400，wire与record.ErrorParam精确top_p，零派发；三条普通标记请求均无BoundSelection残留，ToolPolicy/mixed/传输类型准确，3次精确event CSVv4导出通过。settings27、账号配置、api_key缓存域、cooldown=false和六迁移保持。

B只读原回执，没有再发生成/导出/SQL。脚本SHA `17959721f7f72313b07466c82590104fe3b693a15ac633b93eb8ad9805323653`已绑定。现场没有故意造成绑定失效或真实附件错误，这些正例由本地协议、race和PG覆盖，不能把普通smoke称成自然错误清零。

## 后续

历史六条bound503只能确定旧绑定不在当次候选集，具体原因需新观测。旧版另六条session-CAS错误是http_sse/记录HTTP200，与bound503不同；固定message可能由原CAS冲突或commit通用包装产生，已有聚合不足以定因。只读分支图在`session-cas-next-evidence/REPORT.md`，未据此改变winner或放松跨账号保护。

全部B发布证据在 `.local/cross-session-20260928/01a0e7ed/v0416-release-audit/`，重点`deployment-audit.json`、`package-e145f1ba-audit.json`及`gate-coverage/completed-{pg-go,race}/`。

只读复跑：

```powershell
python -B .local/cross-session-20260928/01a0e7ed/v0416-release-audit/check_deployment.py .local/reliability-release/e145f1ba35394071be855149b9f4931dee4511a3d60ece0b59aa4190663fda65
```
