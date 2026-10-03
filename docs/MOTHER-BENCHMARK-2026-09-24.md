# 母号推送与真实模型评测 — 2026-09-24

已将 28765 母号 `radios.plucks-60@icloud.com` 的唯一推送地点设为 codex-agent。实际推送的是该母号下的 1 个活跃子号 `school_96alcoves@icloud.com`。80 并发测试两模型均有 60% 请求失败，原因定位为当前原生传输 route 的单 helper 32 活跃请求上限。所有成功返回的糖果题共 23 次，均回答 21。

## 结果

| 模型 | 基础功能 | 80 并发完成 | 请求错误率 | 32 并发对照 | 单独糖果题 | 全部已返回糖果答案 | 单独糖果耗时中位数 |
|---|---:|---:|---|---:|---|---|---:|
| gpt-5.6-sol | 9/9 | 32/80 | 60%（48 个 503） | 32/32 | 3/3，均 21 | 16/16 均 21 | 69.85 秒 |
| gpt-6-astra | 9/9 | 32/80 | 60%（48 个 503） | 32/32 | 3/3，均 21 | 7/7 均 21 | 30.24 秒 |

80 并发每模型共 80 请求，在同一 barrier 释放，包含 64 个短输出请求和 16 个糖果题。客户端峰值确实达到 80，无客户端自动重试；两模型分轮运行。所有测试使用 `reasoning.effort=medium`，无输出 token 上限。压力阶段独占本次测试负载，服务进程和二进制哈希在前后保持一致。

糖果题单独测试各 3 次；高并发阶段 Sol 完成 13/16 道，另外 3 道被网关拒绝；Astra 完成 4/16 道，另外 12 道被拒绝。因此“已返回答案正确率”均为 100%，但 80 并发糖果端到端完成且正确率分别为 81.25% 和 25%。这里的区别来自容量拒绝时哪些请求先抢到槽位，不能用来推断两模型的数学正确率差距。

基础功能覆盖 SSE、非流式 JSON、中文、JSON Schema、约 76KB 长输入检索、多轮记忆、函数调用及回传结果，每模型 9 条全部通过。WebSocket 每模型同连接两轮均通过，握手 101，包含 `previous_response_id` 续问。`/v1/responses/compact` 两模型均返回上游 404 `Not Found`，未通过。

短请求采用严格字符串对照。Astra 在 80 并发成功请求中有 2 条、32 并发对照中有 7 条在 `OK_N` 后增加句号；例如 `OK_42.`。原提示写成 `Reply exactly OK_42.`，末尾句号存在解释歧义，所以这些单列为格式差异，不计入请求失败或糖果错误。Sol 没有这种差异。

## 80 并发失败证据

- 160 条客户端记录与 160 条网关记录按 `X-Client-Request-ID` 一一匹配；全部落到账号 `a_441d76471862216532603a0ec1045ad9`。
- 96 条 503 均为 `attempts.error_source=gateway`、`error_code=capacity`、`duration_ms=0`，未拿到上游 HTTP 状态。64 条成功响应的返回模型与请求模型一致，全部仅 1 次 attempt。
- `gateway_native_http.go` 的 `gatewayNativeActiveLimit=32`，当前 route 满额直接返回 `errGatewayNativeCapacity`；Rust helper 同样声明 `MAX_ACTIVE=32`。账号的 `concurrency=80` 不会覆盖这一传输层限制。
- 32 并发对照每模型 32/32 完成，HTTP 请求错误率 0%；网关采样峰值为 32。
- 80 并发时网关 admission 采样峰值为 55，含即将被下层容量限制拒绝的请求；它不是 55 个真实上游执行请求。本次只证明客户端 80 同时发起、当前相同 route 的单 helper 接纳 32 个到原生传输层，未证明上游支持 80 个同时执行。32 不是整个网关跨多个 route 的全局容量。

该轮运行二进制 SHA256：`a673bc08c798d95b3a3af64320008a0418d01aa6753e4da21957634e65cab1c6`。本任务没有修改原生传输上限或重新部署 codex-agent 主程序。

## 初期异常与恢复

初轮基础测试在北京时间 14:31:48 遇到 codex-agent 正常停服重启，产生 1 条流中断和 1 条连接拒绝。systemd `NRestarts=0`，二进制 SHA 在重启前后相同；不能将此次停服归因为压测或模型错误。

初轮还暴露评测解析问题：部分 SSE 的 `response.completed.output=[]`，完整函数调用在 `response.output_item.done` 事件中。脚本已保留原始终态并从独立事件重构有效输出，随后真实函数调用往返通过。该初轮 1 条内容失败不视为已确认模型错误。

第二轮在约 14:36:50 收到 8 条上游 `401 token_revoked`。28765 的主动用量探测也独立观察到 401，自动重新授权，14:37:04 获取新 AT，14:37:12 推送成功；源 AT 和 bridge 状态摘要一致。此后重新运行完整基础功能通过。撤销的具体原因未知。以上两轮异常原始记录全部保留，没有混入稳定的 80 并发错误率。

## 糖果题核验

21 的策略是取 **9 个圆形 + 12 个五角星形**。12 个五角星必含苹果和桃子（不含苹果最多 10，不含桃子最多 11）；9 个圆形必含苹果或桃子（西瓜只有 8），于是必可跨形状配对。穷举 201,600 种取糖组成、450 个形状数量策略，20 及以下均存在失败反例。忽略“可凭手感选择形状”会得到 29。

## 推送部署与复跑

源服务现有 CPA 推送协议经独立 `codex-agent-loophub-bridge.service` 接入，私网监听 `172.19.0.1:18881`。只转发 AT，RT 仍由 28765 负责；后续同 workspace 子号动态接收，撤下时禁用对应 Agent 账号并保留历史。维护文档：[LOOPHUB-CODEX-BRIDGE.md](LOOPHUB-CODEX-BRIDGE.md)。

在本项目根目录复跑本次 80 并发：

```powershell
python tools/mother_benchmark_remote.py --phase load --effort medium --timeout 300 --concurrency 80 --load-requests 80 --load-candy 16
```

该命令通过现有 SSH helper 连接主机，在主机回环请求 codex-agent，并把无密钥的结果打包取回 `.local/mother-benchmark/<UTC>-load/`。服务端 key 仍在服务器私密文件中。其他阶段可用 `--phase functional`、`candy`、`compact`、`websocket`。脚本及每轮完整输入、回答、状态、耗时、SSE 事件和网关日志可复核。

证据目录：`.local/mother-benchmark/`；机器可读总表 `report.json`，全部 23 个糖果回答 `candy-answers.md`，源配置 `source-push.json`，恢复过程 `token-recovery.json`，数学穷举 `candy-math.json`。

流式终态判定参考 [OpenAI 官方 Streaming API responses](https://developers.openai.com/api/docs/guides/streaming-responses)。容量和可用性结论均来自本次实际请求，非文档推测。本次为单账号、单次混合突发样本，不代表持续负载的长期错误率。
