# 固定原题的直连官方 HTTP / WebSocket 对照

**后续网关路径验证：** [v7 八格同账号镜像](CODEX-QUALITY-GATEWAY-DIRECT-2026-09-25.md)观察到新选账号 direct／隔离网关均两次答 21，g7 均两次答 29；包含实际网关出站请求证据。它是新的独立批次，不改本报告 12 格与自然 CLI 四格结果，也不合并为随机准确率。

**后续 v6 独立证据：** 对先行 GET 与 helper 生命周期的[三组前缀实验](CODEX-QUALITY-COOKIE-CONTROL-2026-09-25.md)确认另两次 429 的 32 字节正文与公开固定错误 `{"detail":"Rate limit exceeded"}` 的哈希精确相同，也与本报告两份旧 429 正文摘要相同。由此可识别该错误文本，但不能倒推本轮未保存的 Retry-After 数值或限流策略；原 12 格及自然 CLI 四格统计保持。

2026-09-25，实验 v5。执行时间为北京时间 14:33:40–14:36:45（UTC 06:33:40–06:36:45）。原题按用户既定答案 **21** 验收，没有把答案写入模型输入。

**12 格全部按预登记顺序执行：10 格完成并明确答 29，另外 2 格为 q0 的 HTTP 429、没有模型答案。切换 WS 没有在本轮恢复原题正确答案。** 冻结自动评分器识别了 7 个 29，另 3 个完整答案因格式未被自动解析；独立人工逐份核验，这 3 个也明确结论为 29。429 不计为错答。

本轮是 **native Rust helper 直连官方** 的 transport 对照，不经过 Go 推理转发。因此不能用它宣称生产 Go 网关质量恢复，也不能把本批与旧 24 格合并成随机抽样的正确率。

## 实验固定项

使用官方 CLI `0.155.0` 新捕获原题完整首轮 HTTP 实体，模型 `gpt-5.6-sol`、effort `medium`。模型输入在任何 Go gateway policy/serializer 之前截取，保留官方 Responses Lite 形式，没有使用 Go 后续填入空 `instructions` 的备用请求体。模型目录 GET 可以联网；捕获阶段没有模型 POST。

| 固定对象 | SHA-256 |
|---|---|
| 原题 input | `cd771ba38f8f75696ba703c7d3e91e5273588c91cb8c4145e3ce4b077c075c3f` |
| 官方 CLI | `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501` |
| 实际 native helper | `9110b869e65db6bf7664e39fcfe3b2ec17fa7a0207ddbeccf28d73dbc12ccce8` |
| 原始 HTTP body | `7e8d4732aaa166959a513be1c11c26459f5b4bbec220407c17e20b2c1434131b` |
| 原始无认证 HTTP head | `e7c4d9542c925b388a30c57c9bc44c41c13f78507ee493ba1086074a54516348` |

每格使用新 helper 和新连接，每账号身份独立，固定原题、完整 input、cache/session 元数据，无 `previous_response_id`、无上一格输出历史、无 token refresh、无自动重试或 HTTP/WS fallback。q0/g2/g7 根据既往对照选入；g2/g7 共享工作区但用户不同，不是新随机样本。

WS 请求依照固定源码投影：增加 `type=response.create`；保留 `stream:true` 和原 10 个非 metadata 字段的原始值字节；保留 7 个原 metadata 值；只增加 Lite 标志和实际 101 握手后生成的 WS request-start timestamp。HTTP 使用 zstd，WS 由官方 connector 协商 permessage-deflate。首轮不注入 101 返回的 turn-state，也不继承任何之前的 turn-state。

独立验证脚本在服务器只读真实 raw body/head，用另一套 JSON decoder、Decimal 和原始 value span 比较，确认 36,330 字节的 HTTP 实体、题目精确哈希匹配一处、字段/metadata/header 投影和 timestamp-only 变化全部成立。原始文件在批次结束后按计划删除，保留哈希证明和逐格投影证据。

