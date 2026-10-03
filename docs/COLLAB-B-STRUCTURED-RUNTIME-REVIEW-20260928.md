# B 组：新结构化输出候选独立回归

本报告针对 0.4.10 发布后正在实施的 structured-output 候选。线上 0.4.10 不包含这些新模块。本组只写私有快照、测试与报告，不修改主任务持有的实现。

## 合入后最新状态（21:14:52 CST）

主任务已完成对应修复；同一联合回归原样复跑 **15 个顶层 / 54 个含子项全部 PASS，0 FAIL、0 SKIP，受测期间源码无变化**。包括数值/Unicode所有红例与正例、客户跨namespace身份、历史/动态tool_search、late identity失败终态、取消与usage一致性。

当前绿报告 `.local/cross-session-20260928/01a0e7ed/structured-review-runs/20260928T131452.049877Z/report.json` 完整绑定源码与三份测试指纹。21:14:23 中间快照仅剩late identity红且有2个在途文件变化，保留原结果但不替代最新受测状态。以下21:06内容为修复前历史证据，不再表示这些问题仍未修复。

21:26:40，在stream owner进一步整合A/C修复后再次冻结并复跑相同54项，**54 PASS、0 FAIL/SKIP，运行期间无源漂移**。最新报告 `structured-review-runs/20260928T132640.277399Z/report.json`（同B组根目录），证明B原反例没有因后续整合回退；不覆盖A正在审查的网关写失败分类边界。

数值/Unicode最小补丁交付目录 `structured-numeric-fix-candidate/`；late identity补丁 `structured-late-identity-fix-candidate/`。实现owner按当前源码整合，B没有覆盖其文件。完整真实上游/HTTP/WS/历史/compact验证仍未由本地回归证明。

跨namespace冲突候选封存在 `structured-collision-fix-candidate/`：运行最小diff为1行类型/名称条件，另附既有准入测试契约更新，保留同名custom类型。此项当前源码独立复核10顶层/38子项全通过，动态discovery在此前版本已经转绿，不归功于本次静态冲突补丁。

21:17 主任务正在调查真实 structured case 的 schema mismatch（怀疑上游大数精度，尚待直接探针），并整合 A 异常用量落账及 C 冷却修复。B的本地绿结果只证明所列格式/协议边界，不证明上游必然输出符合精确schema的答案；未放宽本地validator来换取live通过。SSE错误终态交付等新增整合边界仍由主owner收口，线上版本保持0.4.10。

## 一条联合复跑命令

```powershell
python -B .local/cross-session-20260928/01a0e7ed/verify_structured_review.py
```

该命令先复制当前 Go module、BPS 和 publicerrors 源码到独立时间戳目录，核对复制期间没有源变化，再注入三组独立测试。所有模型/数据库配置被排除、依赖离线；结果绑定源码与测试 SHA，失败会返回非零。不会覆盖旧快照或修改共享源码。

## 21:06 CST 的联合结果

`.local/cross-session-20260928/01a0e7ed/structured-review-runs/20260928T130628.732887Z/report.json`：8 个顶层通过、7 个顶层失败；含子项37通过/17失败，零跳过、运行期间无源码漂移。

| 边界 | 当前快照结果 | 需要保持或修复的语义 |
| --- | --- | --- |
| 超大指数输出 | FAIL | 合法 JSON 数字不能使校验器 panic；超出实际可表示域应明确拒绝 |
| schema 超大指数约束 | FAIL | `minimum` 不能编译成功后静默丢失 |
| 六种长度/数量整数界限 | FAIL | 超过 int 可表示范围不能绕回导致错误接受或拒绝 |
| schema/output 孤立 UTF-16 surrogate | FAIL | 不得按替换字符校验后保留不同原始字节对外交付 |
| 有效精度与 Unicode 正例 | PASS | 大整数、1e400、1e-400、有效大 cardinality、代理对、转义字面量、U+FFFD 原样可用 |
| 取消时不下发成功 | PASS | OnComplete、OnEvent、writer 取消均不继续发送 completed |
| 真实失败用量、终态扩展字段、flat工具回显 | PASS | 保留真实 model/id/usage 和未知字段，过滤已登记内部工具 |
| 内部工具名称较晚出现在 arguments 事件、取消终态不含 output | FAIL | 早期匿名内部调用 stub 不能泄露到客户端 |
| 客户函数同内部名、另有 namespace，但上游省略 namespace | FAIL | 应在准入冲突检查中处理或正确恢复客户身份，不能被投影吞掉；旧 tool_search 声明同样复现 |
| 动态发现内部同名 function | PASS（本次新结果） | 较早独立快照红测已在当前联合快照转绿，不继续把旧失败当当前状态 |
| 客户 none/required/forced/parallel 策略基础矩阵 | PASS | 内部序列化不能算客户工具调用或授权新 forced 名称 |

## 已交接的实现边界

- schema owner 持有 `structured_format.go` / `structured_validation.go`。B 正在独立候选中准备最小 Unicode/数值保护补丁，保留可表示大数和有效 Unicode 正例，不采用 float64 舍入或 308 指数限制。
- stream owner 持有 `structured_stream.go`。身份预扫描需从 arguments 事件学习内部调用身份，不能仅依赖 added.item 和 terminal.output。
- request/Policy owner 持有 `request.go` 和网关接入。静态客户 function 名称冲突需考虑所有 namespace；动态发现仍以实际当前回归结果为准。
- B 先前的一行 `gateway/account_selection` 诊断候选仍独立保留，由统一整合者择机接入，未擅自覆盖网关。

## 证据位置与边界

- 数值/Unicode：`structured-runtime-numeric-review/`。此前 `structured-runtime-review/subject` 曾因本组并行复制目录碰撞被覆盖，已显式标记不可沿用；数值测试已在新独占目录完整重跑，之后的联合 runner 为每轮创建唯一目录。
- 真实流式接口：`structured-runtime-stream-review/`；旧 A 原型的问题与新 runtime 已修问题分别记载。
- 工具来源/策略：`structured-runtime-tools/` 与 `structured-tool-boundaries/`。
- 上述目录均位于 `.local/cross-session-20260928/01a0e7ed/`。以每轮绑定的源码判断，不用之前的红/绿结果替代持续变化中的候选验收。

本地协议测试不能证明上游接受转换后的完整历史、reasoning 密文、compact 或 schema 切换；这些仍由主任务安排真实边界验证。当前候选尚不能据此发布。
