# A 组缓存基线与上线后评估 — 2026-09-28

本轮遵守 20:31 源码冻结，只读既有 JSON；没有运行网络工具、生产查询或模型请求。仅新增本报告与 `.local/collab-a-20260928/cache-baseline/` 中的本地冻结输入、分析器及汇总。

## 可用的量化基线

优先使用 `natural-baseline-corrected.json`：起点 2026-09-28 11:46:00 UTC，`captured_at` 12:33:09 UTC，观察时运行二进制 `3d00cac386b7104d20f2adfccde5f583984b5206c60dfd92bb41c9e1c9b1607f`。时间为约 47 分 9 秒的累计窗口；脚本没有保存数据库的精确上界，`captured_at` 只能作为捕获截止近似值。保存的成功请求 key mode 尚未出现 `shared_api_key`，因此这是启用新 scope 前的候选基线，不是 api_key scope 的效果。

| 指标 | 值 |
| --- | ---: |
| 分类为 natural_traffic 的请求 | 917 |
| 成功 / 失败 | 794 / 123 |
| 读取已知 / 未知请求 | 794 / 123 |
| 读取样本请求覆盖率 | 86.5867% |
| 同一批已知读取请求的 input | 68,816,410 |
| 已报告读取 tokens | 50,306,963 |
| 同分母读取率 | **73.1031%** |
| 独立报告创建 tokens / 样本 | 18,259,198 / 794 |
| 此窗口错误率 | 13.4133% |

本地重新相加 `coverage`、`models`、`workloads` 均与 `overall` 一致。794 个读取样本均有完整请求观察；其余 123 条在保存记录中的 input/read 都为 0，不能解释成实际消耗必为 0。该特定快照可以用 raw `cached` 作为已知读取分子；一般报告有显式 unknown 的部分计数时，不能如此替代。创建 18,259,198 与 input-read=18,509,447 不相等，二者保持独立。

此快照 `requests - read_samples - read_unknown = 0`，即修正版口径没有排除出的控制请求残差。`workloads` 只出现 natural，证明没有命中当前已知 smoke/experiment 标记；没有原始 user-agent 清单，不能证明所有请求都来自真实用户而非未标记脚本。

模型维度可作为流量结构辅助基线；保存字段是报表 `model`（取自 request_logs.model），不要当作已经核验的上游 sent_model：

| model | 请求 | 已知/未知读取样本 | 读取 input | 读取率 |
| --- | ---: | ---: | ---: | ---: |
| gpt-5.6-sol | 489 | 441 / 48 | 37,140,519 | 71.8533% |
| gpt-6-astra | 291 | 261 / 30 | 24,016,352 | 77.3066% |
| gpt-6-sol | 88 | 45 / 43 | 4,282,409 | 61.4994% |
| gpt-5.6-terra | 44 | 44 / 0 | 3,263,929 | 73.0172% |
| gpt-5.6-luna | 4 | 3 / 1 | 113,201 | 32.8230% |
| gpt-6-luna | 1 | 0 / 1 | 未知 | 未知 |

## 旧报告哪些仍可用

| 保存证据 | 可用范围 | 不可作的比较 |
| --- | --- | --- |
| natural-before-scope.json | 同 runtime、同 11:46 起点，截至 12:20:04：490 请求、434 个已知读取样本、27,532,917 / 37,422,053 = 73.5740%，创建 9,762,905 | 使用旧控制过滤合同；不能将它与 12:33:09 的 73.1031% 相减后归因过滤修复或 scope；窗口重叠且流量增加 |
| natural-baseline-corrected.json | 当前首选总体/模型分层基线，窗口内观察为启用共享 scope 前 | 不能单独估计启用后的因果收益；缺精确 SQL 上界、逐请求版本/settings 和完整同 owner 分层 |
| natural-errors-before-scope.json | 11:45:21 进程启动至约 12:20:20：502 请求、56 失败，错误构成可用于分类 | 起止均与两份 cache report 不同；不能把 56/502 与 123/917 的变化解释为新版本错误变化 |
| tenant_partitions | 同一观察进程内 owner/key/model/config 的局部定位 | 只含成功请求且只取 top 20；不能代表全部流量或失败率，也不能跨进程按 salted hash join |
| recent_compared | 最近 30 条已比较请求的定性排查 | 不是全量样本；不能计算总体 cold/hot 比例或置信区间 |
| partition probe | 合成同 owner/key 行为机制证据 | 与自然流量、gateway 发布 smoke、前后版本窗口分开；不能合并为自然命中率 |

