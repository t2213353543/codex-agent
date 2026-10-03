# 当前网关修复与账号质量对照

更新至2026-09-27。本目标仍进行中：已修复可复现的网关错误，并在官方 CLI、原生 HTTP、完整 WSS 会话与当前网关路径中复现账号差异；尚未取得账号被标记或被路由到低算力集群的直接证据。

## 工作区最新候选（尚未部署）

当前408文件候选优化路由提示字段读取：完整验证JSON后，只读取model/service_tier，避免为两个字段复制整份input及加密历史；请求正文、路由头和原语义保持。两种近32MiB样本的该步骤耗时中位数分别下降约50%／36%，每次累计分配由约33.6MB降到约1KB；不是端到端延迟或RSS数据。

Windows全套3173通过、382明确跳过、vet通过；作者差分fuzz243281次、独立fuzz51845次均无差异。408文件已与工作区全量SHA一致。另按固定CLI0.155.0复核TLS/代理/Cookie及WS状态、压缩重试，明确源码同源与实际运行的区别，未发现本轮需要额外修改传输代码的新缺陷。没有远端、模型、真实OAuth或部署；Linux/race/实际Rust/PG合并运行验证仍缺，质量根因仍未确定。[请求环境复核与优化](CODEX-REQUEST-ENVIRONMENT-REVIEW-2026-09-27.md)

## 前一轮迟到凭据写入候选（未部署，已包含在最新工作区）

前一轮407文件源码补齐重新推送后的迟到写入：管理检查按凭据代次、再按检查时间原子保存，旧401不再挤掉新凭据已成功的usage；错误写回按稳定ID和凭据版本核对，避免重新授权、改名、旧名称复用时写错状态；重复相同错误不再反复落盘。损坏null快照不再panic。同凭据同秒仍按完成顺序保存，不伪造无法观测的先后。

本机全套3142通过、382明确跳过、vet通过；407源文件与工作区全量一致。真实本地管理HTTP及刷新锁释放窗口复现红绿，两项独立审核通过。TLS/SOCKS与WS取消关闭检查未发现新生产缺陷，只增加回归。Windows及Linux交叉编译完成，但Linux运行/race/实际Rust/PG合并验证仍缺；没有远端、模型、真实OAuth或部署，质量根因仍未确定。[迟到写入补充修复](CODEX-REAUTH-LATE-WRITE-CANDIDATE-2026-09-27.md)

## 前一轮背压与截断候选（未部署，已包含在最新工作区）

前一轮403文件源码已修复慢客户端背压与错误截断：WS下游Write阻塞期间暂停上游闲置时钟，只排除每个活动队首实际受阻时间，独立写/总截止仍有效；HTTP每次Read上游前清除旧写期限，避免思考间隔导致HTTP/2重置或HTTP/1结束chunk失败，真正Write仍受原60秒限制；错误正文仍最多转发2MiB，额外1字节检测超限，遇读错/超限/短写异常关闭HTTP而不伪造cleanEOF。

本机全套3096通过、382明确跳过、vet通过，403源文件与工作区全量一致。HTTP/1与本地TLS HTTP/2缩时红绿、WS大帧真实TCP阻塞和错误体真实截断均有独立复核。Windows与Linux交叉编译完成，但Linux运行/race/真实native合并验证仍缺；本轮未进行任何远端操作、模型/OAuth调用或部署，未重试先前平台拦截。最新产物不是线上版本，原题质量未恢复。[背压与截断修复](CODEX-STREAM-BACKPRESSURE-CANDIDATE-2026-09-27.md)

## 前一轮请求生命周期候选（未部署，已包含在最新工作区）

在上一轮397文件续接候选之上，当前400文件源码新增HTTP401恢复取消优先及WS终态交付结果记录：取消失败出口关闭原401并返回context错误，合法已执行token轮换仍先保存；上游已完成的槽位及时释放，只有最终写给客户端的结果确定后才持久记录，写失败不再误计ok但保留真实usage。原native请求Body关闭实现本轮检查正确，仅补回归，不改其生产文件。

