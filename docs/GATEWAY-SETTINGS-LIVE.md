# 网关系统设置的真实账号验收

`gateway_live_settings_test.go` 提供显式开启的 `TestGatewayLiveSettings`。它将用户指定的单账号导出文件通过临时管理 API 预览、导入，然后清除测试副本的 refresh token。全部存储位于测试临时目录，不修改原文件或已有部署。

```powershell
$env:CODEX_SETTINGS_ACCOUNT_FILE = 'C:\path\account-export.json'
$env:CODEX_SETTINGS_MODEL = '已确认该账号可用的模型名'
$env:CODEX_SETTINGS_EVIDENCE_FILE = 'docs/GATEWAY-SETTINGS-LIVE-2026-09-17.json'
go test . -run '^TestGatewayLiveSettings$' -count=1 -timeout 15m -v
```

需要代理时，Go 进程应显式配置 `HTTP_PROXY` / `HTTPS_PROXY`，并通过 `NO_PROXY` 排除本地测试地址。Go 不自动读取 Windows 系统代理设置。

可选 `CODEX_SETTINGS_PREFIX_REFERENCE_FILE` 读取一份已明确选择的固定参考文本（最多 64 KiB），用于测试较长前缀。它只加入临时内置提示词；缺少此变量时使用基础测试前缀。不要仅为抬高缓存百分比而增加生产输入。

测试顺序固定为 HTTP 非流式两轮、HTTP SSE 两轮、同一 WS 连接两轮，最多六次生成，失败立即停止。三个传输路径共用本次随机生成的会话标识，分别建立两轮历史；HTTP 第二轮保留全部上游输出和加密 reasoning，WS 第二轮发送 `previous_response_id` 与新增输入。单账号实测只验证本次绑定与续接，多账号分配和跨实例一致性另由既有本地及 Redis 测试验证。

测试在临时设置中开启 WS、内置提示词、粘性会话及缓存键补齐，并加入固定的较长工程提示词作为可复用前缀。HTTP 检查实际发出的指令、缓存键、`stream=true` 和 `store=false`；WS 使用标准 transport 完成真正的上游握手，不用 HTTP 观察器冒充帧内容观察。此长提示词只用于测试。

报告仅包含固定字段的状态、断言、token 用量、耗时和错误分类，不保存凭据、账号标识、会话标识、请求原文或模型回复。缺少 `cached_tokens` 时计为缺失测量，不能算零命中。HTTP 上游首个文本增量、SSE/WS 下游首个文本增量与非流式完整响应可用时间分别记录；它们不是相同的时延指标。

每轮缓存率为上游实际 `cached_tokens / input_tokens`，总体使用 token 加权比值。后测模式可能复用先前的稳定前缀，因此各模式第一轮不代表独立冷启动。六次小样本用于证明协议可用及实际命中，不能证明优化前后提升、生产长期命中率或不同传输的性能排序。

## 首次实测发现

2026-09-17 使用已确认可用的 `gpt-5.6-luna`，前两次诊断均得到 HTTP 200 和正常用量，但非流式正文断言失败。第二次独立观察确认：上游文本增量精确匹配请求标记，网关返回的终态 JSON 却没有输出项。证据分别保留在 `GATEWAY-SETTINGS-LIVE-2026-09-17-attempt1.json` 和 `GATEWAY-SETTINGS-LIVE-2026-09-17-attempt2.json`。

官方 Codex 0.154.0 的 `codex-api/src/sse/responses.rs` 通过 `response.output_item.done` 提供完整输出项，`ResponseCompleted` 不要求再次包含 `output`。因此客户端必须处理项完成事件，非流式网关也必须聚合它们。SSE/WS 验收客户端独立按输出索引组装已完成项，生产 SSE/WS 继续透传原始事件。非流式聚合修复保留完整的工具、加密 reasoning 和未知字段，不仅拼接可见文本；这也让后续请求能够保留真实历史。

## 修复后真实结果

模型为该账号实际支持的 `gpt-5.6-luna`。基础前缀、长参考前缀各跑六次，HTTP JSON/SSE/WS 各两轮，十二次全部通过；另有修复前两次诊断，所有尝试均已保留证据。长参考内容选用本项目的网关设置及缓存核查说明，测试快照位于 `.local/settings-verification/cache-reference.txt`。

| 路径 | 轮次 | 长前缀输入 token | 命中 token | 命中率 |
| --- | --- | ---: | ---: | ---: |
| HTTP JSON | 1 | 4920 | 0 | 0% |
| HTTP JSON | 2 | 4960 | 3840 | 77.42% |
| HTTP SSE | 1 | 4921 | 3840 | 78.03% |
| HTTP SSE | 2 | 4963 | 3840 | 77.37% |
| WebSocket | 1 | 4921 | 3840 | 78.03% |
| WebSocket | 2 | 4963 | 3840 | 77.37% |

长前缀合计 29648 输入 token，其中 19200 命中，包含首轮的加权命中率为 **64.76%**；首轮之后五次为 **77.64%**。基础前缀单次约 1337–1380 输入 token，六次均为零命中。两组输入长度不同，不能作为同一负载的优化前后提升证据，也不能据此推断 ChatGPT Codex 后端的精确缓存阈值。

四个 HTTP 请求实际协商到 TLS 1.3、ALPN `h2`；WS 握手返回 101，同一连接的两轮生成及 `previous_response_id` 续接成功。WS 每帧没有独立 HTTP 状态，其报告中的 HTTP 数值零及 HTTP 出站观察布尔 false 表示未在该观察器测量，不能读成 WS 策略失败。WS 协议使用独立 HTTP/1.1 Upgrade transport，相关 TLS 隔离由既有本地握手回归验证。

全部十二次成功请求都有真实用量和完成状态，单账号绑定一致；HTTP 的实际出站内置提示词、稳定缓存键、流式与存储字段断言均通过。非流式返回完整正文，不再只返回空终态。

最终本地全量 Go 测试 **431 项通过、30 项环境条件跳过、0 失败**（含子测试），`go vet ./...` 与 Windows 构建通过。聚合修复另有独立代码审查与内存上限、加密/工具保留、错误用量、终态即结束等回归。没有部署。

原始脱敏记录：[长前缀](GATEWAY-SETTINGS-LIVE-2026-09-17.json)、[基础前缀](GATEWAY-SETTINGS-LIVE-2026-09-17-short-prefix.json)。
