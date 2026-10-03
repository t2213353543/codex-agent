# 近期失败请求与缓存优化

本轮先查线上运行进程和 PostgreSQL 请求明细，再复现修复。原始审计在 `.local/failure-cache-20260925/production-audit.json`，采样为 2026-09-25 北京时间 12:15 起的近 24 小时窗口。

## 现场结果

615 次请求，497 次成功、117 次错误、1 次中断；输入 1,530,563 tokens，缓存 339,072 tokens，加权缓存率 **22.15%**。`usage_events` 与成功 `request_logs` 的输入、缓存总量一致，没有发现统计公式漏计。

| 失败类型 | 数量 | 核查与处理 |
| --- | ---: | --- |
| native transport capacity 503 | 96 | 全部来自旧 benchmark 的两个并发批次。恢复历史 32 并发限制可复现；当前线上及本地已取消该限制，96 并发回归通过，不重复修改。 |
| OAuth token invalidated 401 | 8 | 上游明确作废凭据，来自旧 benchmark；不是缓存失效，未通过换号或重放生成掩盖。 |
| 分组没有可用账号 503 | 5 | 账号选择约束，保留明确错误。 |
| background / user / input.author 被拒绝 400 | 4 | 复现兼容分支并修复可等价转换的形态；日志未保存原请求，无法断言两个 background 请求具体是 true 还是 false。 |
| 不支持的 gpt-5.4 / gpt-6 | 2 | 保留真实型号与上游拒绝，不静默替换成另一模型。 |
| compact 404 | 2 | 旧 benchmark 的上游能力错误；不将当前路径回归通过说成已重现历史请求。 |
| 上游流中断 | 1 | 保留中断状态，不自动重放未知是否已执行的请求。 |

缓存进一步分桶：52 条输入至少 4,096 tokens 的成功请求共输入 1,462,126 tokens，缓存率 **23.02%**；排除明确的 mother benchmark/WS probe 后为 **23.80%**。短请求只占 2.8% 输入，不能单独解释低缓存率。历史记录没有请求前缀或缓存键，也不能反推键变化导致了多少损失。

## 修复内容

`gateway_compatibility.go` 现在清理已经确认上游拒绝的顶层 `user`、`background=false/null`，以及带标准 `role` 的消息级 `author` 元数据。工具 schema、工具输出、加密历史、消息内容、顺序和大整数保持。独立错误场景、HTTP SSE/JSON/compact、WS 的 loopback 上游先返回同类 400，修复后单次请求成功，没有依赖重试。