最终本机全套3044通过、382明确跳过、vet通过，源码与全工作区400文件逐SHA一致。首次全套失败与vet问题均保留，仅在独审后调整两份测试的精确记录等待/CancelFunc清理；原断言未减少。Windows编译和Linux交叉编译已完成，但本轮没有任何远端构建、部署、模型/OAuth调用，未重试之前的平台拦截。Linux运行/race/native合并验证仍缺；线上最后确认版本为下述6ecdf93f，并非本轮产物。[请求生命周期修复与边界](CODEX-REQUEST-LIFECYCLE-CANDIDATE-2026-09-26.md)

## 前一轮续接候选（未部署，已包含在最新工作区）

本轮已修复并同步7个生产文件、4个测试文件：JSON聚合不再被大小写扩展污染事件/item身份；WS已知续接链固定网关改写策略摘要，配置变化需完整input；已知活动response ID准确关联named/default lane，避免槽位不释放或错误中止其它lane；HTTP Connection提名字段不再污染内部冷却与记录。冻结397文件合并源Windows完整2994通过、382跳过、vet通过，独立组件及合并审查通过。

远端Linux合并构建的启动被平台拒绝，未换入口重试。只读核对没有对应容器和构建目录，当前线上仍为下述`6ecdf93f…`。本地Linux交叉编译产物不能代替运行与native/PG/Redis全量验证；本候选未部署，原题质量亦未恢复。[候选及完整边界](CODEX-CONTINUATION-PARITY-CANDIDATE-2026-09-26.md)

## 已修复并验证的行为

当前已上线 Go `6ecdf93f…`，helper为 `bf920a0b…`。压缩精确字段读取改为完整原字节验证后仅定位所需成员，避免重复复制大段密文；近32MiB本机样本累计分配字节降低35%、次数降低80%，不将其冒称峰值RSS或线上延迟。Linux race3411、Windows2942、13个实际native场景及独立fuzz通过，线上HTTP200/WS101。[分配优化](CODEX-COMPACTION-ALLOCATION-OPTIMIZATION-2026-09-26.md)

此前`58580fcf…`保真修复仍包含：兼容压缩接口按精确字段读取type/input/role/encrypted_content，不再因未知大小写扩展误删加密历史或拒绝合法摘要。Linux race3298、Windows2829及新增native13项通过，线上HTTP200/WS101、临时scope清理通过。[压缩字段保真修复](CODEX-COMPACTION-FIELD-PARITY-FIX-2026-09-26.md)

此前 `4190b7ad…` 修复仍包含：新增修复将实际调用者取消归于client/aborted，保留真实上游解析错误及截断；半关闭连接以中断响应结束，避免空200。最终Linux race3265项、Windows2809项与独立真实TCP12个叶子通过，线上HTTP200/WS101、临时scope清理通过。[HTTP取消处理修复](CODEX-HTTP-CANCELLATION-ATTRIBUTION-FIX-2026-09-26.md)

此前 `54615b6e…` 修复仍包含：HTTP现在按固定CLI的序列规则延后failed/incomplete及坏完成事件，后续合法completed可成功结束；EOF保留最后typed错误，WS即时错误语义保持。真实官方CLI19组参照、Linux race3229项和新增native46项通过，线上models200/WS101，临时scope已清理。兼容源缺字段、JSON partial及done/error扩展仍明确单列。[HTTP终态顺序修复](CODEX-HTTP-TERMINAL-SEQUENCE-FIX-2026-09-26.md)

此前 `e069de52…` 修复仍包含：原始响应ID不再因展示截断/脱敏而合并，矛盾终态不再误记成功或用量；原始SSE和数据库schema保持。[计量身份修复](CODEX-STREAM-RESPONSE-IDENTITY-FIX-2026-09-26.md)

