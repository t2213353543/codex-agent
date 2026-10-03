# GET /models 与 helper 生命周期对照：独立审核

2026-09-25，实验 v6。审核者只读源码、冻结证据与合成测试，不发起模型请求、不读取 SSH 凭据、不创建或重启部署任务。既有部署等待 unit 的状态和是否激活由主任务观察；这里不改变其执行。

本轮对照两个账号 q0/g7，各账号三个模式各重复 2 次，最多 12 次模型 POST，最多 8 次预热 `/models` GET。原题沿用用户 21 判据；expected=21 只用于报告，不发给模型。使用新的官方 CLI `0.155.0` 原始完整首轮捕获，模型 `gpt-5.6-sol / medium`，保留捕获 POST 字节、session/cache 和非认证 headers。

初始讨论账号为 q0/g2；正式模型预登记之前，新库存发现 g2/g4 的 usage GET 为401、allowed=null，而 q0/g7 为200、allowed。主任务据此前置观测将 g2 替换为 g7，不刷新401账号、不把它们计入本轮模型错答，v5既有结果不变。最终计划须保存这条选择依据；这不是看到本轮模型结果后换样本。

## 测量边界

| mode | 本格进程与请求序列 | 必须核对 |
|---|---|---|
| `cold` | fresh helper P → POST | 1 个 helper、0 次预热 GET、最多 1 次模型 POST |
| `same_helper` | fresh helper P → 完整 GET/models → 同 P 的 POST | 1 个 helper、1 次 GET、最多 1 次 POST；两请求各用独立 IPC socket，同 helper 身份与存活范围可追溯 |
| `separate_helper` | fresh helper P1 → 完整 GET/models → P1 清理 → fresh P2 → POST | 2 个不同 helper、1 次 GET、最多 1 次 POST；P1 正常回收后才准许 P2；不导出、复制或注入 Cookie |

GET 路径固定为 `/backend-api/codex/models?client_version=0.155.0`。正式模型预登记前主任务选择严格前置条件：GET完整返回200、完整models schema通过`catalog_issues`且目标`gpt-5.6-sol`唯一存在，才进入POST；空目录、缺目标、重复目标、schema不完整全部停止本格，不视为等价自然CLI。GET失败、截断、重复响应头、IPC error或清理失败均不得发送模型请求。GET是目录读取，单列计数，不计作模型调用。quota/usage前后读取使用另外的helper，不得预热本格P/P1/P2，也不得共享它们的cookie jar。

源码表明 `with_chatgpt_cloudflare_cookie_store()` 使用进程范围的共享 jar，允许少量 Cloudflare 基础设施 cookie 和 `__oailb`；`cli_request` 每请求重新构建 HTTP client，仍可使用同一进程 jar。因此 same/separate 对照确实能够改变进程内 cookie 的连续性，但**不是只改变 cookie 的干预**：先行 GET 可能改变服务端状态，同一进程还可能保留其他状态，POST 的真实客户端构造和捕获时刻也与自然 CLI 不同。

若 same_helper 独有改善，可以报告“同 helper 先 GET 与结果相关”，不能直接宣称 cookie 导致；若未改善，也不能证明 cookie 从未生效。仅有 Set-Cookie 不能证明 POST 真的发送了该 cookie；域名、Path、Secure、expiry 和 jar allowlist 都会影响应用。只有真实出口 Cookie 观测才能直接说明已发送，这轮如无该观测须写 `not_observed`，不从入站 Set-Cookie 推断。

## 预登记顺序与混杂

采用已登记六格后全逆序：

| 格次 | 账号 | mode |
|---:|---|---|
| 01 | q0 | cold |
| 02 | g7 | same_helper |
| 03 | q0 | separate_helper |
| 04 | g7 | cold |
| 05 | q0 | same_helper |
| 06 | g7 | separate_helper |
| 07 | g7 | separate_helper |
| 08 | q0 | same_helper |
| 09 | g7 | cold |
| 10 | q0 | separate_helper |
| 11 | g7 | same_helper |
| 12 | q0 | cold |

