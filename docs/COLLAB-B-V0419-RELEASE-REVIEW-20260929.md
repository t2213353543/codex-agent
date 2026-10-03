# B组0.4.19发布复核

2026-09-29 03:36 +08：0.4.19已部署，19:34:07 UTC现场验收及B独立核对通过。

- release：`4f84927039696ea206670487ddd6316b8d9ffb9c8ccebf87ee79799c9916fe6f`
- 线上PID：2338498
- binary：`46119b03532c30f7f0588a254d115ee77ff64474e3848bf5234e3f274a8521c4`
- fullsource：`77fed72b7c1c1df1c205f527e3ee2cf5895ce31e22c59d733980dd50c951c6de`
- 1311份完整源索引；731 backend scope加既有embedded JSON为732执行源。

## 最终变化

结构化输出的内部formatter声明移到原有客户端输入之后、最终约束之前。保留原客户端项的字节和相对顺序，schema/工具权限和历史续接约束不放宽。目的在于schema变化时保留前面的长上下文缓存前缀。

管理端Go/SQL优先使用最终生成已观察到的有限规范错误码，同时尊重后续明确的本地最终错误归因。B真实loopback发现：上游failed带输出/限流，首次交付前又遇会话CAS冲突；新守卫保留真实upstream限流诊断，把请求管理分类恢复unknown而非rate_limit。Go/真实PG/race全部验证gateway/session_binding、visible0、TTFT清和原诊断保持。monitor对应条件也已修正，词表自身保持。

日志页和错误处理弹窗采用服务端已知rate_limit/authentication/overloaded类别解释，其余/旧记录保留原fallback；不再让旧错误文字覆盖服务端的这三类判断。B用实际producer保存行复现原3/5冲突并验证修正；主会话采用相同运行逻辑，前端重新验证。

完整done权威方案仅完成设计评估，**未纳入本版**。错误分类是管理解释变化，不改变模型输出、重试、冷却、计费或成功状态。

## 正式门禁证据

新源77fed的完整Go/vet通过：4804含子PASS/469环境SKIP/0FAIL；新增5本地顶层/18含子均实际通过。本机PG跳过不作为数据库证据。

新PG192204.055838为19top/32含子全部唯一RUN/PASS、无skip/fail；包括同一真实CAS producer保存、unknown筛选命中、保留final限流字段、rate_limit统计0。

新race192204.074371为1162含子PASS/17明确条件SKIP、433topPASS/17SKIP，两包成功/无DATA RACE或源码漂移。5required、20协作、4Redis、95official逐项唯一RUN/PASS；CAS真实BOUNDARY_RESULT与Go/PG一致。17skip为14live、2可选性能、1专用CLI二进制条件，实际10live由独立正式轮执行。

新10live192204.079273自然exit0，C逐项确认10top与20必需子项唯一RUN/PASS；testbinary6031594c…与732源绑定。新增直接候选schema切换：A input82560/read0/create82042，B input82566/read82042/create0，缓存读取占比99.3653562%，两次生成、schema答案、原输入、实际cache key及上游/响应/记录计量相符。此为受控单样本、测试client-cache-key；不等于生产api_key范围的自然全量表现。B次TTFT4069ms大于A次3798ms，没有延迟改善结论。

新前端237单测、build和3浏览器验收通过，246源/55制品与修订fullsource绑定；新monitor30项在Python3.9与隔离PG通过。首轮ad529源的Go/18PG/10live通过及race运行期间授权源码变化原样保留，不能冒充修订源。

## 发布核对

包内原validator、89制品/28证据/90归档文件名称与SHA集合已独立通过。PG/race/live精确映射732执行源同77fed冻结/current；新5required、PG19、10live及新前端/monitor来源逐项一致。首次B审计误把731 backend scope作为732执行源而停止，已精确加入既有embedded JSON后通过，未改包或放宽任意额外源。原首轮历史证据和4次AB保留为各自范围。

manifest/activate/postdeploy同version/PID/binary。现场主会话2生成实际SSE2055ms、JSON2622ms，均completed/HTTP200、2request/0refresh；请求/发送/返回模型及max→xhigh、输入输出/已知缓存读写、transport与ToolPolicy合同正确。top_p=1本地4ms400精确param、零派发；普通bound/mixed字段省略、三个精确CSVv4导出通过。settings27、api_key缓存、冷却false、账号配置和六迁移保持。

实际无凭据GET首页与`assets/Logs-DtgnGG3u.js`均HTTP200/PID2338498；SHA分别b7fc6fb2…/7d652237…与包完全一致，日志脚本含新三类解释分支，证明新前端实际被提供。B只读保存回执，0额外生成/SQL。普通smoke不触发schema变更、终态错误或CAS路径，相关行为依同源正式fixture/PG/race/10live证据。

首monitor-after-deploy实际capture19:33:50在两条成功验收请求写入前，进程累计0，不能扣除尚未出现的三条验收请求，也不能称丢记录或自然零错误。自然业务效果等待后续独立窗口。

证据目录：`.local/cross-session-20260928/01a0e7ed/v0419-release-audit/`；缺陷详细记录见`COLLAB-B-FINAL-CLASSIFICATION-20260929.md`。整体持续协作目标保持active，不将一次发布视为所有错误或缓存问题全部完成。

只读复跑已保存现场与前端回执：

```powershell
python -B .local/cross-session-20260928/01a0e7ed/v0419-release-audit/check_deployment.py .local/reliability-release/4f84927039696ea206670487ddd6316b8d9ffb9c8ccebf87ee79799c9916fe6f
```