此前 `5687aa1c…` 版修复仍包含：关闭/取消HTTP响应后不继续返回残留数据，WS显式总预算覆盖TLS/代理等待及消息阶段，默认长连接不增加总时限。TLS/代理43项Rust及28项候选实测通过，另修复systemd helper切换的环境文件优先级及指令引用错误。[生命周期与TLS完整记录](CODEX-NATIVE-LIFECYCLE-FIX-2026-09-26.md)

源端现为 `5d931132…`，登录设备ID及session cookie读取按实际请求路径、精确名称匹配，避免选择不会出现在该请求Cookie头中的值。本地真实TLS复现与116项相关测试通过，未改设备值或执行真实重授权。[Cookie一致性修复](CODEX-AUTH-COOKIE-PATH-FIX-2026-09-26.md)

重新授权后仍显示旧401的问题已修复上线。新凭据状态按实际发送时的凭据版本关联；旧 HTTP 请求或旧握手 WebSocket 的迟到响应保留为历史，不覆盖新凭据状态。详情、筛选和排序一致。已在受影响 new1 上完成26项只读验收：AT和凭据版本2不变，旧状态解除，历史401仍保留。[修复与线上证据](CODEX-REAUTH-STATUS-FIX-2026-09-25.md)

OAuth grant 永久拒绝不再仅冷却4秒后重复提交同一凭据；实现按固定官方 CLI 的当前进程、当前凭据快照记忆，凭据变化后可重试。普通模型或额度401不会被直接当作 RT 永久失效。[源码和复现](quality-20260925/auth-revocation-handling-review.md)

28765 OAuth 设备绑定修复已上线：OAuth DID 与当前 AT 独立绑定保存，保留旧浏览器 session/device/cookie；修复并发等待后补推、再次保存的来源混淆。真实 PostgreSQL 5 项和完整调用链验证通过，服务健康、调度配置恢复。该 DID 不经现有导出链传入网关，不能称模型质量已恢复。[修复和发布证据](CODEX-OAUTH-DEVICE-BINDING-2026-09-25.md)

28765 子号 profile 连续性修复随后上线：后续请求优先使用该子号授权画像，避免被母号 UA/TLS recipe 覆盖；保存独立 AT 绑定且保留浏览器凭据组。非法画像不继续协议请求，身份核验通过的新 RT 先保存；相同邮箱、不同 user_id 的刷新返回被拒绝。最终镜像 66 项回归通过、11 项旧 PG 测试跳过，另 7 项真实 PG 验证通过，两账号的本地实际请求构造一致。随后 v21 新凭据后测未观察到 new1 恢复，详见下表。[最终部署与边界](CODEX-OAUTH-PROFILE-CONTINUITY-2026-09-25.md)

实验工具另修复了独立 IPC 启动器依赖加载、两种明确答案格式漏计，以及 WSS 合成预热响应缺少必填 `usage.total_tokens`。这些是实验工具修复，不混作模型质量恢复；此前自动评分与原始报告均保留，人工判读另以原文哈希绑定。

## 当前两个账号的真实模型对照

所有批次使用未修改的原题，SHA `cd771ba38f8f75696ba703c7d3e91e5273588c91cb8c4145e3ce4b077c075c3f`。题目约定允许通过触感选择形状时，21=9圆形+12五角星有穷举验证；若另加“不许选择形状”的约束，29对应另一问题。模型输出中的29按既定原题标准记错，不调整答案键。

| 批次 | 干预与边界 | new2 | new1 |
|---|---|---|---|
| v12 | 同一捕获 HTTP 请求；原生直连与当前网关源码实例，各2次 | 两路径各21、21 | 两路径各29、29 |
| v15 | 派生完整 HTTP 与真实 WSS 预热→真实响应 ID→增量推理，各2次 | 两路径各21、21 | 两路径各29、29 |
| v18 | 未修改官方 CLI，内置 provider，各2次；安全事件没有直接证明最终传输类型 | 21、21 | 29、29 |
| v20 | 仅修改 `reasoning.effort`，medium／xhigh | medium一次21；xhigh两次21；最后medium未发送 | medium一次29、一次过载无答案；xhigh两次29 |
| v21 | 环境修复部署后，旧 AT 基线与重新授权后的观察性后测 | 旧、新各21、21 | 旧、新各29、29 |

