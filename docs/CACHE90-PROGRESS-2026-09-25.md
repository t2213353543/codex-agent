# 整体缓存率 90% 持续优化

目标仍是用户提出的 **整体缓存率 90%**。当前未达到，不能以合成热请求、筛除未命中请求或改变分母宣称完成。

## 当前证据

2026-09-25 14:38:26（北京时间）核对运行 binary `bb92216f7ef2aa0f103513a4178dc2af675236c30953e423207bdc41aca277ad`，包括此前缓存改写及之后已合并的身份刷新、WS 链策略和原生 WS EOF 修复。原生 helper 当前为 `9110b869e65db6bf7664e39fcfe3b2ec17fa7a0207ddbeccf28d73dbc12ccce8`，本轮保留。

| 时间窗口 | 输入 token | 缓存 token | 整体加权缓存率 |
| --- | ---: | ---: | ---: |
| 近 1 小时 | 31,139,175 | 9,491,968 | 30.48% |
| 近 24 小时 | 40,314,738 | 10,819,456 | 26.84% |

随后近 30 分钟样本的 494 条成功请求均来自同一租户、同一上游账号，说明账号漂移不是该窗口的主要解释。同窗口有 2,363 条网关冷却拒绝和 109 条上游 429；这些没有输入用量，未被用来抬高或压低缓存率。

Sub2API 当前活跃 OpenAI API key 账号 146849 的上游就是 `http://43.225.196.170:18879/v1`。当前其模型缓存率 28%–39% 与本项目路径一致，不是独立对照。今天其他上游历史中 sol 约 69%、astra 约 75%，窗口和工作负载不等，作为调查线索而非前后改动因果证据。证据在 `.local/cache90-20260925/{baseline-audit,traffic-baseline,sub2api-comparison,sub2api-routing,sub2api-history}.json`。

## 本轮改动

1. 无显式会话的请求按租户、模型、稳定指令/工具和首条用户内容形成独立软路由偏好，对同优先级候选使用 weighted rendezvous。保持当前授权、配额、冷却、并发和凭据校验；不产生七天硬会话、改写请求内容或重放已经发送的请求。代理也保持确定性选择。前缀锚最多 1 MiB，不复制大型工具输出。该改动消除多账号流量的随机分散，不能声称解释目前单账号窗口的低缓存。
2. 支持本地兼容会话头 `X-Session-Affinity`、`X-Session-Id`、`X-OpenCode-Session`、`X-Conversation-ID`，现有标准会话头优先。保留 `Connection` 排除、重复标量处理和 Unicode 精确身份，不伪造上游头或 metadata。
3. 在实际已准备的 HTTP/WS 请求上记录被动缓存诊断：每进程随机盐 HMAC、key 类型、原始 scope 类型、账号/键/指令/工具/控制变化、历史项前缀和字节前缀下界。原文、凭据、原始会话键不落盘。内存最多 4096 scope、每项 128 个历史摘要和 64 个 16KiB 块摘要；超过 2 MiB 的请求明确标记 `body_limit`。scope 不依赖 Authorization，不因 token 刷新掩盖账号漂移。

诊断附加于现有 request_logs JSON，无数据库迁移，不修改 upstream usage、缓存率或计费字段。弱 cache-key/prefix scope 不能被误解为同一真实会话；`previous_response_id` 增量明确不做完整历史比较。原始字节共享只是下界，不当作 token 命中量；首次/限额/缺观测都有显式状态。并发请求的前驱可能尚未完成，需结合原始请求时间及耗时判断。

