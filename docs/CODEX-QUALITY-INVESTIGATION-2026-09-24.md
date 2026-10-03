# Codex 网关账号质量调查与修复

更新：网关主体的请求对齐和账号原生状态隔离现已上线，桥交付状态回归也已纠正。新的完整 CLI、直接先行样本、上线 SHA 与清理验证见[续查上线报告](CODEX-QUALITY-ISOLATION-DEPLOYED-2026-09-24.md)。本文保留第一轮候选阶段与实测历史，不能用旧“尚未上线”描述替代当前运行状态。

日期：2026-09-24。目标是实测糖果题的账号差异，区分网关请求改写、客户端/传输指纹、上游错误与模型答案，并修复已确认的代码问题。

## 当前结论

**异常答案已复现，但没有证据可以宣布“账号降质已被修好”，也不能证明“账号被标记后分配到低算力集群”。** 同一异常账号通过网关、绕过网关的原生 Rust HTTP、增加官方路由头、提高到 xhigh 均答错；官方 CLI 直接访问也产生了 29 的最终回答。网关不是产生这类错误的必要条件。官方 CLI 和原生 HTTP 对照不能排除账号已有服务端状态、历史使用或出口影响。

当前工作区未发现偷偷降低 `model`、`reasoning.effort`、`service_tier` 的逻辑。HTTP/WS 实际 loopback 出站断言验证这些字段保留，真实响应也回显请求的 model/effort；这些回显不证明服务端内部用了哪个集群。

## 实测结果

题目使用 `tools/mother_account_benchmark.py` 的原始 `CANDY_PROMPT`。没有在请求里写入 21 或解题策略。数学独立穷举与自适应策略验证最优最坏情况为 21；29 对应忽略可凭手感选择形状的无差别抽取解法。

| 匿名账号 / 模型 | 路径与结果 |
|---|---|
| Q0，gpt-5.6-sol / medium | 现网 21；未打本轮路由补丁的候选 21；关闭内置提示词 21；透明网关 21；原生 HTTP 直连首轮 **29**；显式路由提示 21。 |
| Q0，交换顺序复测 | 直连→透明网关→透明网关→直连，四次均 21。证明首轮直接路径也会偶发错误；样本不足以证明某路径必优。 |
| Q0，本轮路由补丁候选 | 实际模型返回 21，单次上游 attempt，medium 回显。 |
| q1，新工作区账号 / sol / medium | 候选网关 **28**，直连 **29**，路由提示 **29**；反序复测路由提示/直连/候选均 **29**，六次全部错。 |
| q1，sol / xhigh | 直连与路由提示各一次，均 **29**；response.reasoning.effort 回显 xhigh。 |
| q2，sol / medium | 首个直连即 HTTP 429，网关也为 429；后续部分请求被网关冷却挡住。六条记录不计为数学错答。 |
| q1，gpt-6-astra / medium | 直连、网关均 HTTP 429，无有效数学答案。 |
| q1，源 OAuth 的 WARP 出口 | 经相同已有 SOCKS 出口的原生直接与网关探针均遇 HTTP 429；四条记录不足以比较出口质量。 |

Go 探针共 **31 次调用记录：19 次完成，其中 10 次 21、9 次错答；12 次限流**。这里混合了账号、档位和诊断分支，**总正确率不是网关质量估计**。六个同账号/同正文可比较的配对为：两对双方正确、三对双方错误、一对直连错/网关对；仅一个不一致配对，不能据此建立稳定因果关系。预测的出站正文 hash 明确标作预测，没有冒称完整生产 wire 抓包。

### 官方 CLI 独立参考

使用固定官方 Linux CLI 0.155.0，SHA256 `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`。临时目录、内置 `openai` provider、真实 AT、本地身份 claim、空 RT、禁止远端 refresh；原题直接通过 stdin 传入，不经过网关。

- Q0：进程退出 0、turn.completed、无工具，最终 21，input 6859、reasoning output 3070。
- q1：进程退出 0、turn.completed、无工具，最终 29，input 6859、reasoning output 429。
- 两者 prompt/config/CLI hash 相同；CLI 本身带约 6.8K 上下文，不能与短 HTTP 探针宣称同正文。
- 两个旧报告的回合开始前各有两个 error-shaped 启动事件；原始告警文本未保存，因此独立复核状态为 `completed_with_unresolved_startup_events`、`reference_clean=false`。保留最终回答证据，不能排除当时未记录的 reroute 告警。
- 后续新探针保存有限脱敏告警：未稳定 feature 提示、缺少 code-mode host。q1 后续回合真实经历 WS 403 回退及 HTTP 429，没有最终答案。另一个未进入本次网关模型测试的 q3 在官方 CLI 先行步骤也遇请求错误，因而未执行后续网关质量比较。
- 最早两次官方探针在模型调用后因 Python 3.9 缺少 `hashlib.file_digest` 生成报告失败；没有可恢复的答案证据，不能计为模型成功或失败。已改为兼容流式哈希，并在任何请求前预检。

“推理 token 较少”是观测，不足以证明算力集群或账号标记。`CF-Ray` 是边缘请求相关信息，本轮没有可验证的内部集群身份、分配策略或标记字段。

## 已确认并修复的问题

### 1. 补齐适用请求缺失的 CLI 路由提示

官方 `core/src/client.rs:1023` 根据实际 model/tier 生成 `x-codex-routing-hint`。自定义 provider 经过网关时通常不生成，而网关原先仅透传。

