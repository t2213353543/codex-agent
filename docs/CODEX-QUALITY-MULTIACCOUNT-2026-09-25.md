# 同期多账号请求差异与原题对照

**2026-09-25 后续上线更新：** 当前后端 `bb92216f…aca277ad` 已完成生产验收。导入识别官方 `user_id` fallback；刷新按当前 AT/ID 重建身份、保存旋转令牌并阻断旧身份会话；WS 连续链保持缓存键；HTTP 改键后超限在绑定前拒绝。最终 Linux 946、Windows 822 顶级通过，零失败，另有完整官方 CLI 7 项通过。八账号目录 28 对相同、官方外部令牌本地装载均成功；原 24 次答题结果保持，质量恢复仍未证明。[完整修复与验收](CODEX-GATEWAY-IDENTITY-REFRESH-2026-09-25.md)。下文旧版本号及测试数字按各自阶段保留。

**2026-09-25 12:25 更新：** 新增账号到位后，[八账号固定请求与官方 CLI 复测](CODEX-QUALITY-EIGHT-ACCOUNTS-2026-09-25.md)已完成：16 次固定请求加 8 次官方 CLI 直连调用，自动统计 3 正确、15 错误、6 无法判分，人工核对为 3 次 21、21 次 29。八个独立用户共六个 workspace；q0/g7 本轮均出现对错变化。下文的旧 q5/q6 额度阻塞及旧启动入口是历史记录，不代表当前没有多账号结果。新增 g1–g7 不与下文 q1–q7 按编号对应。

用户指出，近期单个异常账号的模型/推理档位对照不足以定位账号差异。这里追加同期多账号实验，历史标签只用于选取对象，不参与当前答案分类；原题仍按既定 21 判据评分。

## 第一组 Q0/q4 ABBA：中止前缀，不是完整质量对照

事前顺序为 Q0→q4→q4→Q0，固定完整 CLI 0.155.0、gpt-5.6-sol/medium、原题、同一空工作目录、最新透明 Go 网关实验服务器、同一 Rust helper 与直连出口。每格使用新的 CLI home，最多一次推理 POST，无 request/stream 自动重试；错误或不完整参考停止余下序列。

| 已执行格 | 结果 | 实际派发 |
| --- | --- | --- |
| 01-Q0 | clean completed，21，54.595 秒 | 一次 POST，helper 完整接收 |
| 02-q4 | HTTP 200，但上游 failed，无最终答案，12.407 秒 | 一次 POST，无重放；CLI 报 stream closed before response.completed |

q4 源元数据同期 secondary_used_percent 为 98%，随后盘点为 99%；这只是配额观测，现有失败证据没有确认具体失败码，不能将其当作数学错答或断言失败原因。两账号 claim plan 均为 self_serve_business_prolite。两格凭据/owner/workspace epoch 均保持，目录正常，捕获 helper/wrapper 已退出，临时 AT 实际检查删除。后两格按登记规则没有执行。

实际 native IPC 显示 model、reasoning、include、text、store/stream、UA、originator/version、routing hint 一致；input、client_metadata、cache/session 身份有差异。CLI 当前使用 Responses Lite，顶层 tools 不存在、instructions 为空，工具和指令在 input 中。七个 input item 整体摘要不同，但 schema 3 没有记录其内部字段摘要，不能推断内容不同，也不能反过来宣称只差 ID。

离线比较器核验 CLI clean、原题、进程/配置、完整 native POST、实际 token/workspace、request ID、helper 生命周期、账号 epoch 与顺序。Q0 可评分为 21，q4 的失败不能评分。报告明确 **2/4 前缀不足以形成同期正常/错答两类账号对照**。证据为 `.local/research-20260925/multiaccount-abba/{index,plan,comparison}.json` 及每格原始报告。

## 下一组：三个账号、镜像顺序

从源系统仍 active 且配额尚可的账号中，按事前额度盘点选定两个尚未在本次研究评分的候选 q5/q6，不按答案挑选。登记顺序 **Q0→q5→q6→q6→q5→Q0**，最多六次推理，其余条件保持。任一账号遇到无效参考，仅跳过它后续预定格，不重试该格，并继续其他预定账号；全部执行/跳过均写入 index。

