# 请求正文膨胀修复、多轮状态与模型档位对照

本轮在 `30634195…df95be` 之后，修复一个会拒绝正常代码历史的请求序列化问题，并对原题进行了三个预定的完整官方 CLI 对照。

## 可复现的请求缺陷

旧网关的 JSON 序列化会把 `<`、`>`、`&` 变成六字节 Unicode 转义。小正文语义没有改变，但大量源代码/工具输出因此膨胀，可能突破网关自身的 32 MiB 限额。实际测试的第二轮工具历史约 **6,292,601 字节**，HTTP 在策略开/关两种情况下均返回 413；对应 WS 帧约 6,292,612 字节，开启内置提示词策略后返回 400，上游没有收到第二轮。未开启改写的 WS 则能通过。

修复使用标准 `encoding/json.Encoder` 的 `SetEscapeHTML(false)`，用于独立 API JSON/WS 正文。覆盖 HTTP 请求准备、策略生成的 instructions、字符串 input 转换、system→developer 适配、旧 compact 转换及 compact 返回的保留上下文。保留正常 JSON 引号、反斜杠和控制字符转义，不手工替换已转义文本；缓存摘要算法和大小限制保持原值。

相同实际请求现在 HTTP 200/WS completed，工具历史完整送达。真实 Go/native HTTP、压缩开关、两轮模型与 effort/summary/context/tier/include/text 保真用例，以及 header/state 接力均纳入回归。[详细红绿测试](quality-20260925/inference-controls.md)区分了旧版已正确保留的模型参数和这次真正修复的正文大小错误。

## 完整 CLI 与多轮状态

官方 CLI 0.155.0 → Go 网关 → 原生 Rust helper → 隔离上游，执行实际工具和后续请求，两轮解压正文分别 **36806 → 36806**、**37787 → 37787** 字节。13 个顶层字段没有增加、缺失、语义或字段序列化差异，完整 token tree 一致。顶层对象键顺序可变，未宣称整包字节相同。证据为 `.local/gateway-audit-20260924/go-cli-compression-request-json-20260925/compression-report.json`。

多轮状态专项核对官方 turn-state OnceLock 的用户回合生命周期、HTTP 头与 WS client_metadata 分工，测试工具续接、401 同 owner 恢复、新回合不带旧 state、HTTP 200 failed 不自动重放、WS 两连接及 426→HTTP 回退。没有发现此次覆盖正常路径的新串扰缺陷，因此没有为该部分修改生产代码。[状态对照](quality-20260925/turn-state.md)记录源码与实测范围。

## 原题模型/推理档位实测

在固定 q4 同一 owner/workspace、AT、分配和 OAuth epoch 内，事前登记三个完整官方 CLI HTTP 直连请求，每格一次，原题相同，全新 CLI home，request/stream retries=0，无答案注入。三个参考均 clean completed、目录正常、无可见 reroute 或工具调用：

| 请求模型/effort | 原题最终答案 | 耗时 | reasoning output tokens |
| --- | ---: | ---: | ---: |
| gpt-5.6-sol / medium | 29 | 50.395 秒 | 516 |
| gpt-5.6-sol / xhigh | 29 | 82.329 秒 | 1189 |
| gpt-6-astra / medium | 29 | 52.398 秒 | 661 |

三格都绕过网关。这里改变模型/effort 是预定控制因素，不是修复后的同配置 A/B；不能用 token 数推断集群。首格有 5888 cached input tokens，其余没有，模型目录的 instructions/tool surface 也可能随模型改变。CLI 公共输出不提供 response.model 或 TLS 抓包，因此报告保留相应字段为空，不把 requested 冒充 returned。

这些结果没有支持“单纯提高 effort 或换另一模型即可恢复该账号”的方案，也没有证明隐藏账号标记或低算力集群。原题按既定 21 判据三格均未通过，未宣称恢复。原始最终回答、登记计划、身份摘要和清理核验在 `.local/research-20260925/inference-controls/`；评分汇总为 `review.json`。实验服务已正常退出，临时 AT 文件已实际检查删除，未复制 RT。

## 验收与发布

Windows 全套 **778 顶级通过、0 失败、145 个环境跳过**，39.423 秒，vet 通过。独立官方 CLI **7 顶级通过、0 跳过**，13 个含子项终态，15.826 秒；网络隔离压缩 CLI 独立通过，7.933 秒。Linux race/PG/Redis/native 和上线记录将在完成后追加。

首轮完整 Linux race 有 899 顶级通过、1 顶级失败、22 opt-in 跳过。失败仅为新 6 MiB WS fixture 沿用通用小消息读取器的 5 秒期限，两个 policy 子项均在该期限耗尽；没有删除内容/长度断言。已将该 fixture 的 reply 等待独立设为有界 30 秒，生产设置不变，重新冻结后运行完整验证。原始失败保留在 `go-request-json-20260925-final/`，不被新日志覆盖。

最终 v2 冻结全套通过：**Linux race/PG/Redis/native 900 顶级通过、0 失败、22 opt-in 跳过，2632 个含子项终态，331.574 秒，无 DATA RACE，vet 通过**。新 HTTP native 两轮/压缩、大 markup、HTTP 401 状态与 native WS 接力均实际执行通过；不以顶级 pass 掩盖其子项跳过。最终 Windows 778 通过、0 失败、145 环境跳过，41.561 秒；CLI 独立 7 项通过，18.145 秒；隔离压缩 7.915 秒，两轮 `36810 → 36810`、`37790 → 37790` 且完整内容树与所有字段序列化摘要一致。

最终后端 **`f8edfc3e81e0fda3e03293b10a0dd24741a0cdd8976e698ca82e329d0b9a3db7` 已上线**。进程实际二进制与构建匹配，helper 保持 `f72e5c0f…b556`，稳定环境、账号元数据和前端保持。HTTP models 200/9 个模型，WS 101 且 accept 正确，关闭后 in-flight=0、readiness=ready。运行时 smoke 不发模型推理。

构建、最终原生子项清单、差异包、激活与验收记录在 `.local/research-20260925/request-json-continuation/`。最后一级覆盖为 `zzzzz-request-json-20260925.conf`，只切换后端 ExecStart，激活失败会移除自身覆盖回到旧版；本次没有数据库迁移。用户随后要求同期多账号对照，另开同请求 Q0/q4 ABBA 实验，不把本段单账号三档控制当作多账号因果证据。

复验请求缺陷与多轮状态：

```sh
go test . -run '^TestResearch(Inference|Turn)' -count=1
```
