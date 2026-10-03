# A 组结构化输出公开历史真实续接

2026-09-28 21:00 CST。**一组真实 HTTP full-history 对照确认：保留原始 reasoning 密文，将内部函数参数投影为公开 assistant JSON 后，上游能继续并准确恢复标记。** 这不是完整网关适配、WebSocket 或压缩验收。

实验源码和执行入口仅在 `.local/collab-a-20260928/structured-history-probe/`，主树未改。复用既有服务器内凭据读取 helper，单个启用账号、相同模型 `gpt-6-astra` / `xhigh`、与当前 BPS 相同身份头/直接传输。运行时绑定生产二进制 `b3c8acf1694ffe1c055f85b677a537a028435a8491874c8d07db36f0deb32dca`，未改生产配置或重启服务。

## 两组结果

| 运行 | 首轮 | 公开投影历史续聊 | 原始函数+真实本地校验结果续聊 | 结论 |
| --- | --- | --- | --- | --- |
| `20260928T125644Z` | 200/completed，1 function，0 reasoning cipher | 200/completed，准确恢复随机标记 | 200/completed，准确恢复同一标记 | 密文前提未满足，整体退出3并保留记录 |
| `20260928T125851Z` | 200/completed，1 reasoning cipher + 1 function | 200/completed，准确恢复随机标记 | 200/completed，准确恢复同一标记 | 密文续接边界 PASS，整体退出0 |

首组 schema 的 answer 是枚举，模型无需计算而没有生成 reasoning。第二组改为需要求解三个同余式的整数任务，schema 仅约束 answer 为 integer；程序实际检验其为最小正解 `7417`，没有把答案写在问题或 schema 中。首轮实际报告93个 reasoning tokens，并返回一项 encrypted reasoning。

投影分支只保留原始用户问题，按原顺序原样追加 reasoning RawMessage，将精确内部函数的完整 arguments 字符串装入 assistant `output_text`，再追加后续 developer 指令与用户问题。原始控制分支保留函数项，并添加来自程序**确实完成**的校验/序列化结果。两分支最后使用相同 developer 指令明确格式化动作已完成、当前只返回标记纯文本，避免早期“必须调用工具”的 developer 指令污染对照。标记只存在于前轮工具 schema/函数参数/公开JSON中，后续问题没有直接携带答案。

每组至多3条显式请求；两组总计6条生成，无自动重试。代码检查403/429、嵌套结构化限流/额度错误后停止，不依据任意消息文本猜测重试。凭据和实际 encrypted_content 仅在远端进程内存流转，未写磁盘、未回传；证据仅含状态、类型计数、数字用量、摘要和标记正确性。临时远端二进制与压缩包已在 finally 清理。

## 证据

- [首组未覆盖密文的事件](../.local/collab-a-20260928/structured-history-probe/20260928T125644Z/evidence.jsonl)
- [第二组完整事件](../.local/collab-a-20260928/structured-history-probe/20260928T125851Z/evidence.jsonl)
- [第二组退出码/源码及二进制摘要](../.local/collab-a-20260928/structured-history-probe/20260928T125851Z/result.json)
- 每组目录保存对应 `probe-source.go`，第二组源码 SHA `85331202212142d817f52f5becde6b8b10fd5f94e71bb6ab02ee62b8ad50a135`，测试二进制 SHA `c021c179fb223a49a52f865bbc90eab7fb50338054b9ecbc4aeefbe034f6cc2c`。

本次复跑会再次产生最多3次模型请求：

```powershell
python .local/collab-a-20260928/structured-history-probe/run.py
```

## 下一版仍须验证

该实验通过直接 BPS HTTP 发出全历史，未接入当前候选的 `ParseStructuredFormat` / `StreamStructured`，不能替代其单测和客户端集成。需要继续验证：公开结果实际入 Redis、`previous_response_id`、同schema/换schema/撤销格式、HTTP↔WS、compact 后恢复、客户工具与内部工具策略隔离、refusal、失败usage与取消。

两条续聊在相同账号上成功只证明本组受测密文/历史可用，不证明任意密文都可跨变换或跨账号。协议仍保持账号绑定，不应放宽既有owner守卫。
