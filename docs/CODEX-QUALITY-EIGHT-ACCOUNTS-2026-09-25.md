# 八账号固定请求对照与官方 CLI 复测（2026-09-25）

**2026-09-25 14:36 后续对照：** 新增三账号 HTTP／WS 共 12 格已完成：HTTP 四格完成、q0 两格 429；WS 六格完成。十个完整答案人工均为 29，未观察到切换 WS 恢复 21。两次 429 单列失败，不计错答。[原始 CLI 请求与传输对照证据](CODEX-QUALITY-HTTP-WS-2026-09-25.md)。下文保留前序批次与部署记录。

**2026-09-25 后续上线更新：** 当前后端 `bb92216f…aca277ad` 已完成生产验收。导入识别官方 `user_id` fallback；刷新按当前 AT/ID 重建身份、保存旋转令牌并阻断旧身份会话；WS 连续链保持缓存键；HTTP 改键后超限在绑定前拒绝。最终 Linux 946、Windows 822 顶级通过，零失败，另有完整官方 CLI 7 项通过。八账号目录 28 对相同、官方外部令牌本地装载均成功；原 24 次答题结果保持，质量恢复仍未证明。[完整修复与验收](CODEX-GATEWAY-IDENTITY-REFRESH-2026-09-25.md)。下文旧版本号及测试数字按各自阶段保留。

本轮八个独立用户的 16 次固定请求全部完成。按冻结自动评分，结果为 **3 次正确、10 次错误、3 次无法自动判分**；人工复核后三次均明确答 29，因此人工统计为 **3 次答 21、13 次答 29**。没有协议失败或没有最终答案的格。随后两组官方 CLI 直连 ABBA 共八格也全部 clean 完成，人工均明确答 29。Q0 在同一轮从 21 变为 29，g7 在固定请求两次答 21 后，四次官方 CLI 复测均答 29；这些观测不支持把账号永久分成“正常”与“异常”两类。

本轮使用原题，预先登记答案键 21，没有向模型注入答案或更改题意。账号别名 q0、g1–g7 仅对应本轮固定 identity/token epoch，**不应与旧实验的 q1–q7 按数字直接对应**。

| 批次 | 次数 | 自动正确 | 自动错误 | 自动无法判分 | 人工正确 / 错误 |
|---|---:|---:|---:|---:|---|
| 八账号固定 native IPC 请求 | 16 | 3 | 10 | 3 | 3 / 13 |
| g2/g7 官方 CLI ABBA | 4 | 0 | 3 | 1 | 0 / 4 |
| q0/g7 官方 CLI ABBA | 4 | 0 | 2 | 2 | 0 / 4 |
| 合计 | **24** | **3** | **15** | **6** | **3 / 21** |

这里 24 次为 16 次固定 native IPC dispatch 加 8 次官方 CLI invocation；CLI 的准确 wire POST 数未独立抓取。所有批次均正常完成，没有把失败请求替换或重试为新的成功格。

| 账号 | 固定请求首次：答案 / reasoning tokens | 固定请求反向复测：答案 / reasoning tokens | 自动评分说明 |
|---|---|---|---|
| q0 | 21 / 1034 | 29 / 499 | 同账号本轮出现两种答案 |
| g1 | 29 / 516 | 29 / 516 | 两次自动判错 |
| g2 | 29 / 478 | 29〔人工〕 / 516 | 第二次自动无法判分 |
| g3 | 29 / 516 | 29〔人工〕 / 452 | 第二次自动无法判分 |
| g4 | 29 / 1001 | 29 / 516 | 两次自动判错 |
| g5 | 29〔人工〕 / 369 | 29 / 922 | 第一次自动无法判分 |
| g6 | 29 / 908 | 29 / 412 | 两次自动判错 |
| g7 | 21 / 2218 | 21 / 1723 | 两次自动判对 |

〔人工〕只增加解释，不改原始 report、冻结 scorer 或其输出。三个原始 `no_unambiguous_final_answer` 分别为 `06-g5`、`13-g3`、`14-g2`；最终句依次明确写“最少取出 29 个”“最少取出 29 个糖果”“最少需要取出 29 个糖果”。这属于评分器未识别的句式，不能写成模型无答案。

16 次登记顺序为 `q0,g1,g2,g3,g4,g5,g6,g7,g7,g6,g5,g4,g3,g2,g1,q0`，北京时间约 12:00–12:08。八个账号的 user、identity、token 哈希分别全部不同；workspace 共六个，g2/g5/g7 共享一个 workspace。identity 由 workspace 与 user 组合派生，token 表示本轮凭据 epoch；workspace 本身不作为用户去重条件。

