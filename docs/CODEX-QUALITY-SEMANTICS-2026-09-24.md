# 同账号原题配对与语义对照

2026-09-24。本轮进一步检查已有异常样本 q4。**原题经原生直连与透明网关均答 29；仅明确可选择形状后答 21，完全盲抽控制答 29。原题恢复仍未得到证明。** 已上线的传输、账号隔离和运行环境修复保持有效；这组结果没有提供新的生产代码缺陷证据。

本轮为前述 [20 项网关审计](CODEX-GATEWAY-AUDIT-2026-09-24.md)、[账号隔离修复](CODEX-QUALITY-ISOLATION-DEPLOYED-2026-09-24.md)及 [运行时 TLS 修正](CODEX-QUALITY-RUNTIME-TLS-2026-09-24.md)的追加调查，不覆盖历史样本。固定参考版本仍为 Codex CLI `rust-v0.155.0`。

## 固定实验与实际结果

使用 q4 原有 active child，同一 user/workspace、AT 摘要、分配时间及 OAuth 更新时间；每次调用前后重新核对。只复制 AT 至服务器临时 0600 文件，不复制 RT、不触发刷新、不修改 28765 推送地点。实验开始前固定四次调用及顺序；每端一次、新会话、单并发，遇非完整响应即停止，不按答案重试。

四次均使用 `gpt-5.6-sol`、`medium`、相同解答 instructions。原题完全保留；变体仅替换括号内的形状辨别能力描述，未加入答案、形状数量分配或解题方法。离线答案键和数学证明未加入请求。

| 请求 | 结果 | 最终答案 | 请求耗时 | 解释 |
|---|---|---:|---:|---|
| 原题，原生 helper 直连 | HTTP 200 / completed | 29 | 16.035 秒 | 原题不通过 |
| 原题，透明网关 | HTTP 200 / completed | 29 | 33.901 秒 | 原题不通过 |
| 明确取出前可选择形状，透明网关 | HTTP 200 / completed | 21 | 98.780 秒 | 诊断控制答对，不计原题恢复 |
| 明确取出前完全盲抽，透明网关 | HTTP 200 / completed | 29 | 27.035 秒 | 不同任务的负控制答对 |

表中耗时来自探针实际 HTTP 调用，未计入前后身份查询；执行器耗时更长，两者不混用。全部请求完整结束，没有触发停止条件，也没有第五次模型调用。

两个原题的本地序列化请求摘要及预测上游正文摘要均为 `763f3380b4807815c1dab11c2baf7586e4466e3bcc4a4e668c658f0be69df7f5`。这补足了旧 q4 direct/candidate 样本正文策略不同的问题。透明分支关闭提示词注入、指纹 profile、prompt cache key 与兼容正文归一化；它仍经过网关鉴权、必要的头处理和路由提示，不能叫作所有 header 相同的纯 TLS A/B。

`predicted_upstream_body_sha256` 仍只是预测：本轮未捕获真实 Go→Rust IPC 正文或 TLS wire 明文，报告没有把它改称 sent/wire。请求输入证据由固定配置构造，再与 harness 序列化摘要核对；输入证据文件也明确说明这个边界。

## TLS 运行环境与元数据证据

每次探针用显式环境白名单启动，包含固定 PATH、LANG、私有配置路径、helper 路径和 `cli_request` 生命周期；不继承生产服务环境，不设置 CA 覆盖或代理变量。逐次环境及 helper/probe 摘要保存于 `*-process.json`。原生 helper 与当前生产产物相同，SHA256 为 `f72e5c0ff8798c02f430f7aa36f74009dad40fade6111dad722390891239b556`。

历史 q4 transient unit 已被系统回收，查询得到 `LoadState=not-found`。没有历史 `/proc` 环境就不能宣称已经验证旧进程 TLS 选项；本次显式环境记录只证明新样本。