每个账号×模式的位置均值 6.5，中心两次 g7/separate_helper 相邻，必须披露非独立性。真实开始时间和 GET/POST耗时仍需记录，因为相同格次平衡不等于真实时间平衡。账号来自既有对照，不是新随机样本；不能将本轮与 v5 12 格、自然 CLI 4 格或更早结果合并成随机总体率。

自然 CLI 与重放不只差 cookie：每次 CLI 的 POST 含动态 session、turn、item 与 metadata、目录读取和实际执行时刻，而本轮 POST固定新捕获值。v5 自然 CLI input 6893 对固定重放 6881 已表明完整输入不同。这一额外轴不能被 same_helper 模式自动消除。

## GET header 与内容约束

GET 使用固定版本真实模型目录路径的应用 headers，不把 POST的 Lite、routing hint、turn-state、session/body flags 或 `Content-Type` 复制进去。认证/workspace始终取本格同一已核验身份。UA/originator/version 若从捕获保留，不得换成自行编造的更像 CLI 的值；传入 application identity 时只允许核对一致，不能覆盖。

固定源码 `model-provider/src/models_endpoint.rs` 调用 `ModelsClient.list_models(..., HeaderMap::new())`；`codex-api/src/endpoint/models.rs` 只追加 client_version 并 GET 空 body；`login/src/auth/default_client.rs::default_headers()` 设置 originator、UA 和必要 residency，没有声明 `Accept: application/json`。随后真实新捕获 `capture/raw-models-head.json` 已确认 GET 的 query 为 `client_version=0.155.0`、Accept 为 `*/*`，并保留实际 UA/originator/version，无 residency；因此使用捕获的 GET rows，不套用 usage GET 的 application/json。

模型目录只作为固定的先行读取，不得用返回结果改写随后 POST 的 model、instructions、reasoning 或 tools；POST必须仍与登记捕获完全一致。返回模型目录 hash、条目数和目标模型存在性可另作观测，但不能将它们改成输入生成器。

## 请求、身份与停止约束

- 每格 helper 都只服务本格账号；token/workspace/user/identity 四个 epoch hashes 在预登记、GET与POST、前后 quota 读取一致。相同 workspace 不替代 user 身份；fallback只按官方 `chatgpt_user_id` 缺失/null时取 `user_id`，不取 `sub`。
- 程序在发送前严格核 helper 完整 SHA、运行 `/proc/PID/exe` hash、捕获 body/head hash、工具/runner冻结 hash。任何身份/hash/序列边界失败全批停止，不当成可继续的模型失败。
- 每格最多一个 POST，无 refresh/retry/fallback，无跨账号补发；GET重试同样禁止。准备失败或 GET失败时实际POST=0，应保存该事实，而不是凭12个计划格声称发送12次。
- 一个绝对 deadline覆盖 helper启动、GET、清理/切换、POST读取和最终清理；独立预留 after usage/epoch读取。same_helper若GET耗尽预算不得重置POST计时器。
- 任何持久化失败设fatal后停止；同格 GET失败或普通模型错误可按预登记继续后格，具体策略必须在首个模型请求前落盘。所有正常/异常/取消出口都回收已启动进程与 socket。
- Set-Cookie只记录允许 cookie 名、其他名称 hash、值 hash、作用域属性 hash及布尔/数值属性；禁止保存 raw value、token、request Cookie。不能自动把Set-Cookie带到separate helper。

## 报告与判断

逐格保存 mode、alias、顺序、实际 helper数量和PID/运行哈希关联、GET和POST分开计数、实际方法/路径、完整POST body/head hashes、分阶段时间与deadline、GET完整性和目录摘要、cookie安全摘要、HTTP状态、terminal/final/usage、前后身份/quota、清理结果。