v22 增加同 workspace 的不同用户对照：new1 两次29，其同 workspace 的 owner 两次21；另一 workspace 的子号及 owner 四次均21。四个用户使用相同本批正文/非认证头、medium；模型目录52字段相同、使用资格均允许。结果不支持该 workspace 内所有用户都会错答，但 owner `team` 与 child `business-prolite` 的角色及权益仍混杂，不能据此识别隐藏风险标签或角色因果。[372项复核与八格结果](quality-20260926/same-workspace-v22-results-review.md)

v23 的4次实际成员GET进一步确认：new1/new2均为 `standard-user / prolite`，两个owner均为 `account-owner / default`；状态和停用字段缺失，不推断active。四个AT的issuer、audience、client_id与scp摘要相同。已观察角色/席位标签不足以解释new1与new2不同答案，仍不是内部路由证明。[164项成员证据复核](quality-20260926/membership-v23-results.md)

v24按公开mode文档做显式standard/pro诊断，仅发2次即按预登记停止：new2 standard答21，new1首个pro请求HTTP400且没有答案，后6格未发。公开Responses文档不保证OAuth Codex端点接受相同设置，本次没有得到pro质量比较，也未改变默认配置。[117项终审](quality-20260926/reasoning-mode-v24-results-review.md)

v25 对照当前授权出口：真实观察源端走WARP IPv6、模型直连走IPv4，当前代理配置与v21相同。使用固定CLI实际捕获正文/非认证头，medium，两个账号×直连/授权代理×各2次，共8次完成。new1两路径各29、29，new2两路径各21、21；3份明确最终答案单独人工核对并保留原自动null。未观察到仅切换该出口即可恢复new1。账号、画像、凭据和运行版本前后相同，网关持久日志8个窗口重叠为0，4份原始捕获已删除。两次执行器预检失败均在native模型发送前停止；最终使用真实捕获UA，不推导拼接。这里没有证明历史授权IP、每次模型的公网IP或隐藏路由标签。[出口对照记录](quality-20260926/route-control-v25.md)

v26补当前账号语义控制，预登记12格但第7次出现HTTP200流内server_is_overloaded后依约停止，实际6个完整答案、1个无答案、5格未发。new2原题21、澄清21、盲抽29；new1原题29、澄清23、盲抽29。23方案18圆＋5星确实保证成功，但错误声称所有22颗分配均失败，独立穷举证明9圆＋12星＝21更优；因此该次错误不能仅归为忽略选形状。只有单次各控制，未完成镜像重复，不估稳定率。16个批次usage、48份serverhash及清理证据通过221项独审。[语义诊断v26](quality-20260926/semantics-v26-results.md)

v21 两账号均实际更新 AT，源端与网关当前凭据一致，new1 的新 RT/DID/profile 返回、保存和推送链路有完整持久回执。new2 过程回执丢失，仅能由后续状态与事件确认新凭据及绑定，因此原受控 after 保持未完成，后四次另标观察性。new1 本轮没有恢复；没有由此证明隐藏风控、路由集群或错答根因。[完整结果及限制](CODEX-QUALITY-AFTER-ENVIRONMENT-REAUTH-2026-09-26.md)

v20 因 new2 在第8格发送前发生外部令牌更新而停止，实际7次，不能称完成八格镜像。new1 两次 xhigh 均回显 xhigh、报告1552 reasoning tokens且答29；同账号 medium 完整回答报告796 tokens。此前同账号还有371—900等用量，故不支持“恒定516 token硬上限”。回显和用量也不等同于服务端实际预算测量。

两次无答案流内错误的 code 摘要精确匹配固定官方源码中的 `server_is_overloaded`；HTTP状态仍为200。后续对照本网关既有错误类型常量，type 摘要精确匹配 `service_unavailable_error`；错误消息未恢复，不据此推断具体集群、容量规则或风险标记。[v20完整与中止证据](quality-20260925/reasoning-effort-v20-results-review.md)

