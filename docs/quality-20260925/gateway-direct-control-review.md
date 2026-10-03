# v7：生产设置的隔离网关与直连官方对照审核

2026-09-25。审核者仅审阅设计、源码、冻结证据与独立合成测试，不执行真实模型调用、不修改生产路由/账号启用状态、不启动部署。

## 本轮要回答的问题

两个事先选定且当前enabled、usage允许、近期生产流量低的账号，各执行direct与gateway各2次，共最多8个POST，使用同一份新的官方CLI `0.155.0` 完整首轮raw捕获，原题不改、`gpt-5.6-sol / medium`，按用户21判据离线评分。

direct为fresh native helper `9110b869e65db6bf7664e39fcfe3b2ec17fa7a0207ddbeccf28d73dbc12ccce8`直接官方HTTPS，cold、无预先GET。gateway为fresh单账号隔离fixture，运行已核对的实际生产源码链和捕获时的生产settings，仅关闭自动refresh与401recovery以保证单dispatch，随后也由fresh同版本helper向官方发送。

正确表述是 **“生产设置的隔离网关代码链 ↔ direct official 对照”**，不是实际生产监听端口质量测试。隔离fixture没有生产多账号调度、真实客户并发、Redis共享sticky状态与长寿命helper环境。即使它通过，也不能声称生产端口所有场景都恢复。

账号必须在预登记前固定；不沿用已disabled的历史q0。执行中账号删除/停用、credential epoch变化、settings/source/helper/capture hash不一致时全批停止，不恢复账号、不换其他账号补格。普通模型HTTP失败可按预登记继续，但不算错答。

实际事前选择已完成：new2/g7，两账号稳定user/workspace均不同，selection时in-flight均0、最近10分钟已持久化网关请求均0；usage primary分别0%/70%、fresh usage allowed。排序依据为in-flight、近期流量、已用quota、alias，在模型结果出现前执行，证据`selection.json`声明数据库READ ONLY。历史disabled q0不参与；低网关日志不排除外部或尚未持久化流量。

## 变量与顺序

推荐四条件前半加完整镜像：A-direct、B-gateway、A-gateway、B-direct，再完全逆序B-direct、A-gateway、B-gateway、A-direct。最终以root预登记为准，每个账号×路径两次的格次均值4.5；中心相邻B-direct重复并非独立样本。每格开始前不能根据已有答案改序或重试。

固定项是CLI原题body/head、model/effort、账号内凭据epoch、helper binary/lifecycle、原始session/cache值及时间预算。两条路径的实际出口请求**允许因生产settings而不同**，包括prompt policy、兼容化、account cache键、应用headers和压缩选择；这些差异正是本轮要测的代码链行为，必须逐字段保留安全摘要，而不能为了“相同请求”偷偷关闭额外生产开关。

direct必须发送冻结raw body的确切字节；gateway必须保存真实IPC writer已经写出的head/body摘要，并标明此处仍在Rust压缩/TLS之前。`actual IPC sent`不是TLS线上完整抓包，但强于只对准备中的对象重新序列化计算哈希。不得使用旧的synthetic capture peer冒充真实发送。

## fixture边界

- 从空tempStore导入本格真实AT/workspace/exp，用现有生产导入解析取得用户身份；禁止借用带fixture-user的`newTestProxy`。导入后稳定user/workspace/identity hash必须与计划匹配，只有一个账号，不能fallback到其他账号。
- 使用固定普通客户API key及固定key ID，admin独立且不对外暴露；key ID属于cache/sticky命名空间的一部分，需跨重复保持一致。账户ID/owner revision如果每个tempStore随机生成，也会影响account cache键；必须记录并解释，不能误称所有gateway重复的cache key固定。
- 建议同一alias使用限定在实验目录内的稳定account state_dir，保留account ID/owner revision与固定key ID，后格复用前严格核同epoch；其余handler、session绑定、metrics、helper保持每格fresh。不同alias不能共用state_dir。这样控制gateway重复的cache命名空间，同时不能把它冒称生产记录ID或生产运行时状态。最终实现以fixture及实际IPC cache摘要核验为准。
- settings与生产snapshot逐字段一致，仅允许`AutomaticTokenRefreshEnabled=false`、`Upstream401RecoveryEnabled=false`及本地Version CAS管理差异。不能顺手关闭BuiltinPrompt、PromptCache、Fingerprint、Normalization等而仍声称生产设置。
- 仅允许`POST /v1/responses`且最多一次；GET与其他path拒绝，认证失败不消费有效模型格。不得启动/访问真实生产推理端口，loopback监听必须固定127.0.0.1，响应readiness地址和nonce/hash要检查。
- 部署、数据库、账号enable不变；隔离fixture只能写自己的临时目录。stdout/stderr/报告不得出现真实token、email、rawCookie或完整题外敏感context；模型final单独脱敏保留。
- 收尾覆盖成功、非200、超时、客户端断开、writer失败、body中途取消、helper启动失败。绝对deadline包含启动、发送、响应和清理；只收到完整terminal后才可认为模型完成。IPC部分发送不允许补发。