## 逐格结果

H 为 HTTP，W 为 WS。`—` 表示没有模型 terminal usage，不是零。cached 为上游明确报告的 `usage.input_tokens_details.cached_tokens`；reasoning 为其 `output_tokens_details.reasoning_tokens`。

| 格次 | 账号/路径 | 状态 | 完整答案 | 自动解析 | input | cached | output | reasoning | 秒 |
|---:|---|---:|---:|---|---:|---:|---:|---:|---:|
| 01 | q0 / H | 429 | — | 未完成 | — | — | — | — | 0.294 |
| 02 | g2 / W | 101 + completed + ACK | 29 | 29 | 6881 | 0 | 987 | 649 | 19.348 |
| 03 | g7 / H | 200 + completed | 29 | 未解析¹ | 6881 | 5888 | 928 | 715 | 17.867 |
| 04 | q0 / W | 101 + completed + ACK | 29 | 29 | 6881 | 5888 | 1211 | 976 | 23.029 |
| 05 | g2 / H | 200 + completed | 29 | 未解析¹ | 6881 | 5888 | 775 | 516 | 14.985 |
| 06 | g7 / W | 101 + completed + ACK | 29 | 29 | 6881 | 5888 | 549 | 387 | 12.811 |
| 07 | g7 / W | 101 + completed + ACK | 29 | 29 | 6881 | 0 | 659 | 516 | 13.252 |
| 08 | g2 / H | 200 + completed | 29 | 29 | 6881 | 5888 | 604 | 383 | 11.997 |
| 09 | q0 / W | 101 + completed + ACK | 29 | 29 | 6881 | 5888 | 984 | 728 | 20.165 |
| 10 | g7 / H | 200 + completed | 29 | 未解析¹ | 6881 | 5888 | 1191 | 965 | 22.893 |
| 11 | g2 / W | 101 + completed + ACK | 29 | 29 | 6881 | 5888 | 699 | 516 | 15.071 |
| 12 | q0 / H | 429 | — | 未完成 | — | — | — | — | 0.319 |

¹ 自动评分原值继续保持 `answer=null / no_unambiguous_final_answer`，没有事后修改评分器或原始结果。人工判断放在独立字段，并绑定 final 文本哈希：

| 格次 | 人工依据 | final SHA-256 |
|---|---|---|
| 03-h-g7 | 最后一句明确写“最少取出 29 个” | `3406465052b1a20238127d32f4e7119dd509af9550eac576d41d9e5473f4ed53` |
| 05-h-g2 | 开头写“最少需要取出 29 个”，结尾再次以 29 作保证数 | `21069c49f873ee0b07bc05689a7e154883ba40f0fde9279abfd0b6f8d5879be3` |
| 10-h-g7 | boxed 29 后接“才能保证满足要求” | `7f232e65d1ee769e0007d7a8e3063d3d4bf1be68ad906a4a28d2c98d32a7d0c0` |

10 份公开 final 都沿“最多盲取 28 个仍不满足，再加一”的解释得出 29，没有使用按形状选择取糖的 21 策略。这是可见最终文本的判断，不是对隐藏推理内容的推断。10 个 response ID 哈希和 10 个 final 文本哈希均不同。

## 可得结论与边界

