# 请求环境一致性复核与路由字段读取优化

2026-09-27。本轮实际修改的是路由提示生成前的字段读取：保留原来的模型、service_tier 和错误判定，去掉对整份请求的临时复制。HTTP/WSS、turn-state、压缩及身份头另行核对，未将没有复现的问题包装成修复。**当前候选尚未部署，账号质量差异的根因仍未确定。**

## 已完成的性能修改

`gatewayPreparedRoutingHint` 原先把整个请求反序列化成 `map[string]json.RawMessage`，虽然只需要 `model` 和 `service_tier`，也会复制完整 input、工具内容和加密历史。新实现先用 `json.Valid` 校验完整原文，再复用现有字段位置扫描器，仅解码两个控制字段。没有修改请求正文、路由提示内容、身份、档位或推理预算。

保留精确小写字段、转义字段名、重复字段最后值生效、null、非法类型以及完整 JSON 校验语义。未知字段不会成为控制字段；非 JSON 空白、损坏的未知嵌套数据和超深 JSON 不会因为只读取两个字段而被接受。

本机 Windows、Go 1.26.7，两种内容形态分别运行五轮，表内为中位数。旧实现作为独立参考函数与候选在同一 benchmark 二进制中执行。

| 请求内容 | 实际字节数 | 原耗时 | 新耗时 | 原累计分配/次 | 新累计分配/次 |
|---|---:|---:|---:|---:|---:|
| 小型 opaque input | 1,117 | 4.387 μs | 2.009 μs | 2,224 B | 336 B |
| 单个大段加密内容 | 4,194,397 | 12.018 ms | 6.146 ms | 4,203,576 B | 385 B |
| 单个大段加密内容 | 33,554,013 | 88.457 ms | 43.928 ms | 33,555,512 B | 724 B |
| 大量小工具结果、嵌套及转义 | 4,194,351 | 15.135 ms | 9.489 ms | 4,195,448 B | 417 B |
| 大量小工具结果、嵌套及转义 | 33,553,877 | 119.062 ms | 76.307 ms | 33,555,576 B | 1,006 B |

这些数字仅涵盖路由字段读取步骤，不是端到端网关延迟、峰值 RSS、TLS 性能或模型推理耗时。大输入仍需要完整扫描，未声称常数时间。极短测量中的池初始化和 GC 会影响小额分配，原始五轮数据全部保留于 [性能记录](../.local/research-20260927/request-environment/routing-performance.json)。

## 本轮一致性结论

| 边界 | 已核对的行为 | 仍存在的区别或证据限制 |
|---|---|---|
| HTTP TLS 源码 | 30个 vendored Rust 文件与固定 CLI 0.155.0 逐字节一致；WS适配的第31个文件符合记录的补丁；265个依赖锁版本、checksum/fork核验通过 | Go清单不包含完整Rust源码，另行核对了43个关键文件与此前 helper 构建源；未重新运行Rust或采集线上TLS |
| HTTP/WSS路由选择 | 网关使用管理员选定的明确代理或直连；保留请求准入时的代理快照 | 默认CLI可使用transport/system/env代理策略。WS默认分支与网关explicit-route分支的地址选择、连接尝试不是同一路径；同源TLS配置不证明TCP指纹完全等价 |
| TLS回退 | HTTP默认/native及custom CA路径、WSS配置来源逐项核查 | 默认Responses没有自动启用所有route-aware native→rustls回退；特定调用显式启用才适用。无效自定义CA的CLI legacy回退与helper报错是已说明的设计差异 |
| 连接与Cookie | HTTP cli_request/pooled生命周期、账号隔离和基础设施Cookie规则均核查 | 多租户网关的账号隔离不等于单账号CLI进程；连接复用、恢复会话和最终出口仍需真实运行证据，不能由首次ClientHello推出 |
| turn-state | WS流内 `response.metadata` 原样转发；HTTP 401重试保持原请求状态；同owner换token不偷换已建立socket的握手凭据 | 固定CLI core建立WS时向底层传入turn-state `None`，不会靠101头填其回合OnceLock。无显式session的兼容连接无法追加已发出的101头 |
| metadata | 真实TCP验证显式/延迟握手×缓存scope开/关；正文完整tool inventory与兼容头不会互相覆盖，未知大小写字段、大整数和转义保留 | CLI本身有意让兼容头省略无界工具inventory，因此正文与头不应被强制做成相同内容 |
| 请求压缩与恢复 | identity/zstd/拼接frame/可跳过frame产生相同prepared body；一次合成401恢复保持相同重发wire；坏尾帧与重复编码头在派发前拒绝 | native压缩与固定CLI所用源码一致，本轮Go IPC仅验证交接正文及能力标记；没有执行真实Rust压缩。Go fallback的zstd字节不同本身不证明质量问题 |
| 应用身份与attestation | 现有UA/originator重写会清除旧opaque attestation；原生客户端头的透传规则保留 | `AttestationContext.thread_id`用于选择可响应的连接，app-server实际发送空 `AttestationGenerateParams {}`；不能据此认定缓存线程命名空间必然破坏签名。证明有效性、服务端风险判定仍未知 |

详见 [turn-state实际调用及TCP验证](../.local/research-20260927/request-environment/turn-review/REVIEW.md)、[压缩与重试验证](../.local/research-20260927/request-environment/COMPRESSION-REVIEW.md)、[TLS源码核验](../.local/research-20260927/request-environment/tls-review/verification-final.json)。本轮没有新增的模型对照，历史账号21／29差异继续保持原结论，不由这些本地验证推断低算力集群或恢复。

## 验证与候选

差分 fuzz 执行 **243,281** 次，零失败。完整 Windows 测试 **3173 PASS、382 SKIP、0 FAIL**，vet通过。新增字段边界及大请求测试、已有实际HTTP/WS路由头回归均通过；独立审查另行验证控制字段语义。未发现缺陷的压缩和turn-state新用例保留在独立审核目录，不扩大生产改动。

408文件候选只有1个生产文件修改和1个新增测试文件，基于上一轮407文件已验证候选：

- 源码：`afaf23b15bd0f859323498693dbf2200684b31dd83e0b7526e3ec771bb4ae673`
- 归档：`e977dad166b37d89de0f8f68518706165ae5b214e6c2a57e2ae58776284e5be8`
- 完整测试日志：`c4ad61163ecd15690d117747e0d45914474fd7c04178c7558fbdb311bc076cc8`
- Windows产物：`4348c1144a17ed33b6200a4ef3ff052c0ce32417052b8d38bdffd6ee5fa5b1d0`
- Linux交叉编译：`8d2b106df45e51c0ab11c3a77748eae2b9dd2cb5a41701b78a3ee6642a66ca08`

[候选清单](../.local/research-20260927/request-environment/routing-candidate-manifest.json) · [完整测试与编译](../.local/research-20260927/request-environment/local-build/build-summary.json) · [汇总](../.local/research-20260927/request-environment/summary.json)

在工作区复跑行为与两类基准：

```powershell
go test -count=1 -run '^(TestRoutingHintAllocation|TestQualityQ01RoutingHint)' -bench '^BenchmarkRoutingHint(OpaqueRequest|ManyItems)$' -benchtime=250ms .
```

本轮未执行SSH、真实OAuth、模型请求或部署，也没有重试先前被平台拒绝的启动。Linux/race、实际Rust及PostgreSQL/Redis合并运行验证仍缺；交叉编译不能代替这些验证。最后保存的线上版本观察仍为2026-09-25T21:22:21Z的 `6ecdf93f…`，本轮没有重读它。