## IPC观测必须证明的事实

每格网关报告至少记录：

1. 入口raw body/head hash与客户key hash；出口method/path、actual writer body hash/字节数、header名称/值hash、Authorization token hash和workspace header hash。
2. 生产settings hash、源码清单hash、Go test binary hash、helper文件及运行`/proc/PID/exe` hash、已应用的runtime identity manifest及应用身份摘要。
3. 单账号的真实user/workspace/identity hash、record ID/owner revision摘要、会话绑定前后摘要及仅一个account选择；如果hash随fixture重建发生变化，应在报告明确。
4. native writer实际写入完成、最多一次POST、零GET/refresh/recovery、流响应字节hash或terminal证据与下游collector结果一致。
5. 进程/池关闭、临时socket与credential文件清理、所有child已回收，无第二次自动请求。

instrumentation只观察实际写入，不修改body/head、重排原始request，异常时不可泄露token或继续发送无证据的模型请求。header摘要需按真实出站canonical/Connection规则，不能拿未经转发过滤的入口值冒充出口。

## 并发、账号状态与归因范围

即使fixture单账号，所用真实账号仍可能同时被生产流量或其他客户端使用。预登记选择低近期流量，并为每格保存前后只读DB时间窗口/marker；查询按请求start+latency重叠统计，不能把相邻窗口计数求和成独立请求数，不能把“日志0”当作不存在外部流量。

v6已证明q0模型请求期间有线上并发且中途disabled。本轮不复用该账号，不绕过当前enabled条件；事前低流量也不保证过程中一直低，必须保留后验统计和状态变化。

若gateway错而direct对且至少同账号镜像重复一致，最多支持该隔离代码链/设置组合相关，仍需结合实际出口字段差异再定位；不能单凭空instructions、Cookie、cache key或reasoning数直接宣布根因。若两路都错，也只说明这个对照没有展示网关单独决定结果。无最终模型答案的429/超时等不算错答。

## 评分与证据

冻结原始评分器；完整模型答案按21验收。人工判读只另写字段并绑定final SHA，不覆盖自动null或原报告。保留response.completed/status、实际HTTP status、usage完整嵌套字段、finalhash、错误摘要、Retry-After和cookie安全摘要（若实际观测），并区分未报告字段与0。

8格上限是预登记上限，不保证发生8次POST；GET不得计模型，部分dispatch要单独标记。若有前置停止，结果报告明确实际前缀、未执行格、停止时账号状态，不补齐镜像，不合并v5/v6算随机正确率。

## 审核进度

方案已审阅。新`gateway_quality_account_pair_test.go`与`tools/quality_gateway_pair_probe.py`仍在实现，尚未给出最终审核通过。待冻结后运行独立故障注入，优先检查错误身份/多账号/重复POST/错误settings/runtime pin、实际IPC观测失败和清理分支。审核者真实模型请求为0。

Python collector初版独立真实loopback五项中四项通过、一项发现未知SSE error code/type可能原文输出opaque标识；负责代理修为v6固定allowlist+hash后五项全部通过。其余验证包括exact raw body/header仅一次本地POST、不假称native出口抓包、不跟随307重定向、header trickle在绝对deadline退出、外部/非精确loopback地址与capture hash错误零连接。测试仅合成loopback，无真实凭据或模型，红绿证据在v7/review/gateway-probe-faults-*.log。

collector后续cleanup失败沿v6语义保存observed_completion，主answer和eligible清空，避免已收到答案被抹去或误计有效评分。真实出口仍须join独立pinned gateway fixture/IPChook收据，单collector成功不是最终arm有效证明。

Go fixture冻结版通过3项独立overlay测试：12个并发普通客户请求只有1次真实合成upstream dispatch、其余11次409，inflight最终归零；不同源settings只改变登记的两个语义开关与本地Version；不支持路由与歧义认证不消费有效POST槽。fixture使用真实Store.Import身份，不含fixture-user；同alias仅accounts.json持久化，account ID/ownerrevision与cache命名空间稳定，其余settings/key对象、metrics、session、helper每格fresh。Windows初次state路径检查因短路径规范化误拒绝，owner改为逐级Lstat拒symlink后正式回归通过。

