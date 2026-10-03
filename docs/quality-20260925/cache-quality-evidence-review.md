# 原题缓存与答案：v3–v8 已有证据离线复核

2026-09-25。只读取已保存的正式计划、执行收据、报告和既有人工判定；零凭据读取、零模型调用、零生产修改。原题 SHA 固定为 `cd771ba38f8f75696ba703c7d3e91e5273588c91cb8c4145e3ce4b077c075c3f`，模型 `gpt-5.6-sol`，effort `medium`，用户答案判据仍为 21。

**已经有可靠反例：缓存 token 为 0 也能答 21；缓存 token 大于 0 也会答 29。** 因此“缓存未命中必定答错”以及“缓存命中就一定正确”都不符合已有记录。v8 刷新后某一对结果出现缓存与答案同向变化，不能覆盖更早已观察到的反例。

## 1. 最强反例：同一账号、同一凭据、同一完整正文

| 正式记录 | input | cached | reasoning | output | 答案 |
|---|---:|---:|---:|---:|---:|
| v3 `01-q0` | 6879 | **0** | 1034 | 1244 | **21** |
| v3 `16-q0` | 6879 | **5888** | 499 | 701 | **29** |

两格完整 POST body SHA 均为 `0783217c49c6a928ead6f838c9fe7f9ccd2f5565824cabe891d507c6b21f675b`，user/workspace/token epoch 全部相同，实际 native IPC body SHA 也相同。两格都是 completed，返回模型与 effort 明确为 sol/medium，答案由原冻结自动 parser识别，没有依赖新增主观重评分。`cached_tokens` 在两份终态原始 Response usage 投影中均明确存在，不是缺字段填 0。

原始报告：[01-q0/report.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/live-account-controls-v3/01-q0/report.json)，SHA `7d3020f1295d49020befa798e1b214d550ef4f0bd061ed4dc41e1e78e0f93a58`；[16-q0/report.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/live-account-controls-v3/16-q0/report.json)，SHA `099562102e4e86d0104d507cdeefa0cdc2914073ae7cb7a7dd9893b13a326dab`。

这不是证明缓存导致变差：两格位置不同，服务端状态、时间和随机性仍可能变化。它证明的是，**无缓存不是错答的充分条件，也不是错答的必要条件；正缓存不是正确答案的充分条件，也不是正确答案的必要条件**。

同批 v7 也有独立反例，且包含真实网关代码链：

| 正式记录 | 路径 | input | cached | reasoning | output | 答案 |
|---|---|---:|---:|---:|---:|---:|
| `01-d-new2` | native direct | 6881 | 0 | 1266 | 1542 | 21 |
| `03-g-new2` | 隔离生产代码网关 | 6926 | 0 | 1552 | 1832 | 21 |
| `05-d-g7` | native direct | 6881 | 5888 | 516 | 829 | 29 |
| `07-g-g7` | 隔离生产代码网关 | 6926 | 5888 | 516 | 673 | 29 |

这些 v7 行不能当作相同 body 的四次随机重复：gateway 添加生产配置的 instructions 并映射 cache key，input token 数也不同；每行都有独立 source/sent body SHA。[v7 原始证据目录](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/gateway-direct-controls-v7/review/summary.json)

## 2. 全部登记格保留，按批次和来源分层

收集 v3 固定请求 16 格、两组官方 CLI 各 4 格、v5 HTTP/WS 12 格及官方 CLI自然请求 4 格、v6 cookie 12 登记格、v7 8 格、v8 before/after各4格。v4 只有目录 GET 和本地认证装载，不混入推理统计。

共 **68 个登记格、63 次实际推理、59 个完整最终回答**；完整回答中 14 次21、45次29。另有4次HTTP429无答案、1格因epoch变化中止未发、4格未开始。上述数字是历史清点，不是独立随机样本准确率，也不宜据此计算某账号稳定降智概率。

下表只对完成回答做 cache/答案交叉清点；写成 `21次数 / 29次数`，不把失败或缺用量记成零缓存。

| 分层 | 完整回答 | 明确 native cached=0：21/29 | 明确 native cached>0：21/29 | 非回答或未执行 |
|---|---:|---:|---:|---|
| v3 固定 native HTTP | 16 | 1 / 3 | 2 / 10 | 0 |
| v5 HTTP/WS | 10 | 0 / 2 | 0 / 8 | 2×429 |
| v6 GET/helper三组 | 5 | 0 / 0 | 0 / 5 | 2×429、1中止、4未开始 |
| v7 direct/gateway | 8 | 2 / 2 | 2 / 2 | 0 |
| v8 refresh前 | 4 | 0 / 0 | 4 / 0 | 0 |
| v8 refresh后 | 4 | 0 / 1 | 3 / 0 | 0 |

原生终态 usage 可明确比较的完整回答共47格，其中 **cached=0且答21有3格；cached>0且答29有25格**。这里只是列举反例及计数，不把这些批次合并成一个因果估计。