增加 schema 4 只读摘要：每个 input item 保留原完整摘要，另记录每个顶层字段与内部 metadata 字段的摘要、长度、存在性，以比较 tools/content 与 id/turn_id/create_time。官方源码证明这些 ID 可随会话变化，但新摘要仍保留其差异，不删除 ID 冒称整份请求相同。[实现及源码依据](quality-20260925/ipc-summary-schema4-lite.md)。

schema 4 采集工具已在 Linux `--network none`、真实 helper 环境运行 **41 项测试全部通过、0 跳过**，3.290 秒。旧 schema 3 的 38 项 Linux 也全部通过。两代实验保存独立固定工具副本和哈希；新增摘要不改变请求、TLS 或错误传播。

三账号实验的六格已经完整采集复核，全部 clean completed，无跳过、无重试、无额外推理 POST。比较器全部证据检查通过，同一 alias 的 credential/owner/workspace epoch 前后保持，三个账号身份不同。实验服务退出，全部 helper/wrapper 回收，三个临时 AT 文件均已实际检查删除。

| 顺序 | 账号 | 答案 | 秒 | input tokens | reasoning output tokens |
| --- | --- | ---: | ---: | ---: | ---: |
| 1 | Q0 | 21 | 67.808 | 6881 | 2406 |
| 2 | q5 | 29 | 17.202 | 6881 | 516 |
| 3 | q6 | 29 | 21.566 | 6881 | 516 |
| 4 | q6 | 29 | 38.621 | 6881 | 516 |
| 5 | q5 | 29 | 38.063 | 6881 | 516 |
| 6 | Q0 | 21 | 46.537 | 6881 | 1552 |

Q0 本轮两次正确，q5/q6 各两次错误；这是同期两类结果的实际证据，不是引用历史标签。但每账号两次不能建立普遍稳定准确率。错答四格相同的 516 reasoning tokens 是相关观测，不等同已证明推理预算被限制或分配到低算力机器。

证据目录 `.local/research-20260925/multiaccount-abccba/`，最终 `comparison.json` 中 `complete_controlled_comparison=true`、`concurrent_correct_and_wrong_account_classes=true`，其含义是登记顺序、固定参数/头和单格来源核验完成；报告也明确 `complete_request_equivalent=false`，没有掩盖动态字段差异。曾生成的 interim/progress 只代表执行期间前缀，不代替最终报告。

前两格 schema 4 已明确定位上下文项差异：七项的 content、tools、role、type 等字段摘要相同；item 0/1 仅 id 不同，item 2–6 的 id 及 metadata 不同，其中 metadata 差异仅 turn_id/create_time，content_item_kinds 相同。顶层 client_metadata/cache key 和身份/会话头的差异仍保留，完整请求并不逐字节相同。

另一个不能忽略的共同因素：q5、q6 和此前 q4 属于同一 workspace，Q0 属于另一个 workspace；三者 claim plan 都是 self_serve_business_prolite。该实验区分了实际账号身份，但没有独立区分 user-level 与 workspace-level 状态。源系统已有 owner 凭据只读盘点中，多数是 personal free workspace，异常 workspace 的 owner 凭据不可用，不能拿这些不同 scope 的 token 假作同 workspace 对照。

下一步准备固定整份 CLI 正文、请求头和会话标识，只替换有对应 JWT/账号验证的 Authorization 与 ChatGPT-Account-Id，通过同一原生 helper 重放。该实验用于进一步排除动态会话因素，不会在得到结果前宣称已经找到服务端内部机制。

该重放还没有发送模型请求。执行前重新读取源状态：q4/q5/q6 均已达 secondary 100%，20 个 active 账号中只有 Q0 对应账号仍低于 95%；其余状态为 removed/exhausted/error/dead，不能冒作仍可用的同类样本。q5/q6 的源配额预计重置为北京时间 10 月 1 日 20:50:20 / 21:25:05，属于随后几天的外部条件，不是需要反复轮询几分钟的后台任务。详细原始状态为 `.local/research-20260925/exact-account-control/availability.json`。当前继续完成重放工具和不调用模型的捕获验证，保留已登记方案；不把 100% 配额后的失败算成数学错答。