两份重叠 cache 快照差额为 427 请求、360 个读取样本、67 个未知，22,774,046 / 31,394,357 = 72.5418%。这只是同起点累计快照的诊断差额，尚未按精确事件截止重查，不能宣称一个独立完整窗口。73.5740%→73.1031% 的 -0.4709 个百分点尤其不能归因本轮修复。

旧错误快照中的 56 条失败分为：结构化输出 20、service_tier 8、forced tool_choice 3、store 1（参数能力类共 32），session owner/allowed 13、上游 422 10、response.failed rate_limit 1。没有同窗 workload×错误类别交叉表，不能把这些分类套入新的 917 条总体；入场前单独保留的错误也不进入 cache 分母。

## 冷热拆分的已有证据

| 观察分类（成功且读取已知） | 样本 | input | reads | 读取率 | creation |
| --- | ---: | ---: | ---: | ---: | ---: |
| first_observed | 235 | 14,462,737 | 613,088 | 4.2391% | 13,768,225 |
| compared | 559 | 54,353,673 | 49,693,875 | 91.4269% | 4,490,973 |

first_observed 占 input 21.0164%，却占未读取 input 74.8248%、创建 75.4043%，是值得优先跟踪的群组；它是进程内首次观察，不是已证明的上游冷缓存。重启、LRU 淘汰或新 session 都能造成首次观察；反例是 generated_prefix 的 8 个 first_observed 样本仍有 74.8819% 读取率。

`explicit_override` 的首次观察 204 条为 2.8941% 读取，已比较 325 条为 92.6615%；`codex_default` 首次观察 20 条为 2.5749%，已比较 46 条为 86.5638%。因此不能简单用“explicit 总体 73.2835% 对 generated 总体 86.8599%”作为模式效果，两个群组的重复次数和 input 分布不同。

合成 probe 已从 `evidence.jsonl` 八条请求逐条重算，和 comparison.json 一致：同一个当前账户、gpt-6-astra/xhigh、交替四轮、每个组内 input 相同、A/B 组使用不同的隔离前缀，8 次均 HTTP 200/completed/OK，session/task/turn 各自不同且无自动重试。

| 实验组 | 第一次 reads / input | 后三次 reads / input | 含首次读取率 | 后三次读取率 | 总创建 |
| --- | ---: | ---: | ---: | ---: | ---: |
| A 每次随机 key | 0 / 38,736 | 0 / 116,208 | 0% | 0% | 154,672 |
| B 同一个固定 key | 0 / 38,736 | 116,004 / 116,208 | 74.8683% | 99.8245% | 38,668 |

这支持“相同组内内容因随机 key 被持续分区，固定 key 可复用”的机制判断。A/B 前缀不同，不能称跨组输入完全相同；未经过 gateway tenant 隔离；4 次/组的耗时不足以证明延迟改善。probe 结果声明不写 production request_logs，因此不应混入本地自然窗口。

## 证据支持的优先优化候选

top-20 tenant partitions 覆盖 630 个成功读取样本、58,832,858 input（全基线 input 的 85.4925%）。其中四个碎片多且读取低的分组共 152 条、5,876,748 input、1,056,676 reads、4,759,895 creation：

| sent_model | 同进程 account hash 前缀 | config hash 前缀 | distinct cache keys / 请求 | 读取率 |
| --- | --- | --- | ---: | ---: |
| gpt-6-astra | 4ae31b03 | 51b89e31 | 45 / 45 | 2.2106% |
| gpt-6-astra | 82c4d096 | 51b89e31 | 34 / 41 | 19.4832% |
| gpt-5.6-sol | 82c4d096 | 7b8a7f43 | 22 / 30 | 25.2108% |
| gpt-5.6-sol | 4ae31b03 | 7b8a7f43 | 26 / 36 | 28.4576% |

