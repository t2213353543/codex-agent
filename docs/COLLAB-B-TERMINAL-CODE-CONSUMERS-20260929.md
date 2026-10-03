# B组终态诊断读取与分类边界

2026-09-29 04:18 +08状态校准：当前已部署0.4.20。本文保存02:42时0.4.17/后续0.4.18诊断审查的历史边界；**不能把下文旧分类行为或旧临时读取器当成当前线上行为**。0.4.19已完成管理Go/SQL/monitor最终上游码与本地最终来源守卫、日志说明同步，0.4.20保持这一修正。当前实现与正式证据见[分类修正](COLLAB-B-FINAL-CLASSIFICATION-20260929.md)和[0.4.20发布复核](COLLAB-B-V0420-RELEASE-REVIEW-20260929.md)。

02:58 +08更新：终态诊断已随0.4.18部署，release65a40844…/PID2186899/binary133f242c…，18:56:28 UTC现场通过；[发布复核](COLLAB-B-V0418-RELEASE-REVIEW-20260929.md)保留原补验汇总器失败及离线通过证据。owner另给monitor的limit_codes增加usage_limit_reached安全码，本文件旧76bc监控结果保留为此前时点，不再作为当前分类器结果。ErrorType与code/最终规范码仍分开，不据诊断增加推断重试或旧自然根因。

## session_binding临时投影：0.4.17专用旧读取器

主会话临时`production-review/read_current.py`固定阶段白名单漏了已上线的`session_binding`，会把该字段投影为null。B提供单项补丁，基线3fc35981…→候选7ae2602a…，只加入这个固定枚举，原八项及未知值拒绝规则、error_family、窗口和SQL读取次数保持。

五项AST/字面检查通过，没有import/执行读取器，没有新SQL或网络。已知CAS文本原本能归binding_cas_conflict，不能把缺phase说成必然other_error。常驻monitor和管理UI都保留phase，生产record本身不因这处临时投影丢字段。当前已取窗口只有3条验收请求、0自然样本，尚未受此遗漏影响。

产物：`.local/cross-session-20260928/01a0e7ed/production-projection-session-review/{REPORT.md,proposed.patch,verification.json}`。04:18只读确认原脚本仍3fc35981…、未采用该补丁，且它固定断言旧PID2058745/旧binary4cf31…，当前.20不会通过该断言继续执行SQL。该建议只适用于将来明确复用这一旧读取器时重新校准，**不是当前线上待修字段丢失，也不是要求现在再跑一遍旧SQL**。常驻monitor及正式管理链保留session_binding。

## usage_limit_reached诊断码：保留原审查时点

owner真实候选新增此码到TerminalError有限日志名单，不是新实现error→response.failed转换；该转换已在0.4.17。新候选还清理失败包root/response的message/detail字符串，公开输出的验证由A负责。

常规error/response.failed中的usage_limit_reached早已被server事件分类规范为rateLimitExceeded。随后原final generation/attempt账本保存规范码，monitor能正确归upstream_rate_limit。因此没有依据声称新增原码日志会破坏常规配额处理。

B只提取monitor纯函数，分别给原始usage码及规范码的final/attempt/日志文本六个合成row：原码归other_error，规范码归upstream_rate_limit。raw final/attempt形式是能力边界测试，不是假称当前producer确会写这种字段；日志单独出现原码时确有诊断分类缺口。

在本节原审查时点，管理端`managedFailureCategory`与SQL镜像主要看HTTP码/错误摘要，并不看typed final/attempt。HTTP200的安全摘要只含code=usage_limit_reached时，原路径归unknown/P3。**此后0.4.19已加入有限final上游码优先**，只有明确本地最终source时仍走原管理分类；原rateLimitExceeded producer已由file/PG/UI/monitor证据验证。缺少final字段的纯日志合成row与实际producer必须继续分开，不能据原六个合成row声称当前自然quota仍被错分。

详细证据：`.local/cross-session-20260928/01a0e7ed/terminal-code-consumer-review/README.md`、`review.json`、`monitor-results.json`。本轮没有改运行源码、final allowlist、策略或计费，没有Go/SQL/真实模型调用。
