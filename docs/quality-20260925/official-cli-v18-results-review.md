# v18：当前两账号的真实官方 CLI 基线

四次官方 CLI 调用均干净完成：退出码 0、每次恰好一条 turn.completed，没有启动警告、可见错误、工具执行或可见模型切换。独立核验 190 项全部通过。new2 两次正文回答 21，new1 两次回答 29。这说明本轮账号间答案差异也出现在官方 CLI 的直接执行中，并不只出现在自建网关或手工构造的 HTTP/WSS 请求里；它仍不能定位账号、共同环境或服务端内部机制中的根因。

2026-09-25 12:52:36 UTC 预登记顺序为 new2、new1、new1、new2。使用固定官方 CLI 0.155.0 二进制 `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`，内置 openai provider、官方默认端点和目录加载，模型请求为 `gpt-5.6-sol`、medium。没有网关推理跳转、合成服务器、模型目录覆盖、HTTP/WS 请求重建或捕获正文重放。

| 格 | 账号 | 自动答案 | 独立人工注释 | CLI input | cached input | reasoning output | output | CLI 秒数 |
|---|---|---:|---|---:|---:|---:|---:|---:|
| 01 | new2 | null | 21 | 6881 | 0 | 1552 | 1786 | 34.205 |
| 02 | new1 | 29 | — | 6881 | 0 | 516 | 702 | 49.600 |
| 03 | new1 | 29 | — | 6881 | 0 | 516 | 729 | 52.360 |
| 04 | new2 | 21 | — | 6881 | 0 | 1865 | 2147 | 43.132 |

自动统计保留为 1 个正确、2 个错答、1 个未评分。第一格首句及末尾方框明确都是 21，给出 12 个五角星形加 9 个圆形的方案；旧自动评分器把“缺失或让圆形全为西瓜味”的反例叙述识别为条件答案。此次没有继续修评分器或重算原报告，仅添加人工注释，绑定 final SHA `67ca7b93e9bee5e4d6ec8623af93206a4388328040f49a56ab2c3fdc85084e11` 与原 report SHA `c3ea9ddb92b326d06f4476a53ec2878ed4bd5aa6238df7f4df51d20d33006f0c`。自动 `null` 未改写，计入独立注释后是两个 21、两个 29。

原题 SHA 固定 `cd771ba38f8f75696ba703c7d3e91e5273588c91cb8c4145e3ce4b077c075c3f`，四次共同配置 SHA `cda9a167b92c4d68c0ff8629f8db6efadda291737724a609b239ba052ef697a9`。配置禁用可配置工具、web search、插件/技能注入，使用只解答原题的固定 developer 指令，不提供预期答案。这是共同实验配置，不是声称毫无配置的交互式 CLI。

整组与每格 usage/epoch 门禁核对当前 access token、稳定 user/workspace，期间没有实验刷新。每次临时创建独立 CODEX_HOME、认证和配置，固定同一个空 cwd，并在版本检查及 exec 进入前检查目录为空。真实运行用户是 UID 0，按 launcher 实际值记录，没有伪装为生产服务 UID。CLI 自行产生的新 home/session/installation/turn 信息没有固定重写，四次自然请求正文因此不必相同。

四份 CLI 本地目录观察均显示 cache 文件存在且解析成功，client_version 匹配、认证 identity 字段存在、目标模型唯一且在目录中，tool mode 是 code_mode_only。该证据来自 CLI 创建的目录 cache；没有注入目录来改变默认取模行为。工具配置不等于证明服务器看见的工具表为空，报告仍保留 `tool_surface_zero_verified=false`。

这次只能确认 **四次 CLI exec 调用**及四次 `--version` 调用，不能保证只有四个网络模型请求。内置 provider 保留官方预热、重试和 fallback 逻辑；固定源码在 release build 可隐藏第一次 WebSocket 重试通知。probe 在可见 reference error 处停止，脚本没有重试 execute；“没有可见重连”仍不能排除内部隐式尝试。

同样，`transport_requested` 是官方默认 WebSocket/fallback，四份 `transport_observed` 均为 null。现有 safe exec events 不提供成功传输协议证据，本轮没有为测量而开启原始 Rust 日志或修改流量。不能把本轮写成“已观测四次真实 WSS 请求”，也不能把 `model_returned=null` 写成已经从服务器返回字段验证了模型身份。报告准确区分请求的模型、目录所见模型和未观察的返回模型。

表中用量是 CLI 的 turn/thread 累计 Usage 输出。0.155.0 支持 reasoning_output_tokens，但其数据来自 `last_total_token_usage.total`；没有该通知时可默认补零。这里四次 input/output/reasoning 均有明确非零值，cached_input_tokens 全部报告为 0；它仍不同于原生工具直接读取的单条后端 response usage。不能仅凭这些 CLI cached=0 认定全部真实后台采样都未命中缓存。

四格与 v12、v15 的同账号答案分组一致，但各批正文捕获、动态身份、时间和运输状态不同，不能合并为单一随机样本。生产持久化日志只读查询显示四个时间窗中已完成重叠请求均为 0，不覆盖未完成请求或外部使用。本轮没有证据确认隐藏风险标签、后端路由或环境根因。

清理证明显示 version=4、exec=4、凭据文件全部删除、无证据边界失败；实验 unit 为 MainPID=0、inactive/dead、退出码 0。24 份远端 safe artifact SHA 及 execution SHA 与本地一致。最终文本、usage、事件顺序和数目均与 `events.safe.jsonl` 交叉核对；该事件文件不含原始认证或请求正文。

证据位于 `.local/research-20260925/official-cli-controls-v18/review/results-final.json`、`human-annotations.json`、`order-statistics.json` 及批次 `cleanup-verified.json`。审查器事前通过 10 项纯合成故障测试；此处独立审查没有启动 CLI、刷新或模型调用。

只读复核命令：`python .local/research-20260925/official-cli-controls-v18/review/review_results.py`
