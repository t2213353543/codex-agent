# B：最终错误分类与消费者协作

2026-09-29 03:36 +08更新：本轮修正已随0.4.19部署，release4f849270…/PID2338498/bin46119b03…，19:34:07 UTC现场及新前端静态资源回执均独立通过。详细发布证据见`COLLAB-B-V0419-RELEASE-REVIEW-20260929.md`；本文保留原反例与修正链，不把普通smoke当CAS故障现场复现。

## 已修正的显示分歧

后端新候选依据最终生成的有限上游错误码分类，前端日志说明原本只匹配错误文字。作者真实本地网关输出的五条记录中，三条出现管理类别与UI解释不同：authentication-only仍显示请求失败，model_not_found+rate type显示模型不可用，overloaded+auth type显示上游繁忙。

B使用已保存的producer记录直接调用真实JS，原3/5冲突、私有候选0/5冲突；旧测试及两项新消费者测试共3topPASS，另5guard通过。仅status=error时采用服务端三个既有类别，未知/缺失类别保留原fallback，不在前端再实现一套协议码规则。Logs及错误处理弹窗的管理记录都实际携带该类别。

主owner随后采用相同运行逻辑：当前helper21f8cac5…与B私有cc25ecb1…仅注释不同；`adopted-ui-source.json`记录这一源码事实。新前端正式构建/浏览器与发布绑定归A/root，本组未用旧.18前端证据替代。

## 已复现并通过正式Go/PG修订用例的分类覆盖

唯一私有loopback实际到达：上游response.failed同时带合法message output和rate_limit_error，replacement首次交付前遇到真正的会话CAS冲突。最终记录正确保留gateway/session_binding、CAS原因、TTFT清空且visible0；然而它也保留先发生的真实upstream/rateLimitExceeded。新管理规则仅看后者，错将本次本地交付失败归为rate_limit。

上游限流真实发生，保留该观测本身没有错。主owner已为Go/SQL同步加上ErrorSource为空或upstream的优先规则准入条件，显式gateway/client/transport等绕回原本的最终错误分类；CAS场景为unknown，真实final限流仍保留。新增TestBPS和TestPGBPS使用同一个真实producer，其中PG用unknown筛选并验证rate_limit计数为0。B修订源码审查通过（query cd47ca3a…、fixture ff59ad8c…）。03:26独立读取77fed冻结源的正式新Go及PG192204.055838，两个CAS用例均唯一RUN/PASS，BOUNDARY_RESULT逐字段相同，相对原红只category由rate_limit变unknown；其余真实上游诊断、gateway/session_binding、1POST/1attempt、visible0/TTFT清全部保留。记录并未泄露输出，也没有失败当成功或新增重试的证据。

`.local/cross-session-20260928/01a0e7ed/final-classification-review/provenance/`保存原唯一红测、fixture和740项私有源快照（不冒充正式731源定义）。原探针还要求清空final，报告明确排除此额外假设；交接版`handoff_test.go.txt`只要求分类不覆盖CAS、改用TestBPS前缀，交接版尚未运行。没有改canonical运行代码或重复原红。

复跑原红的一条命令：

```powershell
go -C .local/cross-session-20260928/01a0e7ed/final-classification-review/provenance/subject test ./internal/server -run '^TestIndependentFinalClassificationFailedTerminalCAS$' -count=1 -v
```

## 正式门禁范围

B只读首轮PG191259.183825：18top/31含子全部实际RUN/PASS。race191259.202122最终1161含子PASS/17SKIP、432topPASS/17SKIP，两包成功；4required、20collab、4Redis、95official均逐项真实通过。双方731执行源同ad529779首轮freeze。race的source_changed_after_freeze=true准确对应已授权的本地分类守卫修订，不是竞态失败。

修订正式source77fed72b…与PG192204.055838、race192204.074371分开留证；新增第5个required为真实CAS用例，不把它倒算成旧轮漏选。`v0419-release-audit/cas-red-green-audit.json`保存新Go/PG与原红原件SHA，并确认13项相关runtime链只管理分类源码变化；两个fixture名字/断言不同如实记录，不伪造同一fixture哈希。旧PASS不充新修正门禁；整轮新Go/19PG/race/10live按主owner唯一原句柄继续核对。

没有B模型、生产SQL或部署调用；新增执行仅纯JS消费者验证及一次本地Go loopback组合反例。整体协作继续。