只对完整模型答案评分；GET失败、HTTP429、超时、缺终态或无final不计错答。已观察到completed但cleanup失效时保留 `observed_completion`，主协议评分不得冒充有效。原始自动评分器固定，人工判据另列并绑定final哈希。reasoning/cached计数保留字段缺失与零的区别；不推断隐藏推理、内部capacity或账户标签。

本轮模型路径是 native helper 直连官方，部署中的 Go Lite补丁不在推理链上。任何cookie/lifecycle对照结果均不是Go网关质量恢复验收。

## 审核进度

方案和固定源码边界已审阅。执行工具与 v6 runner 仍在生成，尚无最终通过结论；待冻结后进行独立故障注入，覆盖 GET失败零POST、同P与不同P正确绑定、切换清理失败阻断、epoch/hash停止、deadline剩余预算和敏感输出。审核者实际模型请求为0。

工具首版已完成 6 项独立故障注入（`review/check_lifecycle_faults.py`，合成 process/socket，实际执行原生命周期、IPC写入与响应解析函数）：

- 三模式严格 GET/POST 次数、同 helper/不同 helper 关系、完整原始 POST字节和同一认证/workspace得到确认；无 request Cookie 手工注入。
- GET401、截断未END、models类型错误、重复JSON成员、重复HEAD和native ERROR均在POST前停止。
- separate模式GET helper清理失败不启动POST helper；same模式GET后进程死亡不替换进程继续。
- GET消耗剩余deadline后POST不重置预算；所有已启动synthetic helper/socket都收尾。
- Set-Cookie的值、域名、Path及错误私有内容不会进入输出，允许基础设施cookie只保留名称与hash/受限属性。

严格目标目录gate加入后，独立fixture升级为完整合成ModelInfo，并新增空目录/缺目标/重复目标/残缺schema在POST前停止的回归。GET真实捕获后的 header边界补测确认UA/originator/version/residency不可替换，Cookie/Lite/turn-state/Content-Type不可混入目录请求，调用方header对象不被修改；目前独立 **8 项全部通过**。它们不是TLS cookie实际命中测试，runner和最终工具hash尚未冻结。目录字段以hash留存供比较，不要求本轮新目录与v5相同，也不据此重写固定POST。

外层runner已实际执行5项AST隔离故障测试，不导入driver或读取真实KEY。发现并修复一组停止规则遗漏：`capture_contract_or_hash_mismatch`、`models_application_identity_mismatch`、`models_headers_contain_non_catalog_fields`、`helper_compression_unavailable`原先只算单格失败继续，修正后均设置fatal、由after/next epoch gate停止全批；普通GET/模型endpoint失败仍按预登记继续。另验证账号token/workspace与mode无串格、POST同字节、usage走独立adapter、epoch两次API共享绝对deadline、任何report持久化失败阻断后续。5项全部通过，红绿日志分别为`runner-faults-initial.log`/`runner-faults-green.log`。

独立自有故障测试合计 **13项通过**；作者完整工具测试本机复跑为21项通过、2个POSIX real-helper测试在Windows明确跳过。真实HTTPS合成cookie fixture已只读审核：只在测试注入临时CA和loopback CONNECT，三个mode都用同一合成AT；same_helper的POST应带允许且Path匹配的`__oailb/__cf_bm`，cold/separate应无cookie，session cookie及Path不匹配cookie不应发送，且`cli_request`应产生5次独立TLS连接。实际Linux执行仍以主任务冻结构建日志为准。

主任务已采集真实Linux证据：完整工具包运行146项，145通过、1项因镜像缺openssl跳过；启动门禁单独核对相同工具/test hashes的真实TLS套件23项全部通过、0跳过，再逐个核对远端文件哈希。不会把146项称为全部已执行通过。初次TLSfixture因CA/leaf设置失败的日志保留，修正仅在合成fixture，正式probe不注入CA或代理。