新增 `gateway_routing_hint.go`：仅标准 `https://chatgpt.com[:443]/backend-api/codex`、已选 AT/account_id、`/responses` 请求，在缺失时按 prepared body 补头。显式头和未知扩展原样保留；不新增 400、不修改 model/effort/tier，不给 Guardian/models/旧 compact 盲加。WS 首帧后建立上游连接时可生成；已有 session 导致提前握手、尚无正文时不猜测 model，继续保持原 turn-state/preconnect 契约。

该补丁已通过合成 HTTP/WS 出站测试、相关回归、Linux race + 实际原生 helper 回归，并在 Q0 真实请求验证 21。**q1 的显式提示头对照仍为 29，不能声称此补丁修好了它的质量。网关主体新二进制尚未上线。**

### 2. 推送桥不再把已确认失效凭据报正常

旧 `deploy/loophub_codex_bridge.py` 的 active 判断仅依赖映射、启用、分组、旧有效期。现在读取目的端当前有效期和凭据作用域的非 stale 上游快照；仅可信确证 401/撤销/过期才返回 `needs_reauth=true`，不把 429、容量、网络错误或任意文案当凭据失效。快照与账号读取之间版本变化时拒绝用旧 401 触发新凭据刷新。

此独立桥修复**已部署**，SHA256 `c880b9e4cbed0b669b0899f80b74fa028c8be84b766d6d89555a023ed28bfce8`。部署前在服务器用真实管理 API 只读验证；部署后现网账号仍 active、needs_reauth=false。仅重启桥服务，未重启 28765、未替换网关主程序。修复恢复信号不等于改善模型推理。

仍有限制：普通推理 401 不一定持久化为该凭据的管理快照；桥不能仅凭本地 hash 证明带同标签账号未被外部换 AT。这些没有伪称已解决。

### 3. 修复评测本身的误判

- 新评分工具区分完成错答、429、鉴权、超时、不完整和歧义答案；空 terminal output 可从完成项恢复，不拿 reasoning 中出现的 21 代替最终回答。
- 官方探针旧实现整段查找 `429`，把 `reasoning_output_tokens:429` 误报成限流。该旧指标已作废；新实现只从错误事件/诊断来源判断，并分 startup/turn/after-turn。独立真实 HTTP 429 不受此 bug 影响。
- 识别 CLI 将 config/deprecation 警告映射为 `item.completed/error`，不把回合前警告自动当模型回合失败；回合错误、reroute、工具污染仍单列。
- 新探针后续记录加入精确 `X-Client-Request-Id` 关联计量；旧记录的 `gateway_attempt_count` 可能来自上一条 LastFor，不用它证明本地冷却次数。

## 运行时与恢复

本轮开始时，现网网关 SHA256 为 `43e1af500093ae649d71e4cfd76e95b4e791eab5968af5b11d93d5ab7ae375da`，原生 helper 为 `aeb13c0efacf37aa427127e6538e1f83ca6c130e6e27a8c3af69dc1b388cfd4a`；HTTP/WS native、cli_request 已开启。Linux 指纹、prepend 内置提示词、缓存键和兼容转换开启；唯一现网账号直连、AT-only。新候选使用前轮已验证的 helper `f72e5c0ff8798c02f430f7aa36f74009dad40fade6111dad722390891239b556`。

28765 推送配置实际执行了临时地点追加：选定 workspace/user/child assignment allowlist，仅 AT 进入 0700/0600 私有接收目录，不复制 RT、不主动 OAuth、不改变席位/轮转策略。既有源调度仍独立运行；第一次准备至重新准备期间 q1 自然发生过轮转，因此实验以第二份锁定身份为准。正式 q1/q2 实验窗口前后身份和 OAuth 更新时间未变，源/接收 AT hash 一致。

服务器既有 NAT 将本机私网连接源改成主机 IP；临时接收器默认拒绝，随后加入本次唯一 exact IP allowlist，仍要求专属 key，没有扩大到任意公网/CIDR。

**所有临时推送地点已解绑并删除，既有地点保留，receiver 已停止，q0 及接收器 AT 副本已删除。** 恢复过程有独立 receipt，未以关闭进程代替解绑验证。

## 验证与交付

- Go 完整回归通过（34.927 秒），`go vet` 通过。
- Linux 相关 race/native 回归 45 顶级通过、0 失败，3 个依赖未配置的 opt-in 跳过；真实 Go/Python 管理契约本机 5 场景通过。
- 桥状态 13 个 Python 回归；receiver 17 个；评分/接收器/官方探针最终合计 85 个 Python 回归通过。所有运行结果区分合成、实际 CLI、真实上游。
- 分析文件：[请求/路由与桥](quality-20260924/01-request-analysis.md)、[推送映射](quality-20260924/02-push-analysis.md)、[数学及配对实验](quality-20260924/03-experiment.md)、[官方 CLI](quality-20260924/04-cli-reference.md)。
- 实测与恢复证据：`.local/quality-20260924/`；主要文件 `final-probe-report.json`、`paired-report.json`、各原始 JSONL、官方 `report.json`/独立复核摘要、`post-deploy-check.json`、`credential-cleanup.json`。

离线复评不会再次调用模型：

```powershell
python tools/quality_probe_report.py .local/quality-20260924/baseline-q0.jsonl .local/quality-20260924/repeat-q0.jsonl .local/quality-20260924/paired-q1.jsonl .local/quality-20260924/xhigh-q1.jsonl --output .local/quality-20260924/review.json
```

当前未完成的是异常账号质量恢复及内部路由因果证实。进一步验证需要仍可请求的同身份窗口、明确的上游错误/额度恢复，以及服务端可观测信息；不能靠改答案、重试到正确或把其他账号正确结果覆盖异常结果来宣称完成。
