# 账号原生状态隔离上线与质量调查续查

后续更新：现网旧部署还自动设置了 `SSL_CERT_FILE`，实际使 HTTP 走 rustls。该运行环境偏差已修复并完成真实请求验收；三个账号的实时模型目录也已比较，见[运行时 TLS 续查](CODEX-QUALITY-RUNTIME-TLS-2026-09-24.md)。本文件保留账号隔离阶段的原始记录。

2026-09-24 续查结果：**网关主体与推送桥修复已上线，跨账号 HTTP 基础设施 cookie 串用已消除；异常账号的数学正确性恢复仍未证实。** 本文更新上一份[质量调查报告](CODEX-QUALITY-INVESTIGATION-2026-09-24.md)的候选/部署状态，保留其原始实验结果。

## 实际修复的跨账号边界

固定 CLI 0.155 官方库使用进程级 Cloudflare / `__oailb` cookie 容器。旧网关按 proxy、origin、timeout 复用 helper，没有账号维度，因而 `cli_request` 虽新建 HTTP client，却不清空进程 cookie。

本轮通过临时 CA、固定 hostname 的 loopback CONNECT 和真实 Rust helper，复现 A→B→A、同 workspace 不同 user、AT 变化时 HTTP cookie 沿用；WS 不自动读取该 HTTP cookie jar。两种 lifecycle 均验证，原始证据位于 `.local/gateway-audit-20260924/go-quality-cookie-proof/`。

现在 `gatewayNativeKey.accountScope` 保存不可逆的身份摘要：

- 由已选择的 `Account` 注入可信 owner scope，包含管理记录、workspace、user 与 owner revision；已知同一 owner 正常换 AT/RT 仍复用其自己的状态。
- 未知 owner 沿用现有凭据限定归属规则，凭据变化即隔离。
- 没有可信 Account context 的调用，使用实际 Authorization/workspace 头摘要作保守分区，不从未经验证的 JWT 猜稳定身份。
- 无认证请求保持匿名 scope。HTTP、WS、每次 401 重放、OAuth refresh 及管理请求走同一隔离边界。

未修改官方 Rust/vendor、cipher/ALPN、账号代理、模型或并发额度。修复后 6 个顶级、8 个真实 native 子场景及相关单位测试通过；同管理记录/同 workspace、仅 user 不同也被隔离。测试验证了同 owner refresh 连续性、不同 owner 空 cookie、revision 换进程及 HTTP/WS scope 一致。

每个活跃 owner/route 现在可能需要独立 helper。小样本两账号/四账号 summed RSS 约 18/36 MiB，匿名页约 2.5/5–5.6 MiB，线程 8/16；文件映射页可能共享，不能把 summed RSS 当成独占增量。原有空闲 helper 上限 16、账号准入和清理仍保留，未偷偷增加全局 32 请求限制。

**这证明多账号状态耦合已修复，不证明这个 cookie 代表“弱集群”或导致本次 q1 错答。** 之前的 q1 单账号隔离探针本来也答错，不能用该修复抹去反证。

## 推送桥交互回归与纠正

上一轮桥将一般推理 error 或可选快照不可读报告为 unavailable。独立读取 28765 真实健康检查函数发现，这种状态会触发重新推送；`needs_reauth=false` 并不足以阻止该路径。

已纠正为：交付状态只依据 enabled、初始化、当前有效期、分组及确证认证失败。容量、403/429/502/503、网络错误、可选观测不可用不改变已交付凭据的 ready。真实当前 401/撤销/过期继续反馈 needs_reauth，保留 stale/version/owner 检查。

15 个桥回归、6 个真实源函数 AST 契约及 5 个 Go/Python 管理接口场景通过。独立桥已更新为 SHA256 `1cecef42c03a1379d9d03e4c62374055c07ffaf480d2bc458e8d3443c566fae1`。部署后账号仍 active / needs_reauth=false，没有替换 28765 应用或改变原目的地绑定。

## 完整官方 CLI 与新的质量样本

上轮把 CLI 复制成单文件，缺失 `codex-code-mode-host`。源码确认 `model_info.tool_mode` 可要求 `code_mode_only`，仅关闭 feature 不能证明不需要 host。现已固定 SHA-512 下载完整官方 0.155.0 平台包，包含 44 个文件；CLI 摘要保持不变，host SHA256 为 `32da418fe0481054d85909d0201636764c09ec147adb1d58297f2bfb78262096`。模型目录、内置 provider 和题面均未修改。