最终冻结工具SHA为`22c78f9ed0293633a8da74e89cb22ff95d539c6e5d3a1ffc152d7e1b4745be31`，测试SHA为`962c5177cef92da155c40e9b39e8941003f3cf1e3675b2f6d1ebc49365d99474`，已审runner SHA为`96e07070e6ab0376af3ca23b8e96a66f922e3d3e0398bcc3c6c35669a3df85a2`。独立检查与故障注入无剩余阻断项。

启动前真实只读preflight确认新raw body/hash、GET `*/*`/query与POST应用身份相同、q0/g7当前epoch匹配。实际记录的Go运行binary为`a4777428…`，helper仍为固定`9110b869…`；Go不在模型路径，部署观察不能混成实验干预。批次已由主任务显式启动，本审核者未调用启动工具。

逐格结果审核脚本位于`review/review_results.py`，前缀可先核下载完成的格；`--require-complete`用于最终全批。完整12格都发送时应得到12个POST、8个模型目录GET、16个helper（cold4+same4+separate8），并核samehelper同scope/PID及不同IPC、separate前P正常清理与新scope、跨账号不共享scope、实际POST原字节一致、GET目标schema及unique model、前后epoch/usage、finalusage/评分。pending格不把未下载观测计成零实际请求。

## 执行前缀的独立观测

前3格证据已交叉核验、无不一致：01-c-q0为HTTP200/completed、答案29；02-s-g7为GET200→同helper POST200/completed、答案29；03-d-q0为GET200→已清理旧helper→新helper POST429、无答案不评分。此时2个目录目标模型摘要相同，不代表完整批次结论。02的GET记录3个allowlisted Set-Cookie，POST未返回Set-Cookie；仍未直接观测POST发送Cookie，不可推断其实际发送或效果。

03-d-q0的安全错误摘要为32字节、body SHA`96c534cc320264e5091e907e67dc8667bfadc8b0cd6172031bfa91c7c5d8f82d`，唯一顶层字符串字段名称SHA`9c0211c5…`。独立离线对公开固定常量 `{"detail":"Rate limit exceeded"}` 重算UTF-8精确32字节得到同一body hash，`detail`名称hash也精确吻合。作为另附的`review/observed-error-reconstruction.json`，保留依据`known_constant_exact_bytes_hash_match`，未修改任何原report或声称原始body被保留。v5的两份q0 HTTP429也有相同body hash，可同样对应这一固定错误文本；其Retry-After数值仍未观测，不从hash猜秒数。

03本轮Retry-After由实际新工具明确解析为33秒。可以说官方HTTP响应明确描述“Rate limit exceeded”，不能进一步说是哪种私有限流、容量、账号标签或低算力机制；GET200和usage allowed也不能排除模型POST的请求级限流。以上为前缀观测，不用三格替代完成的12格结果。

## 最终停止后的复核

实际执行7次POST、5次目录GET、10个helper后，在08-s-q0的前置身份/启用检查停止：`status=stopped / epoch_observation_failed`。后续只读观察q0为disabled、version21，但四身份hash均不变；这不是凭据轮换，不恢复账号、不绕过停用，不补发08–12。5个完整模型final均为29，2次q0 POST429无答案不计错；04/07的自动null由独立人工hash注解保留29，原评分器未改。

`review_results.py --require-complete`对已执行前缀、停止和清理证据全部核验通过，`findings=[]`，同时明确`full_predeclared_design_completed=false / passed_full_experiment=false`。服务器execution及7份report原字节hash与本地一致，3个unit终态退出，私有raw/credential文件已清理。随后READ ONLY数据库聚合还发现q0实验窗有生产流量40/12/7条重叠完成请求、g7窗为0；窗口可能重复，不能求和为独立请求总数，更不能据此唯一归因cookie或服务端限流机制。

完整结果、请求不变量、限流固定常量证据、并发统计口径和停止范围见[最终v6报告](C:/Users/Administrator/Desktop/项目/codex-agent/docs/CODEX-QUALITY-COOKIE-CONTROL-2026-09-25.md)。审核者始终零真实模型请求、零部署启动。

