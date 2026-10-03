# B 组会话绑定503调查与观测候选

本轮目标是区分尚存bound503的真实原因，保持原账号授权与重放约束。当前线上为0.4.14；本文件不代表候选已合入或部署。

## 最终上线状态：0.4.16已部署

2026-09-29 01:49 +08：BoundSelection已随releasee145f1ba…正式上线，PID1911991/binary70b42e59…，17:48:01UTC现场回执全真；正式Go/vet、PG16、race及完整9live通过。B独立核对包与部署回执，见[0.4.16发布记录](COLLAB-B-V0416-RELEASE-REVIEW-20260929.md)。以下未合入/未部署措辞均保留对应历史时点；实际六字段含义见[合同](BOUND-SELECTION-OBSERVATION-20260929.md)。上线增加可观测性，不代表旧六条具体原因已查清或所有绑定错误已消失。

## 17:30 UTC：已进入主工作树，正式验收待统一执行

17:35 UTC补充：monitor实际合入复核已完成，bound白名单与完整snapshot AST同本组已本地验证候选，仅额外top_p白名单变化。完整当前monitor的server/SQL验收仍未在本组回执中出现。owner正式PG采用`TestPGBPSBoundAndAttachmentObservationRoundTrip`，覆盖绑定与附件账本，B单bound准备test不再要求重复合入。新bound5、附件route1、typed top_p1均匹配当前race选择；实际运行结果须等待最终冻结门禁。

线上现为0.4.15，主会话17:26开始下一轮集成。B在17:29:35读取窗口核对7个bound运行文件，canonical与封存重基候选字节完全相同、前后无漂移；据此确认已合入主树，**尚未声称新版本发布**。

已补真实WebSocket dispatcher前后对照：两侧3主项4leaf全部通过、4业务结果相同。opaque/previous拒绝仍503/零派发/原owner不变，普通warmup无tail字段省略，明文替代在真实commit后才写recovered。私有源和当前集成源一致。

专用`TestPGBPSBoundSelectionObservationRoundTrip`已提供新增文件patch，仅本地编译成功并明确无DB而SKIP，未执行PG。监控白名单错误投影候选36项pure/mock通过，独立AST审查确认cursor、分类、缓存分母与persist不变；新SQL仍待owner真实监控验收。入口为`bound-selection-rebase-0415/HANDOFF.md`，不要把这些准备状态当正式PG/线上通过。

此前各节的线上版本/未合入措辞是对应读取时点，当前状态以本节与后续主会话实际回执为准。

## 17:07 UTC：最终0.4.15基线重基已交

为后续版本准备，候选已移植到正式0.4.15修订PG165500.639410的710项冻结源；原始0.4.14候选保留不变。新目录`bound-selection-rebase-0415/`的runtime.patch SHA为`e172ddf7584254d50e448d79f6447dc4f20eb5ecc11c87f9bd5d685c1b57ca9b`，保留本版ErrorParam、mixed、early transport/WS warmup、summary和compaction修复。五项新增测试已改为TestBPSBoundSelection前缀，可被正式race选择。

新候选定向20顶层/76含子项全部PASS。独立组对这次新基线与新候选各单轮运行原不变9个等价场景，所有wire/调用/记录逐值相同，7个runtime及全源前后无漂移；新回执在`bound-selection-independent/rebase-0415/result.json`。没有将旧0.4.14比较冒充新基线验证。候选仍未合入main/未部署，也未运行PG或真实provider，当前0.4.15发布不会插入该变更。

## 旧六条的直接证据

旧0.4.13进程窗口15:39:15–16:06:42 UTC、id≤44726、精确503尾分支文案，有界只读投影实际返回6条。查询readonly=on、20秒timeout，输出仅计量/状态/存在性与一次性盐关联；没有读取请求正文、凭据或原始session/key，没有扫描Redis。

六条都在同一匿名key组，原请求stream=true、http_sse、schema2、gateway/account_selection503；未保存selected account/sent model/attempts/truncation，也无已知用量。与已部署0.4.13实际attempt写入合同交叉核对后，可以确定当前六条记录没有记录到generation/OAuth发送；不是网络抓包意义的零活动，也不表示没有历史/数据库/Redis读取。用量保持unknown。

同一key不等于同一session或同一个人。同一秒但不同ID的两条记录保留为两条。没有已知release-validation marker也不能证明完全排除了所有合成来源。

固定错误文案定位原绑定无法匹配当前授权候选集的尾分支；外层可能保留priorRecord作为最后公开错误，所以不是必然最后一次选号分支。已记录attempt账本在priorRecord恢复后重新附加，不会因为选择旧record而消失。

旧记录没有session/bound owner、replay判据、候选计数或同次快照来源。禁用、删除重建、授权组变化、owner整体或旧快照差异无法继续区分。不能据现在账号状态猜过去，也不能把这些错误归为0.4.13 post-refresh三行修已经覆盖的问题。

证据目录：`.local/cross-session-20260928/01a0e7ed/bound-owner-next-evidence/`。

- `projection-20260928T162332.449809Z.json`：成功的只读投影；初次SQL括号问题失败回执162244保留。
- `projection-analysis.json`：六条逐值复算、四个关键源码与旧部署一致。
- `independent-review.json`：独立11项检查通过，明确priorRecord/history和unknown边界。
- `metadata-design.md`：六字段与采样时点设计。

## 隔离候选范围

候选基线由正式0.4.14 PG160442.016102的697项冻结源码构成，独立逐SHA确认一致。全部代码只在B `bound-selection-candidate/` 中，普通运行源码由主会话持有。

候选拟加可选`bound_selection`对象，仅在精确尾分支发生后存在。最多记录快照中的绑定状态、候选数、快照来源/年龄、原输入重放类别和恢复结果；全部为固定枚举/计数，不保存account/key/session标识、请求正文或内容哈希。

快照描述必须与选号使用同一份accounts及同次key允许组，锁内记录来源和装载时间；不额外查询Store/Redis。只在尾分支扫描一次原快照分类，正常请求不增加目录扫描。原输入重放类型与已有迁移判断共用解析，在history展开前捕获，不改变允许条件。

候选归属是本逻辑请求第一次精确尾分支的诊断。成功换号后可与成功record并存，但只有真正commit及最后交付成功才标恢复；priorRecord恢复、取消和交付失败保留各自结果，不复制原错误或账号信息污染新的成功记录。各记录使用独立标量副本。

16:40 UTC验证完成，状态READY_PRIVATE：作者定向10顶层/50含子项PASS；独立before/after各2顶层9leaf全PASS，9条wire、HTTP、真实loopback调用数、最终account/error/attempt双HTTP/usage/ToolPolicy以及快照选择逐值相同。最终7运行文件SHA无漂移，原比较时的gateway_bps小改已在final-result.json及最终重验中闭合，旧drift记录保留。没有量测syscall级Store读取次数，未新增读取依据为源diff与cache/stale反例，不夸大为I/O抓踪。

可审补丁 `bound-selection-candidate/runtime.patch` SHA `e64c981d8a877c6d30d8095d547e41cb193e5a40b0f68549f1b3d187f1002a83`；测试独立`tests.patch`。作者回执candidate-result.json，独立最终回执`bound-selection-independent/final-result.json`。新的TestBoundObservation名称不会自动被当前race的TestBPS正则选中，正式合入时应改为TestBPSBoundSelection前缀或显式纳入selector，不能用私有PASS替代正式门禁。

不新增CSV列、DDL或正式PG/真实模型请求。主会话0.4.15正在修改gateway/stats相关逻辑，后续只交最小hunk和base指纹，不能把候选整文件覆盖进去。
