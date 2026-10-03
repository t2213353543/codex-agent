本轮是基于已上线 .22 的上游认证错误语义修复，实际发布目录为 `C:/Users/Administrator/Desktop/项目/codex-agent-auth-release-20260929`。它与已取消的旧世代预留 .23 方案不是同一工作。本报告不把准备好的候选或复制来的旧回执当作新版本上线。

最终冻结 `49b7473457087faa133c21474ad94956af8e5a196f24445c1fdfabf50c2ea61c` 包含1351份源码。相对 .22 仅两处运行逻辑、版本号及相关测试变化，前端、六项数据库迁移、稳定入口、部署工具与 BPS 协议源码保持原字节。初冻结65b的完整Go有一项旧401预期失败，修订只把该测试公开状态预期改为503，身份数量与不串账号断言未改；原失败与首轮验证均独立保存。

B 已核的最终独立证据：

| 门禁 | 实际结果 | 来源与限制 |
| --- | --- | --- |
| 22PG | 22父项、35含子项各唯一RUN/PASS，exit0、0失败/跳过、无漂移 | 新231335.115148，765执行源与49b/current相同，binary eead0b22；新增auth真实gateway用例验证公开/逻辑503而实际两个上游attempt仍401，PG/JSONL/CSV计量未知值保留。 |
| race | 1364 PASS、20明确SKIP，15required全真，无DATA RACE/OOM/漂移，三包PASS | 新231335.134322；765执行源、1320归档文件逐SHA完整同49b。auth5父/32含子、shared7父与实际child proof、Redis4、official95、collab20、front11父15含子均真PASS。 |
| 包审绑定 | 四份明确review和原证据SHA已匹配 | 新PG、C phase59真实12live、当前race/shared与front绑定在 `review-index-49b74734.json`；尚须实际新package通过库存、原validator与归档检查。 |

race首轮230751虽测试通过，但结束时发生测试源码变化，B保留NOT_ELIGIBLE，不将首轮与最终轮拼接。新执行未扩任何门禁复用例外。普通Go及前端由A、真实12live由C独立审查；B未重复数据库、Redis、provider或部署调用。

这次修复的公开HTTP503与内部实际401是不同字段合同：逻辑record.HTTPCode跟公开响应变503，真实请求尝试中的ObservedHTTPStatusCode/StatusCode以及最终上游错误观察继续保留其来源。models旁路在本地回归覆盖，新增PG用例不代表所有models账号数据库关系已测。有效网关密钥的上游认证故障不能被当作客户网关密钥无效；本地无效网关密钥仍保401。

已公开SSE/WS后的认证终态属于明确的剩余边界：候选不会重放已公开请求，C真实created控制仍只1POST；其terminal auth字段保持原形，实际Sub2 semantic分类仍可识别为401。不能把本轮最终HTTP401→503修复概括为所有认证事件永远不会触发下游停调。自定义错误规则和临时backoff也须与默认HTTP401停用分开。

B 原件位于 `C:/Users/Administrator/Desktop/项目/codex-agent/.local/cross-session-20260928/01a0e7ed/v0423-auth-release-audit/`。`pg-review.revised.final.*`、`revised/race-review.*` 是最终49b结果；同级旧文件保留初65b事实。包审脚本仅版本pin改为0.4.23，库存/来源/证据约束沿实际验证过的 .22 脚本，尚未生成新包PASS。

07:27 更新：实际包 `37450c1bc61bacd9d216581c6fa08432afae12360056815651766134953aa9e8` 已通过B独立检查，应用SHA `bad6026f65d6addd0123bffa95fd14c35a58ef39070bb02a625a41a4a5aef5f8`。1351源与49b/current完全同，98制品、35证据、99归档文件库存/逐SHA匹配，归档SHA `48d146928aabd6ac2dafd1faa708bc3c2519531f027a182469b3787dd2dd9212`。包内原validator的纯验证与四份新源审查绑定均通过，记录 `package-37450c1b-audit.json`，未借旧65b的执行。

race原JSONL仍为包外本地原件，其SHA通过固定独立review与包内report/source关联、检查结束复核；不称原raw在tar中。包controller347004与入口source保持，但本包入口binary2299不等于生产现运行0840，主实际切换保留原入口未重启，这两事实不混淆。

新的真实SSE切流回执已出现：一条marked客户端请求、无自动重试，首delta之后切到.23 PID3337873，后续仍收到88个text delta及completed终态；旧.22与入口PID/bin保持。该回执不读取内部upstream generation attempts，也不是WS验收。它由A/C按原脚本/准备SHA具体审查，B只将新runtime身份与已审包绑定，不扩大为所有会话永不中断。

07:33 更新：实际 .23 后置验收已经通过，`post-review.json` 固定23:27:54回执与当前已审包：PID3337873、unit bad602前缀、入口目标revision105/18891/held=false。两次生成SSE2929ms、JSON1973ms均completed/record ok、各一次真实request attempt、无refresh；top_p本地3ms/400/零attempt；三次CSV v4及ToolPolicy原断言通过。settings27、计费默认开启、api_key缓存范围、cooldown=false、账号配置保持。

SSE内部actual read37015/create10，公开计费37025/0；JSON摘要cached字段null且known=true，按已执行脚本“原值缺省0、显式None拒绝”的具体判据可推actual已知read0/create37025，公开37025/0。这个推导不是把任意null转成0；未单独保存raw public/CSV，不冒独立原文重算。CSV仍核actual计量字段，未知本地负例维持空值。现场成功smoke没有故意触发认证失败，503分类依据同源新PG/局部/race用例，不称生产本次已自然复现上游401。

主后续已存切流markedUA账本，一条记录、一次实际request/HTTP200，input37303/output853与SSE回执一致，公开cache等actual read+write且内部不合并；这是另一次只读观测补证，没有再次生成。旧 .22 仅在切流观察期PID不变；随后主以排空与身份守卫退役该shared世代，`previous-shared-retirement.json`为retired=true。旧 .20按legacy策略另保留，不能把“切流时没停”写成旧 .22 永久仍运行。

生产与包身份绑定使用auth隔离树；共享目录另有并行性能改动，B不声称整个共享current等包。原CAS迁移完整修复、已公开streamauth语义及service_tier/history类问题仍分别跟进，本发布通过不代表所有错误已消除。