`background=true` 需要异步任务及轮询契约，当前 Codex 上游未提供本项目可用的对应能力，所以返回明确不支持；不把它悄悄改成同步生成。[后台模式语义](https://developers.openai.com/api/docs/guides/background)。关闭兼容转换开关仍保留原请求。

缓存按 Sub2API 当前源码的默认键识别方式实现：仅当 `prompt_cache_key` 精确等于 `client_metadata.session_id` 时，在同一网关键、账号、所有者版本内生成稳定 64 字符键。显式自定义键、内部任务键、缺少 metadata 的请求不改；实际 session/thread、账号黏性和请求其余字节保留。HTTP 在账号选定后、压缩前改写；WS 每轮授权后改写。没有修改缓存统计、强制 retention/breakpoint 或增加缓存 token。详细参考及边界见 [缓存实现说明](CACHE-OPTIMIZATION-2026-09-25.md)。

官方公开 API 的说明区分模型版本：较新模型自动路由，缓存键可以用于独立计量；该文档本身不证明 ChatGPT Codex 的内部行为。稳定键是参考 Sub2API 的实现改进，实际效果以响应 usage 为准。[提示缓存文档](https://developers.openai.com/api/docs/guides/prompt-caching)。

初版局部改键的 Decoder 分配开销也已修正：标准 JSON 完整校验后定位字段，只复制真正需要发出的新 body。8 MiB 无目标 metadata 的请求不再分配整段 input/tool 副本；新增转义、嵌套同名、重复字段最后值、数组、非法 JSON 等边界回归。HTTP 的准备耗时覆盖最终改键和压缩，可能与账号选择阶段重叠。

## 实测方法

使用 `gpt-5.6-sol`，固定长指令和短输出，各阶段最多三次真实请求；服务端读取本项目 client key，不导出凭据。每轮 body 使用新的默认 session/cache key，网关路由 header 固定以保持账号选择一致。此 header 是实验控制，不伪称官方 CLI 抓包。前后提示词 SHA 相同，缓存量直接读取终态 `input_tokens_details.cached_tokens`，缺字段不会当作已观察的零。

最初发现另一个任务正在发布管理校验修复，运行版本从 `f9e56bb2…` 变为 `020fa045…`。版本前置检查停止了旧基线探测，**零次模型请求**。核对新发布清单后确认管理修复已包含在本地，后续以 `020fa045…` 为基线，避免覆盖并行工作。

基线三次请求均成功：每次输入 4,531，缓存依次 0 / 3,840 / 3,840；包含首轮为 56.50%，后两轮为 **84.75%**。这是修改前即能达到的结果，不能当作本轮优化提升。

上线后同一账号、同一前缀第一轮输入 4,531、缓存 0，正常完成；第二轮上游返回 SSE `error`、`server_is_overloaded`，脚本停止，第三轮未发送。因此 **没有足够的上线后数据证明命中率提高**，更不能写成整体达到 80%–90%。数据库按 client request ID 核对前后均使用同一账号、同一模型、每次恰好一次 attempt。

一次独立真实前台兼容请求也得到上游明确过载（不同账号，JSON 对外 502，`response_event`），没有得到完整生成成功的验收结果。另单独验证 `background=true` 由网关本地返回 400，未调用上游。兼容修复的成功证据来自完整 loopback/Rust 回归，不混写成上游实测全部成功。所有实测明细为 `cache-probe-*.json`、`probe-request-records.json`、`compatibility-live.json`；最后一份 scope 明确是本地后台请求拒绝检查。

## 最终回归

- Windows：807 个顶级测试通过、0 失败、148 个平台/集成条件跳过；`go vet ./...` 无诊断。
- Linux 完整 `go test -race ./...`：931 个顶级测试通过、0 失败、23 个真实 CLI/模型等显式 opt-in 跳过，共 2,798 个含子项终态；没有 DATA RACE，vet 无诊断。
- 新增真实 Rust 路径的缓存 JSON/SSE/zstd/compact/WS 五项全部通过；兼容 HTTP 三入口的 Rust 路径全部通过。
- 最终冻结源 SHA：`330de37216d81a55d276bdbc4d2e1068b67b6325837383d5bbc5c27bdca8e0f5`。构建 SHA：`ddb0f96dd4913bb651418aadf28051fe9e88f80556bd6573865d8f89ceb6afad`。

完整本轮证据：`.local/gateway-audit-20260924/go-failure-cache-final-20260925/`、`.local/failure-cache-20260925/windows-full.jsonl`。发布脚本单独验证了失败回退和旧版本 SHA 核验；不会将第一轮尚未做分配优化的测试当成最终构建验证。

## 部署结果

2026-09-25 北京时间 **12:44:56** 新后端通过激活检查，运行 SHA 为 `ddb0f96dd4913bb651418aadf28051fe9e88f80556bd6573865d8f89ceb6afad`。models=200/9项，WS=101，health=ready；随后真实缓存请求成功一条，上游过载请求得到明确记录，最终 in_flight=0。环境、设置、8 个账号元数据、UI、runtime identity 和 Rust helper 前后一致，没有数据库迁移。回执在 `.local/failure-cache-20260925/release/{activation-result,runtime-smoke}.json` 和 `final-health.json`。

发布中两项工具问题已纠正：文本上传改变 Windows 换行，改为原始字节传输后 hash 一致；settings API 带有动态 `uptime_seconds`，最初错误地用于重启前后全量比较，引发自动回退。旧后端确实恢复并 ready，单独核对环境/账号/工件一致；校验改为仅排除 uptime 后重新通过发布。原失败回执保留于 `release/attempt-uptime-check/`，不是应用测试失败或隐藏回退。

前端没有改动或发布；设置页旧文案仍概括“保留已有缓存键”，准确行为现为保留自定义键、收敛已识别的默认会话键，见更新后的设置文档。

## 复跑

```powershell
go test . -run 'TestRecentCompatibility|TestGateway(DefaultPromptCache|NativeDefaultPromptCache|AccountPromptCache)' -count=1
```

线上只读复查：`python tools/recent_failures_cache_audit.py`。真实模型对照脚本为 `.local/failure-cache-20260925/cache_probe_remote.py`，须在服务器传入当前 PID、二进制 SHA 和服务端凭据路径；它有三次上限、版本核验和遇错即停，不自动重试。