同 tenant/account/model/config 的局部模式对照也存在：account 4ae31b03 + gpt-5.6-sol + config 7b8a7f43，generated 59 请求 / 1 key / 96.0462%，explicit 36 请求 / 26 keys / 28.4576%；config 72b06e9c 中 generated 20 / 1 / 99.6045%，explicit 45 / 21 / 55.3475%。这些是优先评估 api_key scope 的对象，仍是观察相关性：配置 hash 不含完整 input，BPS 会把 tools/instructions 移入 input，不能把 config 相同说成实际前缀相同。

下一步应先验证 key 数是否按同 owner/key/model 收敛，再看这些群组的跨 session 首次请求读取是否改善。没有证据支持把不同 owner/key/model 合并，或删除工具、系统指令、历史以追求表面命中率。`controls_changed` 的 84 个创建样本为 851,037 creation，可作为第二级排查；它与 history/stable 等 change 桶重叠，不能相加成总损失，也不应擅自冻结用户 reasoning 参数。

## 新 scope 上线后的明确评估方法

1. **记录干预边界。** 以主会话实际启用 `bps_prompt_cache_scope=api_key` 的设置版本、UTC 时间 T、running binary SHA、进程启动时间和账户设置回执为准。软件发布与设置开启可能不同步。保存脚本 SHA、查询上下界；记录请求/完成时间的选择，排除跨 T 的在途旧请求。没有回执不认定新 scope 生效。
2. **固定不重叠窗口。** 第一份 post 窗口从 T 起单独报告启动/填充段；后续用固定且等长的自然窗口（例如连续两个 30 分钟窗口）持续比较。始终给出包含填充段的整体读取率，并另给重复 key/前缀的读取率；不要只丢掉低读取首请求来展示结果。现有 47 分钟 baseline 可做上下文，严格同窗比较需要重新取固定长度的旧明细。
3. **共同 workload 过滤。** natural 与 `gateway-release-validation/*`、`cacheNNN-<32hex>`、`cache90-*` 实验分开；已知其他 marker 也单列。控制/warmup 不属于缓存样本/未知；错误请求保留在总数，已知用量的失败仍进入同一已知读取统计。没有 marker 的脚本只能标注分类能力边界。
4. **同样本分子分母。** 对 cache eligible 且 `usage_known`、`cached_tokens_known` 有效、0≤cached≤input 的记录计算 `sum(cached)/sum(input)`；输出 read_samples、read_unknown、eligible_requests、read_input、read_tokens、creation_sum/samples。显式 0 进入分母，缺失不当作 0，creation 不加入命中分子。raw 全请求 input/cached 另列，不能假定与已知子集相等。
5. **控制流量结构。** 首先按稳定 tenant key、已确认 owner namespace、实际 sent_model 分层；再看客户端类型、input 长度、配置、前缀重复、请求间隔、reasoning/tool 分布。各层同时输出冷含总率和重复率。总体变化外再按旧窗口 input 权重报告可匹配模型层变化，避免模型构成变化造成假收益；缺同层数据明确列为不可匹配。当前聚合没有足够逐请求样本构造可信置信区间，不按 tokens 当独立试验做显著性检验。
6. **不要跨进程直接 join 观察 hash。** `account_hash`、`prefix_hash`、`cache_key_hash` 是进程随机盐 HMAC；重启后即使同 owner 也不同。现有 baseline 只能在其同一个 instance 内分组。后续授权的只读明细审计可用单一审计盐对旧新稳定 owner/key/model 映射或保存受控映射；不能从已保存的 salted 聚合反推同 owner。若只有当前 JSON，明确降级为整体/model 描述性比较，不构造同 owner 关联。
7. **验收两层结论。** 功能层：同一 tenant/已确认 owner/sent_model 的 `shared_api_key` 只有一个最终 key，跨边界仍隔离、响应/历史/计量不变。效果层：自然且匹配流量中读取率/创建量改善，并且错误分类和用户请求完成率未恶化；scope 收敛与读取收益分别陈述。≥70% 早在此基线就已满足，不能作为本次新增收益证明。

## 复跑与比较保护

冻结输入和汇总位于 `.local/collab-a-20260928/cache-baseline/inputs/`、`summary.json`，本地分析器不导入任何生产或网络工具：

```powershell
python .local/collab-a-20260928/cache-baseline/analyze.py
```

对后续已保存报告执行保守的同桶比较检查：

```powershell
python .local/collab-a-20260928/cache-baseline/analyze.py --compare-report PATH_TO_SAVED_REPORT.json
```

