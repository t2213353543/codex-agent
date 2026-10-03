# 生产事件观察链：复用精确 JSON envelope

2026-09-25。以已部署 `33f4535f…cdbe5e` 对应的 WS/cooldown 源码为基线，对网关实际调用的事件观察链测量。发现大 WS done item 被重复解析、复制到多个 `map[string]RawMessage`，1 MiB 工具结果每事件累计分配约 5.44 MB。已局部改成一次解析复用，保持精确字段语义、原始转发字节与请求归属判断。

本地基准不是模型请求或网络吞吐测试。它调用 `gateway.go` 实际 SSE callback 顺序（attempt/model observation → event observation → cooldown identity），以及 WS upstream pump 的 cooldown → turn observation。WS 改后使用生产新增 `observeFrame` 入口，包含 lane、response ID、metadata、terminal 判断；没有只测孤立 map helper。设置均在计时外，循环不包含 TLS、网络、持久记录写盘或模型生成。

## 实测

Windows amd64、Go 1.26.7、Ryzen 7 9700X；相同机器和基准命令，三次 200 ms 结果取中位数：

| 生产观察链 | 之前 → 之后时间 | 之前 → 之后 B/op | allocs/op |
| --- | --- | --- | --- |
| SSE token delta | 8.256 → 6.079 µs | 5,441 → 4,439 | 123 → 97 |
| WS token delta | 10.958 → 6.040 µs | 7,743 → 4,657 | 176 → 96 |
| WS 1 MiB tool done | 20.181 → 6.814 ms | 5,435,740 → 2,168,937 | 173 → 98 |

这分别是约 26%、45%、66% 的本地观察链耗时下降；大 item 累计分配减少约 60%。B/op 是累计分配，不能说是 RSS 降幅；20.181→6.814 ms 也不能换算成线上模型吞吐提升。基准数据保留为 `.local/quality-20260925/parse-performance/baseline.txt`、`after.txt`。

```powershell
go test . -run '^$' -bench '^BenchmarkResearchParseProductionObservers$' -benchmem -benchtime=200ms -count=3
```

## 局部实现与保真

- WS pump 先解析一次 envelope，cooldown、turn ownership、observation view 和 terminal helper 复用该 map。RawMessage 字段引用这份 envelope 自有的不可变副本，不再复制第二份大 body；没有手写 JSON 扫描器或自制 AST。
- HTTP cooldown 同样复用一次 envelope 给 identity 和 rate-limit 判断。**所有普通事件仍参与 response ID 绑定**；只在 exact `type=error/response.failed` 时解析额外错误详情，不能让后续 foreign failure 污染 cooldown。
- 精确大小写、top/nested ID 矛盾、stale/completed IDs、anonymous legacy error 和原有状态选择继续保留。非终态中的 status=429/error 字段不被当作失败。
- observation view 只在确有大小写 alias 时浅复制顶层 map；nested response 另行解码。共享 map、原始 payload 和 failure response 保持不变，日志视图不能反过来改变 terminal 语义。canonical event 不复制 view map。

独立 review 指出初版 view 会修改共享 map，已在冻结前改为条件复制并新增不变性测试。另一次 1 MiB 真 WS 测试最初被测试客户端默认 32 KiB 上限拒绝，已在 fixture 显式使用 maxBody；这不是生产缺陷，未为此改生产限额。

新增五项测试涵盖：实际双 WS 传递 1 MiB done 原字节及 lane 释放、HTTP 普通 delta 建立归属后忽略 foreign 429、invalid JSON/UTF-8、共享 map/原始 error 不变、非终态毒 status/error。相关 WS/raw SSE/cooldown 回归 **47 个顶级通过、0 失败、2 个环境跳过**；含子项 136 通过、3 跳过，10.918 秒。日志 `green-associated.jsonl` 在同证据目录。Q12 另做合流前后 20-payload 状态等价矩阵与实际 WS 原字节比较，证据由其独立记录；Linux 容器基准由主任务安排，不由本地数字代替。

## 冻结与重测

旧三文件副本留在上述证据目录；新生产文件只改 `gateway_ws_event.go`、`gateway_websocket.go`、`gateway_cooldown.go`。测试为 `gateway_research_parse_performance_test.go`。生产基线没有 `observeFrame`，重测旧版时应在隔离副本中将 benchmark 的新入口换回旧 pump 的两次调用，其余 setup/payload 不变，不能在工作区覆盖正在集成的代码。

| 文件 | 冻结 SHA-256 |
| --- | --- |
| gateway_ws_event.go | `e57e8be2b84ac2bb6d321fcb42a9ccfb4fa3697962a973328e4ee30b8c595a4d` |
| gateway_websocket.go | `1681184d0dc7984f57456d95448c69e6f816eeeb572d76646cf6169d908484e8` |
| gateway_cooldown.go | `eb84c2cfc3b49bdaf34052612fe7aa787bdbddc7798e59f07f702b2c0a2c6fe7` |
| gateway_research_parse_performance_test.go | `354ee14ce842249710b5292c0777f2b6c2e9e002da7489d3c2403687755a908a` |

本改动降低已确认观察链开销，不更改模型参数、请求内容或答案判据，也不证明原账号质量已恢复。完整 Linux race/native、CLI 与部署验收由主任务完成后另记。
