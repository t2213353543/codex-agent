本轮 pro 参数请求被当前 OAuth Codex 端点以 HTTP 400 拒绝，试验未能比较 pro 的答案质量。预登记最多八格，实际只执行前两格，按失败即停规则停止；没有重试、补格、刷新或修改账号。

| 格 | 账号 | 请求 mode | HTTP | 模型答案 | 输入 | 缓存输入 | reasoning tokens | 输出 |
| --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 01 | new2 | standard | 200 | 21 | 6883 | 0 | 3006 | 3287 |
| 02 | new1 | pro | 400 | 无，未评分 | 未返回 | 未返回 | 未返回 | 未返回 |

01 完整返回 `response.completed`，公开响应控制字段为 `gpt-5.6-sol`、`effort=medium`、`mode=standard`，mode 回显匹配。冻结评分器 SHA `0441c9c5…` 自动提取 21；正文使用 9 个圆形加 12 个五角星，没有人工改分。02 是请求拒绝，不计作数学错答、低 reasoning 用量或模型失败答案。

原题、答案合同 21、model 和 effort 均固定。公开推理指南描述 standard/pro mode，且 mode 与 effort 独立；Sol 页面确认它属于 GPT-5.6 系列。但公开 API 文档不能证明当前 `chatgpt.com/backend-api/codex/responses` OAuth 路径接受该参数。HTTP 400 也不足以认定账号缺少某项 pro 权限或权益。[Reasoning guide](https://developers.openai.com/api/docs/guides/reasoning)，[GPT-5.6 Sol](https://developers.openai.com/api/docs/models/gpt-5.6-sol) 的实际读取证据保存在本轮 `review/official-doc-fetch.json`。

02 的安全错误投影确认 JSON 错误类型为 `invalid_request_error`，响应体 189 字节，原始 body SHA 为 `b625ca75df11e42bd80d6470b646de6a4b1ac7e333d4d070a0b03cac7a876810`。错误 code 未在工具白名单中，因此保留 `other` 和 UTF-8 SHA `c571ec8d69ec9bd6753e9c4da36700bd36a2ae573436ad91f3123882e4be745a`；message 只保留 JSON 字节长度 52 与 SHA `428d45ce5cd89aac4e2f2f0a0f21a9890edce2ca773b32413006c4eb1e949602`，没有原文。

仅从固定官方 CLI 0.155.0 源码实际出现的相关引号字面值中选取 449 个候选进行 code 哈希比对，没有匹配；没有猜测 message 或构造外部词典。因此本报告不补写“unsupported mode”“权限不足”等未观察到的错误内容。报告中的 `response_bytes=0` 属于 SSE 收集计数，不能误读为没有收到那 189 字节 HTTP 错误体。

投影只在原始请求 `reasoning` 对象结束前的字节位置 35109 插入 mode：standard 插入 18 字节，pro 插入 13 字节。原始正文 SHA `92786b9be68867e2777f23cef726e75d0e75b38d8c2cb635ac7b7cd874126fcb`；standard 实际 IPC body 为 `f60e406609e8124c6f187b168111f2ad3c9bab544d29ed97c3742bc9ae306dc5`（36,362 字节），pro 为 `340d85766955cd14e4700826a97c393232cfd92d9355db9ebcf3df9bcb24cc7a`（36,357 字节）。两投影保存的原前缀、后缀 SHA 完全相同。

离线复核逐一验证实际 IPC 与计划正文 SHA、其余十个顶层字段摘要、完整 input/metadata 内层摘要、全部非认证头和 zstd 意图一致；Authorization 与 workspace 头分别匹配各自冻结凭据。每格使用独立 helper scope，分组与逐格前后 usage 均允许且代次稳定。词法不变证据来自冻结投影和 runner 的发送前原字节校验，以及保存的前后缀摘要；本地复核没有重新读取已清理的原始请求，也没有声称仅靠 JSON 摘要能重建原字节。

执行代码冻结为 `run_modes_remote.py` SHA `b0632a94…`，投影工具 `b1310185…`。运行基线是网关 `e069de52…`、helper `bf920a0b…`、来源 image `5d931132…`。终态时间 2026-09-25 18:28:27 UTC（北京时间 9 月 26 日 02:28:27），模型 unit 为 PID 0、inactive、exit 0，两个 helper 均正常回收、无强制杀死、socket 不存在。原计划后六格没有创建任何执行目录，批次 stop reason 为 `model_failure_policy_stop`。

最终独立审查 117 项全部通过，13 个服务器安全产物 SHA 与本地匹配；四个原始捕获文件已清理，临时凭据不存在。两格窗口中已完成、已落库的生产网关请求重叠均为 0；不涵盖外部或未完成行为。

独立产物位于 `.local/research-20260926/reasoning-mode-controls-v24/review/`：`results-final.json` 保存逐项证据检查、两格输出及未执行六格；`error-code-source-literals.json` 保存有限源码哈希比对；`runner-review.json` 保存执行前失败门禁与投影测试。离线复核命令：

```powershell
python .local/research-20260926/reasoning-mode-controls-v24/review/review_actual_results.py
```

本轮只有 standard/new2 成功和 pro/new1 请求拒绝，缺少同账号 standard/pro 质量配对。它不能回答 pro 是否改善 new1，也不能推出账号全部 pro 权限、隐藏路由、风控标签或降智根因。