检查 `captured_at/since`、明确时区、窗口重叠/长度、runtime SHA、workload 分类和修正版控制过滤合同；混合或跨版本桶拒绝直接相减（exit 2），不自动声称因果收益。跨版本是本任务的最终目标之一，但必须根据上面的回执和分层方法显式分析；不能绕过守卫把总率差包装成 scope 效果。已用旧报告实际跑出拒绝结果，保存在 `comparison-rejected.log`。

冻结输入 SHA-256：

| 输入 | SHA-256 |
| --- | --- |
| corrected baseline | 1dfd58a788ba39d2f970d1c1c8407eca27b66785228b9a44f831e637a325e5de |
| old baseline | 13efc3ebc04a2e79e3970ee4ca277a8714c15635c9d1676f0b2f8a7f265634b7 |
| old error snapshot | b7a3182ce3793c8a1d88c32e16c10b53efcd0da80c89f95bde67176dce0af824 |
| probe evidence.jsonl | 8477f42cb8e1d93a820e8a63bdf69894ef7740441e90fa66e92df50dd2339ecc |
| probe comparison.json | d434f2113876d2745a8ff97099e7b65c11f61fa231dff4303eef5670d7ba476c |
| 当前 cache_rate_report.py 文本快照 | c44b62fba2e2d92eb7627428cde22cca6358407f689ca8f6d009f753af112d4a |

完整输入哈希在 summary.json；以上为上线前分析，下方独立追加实际上线回执，避免把前述基线当成新 scope 效果。

## 上线后设置与合成回执补充

本轮只读新增 `activate.log`、`postdeploy.json`、`postdeploy-statistics.json`，已复制至本地 inputs，未发起任何新请求。

- `activate.log` 确认 release `da708caa5ae5776c79bc604d3df4286ddced2010b8440609375201f9005d58dd`、PID 775841、binary SHA `b3c8acf1694ffe1c055f85b677a537a028435a8491874c8d07db36f0deb32dca`，当时 runtime settings_version=26。
- `postdeploy.json` 的 `checked_at=2026-09-28T12:45:13Z` 确认 0.4.10 / powerpoint_bps / api_key / settings_version=27，账户配置保持，8 条已保存 smoke 请求均 PASS 且最终 key_mode=shared_api_key。这证明 binary 激活之后另有设置变化，不能把两者生效时间混为一谈。
- 回执没有保存实际 settings PATCH 时刻。可确认 api_key **不晚于 12:45:13 UTC（20:45:13 +08）生效**；严格 post 自然窗口从该保守确认时点或更晚开始。不能根据本地文件 mtime、总耗时倒推出精确启用时刻。
- 两个账号各 4 条 synthetic smoke，分别各有一把稳定最终 key；每账号首条 read=0/write=37,901，后三条均 read=37,901/write=0。合计 227,406 reads / 304,384 input = **74.7102%**，独立 creation=75,802；按每账号剔除首条后的 6 条为 227,406 / 228,288 = **99.6136%**。这证明新设置的真实 gateway 合成复用与隔离行为，尚不构成自然流量成效。逐账号重算结果在 `postdeploy-smoke-summary.json`。
- `postdeploy-statistics.json` 卡片读取率 77.3679%，没有携带查询时间范围或 workload 分类，且 input=51,453,753 包含未知时间范围的保留记录，不能标作新版本自然流量读取率，也不与 73.1031% baseline 相减。

本地分析器新增显式 `--descriptive-cross-version` 模式，允许受约束的跨版本描述性比较，保留默认同桶保护。模式必须明确提供旧新 runtime SHA；验证两个窗口时区明确、时间顺序正确且不重叠、后窗口在 scope 保守确认时点之后、两份统计合同一致、选取同一个 workload 桶、分子分母可复核、各分组与总体可对账。若原报表混有 smoke，则仅比较其完整 natural workload 子桶，其他桶输出在 excluded_workloads；不把混合 models 表用于自然 model 标准化。窗口不等长作为混杂因素输出，永不输出 scope 因果收益，不关联跨盐 owner。