新增的 test-only 采集器读取 HTTP 头及 SSE `response.headers`、`response` 字段，复用生产解析规则；保存有界模型信号和请求 ID 哈希，不保存认证、cookie 或任意元数据，也不改变 native 路由。四次实际观测均为：

- 可见 response model 为 `gpt-5.6-sol`，reasoning effort 为 `medium`，没有捕获到其他模型名；本轮没有收到独立 `openai-model` 值，不能把缺失视为额外一致证据。
- `service_tier` 先后观测到 `auto` 和 `default`；这是可见值变化，不是后台换档或服务端矛盾的证据。schema 1 没有保存对应事件阶段，不能从旧记录反向补出具体 created/completed 归属。
- 无 response identity 混合、无采集截断；请求 ID 仅保存归一化 SHA-256，不能反解为工单所需原始 ID。

本轮探针使用 schema 1，旧 `conflicts.service_tier=true` 仅表示出现过不同值。发现命名容易误导后，未来采集器升级 schema 2，分开记录 `observed_value_changes` 与 `same_event_source_differences`，附事件类型、阶段和首次事件序号；四项专项与关联测试通过。既有探针二进制、schema 1 原始结果及其 hash 均未覆盖，没有为升级采集器重发模型请求。

## 能支持和不能支持的结论

独立穷举全部 201,600 个糖果组成，以及固定取法和自适应选形状策略，确认可选择形状时最小保证值为 21，完全盲抽时为 29。数学证明只在本地评分端使用，详见 [Q10](quality-20260924/10-semantics-control.md)。

新结果支持：该账号本次能根据形状选择能力的明确描述改变解法；两个原题响应都采用了盲抽的最大失败集合推理。这使题意理解成为有实测支持的影响因素。它不证明原题无歧义地答对、不证明正常与异常账号等价、不估计稳定通过率，也不排除未观测的账号或服务端状态。

相同 model/effort/tier 回显、相同模型目录、reasoning token 数及时间差，都不能独立确定真实模型构建、执行预算、风险标记或后台集群。要验证“账号标记导致弱集群”的因果，仍需要上游路由或执行记录；不能用改 UA、cookie、cipher 或改题面后的 21 代替这些证据。[Q11](quality-20260924/11-completion-audit.md)给出逐项完成判定。

## 清理、现网状态与复验

四次完成后 AT 快照已删除。最终只读检查：readiness 为 ready，生产账号数 1、in-flight 为 0；三处旧 receiver 的凭据文件均为 0，CA 覆盖变量均不存在。网关 SHA256 仍为 `84ca77af5c1d9a49d293180f4ccf38396df2e8ed3a1108d73df0c868315985f0`，本轮没有替换生产二进制或重启服务。

主要证据为 `.local/quality-20260924/semantics-live-q4/` 内的计划、执行回执、四条原始结果、输入摘要与前后身份记录；另有 `semantics-final-runtime.json`、`q4-historical-unit.json` 和 `probe-semantics-build.json`。实验探针 SHA256 为 `b1e41d737303000da5f46ad09293474c96d3e4d5577967eaf8138436466bbd42`，与后续 schema 2 源码明确区分。

本轮新增的是诊断工具与证据，没有凭推测改生产策略。原题质量目标仍未完成；当前明确修复和此前 836 项完整 Linux 回归通过，不等于所有账号质量保证或所有平台逐字节对齐。

离线变体和汇总器共 **45 项测试通过**；Go 元数据采集及关联观测/错误测试通过。以下命令仅重新核验保存的证据和评分，不发送模型请求：

```powershell
python tools/quality_semantics_report.py .local/quality-20260924/semantics-live-q4 --output .local/quality-20260924/semantics-live-q4-report.json
```

当前汇总结果为 `integrity_valid=true`、`execution_state=completed`、四次调用；原题与控制结果分别统计。完整结果见[离线汇总](../.local/quality-20260924/semantics-live-q4-report.json)。