请求由 SHA 固定的完整官方 Linux CLI 0.155.0 生成，通过只捕获 fixture 的既有网关 writer，取得压缩前 native IPC body 与非鉴权头。捕获阶段记录一个 POST、零模型推理；它返回 synthetic failure，未计入质量结果。后续 16 格各使用全新固定 Rust helper 直连官方接口，Go 网关不在推理路径上。body SHA256 为 `0783217c49c6a928ead6f838c9fe7f9ccd2f5565824cabe891d507c6b21f675b`，每格实际完整 IPC body 均匹配；非鉴权头的名称、大小写、顺序和值摘要全部相同，只替换 Authorization 与 ChatGPT-Account-Id。固定的 session/cache/item ID 是本实验条件，不能据此推断后端计算独立或所有自然 CLI 请求等价。

每格在调用前、usage GET 后、调用后核验四维 epoch，实际 POST Authorization/token 与 workspace header 也匹配。调用前后各有一次真实 usage GET，helper 均退出 0、已回收且 socket 删除；16 个 response ID 摘要全部不同，终态均为 HTTP 200、`response.completed`、`status=completed`。采集未得到 TLS 完整 wire bytes，因此“请求一致”限定在已观测 native IPC 边界。usage GET 的应用身份来自生产 runtime identity，POST 来自此次 CLI capture，两者的 UA/originator 并不相同；两类请求的账号身份已分别核对。

16 次请求和可见服务端响应都为 `gpt-5.6-sol`、`reasoning.effort=medium`；响应 reasoning 还统一记录 `mode=standard`、`context=all_turns`、`summary=null`。reasoning tokens 范围 **369–2218**，其中恰为 516 的为 5/16，不能据此描述为普遍固定 512 预算。错答也出现 908、922、1001 tokens；相同 token 数不能证明相同隐藏推理。

完整 reasoning item 证据保留了字段哈希。跨响应没有观察到相同的非空 reasoning content/summary/encrypted_content 序列；content/summary 的空数组哈希相同不构成推理相同的证据。encrypted_content 比较只比较密文；密文不同也不能证明其隐藏明文一定不同。终态 output 可以为空，最终文本来自已完成的 `output_item.done`，已按终态与文本来源进行核验。

调用前常规 usage 均允许，前后 observed usage 未显示身份矛盾或目标模型明确拒绝。但目标模型在 usage 的 `model_usage` 中为 `not_observed`，不能写成服务端显式授予 sol 权限；多数百分比读数相同也不能证明请求未扣费或没有异步计量。该 GET 不是模型执行瞬间的私有服务端状态。

第一组官方 CLI 补测按 `g2,g7,g7,g2` 登记，四格均 `reference_clean=true`、退出 0、正常 `turn.completed`，无工具事件、启动告警、重连、限流、认证错误或模型重路由诊断。CLI/hash/config/题面/请求 model 与 effort 相同，采用 fresh home、直接官方 HTTPS、自定义 HTTP provider 禁止重试；没有 Go 推理转发。每次 CLI 会自然生成新的会话和安装元数据，因此不能把它与固定 body 实验当作逐字节相同请求。

| 官方 CLI 补测 | 最终答案 | reasoning tokens | 冻结自动评分 |
|---|---|---|---|
| 01-g2 | 29 | 516 | 错误 |
| 02-g7 | 29〔人工〕 | 888 | 无法自动判分 |
| 03-g7 | 29 | 516 | 错误 |
| 04-g2 | 29 | 516 | 错误 |

这组 `02-g7` 最终明确给出 `boxed{29}`，其后还有“个糖果才能保证满足条件”，自动 scorer 未识别为终止答案块。自动统计保持 3 错误、1 无法判分；人工为 4 次答 29。g2/g7 在第一轮对照中被选中后才进行该补测，属于有明确选择依据的后续复测，不是新的随机账号样本。

第二组官方 CLI 对照按 `q0,g7,g7,q0` 登记，北京时间约 12:17–12:18，四格均 clean 完成，前后身份和 usage 复核通过，无工具或错误事件。自动统计为 2 错误、2 无法判分；人工逐字检查四格均明确答 29。