- **g2、g7：** 每账号 HTTP 2 次和 WS 2 次均完整答 29。本轮没有观察到换 WS 就能达到用户的 21 判据。
- **q0：** WS 2 次完成并答 29，HTTP 2 次 429。可以报告这组准入/响应状态差异；因为 HTTP 没有完成答案，不能计算 q0 的 HTTP↔WS 答案质量改善。
- **429 原因未知：** 两份安全错误摘要仅为 `{"type":"error"}`，没有足以判定具体限流机制的 code/message。所有格前后 usage eligibility 都为 allowed，只证明该 GET 返回了普通 usage 允许状态，不能证明模型调用当时不存在请求级限制。
- 两份 429 错误体哈希相同：`96c534cc320264e5091e907e67dc8667bfadc8b0cd6172031bfa91c7c5d8f82d`。响应 header 摘要包含 Retry-After 的哈希，没有保留可判定的秒值；不能据此解释为私有 capacity 原因。q0 的 primary usage 在首格前后均为 64%，末格前后均为 66%，全部 allowed；“HTTP 因总额度不足失败”仍没有证据。
- **缓存/516 没有因果结论：** 3 格 reasoning=516，分布在 HTTP 和 WS，且 cached 既有 0 又有 5888；本轮没有正确答案可作为反向控制，不能据此定位服务端内部预算、账户标签或缓存机制。
- **小样本和时序限制：** 每条件仅 2 格；strict mirror 的每条件格次均值都是 6.5，但实际时长不同。中间两次 g7/WS 相邻；固定 session/cache 可能带来服务端状态相关性，新 helper 不等于独立服务端计算。

按用户标准，人工确认的完整答案为 **0 个正确、10 个错误，另 2 格调用失败不评分**。这个数字只描述这批预登记对象与请求，不能外推为稳定正确率。

## 独立核验和清理

启动前冻结 Linux 工具测试 **157 项通过、0 跳过、0 失败**。独立审核曾复现并修复身份 fallback、数字冒充字符串校验、completed 证据被清空及 runner 故障停止边界；自有 15 项合成验证通过，详情见[预登记与审核记录](C:/Users/Administrator/Desktop/项目/codex-agent/docs/quality-20260925/http-ws-control-review.md)。

最终程序核验全部 12 格，`passed=true / findings=[]`，检查了：

- 预登记镜像顺序、最多 12 次单次 dispatch、实际 helper 与运行时哈希；
- 同一账号前后 user/workspace/token/identity epoch，24 份前后 usage 与 sidecar 一致；
- HTTP 200 与 WS 101 的不同成功条件、WS 逐格一次 SEND/ACK、terminal 和清理状态；
- HTTP 原始 body 哈希不变；WS 原 10 个字段与 7 个 metadata 值不变、非认证 header 不变、仅 timestamp 按登记更新；
- response usage 两视图、完整 final 字节哈希、冻结评分器重新解析与原报告一致；
- `execution/control/report/projection/usage` 的逐格一致性。唯一预知的结构差别是 runner 按字段名将报告内已哈希的 authorization 摘要再次标为 `[REDACTED]`；原报告的 bearer token hash 仍核对账号 epoch。

WS 收到 terminal 和 ACK 后主动停止读取；报告明确后续 transport 事件未继续观测，不能称整个上游 socket 正常关闭。所有本地 helper 均正常回收，临时 socket 已删除。最终 batch `finished=true`、`model_dispatch_attempts=12`、`deadline_exceeded=false`，无 projection failure；systemd unit 为 `MainPID=0 / ExecMainStatus=0 / inactive / dead`。raw 请求文件与捕获凭据已清理。

主任务在 UTC 06:40:31 又实际读取服务器状态：[cleanup-verified.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/http-ws-controls-v5/cleanup-verified.json) 确认 inventory、capture、model 三个 unit 全部 `MainPID=0 / inactive / dead / exit=0`，5 份 raw/credential 私有文件均不存在，并记录服务器 execution 与 12 份 report 的原始字节 SHA-256。独立审核脚本逐一重算本地原字节哈希与该收据比较，防止下载器重新序列化或文件串格。

## 证据与离线复跑

- [冻结计划](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/http-ws-controls-v5/plan.json)
- [实际执行收据](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/http-ws-controls-v5/execution.json)
- [独立真实捕获验证](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/http-ws-controls-v5/review/remote-pair-verification.json)
- [独立逐格核验结果及 final](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/http-ws-controls-v5/review/results-final.json)
- [人工判据与 final 哈希](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/http-ws-controls-v5/review/human-annotations.json)
- [独立摘要](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/http-ws-controls-v5/review/summary.json)
- [服务器退出、私有文件清理及原始报告哈希](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/http-ws-controls-v5/cleanup-verified.json)

