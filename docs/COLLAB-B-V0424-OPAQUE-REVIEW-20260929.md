本轮 .24 目标是将本系统已经成功生成并登记的 opaque 内容继续路由给原账号，避免新会话或推断会话键变化后将原密文派给另一个账号。实际发布树为 `C:/Users/Administrator/Desktop/项目/codex-agent-opaque-release-20260929`，基底是已上线 .23 的37450包，生产在本报告初稿时仍为 .23。

初始冻结 `47fc5c834d9858d88ffced9496cb1b5aafb4a2af753bc27452f6ba891471f567`：1357份完整源、771份后端执行源。B `source-lineage.initial.json` 已核当时current完全相同；对.23只有9文件差异，含3个运行逻辑、两个本地测试、PG/Redis/live各一个测试及版本号，前端、数据库迁移和部署入口未变。后续任何来源变化须独立登记，不能让初稿代表新的源码。

需以真实门禁分别证明：

| 范围 | 应核证据 | 不由此推导 |
| --- | --- | --- |
| PostgreSQL与Redis接线 | 23个PG用例原终态；新用例真实账号、fresh Proxy读取归属、改变会话及B优先级仍实际派发A；持久化行的AccountID/attempt正确 | 同OS进程复用Database/Redis客户端不等独立两OS进程，也不自动验证所有计量/CSV/删除账号分支 |
| 并发与Redis原子批次 | 新race完整flags、20required、新opaque Redis4项真实通过，含末项冲突/WRONGTYPE/隔离与无部分写 | 普通Go环境的SKIP或私有内存用例不代真实Redis |
| 真实compact续接 | 新13live完成；compact生成后，换会话/B优先级仍使用A、恢复marker、无B调用、public输入逐字保留 | 当前fixture的public→下一跳保真，不等已经独立断言raw upstream→首次public的cipher完全相等；真实范围由C记录 |
| 新包与生产 | 最终source与package/evidence/tar一致，实际新unit/PID/bin与入口目标、现场请求和计量对账 | stage或复制旧.23回执不等.24上线 |

范围限制保持明确：归属索引只登记成功生成的完整reasoning/compaction结果，不登记任意入站密文，不改写cipher，不放宽已删除/替换账号或跨key/mode约束。没有回填旧历史；升级前或旧排空进程产生的未索引密文仍可能miss。`previous_response_id`已有history.Owner路径与新索引是不同恢复依据。内存/本地测试通过不能据此宣称自然7条opaque503全已找到根因。

B仅审主唯一执行的PG/race原件并准备不可变包检查，Go/前端由A、13live与可见cipher边界由C持有。B的私有CAS原子/生命周期候选属于另一问题，未混入 .24；完整同session失效owner迁移红测仍须后续真正网关接线解决。

本轮证据目录：`C:/Users/Administrator/Desktop/项目/codex-agent/.local/cross-session-20260928/01a0e7ed/v0424-opaque-release-audit/`。本初稿不声明正式门禁、发布包或上线已全部通过。

08:05 更新：B独立核23PG、36含子项全部通过，771执行源与47fc/current完全同；新增PG用例的双Proxy、真实账号、Redis归属及durable AccountID/attempt通过。race亦完成：1396 PASS/21明确SKIP、493父项PASS、三包PASS、0DATA RACE/OOM/漂移，20required及opaque9父32含子（含4Redis父9含子）全部实际通过，1326归档文件逐SHA相符。原件见 `pg-review.*`、`race-review.*`。

真实13live的新增OpaqueOwnerRecovery却失败：case1 compact200通过，case2公开HTTP502；原结果没有保存失败分支的actualposts、owner或记录phase，无法据此断言错派B、密文改变或上游crypto拒绝。它是尚未通过的必需验收，不被PG/race绿色替代。`review-index-47fc5c83.incomplete.json`只绑定PG/race/front三角色，明确NOT_ELIGIBLE且缺live，不生成包PASS。纯fixture的JSON序列化等价性边界由C唯一复验，B只补失败短路与安全诊断字段合同，不重发模型或修改冻结代码。

08:08 更新：B `opaque-live-502-boundary.*`核原fixture的method/顺序、owner headers、GetBody、形状及input前缀保护，都可能在`posts++`及真实`base.RoundTrip`之前返回error，再被网关转为502。wrapper被调用或record有一次attempt，不等实际已向provider发出请求；HTTP200后解析/完成分支也可能502。原失败没有计数，不能倒填0或1。作者私有新fixture7a0aecf4已经改用实际客户序列化body的input作为出站逐字基准，并保留decodedcipher、owner、dispatch数量、marker与usage断言，另在失败前保存安全分类/计数；B没有再创建竞争补丁。该新写法与C本地合成误拒只证明可达性，尚不证明首轮真实502根因；首跳upstream.Output到public.Output的独立cipher对账仍按C明确限制处理。