runner审核已修复：完整/api/settings管理附加字段不能直接喂入严格Settings struct，改按部署源码Settings JSON字段白名单取真实策略；readiness核identity、客户key、test binary、helper、launcher及CLI source hashes；server-report的source/effective settings逐字段比对（prompt仅hash与字节数），只容许两flag和Version差异；direct返回helper/hash/cleanup错误设fatal；credential写入后即使install_launcher失败也由外层finally删除。上述两类runner故障先红，修正后独立AST三项通过。

完整gateway callback的合成进程/文件/IPC graph正向测试也通过，确认report join与清理可以走通；额外注入第二GET或重复/外来Authorization摘要时发现证据gate原本只“筛出一POST”和token membership检查。root已收紧为全部native requests恰好一POST、Authorization token hash列表恰好一个本格值；两项红测修后通过，正向graph仍通过。不能用body hash正确掩盖额外请求或身份摘要冲突。

最终独立自有测试 **14项通过**（Python真实loopback 5、Go实际转发overlay 3、runner完整/故障graph 6），无剩余阻断项。工具owner另有13项本机Python通过，主任务报告冻结Linux工具177项通过、0跳过；Go fixture 7项通过、live入口明确skip（不能在离线测试触发真实模型）。正式执行仍需通过主任务的最终hash/preflight gate，本审核不替代真实账号状态检查。

已审核冻结SHA：

```text
gateway_quality_account_pair_test.go
05b3c1d8ff2f38050b440cdb12103a3b980069262a199c4485442e31e7b54f08
quality_gateway_pair_probe.py
df8748c841d2432e8356552ae2452e598e59a52285691fe696f54dc9a64308e2
test_quality_gateway_pair_probe.py
99aefcb315aa8066a105f0896a8e21829772c16542d436e07e5509bace8a2c2e
v7/run_remote.py
45a5e30caf544a444bbb58e20d10f9a121195cc599958c6db8497840fb0e8dc1
```

## 执行前缀核验

主任务完成真实零模型fixture preflight：new2身份、pinned Go test binary、helper、全部生产Settings原值及仅两开关+Version差异均验证，随后按已冻结8格镜像显式启动。审核者只读收集结果。

前6格独立`review/review_results.py`核验`findings=[]`：new2的direct首格与两次gateway均为21，公开final明确按形状取9圆+12星；g7的gateway首格和两次direct均为29。前缀不是全8格结论，剩余格继续原登记，不按结果改序。

实际IPC字段证据：gateway相对于原capture只新增`instructions`并映射`prompt_cache_key`；232字节内置prompt的独立JSON string hash与actualinstructions hash精确一致。原input每个item content hash、client_metadata、model/reasoning等未变。actual `all_headers`去认证/workspace后13个header值hash全部相同，包括thread-id、window-id、beta、routing hint和Lite；不能把摘要plain子集未展开误称丢头。direct保留zstd，gateway本地identity ingress导致其实际IPC压缩缺省none，已在计划声明。

new2两次gateway的持久account ID/owner revision摘要、cache映射值、全部出站body field摘要均相同；g7使用另一独立账号namespace。这些是实际writer观测，不是预期字段推算；仍未声称TLS wire字节或真实生产端口等价。

## 全8格最终复核

镜像8格完整完成，全HTTP200/response.completed，new2 direct2+gateway2均21，g7 direct2+gateway2均29。冻结评分器8格均直接解析成功；独立人工读取final并单列hash判据，未改原报告。最终审核`passed=true / findings=[] / full_predeclared_design_completed=true`。

32份服务器公开证据文件与本地原字节hash全部匹配，unit正常退出、account state/临时credential/raw文件清理，所有native helper回收、无inflight。全8窗生产已持久化请求重叠数为0，但外部/未完成流量未观测。new2当前来源找到且同AT、保存synthetic macOS profile；g7当前此来源404，均不能当历史真实wire环境证明。

完整逐格usage、实际canonical字段差分、13头一致、压缩差异、源码/设置/namespace限定与结论见[最终v7报告](C:/Users/Administrator/Desktop/项目/codex-agent/docs/CODEX-QUALITY-GATEWAY-DIRECT-2026-09-25.md)。本批仅支持当前两账号在两条路径下结果保持各自一致，不定位服务端cluster、账号标签或整个生产端口质量。