```powershell
python .local/research-20260925/http-ws-controls-v5/review/review_results.py --require-complete --output .local/research-20260925/http-ws-controls-v5/review/results-final.json
```

该命令只复核本地证据，不联网、不发模型请求、不修改原报告。要判断 Go 网关质量恢复，需要另登记固定账号与输入的 direct official ↔ Go gateway 对照；本批没有执行这一步。

## 独立补充：自然 CLI HTTP 的 q0/g2 ABBA 四格

主任务另行预登记并完成 4 次官方 CLI 调用，顺序为 q0→g2→g2→q0，UTC 06:43:42–06:47:32。目的限定为复核 q0 在固定 HTTP 重放中的 429 是否也出现在自然的新 CLI 会话；它不是对主实验失败格的自动重试，**不并入前面的 12 格统计**。

四次都使用相同官方 CLI `0.155.0`、原题、`gpt-5.6-sol / medium` 和 HTTP-only 禁重试配置，每次 fresh home、fresh 模型目录与动态 session/item metadata，直接访问官方，不经过 Go 推理转发。配置 SHA 为 `10f21abf0fab92c0818ebb4332163fc703524bc75490e38776a39ca302626373`。

| 格次 | 账号 | CLI 终态 | 独立重算答案 | CLI input | CLI cached | CLI output | CLI reasoning | 秒 |
|---:|---|---|---:|---:|---:|---:|---:|---:|
| 01 | q0 | completed / reference_clean | 29 | 6893 | 0 | 1122 | 844 | 71.943 |
| 02 | g2 | completed / reference_clean | 29 | 6893 | 0 | 919 | 653 | 54.500 |
| 03 | g2 | completed / reference_clean | 29 | 6893 | 5888 | 689 | 516 | 47.832 |
| 04 | q0 | completed / reference_clean | 29 | 6893 | 0 | 727 | 516 | 48.734 |

自然 CLI 的原 report 不含 `answer` 字段，因此外层 execution 的四个 `answer=null` 不是模型无答案，也不是这四格的自动评分失败。独立审核从未修改的 final 调用冻结评分器，四格均识别为 `29 / terminal_boxed`；人工逐份可见结论也均为 29，按用户 21 判据均错误。各 final 哈希及原报告/事件文件哈希保存在[自然 CLI 独立审核](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/http-ws-controls-v5/review/natural-cli-review.json)。

**这四格证明两次 q0 的自然 HTTP CLI 会话都完成了模型答案，固定重放的两次 429 不代表 q0 普遍不能通过 HTTP 调用。** 它们仍未恢复到答案 21，也没有定位固定重放 429 的原因。自然 CLI 的 input 为 6893，而固定重放为 6881，且完整会话 metadata、请求构造时刻不同；不能只归因于 session ID、时间戳、header、Rust helper 或 cache 中任一因素。

独立审核 `passed=true / findings=[]`：4 个前后身份 epoch 与主实验同账号 epoch 一致、8 份前后 usage 全 allowed、CLI/prompt/config 控制一致、每格一个 `thread.started/turn.started/item.completed/turn.completed`、safe event 的 final 和 usage 与 report 精确相符；离线重分类均 completed，unit 退出状态为 `MainPID=0 / inactive / dead / exit=0`，execution 记录凭据已清理。

这个补充没有原始 HTTP trace：`transport_observed=null`、`model_returned=null`，HTTP 状态码未知，不能把 completed 写成“HTTP 200”。配置请求 HTTP 且禁重试；CLI stdout 不是独立捕获的实际 POST 次数，也不是上游原始 nested usage。尤其 CLI `cached_input_tokens=0` 不能区分上游明确零和字段缺失。补充结果保持这些边界。

离线复跑：

```powershell
python .local/research-20260925/http-ws-controls-v5/review/review_natural_cli.py
```
