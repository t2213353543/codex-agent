本轮在修复后的来源环境中实际重新授权并使用新凭据后，new1 在同一题上仍两次回答 29；new2 仍两次回答 21。原题、答案合同 21、模型 `gpt-5.6-sol` 和 `reasoning.effort=medium` 均未改。本轮没有观察到 new1 的本题质量恢复，也不能从结果确定账号差异的隐藏原因。

前测和观察性后测各按 `new2,new1,new1,new2` 执行 4 次，共 8 次原生 HTTP 推理，全部 HTTP 200、`response.completed`。后测于 2026-09-25 15:54:18 UTC（北京时间 9 月 25 日 23:54:18）结束。前测发生在来源环境修复部署后、重新授权之前，因此比较的是同一修复环境下旧凭据与新凭据的表现，不是旧版与新版代码的独立对照。

| 阶段 | 格 | 账号 | 自动答案 | 独立人工注释 | 输入 | 已缓存输入 | reasoning tokens | 输出 |
| --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 前测 | 01 | new2 | null | 21 | 6881 | 0 | 2239 | 2484 |
| 前测 | 02 | new1 | 29 | — | 6881 | 5888 | 516 | 783 |
| 前测 | 03 | new1 | 29 | — | 6881 | 5888 | 516 | 688 |
| 前测 | 04 | new2 | 21 | — | 6881 | 6656 | 1346 | 1622 |
| 观察性后测 | 01 | new2 | 21 | — | 6881 | 6656 | 1398 | 1645 |
| 观察性后测 | 02 | new1 | 29 | — | 6881 | 5888 | 516 | 720 |
| 观察性后测 | 03 | new1 | 29 | — | 6881 | 5888 | 516 | 753 |
| 观察性后测 | 04 | new2 | 21 | — | 6881 | 6656 | 2070 | 2356 |

自动评分器按执行时的 manifest SHA 加载，未修改或替换。前测 01 的原自动结果 `null / conditional_final_answer` 保留；人工注释绑定原报告 SHA `6e5f9109…` 和 final SHA `c08df088…`。该正文首句和末尾都明确回答 21，并采用 9 个圆形、12 个五角星的取法。后测四格无需人工替换自动答案。

new1 前后四份 29 的解释均在求“任意混合摸取时，最多能取多少仍未配对”的上界，没有使用题目允许的按形状选择策略。new2 四份正文均明确使用 9 个圆形加 12 个五角星。答案合同仍由原题及既有独立数学验证限定，没有因结果修改题目或给提示。重复出现 516 个 reasoning tokens 是返回用量观察，不证明硬上限、风控或其他隐藏策略。

两账号在实际后测请求中使用了不同于前测的新 AT：new2 从 SHA `889c9b1d…` 变为 `8bfb62a2…`，new1 从 `0e28971d…` 变为 `2b4236f2…`。账号记录、稳定用户和 workspace 一致。来源端与网关回读的当前 AT 一致，OAuth DID 和指纹均绑定当前 AT；原 browser bundle 和保存的注册指纹没有被覆盖。

new1 的剩余一次授权采用独立、可落盘的回执传输，实际完成 1 次服务授权调用、1 次 saved-token 推送。容器内 7 个顺序 checkpoint、`latest.json`、`worker-outcome.json` 共 9 个文件已取回并逐个匹配 SHA。fresh AT、RT、DID、指纹的哈希与来源库、当前观察一致；最终 checkpoint 包含 owner lease 已释放。宿主端仅在真实网关回读后增加 `after_epoch` 和 `push_gateway_readback`，其余原回执字段与容器落盘内容一致。传输返回 7 帧、0 协议错误，reader 已结束，进程已回收。

new2 的原操作在授权调用后丢失 stdout 协议。之后的只读观察证明新 AT 已同时存在于来源端和网关，DID/指纹绑定有效，出现成功状态和 `cpa_destination_synced` 事件，worker 与 owner lease 已不存在；这些证据不能恢复原 fresh OAuth 返回值、在线验证和推送终态回执，也不能据此重新归属原 worker 的行为。new2 未重复授权来补证据。

因此原受控双账号干预仍不完整。冻结的 `epoch_contract.py` 仍实际拒绝，错误为 `transition_not_complete`，原 `after/started.json` 从未创建。后四格由另一个预登记的 `observation-after-v21-amendment` 合同执行，明确写入 `original_controlled_design_complete=false` 和 `fresh_result_unavailable=["new2"]`。总推理仍为原预算 8 次，没有额外补跑；服务授权调用数不等于已观测的 OAuth wire 请求数。

八格的实际 IPC body 都匹配同一原始 CLI HTTP 捕获，SHA `7a0763348d70ddc20103b9bcdc0d6adb6561a80c0a38c80440f3ae69f4a345be`；非认证头逐字段哈希在前后两阶段一致，压缩均为 zstd，每格使用独立 helper scope。实际 Authorization 和 workspace 头分别匹配该阶段的固定账号代次。分组及逐格前后 usage 均通过，未自动刷新或重试。返回公开 model/effort 字段一致，只能说明公开控制字段，不能证明隐藏后端或计算预算。

独立检查包括前测 140 项、后测 150 项、实际观察性准入 35 项，以及最终跨阶段、落盘与清理审查 172 项，全部通过。47 个服务器安全产物 SHA 与本地文件匹配；四个原始捕获文件已清理，捕获凭据不存在，五个实验 unit 的记录均为 PID 0、inactive、exit 0，model helper 已回收且 socket 不存在。前后八个窗口的已完成、已落库生产网关日志重叠均为 0；该观察不涵盖外部使用或未完成请求。

可复查产物位于 `.local/research-20260925/post-environment-controls-v21/review/`：`results-final.json` 保存八格矩阵及全部证据 SHA，`observation-after-final.json` 保存逐格协议与评分复核，`observation-transition-review.json` 保存新凭据准入证据，`intervention-outcome-review.json` 保存 new2 缺失证据界限。离线复核命令：

```powershell
python .local/research-20260925/post-environment-controls-v21/review/review_final_comparison.py
```

这些结果限定为同一道题、同一批请求：环境修复和重新授权尚未使 new1 的本题表现恢复。两账号都改变了凭据，授权、时间和服务端状态没有被独立隔离，不能据此宣称某个设备字段已被排除，也不能给出隐藏风险或降智根因结论。