探针预检完整 bundle、系统配置污染、真实 AT 期限/身份；抑制已知实验配置提示，遇可见真实错误即停止，不继续重试无效参考。未知工具事件会废弃；官方 JSONL 可省略部分内部项目，所以 `tool_observed=false` 不声称协议绝对无工具。所有工具与配置边界见[Q07](quality-20260924/07-reference-fidelity.md)。

新增账号 q4 按**测试前最低已观测周用量**选择，约 3%，未按答案挑选；没有改其目的地、触发 OAuth 或新建身份：

1. 在首次进入本次网关质量路径之前，完整官方 CLI 直接请求。零启动告警，模型目录版本/身份匹配，但返回 `Selected model is at capacity. Please try a different model.`，没有答案。固定 CLI 源码将该消息对应 `ServerOverloaded`，不是已证实的账号配额耗尽。
2. 随后独立 Rust HTTP 直接请求返回 HTTP 200、最终 **29**；此时尚未经过本次候选网关。
3. 再经新隔离候选网关，HTTP 200、最终 **29**。两条均原题，周用量响应约 3%，未注入答案。

q1 使用同身份、同 AT 补采一次直接请求，HTTP 429，并首次取得 `Retry-After:43`。该值只证明该响应给出的最早重试建议，不保证届时恢复。期限后完整官方 CLI 的参考又遇 WS 403，在首个可见错误停止，零启动告警、无最终答案。该结果与数学错答分开记录。

累计普通探针/上线糖果验收共 **35 条调用记录：22 条完成（11 条 21、11 条错答），13 条 429**。混合账号/路径/档位的总正确率不代表网关性能。官方 CLI 的失败事件也不能当作独立数学样本或独立请求数。保持原始失败记录，没有重试到正确才选样本。

这些结果进一步排除了“只有经过本网关才会答错”的解释，但不能排除账号历史、其他客户端、出口、模型随机性或上游服务内部策略。当前没有内部集群标识/分配日志，可以验证的仍是请求和结果。

## 上线与验证

本轮包括上一轮已验证的网关修复、路由提示补齐及账号 scope 隔离：

| 项目 | 当前证据 |
|---|---|
| 网关运行 SHA256 | `84ca77af5c1d9a49d293180f4ccf38396df2e8ed3a1108d73df0c868315985f0` |
| 原生 helper SHA256 | `f72e5c0ff8798c02f430f7aa36f74009dad40fade6111dad722390891239b556` |
| Windows Go | 全量通过，35.356 秒；vet 通过 |
| Linux 全量 race / PG / Redis / native | **836 顶级通过、0 失败、21 opt-in 跳过；2365 个含子测试终态**，264.225 秒，vet 通过 |
| 真实状态隔离 | 修复前串用 3 顶级通过；修复后 6 顶级通过、零跳过，5.644 秒 |
| 上线健康 | `/health/ready=ready`，当前账号数 1、在途 0，桥 active |
| 状态一致 | 切换前后账号元数据摘要均 `9c2bcc007e5b76515885ce9d75a5d37da333e2f3e3ec1e7ebb8d385761653324` |
| 真实 HTTP | 上线后原糖果题 `gpt-5.6-sol/medium` 返回 21 |
| 真实 WS | 上线后 101，同连接两轮均成功 |
| 前端 / 数据 | 保留现有 UI 路径；没有执行 schema migration；没有替换账号、分组或代理配置 |

二进制来自冻结生产源码；最终工作区与 Linux 全量快照逐文件相符。Go/Python 桥契约、真实 CLI 和 live 探针因依赖条件在不同批次执行，不把 skip 计为通过。

版本路径：`/opt/codex-agent/quality-releases/<网关SHA>/`。切换采用专属 systemd drop-in 与仅 native 配置的补充 env file，保留原服务文件，启动失败会自动撤销本次覆盖并恢复旧版本；没有执行数据库回滚。部署原始记录位于 `.local/quality-20260924/activation-before.json`、`activation-result.json`。

## 清理与下一步边界

之前三次临时 receiver 均为 restored，原推送地点保留，无残留临时地点。本轮直接读取的 q4、q1 状态探针 AT 副本也已删除；临时 CLI auth/home 由脚本自动清理。最终运行与清理证据为 `.local/quality-20260924/continuation-final-runtime.json`。

目前能交付的是已经上线的明确隔离/协议/桥状态修复，以及进一步收紧的质量证据。**不能将异常账号宣布为“已恢复满血”，也没有证据能够指定或保证进入某个隐藏集群。** 后续应在可请求的同一身份窗口继续做无答案泄漏的配对验证；持续请求错误时停止并依据明确恢复信号重测，不用换账号的正确答案覆盖原账号问题。