完整 CLI 的 capture-only 验收随后通过：从真实 native IPC writer 捕获 36,352 字节、12 字段、7 个完整 input item 的请求，`capture_posts=1`、`model_inference_requests=0`、`extra_posts_refused=0`。CLI 返回明确的 synthetic_capture_only 错误，该结果不计质量。捕获体/头原文件仅在服务器私有目录短暂存在，随后与临时 AT 一并删除，报告保存摘要和完整性证据。Linux network-none 的 exact Python **15 项全部通过、0 跳过**，真实 Rust 单 POST 回环已执行；Go race 普通/压缩 IPC 捕获 **2 项通过、0 跳过**，vet 通过。独立复核发现的 foreign response 混入和分片绝对期限问题已先修再验证。

下一次执行入口已实现，先查当前 quota，不满足时明确返回 `started=false,model_calls=0`；满足时创建独立时间戳实验目录、重新固定凭据 epoch、捕获新的完整 CLI 请求，再执行登记的 Q0/q5/q6/Q0 四次原生请求。已实际检查当前 100% 状态下不会启动模型请求：

```powershell
python .local/research-20260925/exact-account-control/start.py
```

本轮产出证明同期账号相关的答题差异，并排除了覆盖字段中的正文内容/工具定义、model/effort/tier/UA/路由提示偏差；它尚未证明账号标记、内部集群或该状态的形成原因。网关合法大历史拒绝缺陷已独立修复上线，但不能把它说成这批短原题错答的根因。

后续[四账号实际权限/额度读取](CODEX-QUALITY-WORKSPACE-METADATA-2026-09-25.md)进一步确认当前 q4/q5/q6 的原因是 `workspace_member_credits_depleted`，Q0 未到限；相同 JWT scope/plan 不能解释质量差异。发现并修复管理 API 丢弃该原因、消费限额和模型可用性的问题，最终管理补丁 `161bc615…f888126` 已上线并完成真实额度往返验收；推理源文件未改。这是诊断缺陷修复，不是此前答 29 的因果证明，不把事后额度数据当成请求时状态。

随后网关关闭硬编码 Linux 6.8 指纹模板，使用实际 CentOS 9 官方 CLI 身份；28765 登录源头也上线 `20260925-auth-device-v2`，修复 `oai-did` 的 chatgpt/auth 域漂移、OAuth fresh device 错绑及异常 device 值进入 header/Sentinel。两处均有生产容器/服务验收，但由于 q5/q6 当前额度已耗尽，尚未取得环境修正后的新一轮 q0/q5/q6 同题模型结果；不把旧六格答案改写成修复后结果。

### 额度可用候选复核：q7 不是新账号

额度盘点找到一个 active、secondary 仅 4% 的候选，临时登记为 q7 并执行了 Q0→q7→q7→Q0 四格固定请求。实验结束后发现 q7 的 identity/token/workspace 三元 hash 与 Q0 **完全相同**，说明 source child 是同一账号的另一条登记，不是第二个账号。四格 HTTP 200/completed，最终文本实际都回答 21；响应 ID 与 reasoning usage 各不相同，但全部归类为同账号重复测量，**不计入多账号质量对照**。凭据已清理，证据为 `.local/research-20260925/exact-account-q7-20260925-215000/identity-collision.json`。

这次暴露了实验门禁缺口：过去只按 email alias/child 选择，未在模型请求前强制比较 identity/token/workspace 三元组。新的 `exact-account-control/start.py` 已增加 distinct-account 门禁，身份折叠时在发送模型请求前终止；以后不会把同账号伪装成多账号结果。

随后执行了全量只读候选盘点，而不是继续按 alias 猜测：当前 active 且 secondary<95% 的候选只有 1 个，其 `identity_sha256/token_sha256/workspace_sha256` 均与网关 Q0 相同；`distinct_candidates=[]`。盘点没有模型调用或 OAuth 刷新，脱敏结果为 `.local/research-20260925/account-inventory-v2.json`。因此当前没有符合门禁的第二账号，不能再发起新的“多账号”模型实验。

另发现评分器原先不识别“因此答案是 21 个（9 个圆形、12 个五角星形）”这类行内结论，导致真实 21 被记为 ambiguous；`tools/quality_probe_report.py` 已增加行内结论和尾部说明识别，53 项测试通过。对 q7 同账号四格的离线重评分结果为 4/4 个 21，但仍不改变其“同账号、非多账号实验”分类。

离线重建第一组报告，不发送模型请求：

```sh
python tools/quality_multiaccount_compare.py .local/research-20260925/multiaccount-abba/index.json --output .local/research-20260925/multiaccount-abba/comparison.json
```
