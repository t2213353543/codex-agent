# v15：派生完整 HTTP 与真实 WSS 预热链独立结果核验

v15 的 8 次推理和 4 次 WSS 预热均完成，独立核验 468 项通过、0 findings。new2 的 HTTP 两格和 WSS 两格都回答 21；new1 的两条路径都回答 29，其中一格保留自动 `null`、以独立人工注释确认 29。WSS 已使用真实上游预热 response ID，未将本地合成 ID 作为真实前次响应发送。

模型固定 `gpt-5.6-sol`、medium、原题 SHA `cd771ba38f8f75696ba703c7d3e91e5273588c91cb8c4145e3ce4b077c075c3f`。2026-09-25 12:22:45 UTC 前完成两账号 usage/epoch 预检；每格另检查前后 usage 和稳定身份。实验严格按 H-new2、W-new1、H-new1、W-new2 及其反向镜像执行，无自动刷新、重试或 HTTP fallback。所有应用推理输入用量均为 6894 tokens，不将四个预热算作额外四个模型答案。

| 顺序 | 账号 | 路径 | 自动答案 | 人工注释 | input | cached | reasoning | output |
|---|---|---|---:|---|---:|---:|---:|---:|
| 01 | new2 | HTTP | 21 | — | 6894 | 0 | 1552 | 1780 |
| 02 | new1 | WSS | 29 | — | 6894 | 0 | 371 | 646 |
| 03 | new1 | HTTP | 29 | — | 6894 | 5888 | 481 | 671 |
| 04 | new2 | WSS | 21 | — | 6894 | 6656 | 1352 | 1586 |
| 05 | new2 | WSS | 21 | — | 6894 | 0 | 1034 | 1252 |
| 06 | new1 | HTTP | null | 29 | 6894 | 5888 | 510 | 757 |
| 07 | new1 | WSS | 29 | — | 6894 | 5888 | 731 | 975 |
| 08 | new2 | HTTP | 21 | — | 6894 | 6656 | 3624 | 3904 |

自动统计为 4 个正确、3 个错答、1 个未评分。第六格正文方框和末句均明确给出 29；人工注释绑定 final SHA `0e3038d76a36ef3ea658a70dd8d264f57b54b5b61fd6a22b9c71aa22c274323a` 和原 report SHA，未改写自动结果。评分器严格绑定本批 manifest 的 `0441c9c5faa5f7a3fa4c99462fbad0c77fd2ae145debc0860023d6c53d2fe921`，已保存完整冻结副本；没有继续修改评分器以适配本轮文案。

四个 WSS 预热均报告 input=5919、total=5919、output=0、reasoning=0、cached=0。terminal output 为空、独立 output_item.done 数为 0，且未出现生成内容事件。真实预热上下文处理有输入用量，不能因为 total 非零就当成生成了答案。四格均只有一次握手、一条预热 create 和一条推理 create，ACK 序号分别为 1/2；全局接收序号递增，真实 completion ID 与增量 previous ID 关联证据匹配，推理最终 response ID 与预热 ID 不同。

这次四条 WSS 链均未观察到 `x-codex-turn-state`，报告保留 `observed=false`，没有伪造空状态为已验证值。工具已实现固定官方源码的首值继承，但本轮不能声称真实上游验证了该分支。每条 WSS 的实际请求投影只修改原有发送时间和第二帧的 previous_response_id，未新增 turn-state。未编辑字节段、原模板 SHA、实际 IPC 正文 SHA 和字节数守恒均通过。

两个 ID 哈希基准必须区分：预热 response/link 哈希是解码后字符串 UTF-8 的 SHA，projection 的 next-value 哈希是 JSON 字符串值字节的 SHA，包含引号和必要转义。审查没有把两者直接比较为相同哈希；实际关联由固定工具的唯一值区间替换、同一连接回执、模板绑定和正反例测试证明。原始真实 ID 不写入公开报告。

HTTP 是明确派生的完整逻辑请求，非官方原始 HTTP 捕获。它把新捕获的两个 warmup input 项与五个 inference delta 项按原序合成七项，空预热输出条件由源码和实测共同校验。七个 item 的语义哈希逐项保持，稳定控制字段一致，原题恰好出现一次。派生正文共 36,362 字节，SHA `bd3c90982e377d403d046f0e1ef18a00f1ec5a6442a5b30d46ac37c5696f3bca`，四次实际 HTTP IPC 正文均等于此值，使用 zstd 压缩。

HTTP 转换删除 WS 顶层 type/previous_response_id，移除 WS 发送时间 metadata，将 Lite 标记投影为 HTTP header。实际头部差异为新增 Accept、Content-Type、Lite header，移除 WebSocket beta header，并把 `x-codex-turn-metadata` 从握手预热值更新成 inference 当前回合值。其余应用头保持，安装/session/thread/window 等非运输差异未另改。不会把这条 HTTP 路径与“多一次真实预热、有服务端前次状态的 WSS”说成除 TLS 外完全相同。

本批仍同时出现 cached=0 的正确与错误答案，以及 cached>0 的正确与错误答案。第五格是直接可核验的真实 WSS 反例：cached=0、回答 21；第二格同样 cached=0、回答 29。两账号在两种运输路径上的答案分组一致，当前证据不支持把错误答案单独归结为 HTTP 或缓存未命中，也不足以识别内部模型路由或账号风险机制。

生产日志只读检查显示八格时间窗中已完成并持久化的重叠请求均为 0；它不覆盖未完成请求或外部账号使用。实验是每账号、每路径两次的镜像重复，时间和服务端状态未随机化，`review/order-statistics.json` 仅作描述统计。

最后一条 terminal 和 ACK 后的协议事件没有继续读取，报告明确限定该边界；helper 清理另行检查，四条 WSS 均正常回收、无强杀、socket 不再存在。批次终态 unit MainPID=0、inactive/dead、退出码 0，原始帧和凭据文件清理完成；32 份远端公开文件 SHA 和 execution SHA 与本地一致。

独立审查器事先通过 14 项隔离副本故障测试，覆盖正文、认证、预热输出、ID 关联、ACK、额外字段修改、状态、完成状态、自动答案、清理及 input 顺序。主要证据位于 `.local/research-20260925/http-ws-chain-controls-v15/review/results-final.json`、`order-statistics.json`、`human-annotations.json` 和批次 `projection.json`、`prepared-summary.json`、`cleanup-verified.json`。本次独立审查没有联网、刷新或调用模型。

只读复核命令：`python .local/research-20260925/http-ws-chain-controls-v15/review/review_results.py`
