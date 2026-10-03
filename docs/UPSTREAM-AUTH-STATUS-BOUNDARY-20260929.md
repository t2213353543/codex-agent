# 上游账号认证失败与客户端密钥认证的公开边界

本轮 .23 已将最终、尚未交付的上游 HTTP401 表示为公开 HTTP503，避免下游把上游账号 OAuth 失效误认成网关 API key 失效。2026-09-29 07:26（北京时间）已切到release `37450c1b…`、PID3337873、binary `bad6026f…`；07:27:54的原线上验收回执为通过。认证修复工作区是 `codex-agent-auth-release-20260929`，与已取消的旧实例预留方案目录不同。

截至09:00，生产已升到继承上述行为的 .24（release `b3adc5ac…`、PID3588453），尚未部署下文的新流内认证候选。原 .23 发布数据作为历史证据保留。

| 事实 | 公开结果 | 内部保留 |
| --- | --- | --- |
| 客户端网关 key 无效 | 原 HTTP401，不派发上游 | 原客户端认证失败分类 |
| 已认证请求最终遇到上游 HTTP401，响应尚未交付且来源可信 | HTTP503，`server_error/upstream_authentication_unavailable` | 实际尝试的401、上游错误来源与诊断、适用时最终上游错误码 |
| 其它账号接替成功 | 原成功响应 | 所有实际尝试，失败401与成功200分别保留 |
| 本地权限拒绝、内容策略403、参数400 | 原状态及对应错误 | 不按401故障泛化处理 |
| 已交付输出或协议事件后失败 | 保持原流协议和终态 | 不回放请求、不改已发HTTP头、不清已确认用量 |
| 模型列表 GET 最终上游401 | 同样区分为上游依赖503 | 实际catalog HTTP401与账号身份 |

候选的逻辑请求 `HTTPCode` 随最终公开状态变为503；真实401保留在 `Attempts.StatusCode`、已实际观测到时的 `Attempts.ObservedHTTPStatusCode`，以及相应上游错误字段。不能概括成“内部全部仍是401”。旧 AppServer 回退记录未观测到HTTP响应时，`ObservedHTTPStatusCode` 仍为空，不能为了对齐新测试补造401。失败没有可信终态用量时，usage、缓存和TTFT继续未知，不能填零或成功。

投影需要已认证key、已选择账号、明确上游来源及HTTP401，并排除已交付、CAS提交冲突、取消和超时等边界。最终耗尽返回前一次失败时，公开映射必须使用对应的原失败对象，不能把当前“候选耗尽”或历史安全错误码单独当成映射依据。公开新错误清理原认证challenge和已不适用的body长度/编码头；内部保留诊断。

生成、compact和WebSocket虚拟POST使用最终failover写出边界；模型列表GET不经过该writer，须在真实上游响应写出时处理。A用准确 .22 和旧writer-only候选各一个localhost GET确认过catalog仍外泄401，作者随后补了旁路。旧观察是未覆盖该入口的证据，不能用于否定后续修订。附件上传认证失败也需保留真实 `/attachments` 尝试，不能伪装成已生成响应。

测试已覆盖公开JSON/SSE/compact、opaque不重放、接替成功、无效客户端key、403策略、已输出流、models、附件、WebSocket和投影守卫。扩大回归发现的旧401预期与AppServer未知传输状态断言已分别保留原失败。修订正式冻结 `49b74734…` 的 Go 5000 项通过、vet通过且源码未漂移；B已独立核对该源22个PG顶层、35个含子项均实际通过，新增真实producer→持久记录→新Metrics读取→统计/CSV/JSONL证明公开503与实际上游401分别保留、未知用量不补零。完整参数race与12组真实模型门禁也已按最终同源回执通过，旧轮结果不替代新轮。

发布时另做了一次实际SSE跨代观察：旧.22先输出真实文本，随后切到.23；切换确认后继续收到88段文本，4.117秒后唯一completed终态和随机marker均正确，旧应用与入口PID/binary在观察期间不变。reader未暂停、未用barrier制造长流。这证明该客户端SSE跨本次切流完成，不扩大为所有连接或WebSocket都实际验证。探针本身只计1次客户端调用，后续按exact标记读取的独立ledger进一步确认唯一逻辑记录、1次真实上游请求、0刷新和不截断；内部read0/create37079与公开read37079/write0分别保持。之后两次新实例生成和一个本地top_p负例、三次CSV对账通过，配置与计费默认值保持。旧.22共享实例后续退休属于独立排空操作，不能把观察期PID不变写成永久保留。

下游判断也有独立范围。本地保存的Sub2API源码表明，默认OpenAI API key HTTP401会进入持久认证错误并停止调度；默认503避开这条401分支，但可触发暂时的账号/模型退避，管理员自定义错误规则和结构化账号禁用错误另有处理。HTTP200流内认证终态又有独立分类，候选保持已公开流原样时，不应宣称所有上游认证失败都不会再触发下游停调。该本地源码没有单独证明与生产二进制同一版本。

下游账号146849已在 .22 上用原saved key完成一次标记生成后，经管理API清错并恢复调度，受保护配置摘要不变。这次恢复证明当时可用，不证明401映射候选已上线或上游OAuth不会再次失效。

.24运行期间，该下游账号在08:38再次处于停调状态；08:52的原saved key模型列表查询仍成功，08:54标记生成成功后，主会话再次恢复该账号，受保护配置保持。停调早于08:46:49启动的固定历史索引回填，不能归因于后来的回填；现有请求开始时间与安全错误摘要还不能唯一定位触发停调的请求。

作者在 .24 基底的本机上游复现了两个额外边界：HTTP200流内的 typed 认证失败，尚未公开时原writer保留200或缓冲JSON的502；账号在执行中被删除时，结构化完成校验失败的公开措辞含 `authentication recovery`。新的私有候选分别扩大可信认证终态的503投影，以及为后者使用 `response_access_changed` 和固定公开措辞。客户端无效key仍401；已公开的SSE或已提交身份的WS继续原协议，不回放。该候选正在局部回归与正式验收准备阶段，不能把它写成线上已修，也不能据本机复现唯一归因本次停调。

进一步的实际消费者验证发现，WS错误事件中的 `status:503` 并不能覆盖本地Sub2分类器对 `authentication` 文案的401判定。A追加的私有修正只在WS最终503正文精确等于网关固定依赖错误时，改用 `upstream_unavailable` / `upstream temporarily unavailable`；HTTP原错误内容与已created的WS路径保持。真实本机gateway完成了相同测试旧版失败、新版通过的对照，内部逻辑401、实际上游HTTP200和已确认用量不变。真实候选finish函数的输出经过同一Sub2分类函数，结果为通用502而非认证401；这是离线消费者兼容证据，没有执行Sub2账号副作用，也不等于生产已发布。原初版503→401反例保留。

证据入口：

- [公开边界与catalog对照](../.local/collab-a-20260928/upstream-auth-public-boundary-review/source-contract.md)
- [下游分类条件](../.local/collab-a-20260928/upstream-auth-public-boundary-review/downstream-classifier.md)
- [作者V2局部日志核对](../.local/collab-a-20260928/upstream-auth-public-boundary-review/owner-v2-log-review.json)
- [指定下游账号恢复回执审查](../.local/collab-a-20260928/upstream-auth-public-boundary-review/target-recovery-review.json)
- [WS公开依赖错误候选与真实gateway验证](../.local/collab-a-20260928/ws-auth-projection-candidate/HANDOFF.md)
- [候选实际输出的下游分类验证](../.local/collab-a-20260928/ws-auth-projection-consumer/REPORT.md)