另有12个官方 CLI JSONL 完整回答，全部29：6格 frontend `cached_input_tokens>0`，6格 frontend显示0。这组单独记录为 `CLI_positive_reported` 或 `CLI_zero_reported_backend_presence_not_observed`；CLI turn usage 是客户端汇总形式，没有原始 HTTP Response usage 字段存在性证据，因此不能把其显示0混同为本研究已经独立抓到 backend明示0。

## 3. 每行保存什么、怎样核验

完整可机器读表：[rows.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cache-key-controls-v9/historical/rows.json)。逐行保存：batch/phase/cell/alias/arm、实际或计划epoch区别、token/user/workspace/identity摘要、请求与实际出站body摘要、报告SHA、完整状态、原自动答案、是否引用既有人工判定、最终文本摘要、input/cached/reasoning/output、缓存字段路径与是否存在、来源语义。

native direct 的身份来自执行前后epoch并与报告 token/workspace hash匹配；gateway行额外核对实际 server-report 的导入user/workspace/token和native请求核验标记。CLI行使用各格 identity-before/after和冻结review；完整body本来没有捕获，明确标记 `not_observed_cli_stdout`，不拿config或题目hash冒充完整bodyhash。

人工只引用此前已经保存的判定：v3 review-summary、v5/v6 manual_parser_misses、v5 natural-cli-review。所有引用都先核最终文本SHA或报告SHA；没有改原报告、没有放宽冻结parser、没有新增“猜一个答案”的规则。自动字段和人工答案分开保留。

执行了 **349 项断言**，包括原题/model/effort、报告与既有review摘要、epoch绑定、native返回控制字段、原始报告数量与已登记格总数、同账号反例body与epoch相同。每份读取文件有字节长度及SHA清单：[source-manifest.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cache-key-controls-v9/historical/source-manifest.json)。派生汇总和全部反例：[summary.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cache-key-controls-v9/historical/summary.json)、[counterexamples.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cache-key-controls-v9/historical/counterexamples.json)。

重跑只读清点：

```powershell
python .local/research-20260925/cache-key-controls-v9/historical/build_review.py
```

该脚本只更新 historical 下派生JSON，不读取凭据或联网。原正式证据保持不变。

## 4. v8 相邻差异仍有价值，但只是下一实验线索

v8 after 的 g7 两格使用相同新AT、相同完整body/所有非认证headers及相同返回model/effort/tier等控制字段：`after/02-g7` cached=6656、reasoning=2388、答21；`after/03-g7` cached=0、reasoning=425、答29。不同response ID说明这是两次实际回答，记录没有混错账号。[v8 独立汇总](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/credential-controls-v8/review/summary.json)

该局部观察值得做**只改 cache key 字段**的受控实验；但缓存token是响应结果的一部分，不是客户端随机分配的干预变量。它也可能和后端路由/状态、上下文重用、时间等共同变化。不能把这个二点关联提升为“cachemiss导致低算力”，更不能忽略上面的反例。

## 5. 固定 CLI 源码支持的 override，及其真实范围

[core/src/client.rs:487](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:487) 的内部 `ModelClient::with_session_context()` 接受 `prompt_cache_key_override: Option<String>`；`:497` 优先返回该override，否则内部会话可用 `source:parent_thread_id`，普通会话最后采用 `responses_metadata.session_id`。

当前可定位的生产caller在 [session/session.rs:1562](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/session/session.rs:1562)，传入 [guardian/review_session.rs:297](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/guardian/review_session.rs:297) 的Guardian专用 `guardian:<parent_thread_id>`。普通 SessionSource::Cli 在该函数返回None；对应源测试断言也明确如此。

所以可以确认**底层原生请求结构允许独立 cache key 值，cache key 不必永远等于session ID**，但本次源码没有证明存在任意用户可配置的公开 CLI `prompt_cache_key` 配置开关。下一实验应称“直接 native 请求中对已知协议字段的受控替换”，不能包装成“普通官方 CLI 设置里的自然新会话行为”。

cache key写入 `[client.rs:869–886]` 的request对象，与 `client_metadata=responses_metadata.client_metadata()` 分别构造；[client.rs:1213](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:1213) 的HTTP options仍从同一responses_metadata取session/thread；[codex-api/src/endpoint/responses.rs:117](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/codex-api/src/endpoint/responses.rs:117) 把thread写x-client-request-id并投影session/thread headers。没有“override cache key 顺便重写session/设备信息”的分支。

WS增量匹配还明确比较前后prompt_cache_key；改变key可能使增量请求不再等价。因此本轮应限定HTTP完整首轮，fresh helper，无previous_response_id、不复用WS状态。已有gateway还会按账户namespace重写cache key，因此应直接native官方HTTPS做干预，不能把gateway重写混入同一唯一变量假设。

## 6. 下一轮有限 AB 建议

初始方案选择new2与g7，但新 v9 preflight 发现g7同一刷新后AT返回401。随后一次独立只读native usage诊断明确得到 `token_revoked`，182-byte错误body SHA为 `ec94b559fd8f6b159922f3183f517600dd049dcaa800fc96ca5e21930a358a4b`，前后epoch未变；g7不再用于接下来的缓存key实验，也不追加refresh来强行继续。最后一次v8同AT usage200在08:40:16Z，v9首个401在08:56:10Z，独立确认在09:02:01Z。未证明撤销原因，不把401算29。[只读诊断收据](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cache-key-controls-v9/auth-diagnostic.json)

