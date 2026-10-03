# v14 官方 CLI WebSocket 实际捕获独立核验

v14 已成功观察固定官方 CLI 0.155.0 的实际 WS 发送链：一次本地 TLS 握手、一次模型目录 GET、一个预热帧及一个首推理帧。独立离线核验 88 项全部通过，真实上游模型请求为 0，没有模型质量答案。首推理帧引用夹具生成的前次 response ID，不能当作独立完整首回合请求重放。

CLI 二进制 SHA 是 `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`；这次使用 new2 当前已有凭据，token/user/workspace/record 哈希与种子一致，没有提供 refresh token，刷新端点禁用。模型为 `gpt-5.6-sol`，推理强度 medium，原题 SHA 为 `cd771ba38f8f75696ba703c7d3e91e5273588c91cb8c4145e3ce4b077c075c3f`。内置 openai provider 通过根级 `openai_base_url` 定向到本地 TLS 捕获服务，信任临时 CA。目录请求可经固定 native helper 读取真实目录；捕获服务的模型路径没有上游转发能力。

| 观察项 | 预热帧 index 0 | 首推理帧 index 1 |
|---|---|---|
| 原始消息字节数 | 30,524 | 7,336 |
| 原始消息 SHA | `e30f3123af14dce759df2a63d2e94b7ad5702e3de12c06efb1598717da0c8b92` | `91f1e7f71510753bf338ed168d032ba25600014c93ba32e4260d5e7049562ae4` |
| input 项数 | 2 | 5 |
| generate | 显式 false | 字段缺省 |
| previous_response_id | 缺省 | 精确对应合成 `synthetic_capture_warmup` |
| 原题全文匹配 | 0 次 | 1 次，`/input/4/content/0/text` |
| 独立完整重放资格 | false | false |

原始消息指 WebSocket 解码后的 text message 字节：未重序列化 JSON，但压缩、掩码及分片已由 WS 库处理，因此不等于原始网络帧。`capture-boundary.json` 的实际本地路径为 `/v1/responses`；脱敏 head 里的 `/backend-api/codex/responses` 是规范化目标路径，不是这次连接实际发送的 URL。头部来自 Go 解析后的值，不保留原始 wire 大小写和顺序。本地自签 CA 的 TLS 捕获也不证明与真实上游 TLS 握手相同。

两帧的 `include`、`model`、`parallel_tool_calls`、`prompt_cache_key`、`reasoning`、`store`、`stream`、`text`、`tool_choice`、`type` 字段摘要均相同。独立以常量计算哈希确认 `type=response.create`、`model=gpt-5.6-sol`、`stream=true`、`store=false`；两帧均没有顶层 instructions、tools 或 background。稳定控制摘要相同，不能据此宣称完整请求相同。

顶层实际差异只有四项：client_metadata、input、generate、previous_response_id。预热帧的两项 input 是 additional_tools 和 developer message；首推理的五项是三个 developer message 和两个 user message。原题全文出现位置由删除原始帧前的只读 `prompt-placement.json` 证明，绑定第二帧原始 SHA；没有从摘要臆造正文。

扁平 client_metadata 的变化为 turn_id、root_turn_id（旧摘要器将字段名表示为哈希）、`x-codex-turn-metadata` 和 `x-codex-ws-stream-request-start-ms`。嵌套 turn metadata 的变化为 request_kind 从 prewarm 到 turn、turn_id 从空串到本回合 ID、新增同值 root_turn_id，以及新增 turn_started_at_unix_ms。installation_id、session_id、thread_id、window/context-window 等其余已观察嵌套字段保持相同摘要。握手中的嵌套 metadata 精确对应预热帧，而非稍后首推理帧；握手明确携带 `OpenAI-Beta: responses_websockets=2026-02-06`、originator `codex_exec` 和 version `0.155.0`。

这条链符合固定源码的预热与增量规则：预热完成后，CLI 可省略已提交 input，并把前次 response ID 放入首推理帧。这次 ID 来自合成完成事件，真实上游并不存在同一状态。把两帧的 input 拼接、删除 previous_response_id 或改 generate，会得到人工构造的派生请求，不能称为官方捕获原帧；本轮没有实施这种转换或重放。

v13 的失败属于捕获夹具回执问题。其合成 usage 缺少固定 CLI parser 要求的 total_tokens，真实记录只捕到预热帧，随后 `message_read_failed`，CLI 可见重连；当时没有直接显示“缺少 total_tokens”这一文本。v14 仅在独立测试夹具合成回执中增加 `total_tokens:0`，生产文件未变。Docker `--network none` 下的固定真实 CLI、合成账号单字段对照重现旧夹具失败和新夹具两帧成功，提供原因证据。真实 new2 的 v14 捕获随后也获得两帧。

捕获到首推理帧后，服务主动返回 `invalid_prompt` 合成终止，CLI 以退出码 1 结束，没有重连、工具执行或生成答案。报告的 `invalid_reference` 因而正确保留；这表示它不是可评分的成功模型参考，不否定捕获链已完成，也不是账号质量故障证据。

最终核验确认凭据文件与原始捕获已删除，服务回收，unit MainPID=0、inactive、退出码 0；七份远端公开 artifact SHA 与本地逐项一致。原始 JSON 各值区间 SHA 未单独记录，现有字段哈希采用排序对象键、规范化字符串转义、保留数字词法的语义摘要；不补造已删除的 raw span 证据。

主要证据：`.local/research-20260925/ws-positive-controls-v14/review/capture-proof.json`，以及同目录批次的 `capture-evidence.json`、`prompt-placement.json`、`capture/capture-boundary.json`、`capture-cli/report.json`、`actual-cli-red-green.json`、`cleanup-final.json`。后续真实 WSS 对照必须先获得真实服务器的预热响应，再在同一真实连接上发送官方后续序列，不能沿用本地合成前次 ID。

只读复核命令：`python .local/research-20260925/ws-positive-controls-v14/review/review_capture.py`