v12实际网关改写范围包括内置 instructions、缓存键、会话元数据和五项会话头；其余正文及非会话元数据保持一致，设备安装、窗口和请求字段已逐项核对。本轮各账号两路径结果一致；这不证明网关历史上从未影响账号状态。[v12](quality-20260925/gateway-direct-v12-results-review.md)

v15实际8次推理加4次预热均完成，四个预热 input=5919、output=reasoning=0，真实响应 ID 关联验证通过，未观察到 turn-state。HTTP输入是明确标注的派生完整请求，不冒称官方原始HTTP字节。[v15](quality-20260925/http-ws-chain-v15-results-review.md) · [官方 CLI v18](quality-20260925/official-cli-v18-results-review.md)

## 环境及能力证据

固定官方 CLI `660e159a…` 与原生 helper `9110b869…` 已在断网容器采集10个实际 ClientHello，并在真实 CentOS、服务 UID61787、独立仅回环网络中采集6个。HTTP协商字段和扩展载荷一致；WSS扩展顺序在两个实现各自重复连接中都会变化，其余稳定字段、扩展集合和非 key-share 载荷一致。保留标准 JA3 原顺序，没有将排序结果冒称 JA3。这仅证明首个 ClientHello 的已观察范围，不证明真实上游证书、恢复会话及完整TLS/HTTP协议等价。[宿主验证](../.local/research-20260925/tls-host-hello-v17/README.md)

v19 两账号各一次原生 models GET 与 usage GET 均200；9个模型目录、目标模型52个字段相同。issuer、audience、client_id、scp、plan声明相同；这些是未验签的本地 JWT 声明，未知字段和时间差异未赋风险含义。目录均声明支持medium和xhigh。[账号能力对照](current-account-capabilities-v19.md)

28765 当前保存的两份设备配置不同，且均标注 synthetic_protocol。保存的 `profile.device_id` 与 OAuth设备UUID具有不同语义，不能因字符串不同就判串用。当前保存配置不等于历史注册或授权时的真实网络抓包；具体请求构造一致性另行核验，不把配置相关性当成质量根因。

下一步继续区分账号相关的输出差异与具体服务端原因；不再把重新授权成功或配置一致当作质量恢复。新令牌代次单独登记，不补发已中止实验、不把新旧代次混作同一对照。最新逐批索引保存在 [latest-status.json](../.local/research-20260925/latest-status.json)。

源端授权日志脱敏修复另已上线，保留 SSE 状态信息，移除上述选择与回调路径的上游 body、Location、session 与异常正文；该修复没有更改模型请求或账号凭据。[发布记录](CODEX-AUTH-LOGGING-FIX-2026-09-26.md)

重新授权状态另以当前已发布ff13前端完成6个Chromium合成场景复核，55份静态文件和220份源码与发布清单相符。unverified显示“尚未验证”，本地替换凭据及自动刷新能更新旧错误；选择明示“手动刷新”时，外部推送后旧页面可保留先前状态，点击刷新才重验。未发现新前端缺陷，未更改轮询策略；用户当时的具体标签页状态仍未知。[前端核验](../.local/research-20260926/front-status-review/review.md)

公开线索另已核官方支持说明及原作者材料：账号级可用性限制确有官方说明，但不能识别本项目账号的内部状态；turn-state“292/312”是字符串形状而非HTTP状态。本地19份历史HTTP报告均为200且有该头哈希，但未保留长度，不能追溯。新的v27被动观测工具已通过合成TLS及执行器审查，但在线启动被平台拒绝，实际模型POST为0，已核worker未启动并删除捕获；未绕过、未注入状态。[未启动记录](quality-20260926/turn-state-v27-unstarted.md) · [官方时间背景](quality-20260926/public-status-v26-context.md) · [公开线索](quality-20260926/public-account-state-research.md)