root在新的v10目录选择preflight200的new2与new1；使用同一份新冻结完整CLI raw body，每账号两组各两次：

- A：保留该捕获原prompt_cache_key。
- B：仅替换顶层 `prompt_cache_key` 的字符串value span，使用该格预登记的新UUID；其余body bytes与全部非认证headers逐字节不变。

v10实际runner预登记的8格镜像为：`new2-A, new1-B, new2-B, new1-A, new1-A, new2-B, new1-B, new2-A`。B的每格使用不同预登记UUID，表示新key干预；不能第二次偷偷复用B1然后把它叫新key。A保持同一个原key。无模型结果前生成并保存所有UUID摘要与源/变体bodyhash，禁止看到结果后换key或补发。

正文投影必须拒绝重复JSON成员，保留原JSON数字/转义/空白，证明除目标value span以外前缀与后缀原字节完全相同；cache key为字符串且合法UUID。所有headers—including session/thread/routing/metadata—保留原值；不要联动安装ID/设备信息/turn ID。题目和答案判据仍然不变。

每格仅1 POST、fresh helper、无RT刷新/GET预热/重试/切换transport。记录当前tokenepoch、请求变体hash、实际IPC发送hash、回答用量及完成状态。B新key**不保证cached_tokens=0**，A旧key**不保证命中**；这种不遵从预期本身就是结果，不能据此排除样本。

有限结果只能说明固定body下cache-key字段变化的同期关联，仍不能证明服务端隐藏cluster或账号标记。若B影响答案但实际缓存指标无对应变化，应明确记录，不把观测硬套成cache解释。

## 7. v10 runner 独立离线验证

新增独立 [check_runner_graph.py](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cache-key-controls-v10/review/check_runner_graph.py)，复用先前合成工具但不修改v9历史。它执行v10实际main AST、真实 `run_control_batch` 和真实cache-key投影函数，替换管理/usage/native I/O为合成适配器；不导入实际inventory密钥，不联网。

20项模拟全部通过，核对：flat alias→epoch seed、错误嵌套旧shape在计划/调用前拒绝；只选new2/new1、每账号四格、8格镜像、五个key摘要在首个模型调用前冻结；180秒group preflight的过期/未来/拒绝/身份变化阻断；注册时在途流量阻断；实际token选择匹配alias；only-cache-key字节投影额外改input/header均阻断；实际发送body/auth不符、持久化失败、cleanup失败不继续下一格；普通429继续后续登记格但不重试；零dispatch不虚构已核验出站。

runner SHA为 `bd412a40c7f307b48442f87a3f3772c0d26a5fbf5424046befd8a619e22074e0`。证据：[runner-graph-summary.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cache-key-controls-v10/review/runner-graph-summary.json)、[runner-graph.log](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cache-key-controls-v10/review/runner-graph.log)。这里仅证明执行与停止边界，不能把20项合成测试记作20次真实模型实验。

## 8. v10 前置中止与 v11 新 epoch

v10 在零推理捕获开始前检测到 new1 的 AT 和版本被外部更新，因此没有创建私有凭据文件、没有捕获模型 POST、没有真实模型调用，已归档为 `not_started_capture_epoch_changed`，并非一组失败答案。[v10 experiment.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cache-key-controls-v10/experiment.json)

v11 使用新独立目录和显式当前seed：new1 Version=8、token SHA `0e28971d…52b2c97`，其user/workspace保持；new2 Version=3、AT SHA仍为 `84ba24ba…59284c`。本审计直接核对 v11 的 flat seed 与 v10 `capture-epoch-change.json` 每项完全相等，并与 `2026-09-25T09:30:01Z` 新inventory逐项匹配；二者usage均200/allowed。没有将外部token更新隐含合并进旧实验，也没有重新刷新账号。

v11 runner 与已测试v10唯一代码差异是plan schema label `cache-key-quality-v10` 改为 `cache-key-quality-v11`。独立20项main模拟已按new1新token/version8、new2version3适配，全部通过；仍执行实际control runner与字段投影，仅I/O合成，不联网、不读凭据。runner SHA为 `f33c0d83be59a1800f592e94ea3d52ab5042bb0dd2dbe8b59f6cd7eb599c159a`，证据：[v11 runner-graph-summary.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cache-key-controls-v11/review/runner-graph-summary.json)。v9/v10历史文件未修改。

v11随后已实际完成8个POST。独立448项证据检查通过：new2固定/新key四个完整答案均21；new1固定两格及新key一格均29，另一个新key格HTTP200后SSE error无答案。完成的新key格均有正cached tokens，未观察到更换key恢复new1答案。[完整v11结果复核](C:/Users/Administrator/Desktop/项目/codex-agent/docs/quality-20260925/cache-key-v11-results-review.md) 保留原始parser与单独人工注释、失败分母和全部证据边界；本页v3–v8历史数字保持原范围，未偷偷并入v11。