```powershell
python .local/collab-a-20260928/cache-baseline/analyze.py --compare-report PATH_TO_SAVED_AFTER.json --descriptive-cross-version --before-runtime 3d00cac386b7104d20f2adfccde5f583984b5206c60dfd92bb41c9e1c9b1607f --after-runtime b3c8acf1694ffe1c055f85b677a537a028435a8491874c8d07db36f0deb32dca --output .local/collab-a-20260928/cache-baseline/post-natural-comparison.json
```

已用实际旧报告冒充 after 验证保护会拒绝：runtime 不符、旧统计合同、窗口重叠、早于 scope 确认时点；证据 `cross-version-invalid-evidence-rejected.json`。后续自然流量实际证据单独接续如下。

## 第一份新 scope 自然快照：12:45:15–12:47:00 UTC

主会话保存的 `natural-postdeploy.json` 已冻结为 `inputs/natural-postdeploy-124700.json`。窗口起点晚于 12:45:13 的 scope 确认时点；runtime 为 b3c8acf…；只有 natural_traffic workload，和 baseline 不重叠，所有聚合对账通过。显式跨版本模式实际执行成功，输出 `post-natural-comparison-124700.json`，同时保留 `causal_scope_effect_estimate_allowed=false`。

| 指标 | 上线前基线 | 新 scope 初始窗口 |
| --- | ---: | ---: |
| 近似窗口长度 | 2,829 秒 | 105 秒 |
| 请求 | 917 | 41 |
| 已知 / 未知读取样本 | 794 / 123 | 32 / 9 |
| 已知读取 input | 68,816,410 | 2,416,157 |
| 已报告 reads | 50,306,963 | 1,720,067 |
| 读取率 | 73.1031% | **71.1902%** |
| 独立 creation | 18,259,198 | 691,488 |
| 读取样本覆盖率 | 86.5867% | 78.0488% |

原始读取率差为 -1.9129 个百分点，只表示这两份不同流量快照的数值差，不能判定 scope 退步：post 仅 32 个已知样本，运行时、样本长度、模型组合、重启后的缓存状态和错误构成都变化。32 条成功请求全部实际观察到 `shared_api_key`；保存的 7 个 tenant/account/sent_model/config 分组均只有 1 个 key，说明分组内实际收敛。因为报表没有各组的 key hash 集合，不能仅凭每 config 各 1 把就断言跨 config 合集也恰为 1 把。

成功请求中：26 条 compared 为 1,572,007 / 2,025,593 = 77.6072%；5 条 session 首次观察为 148,060 / 204,413 = 72.4318%；另 1 条 cache_key 首次观察 input=186,151、reads=0、creation=186,003。后者与另一条 gpt-5.6-terra 零读取样本合计 227,904 input，说明初始窗口仍包含真实的零读取创建。首次观察现在也能有高读取，符合跨 session 共享有机会复用的机制，但样本和前缀组成不足以建立因果。

模型辅助观察：gpt-5.6-sol 为 22 个已知样本、88.9333%；gpt-6-astra 为 8 个已知样本、65.1311%；gpt-5.6-terra 为 2 个已知样本、0%；gpt-6-sol 的 8 条和 gpt-5.6-luna 的 1 条没有已知读取，不能给这两类填 0% 读取率。继续等待较长、固定窗口自然数据比立即判断胜负更有信息价值。

`natural-errors-postdeploy.json` 另有两个时间范围：主 snapshot 在 12:46:03（自 12:44:04 进程启动累计 54 条/5 失败，含 scope 设置前后与 smoke）；错误分组查询已包含截至 12:46:51 的 8 条 rate_limit 和 1 条结构化输出错误。不能把“5”和“9”相加，或将其当作同一个 snapshot 的矛盾；也不能把 66.5588% 进程累计读取率混入这里的 105 秒自然窗口。具体错误类别与此自然窗口的对应需要同一事务、同一上下界的 workload×error 查询；本任务未访问生产进行补查。

新增证据 SHA-256：

| 保存输入 | SHA-256 |
| --- | --- |
| postdeploy.json | 7f98b091d14f5ff454f36cfba566e6c25284c5f3120ea59ad729b62344bab930 |
| natural-postdeploy-124700.json | 3d2b740869fa787b6a1612870cbb2629dbbdbf4a047feaee2b1909a9ebb45c4a |
| natural-errors-postdeploy-124603.json | ded5c01af981a8c4b18da5980f499f8f2c5a0e86a5d56dfb08fb09763e42e91c |