08:16 更新：中间focused run000708.978812只执行新7a0的OpaqueOwnerRecovery，exit0、两个case都200、真实posts1→2/no blocked owner/no wrapper拒绝，第二步marker与输入保真成功。它证明本次有限续接成功，不倒填首轮失败根因，也不替代完整13组。

最终重新冻结为 `05135160ee13199fdb86c1a956c3f27a288670bc2c9c5bb6702bfdf32ef0a22b`，1358完整源、772执行源；仅将live fixture改为f6033862并新增040062f9序列化测试，所有运行代码与首47fc相同。f603新增首跳raw compact→public的count/type/id/decodedcipher一致断言，该断言不属于中间7a0已执行范围。最终全Go由A核5025PASS/491SKIP、vet0/no drift，新23PG001000.477097、race001000.516344、13live001000.515693分别跟原新run；原47fc、focused与最终结果独立保存。

`FINAL-REVIEW-BINDINGS.md`已将最终包审的首跳字段与新增序列化用例列为明确绑定要求；旧 `review-index-47fc5c83.incomplete.json`保持未满足完整验收，不原地篡改成新源PASS。检查器最初仅把新测试文件名猜错多了live_，已保存该本地检查误报并按实际路径更正；不把它说成产品源码漂移。

08:22 更新：最终0513的新23PG、完整race和13live均已独立闭合。新race1397 PASS/21 SKIP、20required与新增serialization实际通过，772执行源/1327归档文件同最终源码。C最终13live实际执行f603：compact首跳count/type/id/decodedcipher匹配，case2在新会话和B更高优先级下仍同owner、2actualPOST/noB/marker恢复，actual read36970/create11030→public48000/0、内部数保持。这里不把首跳特定字段相等扩大为全部未知扩展字段逐字相同。

实际包 `b3adc5ace7a4ca819a334e1e8b828aee00a7f36f797c8d048d7110f75babe5d4` 的B独立检查通过，应用binary `0cdd4ed7048b36662ff5166c9129757014d1e555401fe3d88142e367a740156e`，1358源=current，98制品/35证据/99归档文件逐SHA与库存完全一致，tar摘要 `d5e9f42756f2d02f66488d6d616a1381b20a1e994e7b312ce310ce4a7e2200d1`。包内原validate纯执行与四份最终review绑定均通过，见 `package-b3adc5ac-audit.json`、`review-index-05135160.json`。race raw仍包外精确关联，不伪称随包封入。

检查器支持了C所用的明确整体 `eligible_for_current_backend_acceptance` 判断指针，仍要求严格类型true、review无ineligible、对应原source映射精确等包backend、每个scope断言和raw/result SHA齐；没有放宽原失败或省略新增opaque用例。此前仅版本替换的prepared记录保持历史，最终脚本以实际包审文件为准。包审通过不等生产已切换，后续以主唯一activation/post回执继续。

08:25 更新：主已保存实际active-status，B `deployment-identity-review.json` 15项全部通过。registry.active 为.24、PID3588453、app0cdd、releaseb3adc、后端18890；front精确同instance/address、revision106、held=false、durable=true、无pending，符合shared admission。post00:22:16的新version/PID/bin/unit与包一致、身份前后未变，2生成+1本地负例+3CSV通过，settings27/计费开启/api_key/cooldownfalse及账号配置保留。

本次health观察94次全部ready、同一client keepalive跨旧bad602→新0cdd、最大17.94ms，旧PID保持只限该观察期；本轮没有再做模型SSE切换请求，不能套前版SSE运行当本轮新执行。保存registry有2个draining世代，这个数量不能独立证明它们已停止或无活动。B只核原回执，未新增production/SQL/provider调用。

新成功生成结果的归属索引已随本版上线；旧history回填工具仍未在生产apply，不能声称所有升级前密文已恢复。CAS迁移完整网关协调同样仍属另一未闭合工作。

08:28 更新：B `post-review.*` 完成新现场回执细核。SSE3995ms与JSON3233ms各一次实际request/200、0refresh；top_p本地3ms/400/零attempt；三CSV v4/77列/脱敏唯一记录/actual计量及账本对照原谓词全通过。两次生成input37249/output5，内部knownread0/create37025，公开37025/0；read0仅由当前严格谓词“缺省0、显式null拒”与实际passed共同推导，摘要null原样保留，不把未知值一般化成零。没有另存公开response/CSV全文，不冒二次原文解析。

post脚本c6a30对上一版只改版本和marker两处字面量，8个相关runtime/fixture/计量/版本源与本包一致；无额外provider、SQL、SSH、export或部署操作。主旧.23退役尝试因仍有连接/活动返回false并保留，不把guard拒绝称故障或改写为已停；旧记录回填正在另行只读dry-run准备，不计本次发布覆盖。
