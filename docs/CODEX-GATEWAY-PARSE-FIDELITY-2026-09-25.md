# 流事件解析优化与完整正文保真

本次从已部署的 `33f4535f…cdbe5e` 继续修改，范围为三个 Go 生产文件：`gateway_websocket.go`、`gateway_ws_event.go`、`gateway_cooldown.go`。固定参考仍是 Codex CLI 0.155.0；没有改变原生 HTTP/WSS TLS 实现。

## 实际改动

WS 上游 pump 原先分别为冷却、响应归属、统计和终态判断重复解析完整事件。大工具参数也随 envelope 解析重复复制。现在 pump 调用一个入口，复用一次解析的 envelope；RawMessage 字段只读复用，非失败事件不再解析限流详情。HTTP 的冷却观察器同样复用归属检查已经解析的 envelope。

统计所用的大小写过滤视图只有在确实遇到别名时才复制 map。原始字段保持只读，终态分类仍检查原始对象，转发仍发送原始 payload。大小写精确匹配、foreign/stale response 过滤、具名 lane、限流作用范围及准入释放维持现有约定。这是观察和状态处理开销的优化，不是模型推理速度或质量的测量。

## 正文是否被改坏

真实官方 CLI 经网关、Rust helper 到隔离 HTTP 上游，执行实际工具及后续请求。初次采样两轮正文分别 `36806 → 37106`、`37787 → 38087` 字节，均增加 300 字节。两轮各有 60 个 `<`、`>`、`&` 被写成 JSON Unicode 转义，每个增加 5 字节，完整解释本次样本的增长。

13 个顶层字段没有增加、删除或语义变化；只有 `input`、`instructions`、`tools` 的序列化哈希变化。证据是 `.local/gateway-audit-20260924/go-cli-compression-fidelity-20260925/compression-report.json`。该初次采样使用 UseNumber/map 语义摘要；最终验证进一步采用保留重复成员次数、顺序以及精确数字的完整 token tree 摘要，避免 map 比较掩盖不透明扩展字段损坏。

另有包含工具调用、加密历史、大整数、`1e1000`、重复嵌套成员、Unicode 和未来扩展的 HTTP/native 请求保真用例，以及六类显式适配的允许字段验证。详见 [正文保真方法](quality-20260925/body-fidelity.md)。这些采样不能倒推上次真实账号请求 `36243 → 36636` 的 393 字节差值，因为那次没有保存两端完整正文。

## 验收记录

最终网络隔离 CLI 重跑通过，两轮 `36809 → 37109`、`37790 → 38090` 字节，完整内容树各自保持一致：`e084bf8b…854f411f`、`a04e861b…f3dfd696`。两轮 `content_tree_preserved=true`，没有字段语义变化。最终证据是 `.local/gateway-audit-20260924/go-cli-compression-parse-fidelity-20260925/compression-report.json`。

Windows 全套 769 顶级通过、0 失败、145 环境跳过，38.772 秒，vet 通过。Linux race/PG/Redis/native 全套 **891 顶级通过、0 失败、22 opt-in 跳过**，2609 个含子测试终态，291.513 秒，未发现 DATA RACE，vet 通过。官方 CLI 独立 7 顶级通过、13 个含子项终态，无跳过，15.619 秒；压缩 CLI 独立通过。四批源码清单逐文件一致，构建前后再次核对。

同一个隔离 Linux 容器、同一镜像和资源限制下，先编译两版再各测五次，得到中位数：

| 观察链 | 旧版 → 新版 | 耗时下降 | 分配字节下降 |
| --- | --- | --- | --- |
| SSE token | 19.704 → 15.313 µs | 22.3% | 18.4% |
| WS token | 26.772 → 14.145 µs | 47.2% | 39.8% |
| WS 1 MiB 工具事件 | 39.917 → 13.273 ms | 66.7% | 60.3% |

这测量的是单个事件的观察处理及累计分配，不是模型响应速度、端到端吞吐或常驻内存。原始日志、源文件哈希和重跑方式见 [Linux 对照](quality-20260925/parse-performance-linux.md)及 [Windows 性能专项](quality-20260925/parse-performance.md)。

二进制 **`30634195e375a51fe3372bac14fdf5f86759fa148ef444afbd3bd534dadf95be` 已上线**，`/proc/<pid>/exe` 实测与构建清单一致。原生 helper、进程稳定环境、前端参数及账号元数据保持原值。实际 HTTP models 返回 200/9 个模型，WS 握手返回 101 且 accept 校验通过，正常关闭后 in-flight 为 0、readiness 为 ready。该运行时 smoke 仅包含目录和 WS 握手，不包含模型推理。

构建、补丁、冻结清单及部署验收集中于 `.local/research-20260925/parse-fidelity-continuation/`：`build-manifest.json`、`production.patch`、`activation-result.json`、`runtime-smoke.json`。四种 native 正文组合（压缩开/关 × compatibility 开/关）全部通过，单列在 `body-native-results.json`。

当前 systemd 最后一级 ExecStart 覆盖为 `/etc/systemd/system/codex-agent.service.d/zzzz-parse-fidelity-20260925.conf`，只切换后端路径；激活脚本在验证失败时移除自身覆盖并恢复旧版。没有数据库迁移或账号调整。

本次没有新的真实模型推理样本；没有将合成上游答复计作原糖果题恢复，也没有从正文保真或性能改善推断隐藏集群分配。

可复跑观察链基准：

```sh
go test -run '^$' -bench '^BenchmarkResearchParseProductionObservers$' -benchmem -benchtime=300ms -count=5
```