参考 Sub2API 当前源码 `openai_gateway_scheduling.go` 和 `openai_content_session_seed.go` 的无会话内容锚及兼容头实现。未照搬 Grok 的 prefix helper，也未强制添加 ChatGPT Codex 未证实支持的 retention/breakpoints。[公开缓存原理](https://developers.openai.com/api/docs/guides/prompt-caching)与[诊断维度](https://developers.openai.com/api/docs/guides/prompt-caching/diagnostics)仅作原理参考，上游实际行为以日志为准。

## 验收与复跑

整体始终使用 `sum(cached_tokens)/sum(input_tokens)`，包含窗口内所有请求；分组统计仅供定位，不能替换整体分母。SQL 在同一 PostgreSQL statement 的快照内聚合，避免采样时新请求完成导致各表数字不一致。

```powershell
python tools/cache_rate_report.py --minutes 30
```

也支持 `--since 2026-09-25T00:00:00Z` 固定上线后窗口。后续需先确认真实覆盖率，再区分新会话、键分裂、前缀变化和稳定请求仍低命中；达到目标以前保持持续目标 active。

## 组合验证

Windows 完整测试 851 顶级通过、0 失败、149 条件跳过，vet 无诊断；Linux race/PG/Redis/当前 native helper 完整测试 976 顶级通过、0 失败、23 opt-in 跳过，共 3003 个含子项终态，vet 无诊断。真实 Rust HTTP、WS诊断及PostgreSQL持久化回归均通过。冻结源码 `b2ef2700cdfe6c6acc72d0a5014c21ad76847183bbe91b4492708777ef03d302`，候选 binary `a47774284a6be8416b9d0ed2a20b2fb8505f9064fbb9de38258ec534ae20136a`。

当前前缀诊断对 1 MiB body 约增加 8 ms 准备 CPU 时间及约 6.65 MB 临时分配（本机微基准）；这是有界的调查成本，未改变请求正文/转发协议。后续需按实际流量收益继续优化该观察成本，不能将准备阶段增加隐瞒为“零开销”。

发布前45秒自然空闲等待超时，未切换；随后采用现有服务55秒优雅退出路径进行更新。未新建第二个gateway或改变PG/Redis/账号并发。

## 第一阶段已上线，目标仍未完成

15:10:05（北京时间）新 binary `a4777428…0136a` 完成 smoke：models 200/9项、WS 101、health ready。运行环境、11z以前的所有drop-in、账号调度配置、UI和当前 native helper 保持；无数据库迁移。验证用临时key/group在15:10:50删除，账号原有分组保留。

先前一次未限定账号的 models smoke 得到上游 `token_revoked` 401，已回退旧版；旧版本同池也复现，不能归于新代码。后改用最近成功请求的账号单独临时分组核验，避免不相关失效凭据影响发布判断。持续流量期间正常凭据刷新会改version/expiry，因此发布检查对比操作员配置，相关动态字段不当作发布篡改。失败回执保留，最终成功以 `release/activation-result.json` 和 `runtime-smoke.json` 为准。

最初不足一分钟的12条成功请求仅8,110输入token，全是短/中等请求，缓存0；该样本不是有代表性的90%验收。正在积累新诊断下的真实长会话，不能借用改动前的84.75%热轮数据宣称本轮已完成。

15:22:07 的上线后窗口：737 请求、187 成功，输入3,459,277、缓存772,992，整体22.3455%。诊断确认有同一账号/模型/key/config、前1MiB字节完全一致的13.2万token长对话，连续多轮仍零缓存；另一个约10万token会话出现95%–98%单次命中与零命中交替。这排除了这些具体样本的账号漂移、键变化及前部JSON变化，但1MiB以后的历史与隐藏渲染内容仍未完整观测。

发现上游Sub2API API-key转发白名单只接受旧underscore身份头，会丢官方CLI的session-id/thread-id，正文真实metadata仍保留。已准备仅恢复缺失真实metadata头的候选 helper 和26项测试，**未接入生产**。隔离header A/B完成三次17,000-token请求均零缓存，第四次429停止，无法证明该补头能提高缓存率。

官方 `prompt_cache_options.comparison_response_id` 经当前ChatGPT Codex可接受；实际第二轮返回 `prompt_cache_diagnostics.type=unavailable`，没有服务端原因。此能力已实测，不能把unavailable算cache hit或归因证据。

同一账号级缓存键覆盖15种prefix、22个scope，实际默认键组3,644,684输入/743,936缓存=20.41%。这不是已经证明的路由过热（平均约2.6RPM，较新模型的公开协议也不依赖key做路由）。第一组配置前缀分键隔离ABBAAB：每次16,990输入，B三次均16,128缓存（94.93%）；A三次0/0/16,128。样本很小且B可能受预热顺序影响，需反序复验后才能决定是否采用。WS若改算法须冻结响应链策略，不能直接按缺省delta重新派生key。

所有隔离请求标记 `client_request_id=cache90-*`；统计工具额外分层 `bounded_experiment` / `natural_traffic`，整体分母仍包含全部请求，不用实验数据抬高自然流量结果。实验凭据仅保留服务器，结束清理临时key/group。

反顺序复验第一轮B为0缓存，第二轮A返回429即停止，因此第一组B=94.9%尚不能认定稳定胜出。实验用临时分组、密钥和密钥文件已于15:32:47清理，主服务ready。未上线补身份头或前缀分键算法。

## 15:36 状态

15:32:50 自上线窗口总率24.5785%，自然流量24.3128%（9,166,940输入/2,228,736缓存），仍未达到90%。新增诊断的覆盖与真实数据足以确认“同账号、相同已观测前缀仍有间歇零命中”，不足以把原因确定为头、键或上游路由。

为消除短时间突发限流的干扰，准备了8次、间隔至少90秒、尊重真实cooldown、自动清理临时访问权限的有界ABBA/BAAB任务。实际启动后前置条件失败，**零请求**：实验账号 a_441d… 已变为 enabled=false/version21（早先为true）；其余账号最近5分钟没有成功推理记录。未重新启用该账号，未绕过其绑定或冷却。任务 `codex-cache90-paced-probe.service` 已终止，`paced-partition/result.json` 明确stopped/requests0/temporary_access_cleaned=true，不是仍在运行的等待。

目标保持active，当前工作可以继续做候选离线验证，实测必须重新确认当前可用账号及运行版本，不能沿用已失效的实验账号状态。需要保留已禁用账号的管理决定，不把“恢复测试”理解成可擅自启用。

未证实收益的补头候选已从root Go文件移出，保留在metadata-header/candidate-overlay.json隔离加载；前缀分键候选同样仅为partition-candidate overlay。正式源码与已发布版本逐文件一致，避免下次常规构建误带实验改动。

## 下一轮已推进（15:41 起）

重新核验发现母号自然轮转推送了新子号 `a_f5476f50860abeeba6f6f1c5b8f8b511`，enabled=true，models管理检查200/9项。未恢复已撤下旧号。低频paced实验改为这个新号，以独立临时分组隔离：ABBA/BAAB共8次、每次至少90秒、同account/model/tier/body前缀，只有cache key策略变化；每次前查真实cooldown和enabled，任务单位 `codex-cache90-paced-new-child.service`、PID116190。运行中进度由systemd和实际result.json双核验。

前7次均成功且没有429：前缀0按A/B/B/A为0/0/16768/16768缓存，每次输入16996；前缀1已完成B/A/A为0/16768/16768。A和B都能达到98.66%热轮，先前B单独胜出的短样本没有稳定复现，最后B仍待实际完成。此对照不能证明整体90%，总体包含首轮成本。

同时定位自然流量的配置阻断：业务KEY“测试”只绑定 `g_e96ca29b0ffd`（旧“工作台Pro并发验收”组），该组唯一账号a441已被源正常轮转撤下；新号只在28765的 `g_d90349cb75cb` 和临时实验组，所以业务503。已向用户询问是否让该KEY接续同一母号新子号，收到答复前不扩大KEY分组权限；独立实验不受此问答影响。

候选独立审查修复了4个离线问题：HTTP未知previous部分请求错误派生v2、大配置450MB级临时分配、BuiltinPrompt省略变通、显式回传同key绕过WS配置校验。候选仍隔离于.local，helper11顶层+9子、WS overlay23顶层+15子通过及vet通过，未晋升生产。官方CLI本身HTTP也每次建client，因此当前cli_request不构成偏离CLI的证据；TCP池是否改善缓存只能另做有连接观测的单变量实验。

## 15:52:58 完整paced结果

8次全部完成、零失败/429。A账号共用key四次输入67,984、缓存50,304=73.9939%；B前缀key四次输入67,984、缓存33,536=49.3293%。全部8次输入135,968、缓存83,840=61.6616%，所有成功与首轮都保留分母。两组的非零热请求都为16,768/16,996=98.66%，不能拿该热比例替代整体。该实验没有复现分键优势，候选不晋升，根源码仍与线上a477冻结一致。

systemd 已确认该实验 terminal：ActiveState=inactive、MainPID=0、ExecMainStatus=0；result.status=complete、temporary_access_cleaned=true。结果/摘要为 `paced-new-child-result.json`、`paced-new-child-summary.json`；账号/模型/tier与key模式落库关联为 `paced-correlation.json`。目前业务KEY接续新子号的确认仍待回复，未改其权限。

## 16:00 起连接复用实验

重新读取线上确认运行版本仍a477，业务KEY组没有已启用账号，授权范围未调整。上一轮是实际进展（完成8次低频对照并清理资源），不是成功达标或空闲等待。为验证下一项独立假设，在新子号上启动两个专用helper，主服务配置不变。任务 `codex-cache90-connection-ab.service` 已核验active/PID210271，两个helper PID210300（cli_request）和210383（pooled），固定当前9110 helper及实际runtime identity、相同AT/请求字节/全部应用头。

最多10次，默认30秒间隔（90秒会让reqwest默认idle pool多数过期）、首错即停、无自动重试。凭据仅服务器内存经版本核对导出，不保存AT/RT；原native END必须消费完。响应头时读取各helper的实际TCP inode/endpoint摘要，只有同arm相邻请求在该边界仅一个ESTABLISHED socket且同标识才算观察到复用；不同进程cookie jar并非相同cookie控制，不能把策略组合差异直接归因为TCP。代码 `.local/cache90-20260925/connection-experiment/persistent_ab.py` 经4项localhost合成验证，helper完整路径和真实UA读取由根复核修正后才执行。

这些直连helper实验不进入网关业务usage_events，报告独立，不能拿实验热请求填充整体90%的分母。

## 16:11 完整连接实验与真实阻断

10次全部成功、零失败/限流，A/B各5次输入35,495；包含首请求A=27,648缓存/77.8927%，B=20,736缓存/58.4195%。按预注册各arm首请求单列后，A四次97.3658%，B四次48.6829%。B的第2/4/5/7/10次请求响应头边界是相同唯一ESTABLISHED socket；其中第4和10次零缓存，其他同连接又命中。A5条请求各自不同TCP标识。该小样本足以否定“连接池复用即可消除未命中”，不证明所有负载pooled一定较差；cookie仅记录整个Set-Cookie头hash，不等于已经控制实际出站cookie值。主服务保持cli_request。

终态result complete、systemd inactive/MainPID0/exit0；两个helper正常exit0、未强制kill、socket移除，随后/proc两PID均不存在。证据 `connection-experiment/live-result.json`、`live-connection-evidence.json`、`goal-blocker-current.json`。

16:10:15近30分钟只有8条此前paced隔离实验成功（135,968输入/83,840缓存）；210条自然流量全部失败、输入0。因此显示61.66%的该窗口总率是实验占据分母，**自然整体缓存率为无可计算数据，不是达标**。不能以直连helper的97%热轮或此61.66%代替用户90%整体目标。

同一阻断在连续目标轮均已从权威状态核验：业务KEY仍只授权g_e96ca29b0ffd，该组唯一原号已被28765撤下；当前新号enabled=true但不在该KEY范围。用户“接续同一母号新子号”的问题仍未回复。已完成当前可独立验证的分键/连接实验并清理，不能靠不断制造合成流量完成整体目标。若第三次核验后仍无回复，将目标标记blocked等待此范围选择，不标记complete；待回复后按business-route-proposal.json进行具体版本CAS、当前母号身份核验及真实流量验收。

业务接续已具体化为隔离候选 `business-route-candidate/bridge.patch` 与 `proposal.json`：仅原workspace桥配置additional_group_ids=[g_e96ca29b0ffd]，当前新子号以fresh-version CAS追加该分组；KEY本身不改，其他workspace不继承。34项桥状态/身份/源契约/分组继承回归通过，无root源码或生产修改。用户确认是这项实际可访问账号范围变更的最终步骤，待答复。

本轮已完成实际连接对照和清理，连续目标轮确认同一业务流量阻断仍存在；新16:11证据仍为authorized_enabled_accounts=[]。可独立工作已收口，目标进入blocked等待该选择，保持原90%完整目标，不声称已达到。

## 17:24 后重新检查：旧阻断已变化

目标恢复后按新一轮审计处理。17:24:48仍见业务组无enabled账号、10分钟63条0成功；17:31只读部署计划发现外部已经将新子号加入业务组，账号version8，group授权现在满足。此任务只上传候选到preview目录并执行默认只读plan，**没有apply或修改该账号组**。17:34近10分钟开始出现12条成功，32,063输入/12,416缓存=38.7238%。因此不再沿用旧阻断，也不把该短窗改善归因于本任务代码。

未来子号自动继承业务组的候选仍未上线：构造时校验additional_group_ids并固定required_groups、GET readiness核对全部组。48项回归通过，配置错误/漏组旧代码红测；部署脚本另8项本地模拟覆盖准确目标读回、账户CAS失响应后确认提交、并发配置不覆盖、按版本回滚、停止后旋转与旧计划拒绝。默认只读计划hash随现场变化重新核验，--apply必须匹配当前planhash，且只本workspace独立service override，不替换另一工作区shared源码。用户未来分组继承选择仍待答复，但当前接流已不依赖该选择。

缓存控制能力测试已启动独立4请求（baseline、implicit+30m、explicit breakpoint、invalid mode控制），固定model/account/body前缀，每次30秒。只证明当前ChatGPT backend是否识别公开API的控制项；接受状态不等于实际缓存行为，也不将此小样本算整体90%。任务 `codex-cache90-options-capability.service` 已确认active/PID593874，原生helper专用，不改变主网关配置。

17:39缓存控制能力第一阶段已结束：baseline成功输入7104/cache0；implicit+30m与explicit+breakpoint各返回400 invalid_parameter/param=prompt_cache_options；invalid mode控制返回400 invalid_value/param=prompt_cache_options.mode。专用helper正常回收，原生END完整、socket删除，systemd inactive/MainPID0/exit0。这是当前后端拒绝组合控制的直接证据，不将所有单字段支持情况一概归零。后续正在拆分mode-only/ttl-only/breakpoint-only三项，属于字段能力检查，不作命中率比较。

17:38:38近10分钟没有任何网关请求，授权已修正但自然需求暂时无流量。90%整体目标未完成，当前不能用直连能力测试替代自然统计。接续部署候选的回滚和状态判定已经准备齐全，未应用。

17:41:49独立字段检查终态：mode-only、ttl-only均400 `prompt_cache_options is not supported on this model`；breakpoint-only 400 `prompt_cache_breakpoint is not supported on this model`，原生END完整。该结论仅覆盖当前账号/模型gpt-5.6-sol路径，不能推到所有模型。专用helper exit0、socket清理、unit inactive/MainPID0。完整结果 `options-fields-result.json`；第一阶段结果 `options-capability-result.json`，实际执行脚本快照已保留，不能将只清理无效表达式后的本地脚本称为原发送脚本。

目前没有证据支持将分键、pooled或强制cache controls推广。17:42起启动一次15分钟只读自然流量窗口 `codex-cache90-natural-window.service` / PID623950；采样最多每30秒，100成功且1M输入仅表示可评审样本门槛，**不自动宣称90%完成**。目标仍active，当前是已验证活进程等待；后续必须复查同unit/PID/result，不因一次工具timeout重启。程序不发模型、不改配置，始终保留自然窗口全量token分母；cache90实验单列，不替代业务目标。

## 17:48 新阻断审计（与旧权限问题不同）

当前业务KEY已获准使用两个enabled推送账号，health ready，旧“无可用账号”阻断不成立。但连续目标轮17:43/17:44/17:46/17:48均无新自然请求；17:48:13独立SQL明确最近15分钟0请求/0输入，不能计算缓存率。已上线修复、候选审查、分键与连接对照、显式缓存控制能力检查均已形成证据；不能再用合成请求替代真实整体目标。因此将目标标记blocked等待自然业务流量恢复，90%未完成。

只读观察unit仍是同一个MainPID623950/active，11次采样全0，实际运行约312秒。它仍按原定15分钟截止自行结束，**没有因采样超时重启或声称进程已停止**；结果留在 `natural-window-01/result.json`。恢复目标时先读该unit与result实际终态，再查询新业务数据，不新建重复采样任务。最终审计 `natural-window-final-audit.json` 记录当前授权、SQL流量和健康事实。