| q0/g7 官方 CLI 对照 | 最终答案 | reasoning tokens | 冻结自动评分 |
|---|---|---|---|
| 01-q0 | 29〔人工〕 | 723 | 无法自动判分 |
| 02-g7 | 29〔人工〕 | 472 | 无法自动判分 |
| 03-g7 | 29 | 414 | 错误 |
| 04-q0 | 29 | 516 | 错误 |

`01-q0` 开头明确写“最少取出 29 个”，结尾再次保证 29 个；`02-g7` 末句明确给出 `28+1=boxed{29}`。这两格仍保留冻结自动评分的无法判分标签。两组直接 CLI 复测合计自动为 5 错误、3 无法判分，人工为 8 次答 29；不能把原 16 格的自动统计混写成合并后的结果。Q0 后续也答 29，说明最初那次 21 不能作为同时间窗口内永远正确的对照。

时间、会话元数据、缓存命中及随机采样变化没有在这些批次间独立控制。当前结果不能区分这些解释，不能据此声称账号刚被本轮实验“标记”、发生了永久切换或进入某个特定后台集群。

官方 CLI stdout 提供的是请求 model/effort、正常完成状态和 usage，不记录实际 POST auth/header/body、上游 response ID 或返回的 model/effort。其身份链由已审 runner 的 AT 文件输入、前后 epoch 和 usage 支持，不能声称获得与 native IPC 一样的发送边界证据。`tool_observed=false` 表示未见工具调用事件，`tool_surface_zero_verified=false` 仍保留，不能扩大成请求中完全没有工具定义。以上边界已写入独立复核报告。

来源环境只读调查没有补足新增七账号的历史注册/登录记录：g1–g7 按 JWT 邮箱查询当前 28765 注册库均为 HTTP 404，当前 Team child/owner 元数据也没有匹配。404 只说明当前来源未找到记录，不证明这些账号从未在该来源注册。网关 `created_at` 是导入时间，JWT `iat` 是 token 签发时间，都不能替代原始注册时间。

| 环境层次 | 已有证据 | 仍未知 |
|---|---|---|
| 历史注册与浏览器 profile | q0 可按 user/token 匹配；保存 `synthetic_protocol`、macOS/chrome150 模板 | 新增七账号没有来源匹配；保存模板也不是当时真实 OS、TLS、设备 cookie 的抓包 |
| OAuth 登录与出口 | q0 有重授权成功元数据；当前源码和当前路由关系已记录 | 各账号签发当时的实际设备、TLS、代理/地理出口及其影响未知；当前代码晚于 q0 token 签发 |
| 本次推理与服务端 | 同一主机、固定原生 helper/CLI、隔离环境及本轮身份/请求/响应证据 | 私有集群分配、账号历史状态、风控标记及上述环境是否导致答案差异均未知 |

q0 保存的两个 device ID 字段不同，但当前代码优先按主机作用域取 `oai-did` cookie；这个存储差异不能证明某次请求同时发出冲突 ID。不能把 owner profile 当 child 的真实登录环境，也不能把当前修复后的代码行为倒推到历史签发时刻。本轮不足以把质量差异归因于注册、登录或 TLS 环境，更没有证明“低算力集群”机制。

冻结工具在 Linux `--network none` 下完成 123 项测试、零失败。新增离线校验器对最终 16 格执行 **1000 项通过、零失败**；六项复制证据的故障注入验证能检出 token、body、非鉴权头、helper cleanup、重复 response ID，并分开失败与无答案分类。两组官方 CLI 补测分别独立校验 **216 项通过、零失败**。临时凭据与原始捕获体/头已删除；固定请求任务退出 0、进程 inactive；两组 CLI receipt 也记录凭据清理完成。冻结工具和原始实验记录未因评分或复核改写。

证据均在 `../.local/research-20260925/live-account-controls-v3/`：`plan.json`、`execution.json`、各格 `report.json` 与前后 usage/identity、`review-results.json`、`review-followup.json`、`review-q0-control.json`、`source-environment-summary.json`、`source-runtime-code-evidence.json`。机器汇总 `review-summary.json` 保存逐格原始自动评分、六项人工判读依据、final text 与证据文件哈希；`review-summary.md` 提供批次及账号表。离线复核无需模型、SSH 或生产变更：

```powershell
python .local/research-20260925/live-account-controls-v3/review_results.py
python .local/research-20260925/live-account-controls-v3/review_followup.py
python .local/research-20260925/live-account-controls-v3/review_followup.py --subdirectory official-cli-q0-control
python .local/research-20260925/live-account-controls-v3/build_review_summary.py
```
