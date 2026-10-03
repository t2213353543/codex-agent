# 同期三账号六次原题：独立字段核对

最终材料为 `.local/research-20260925/multiaccount-abccba/index.json`、各格原始脱敏 IPC、`comparison.json`；未使用中间报告或 q4 的历史答案替补。北京时间 2026-09-25 03:28:53–03:33:02，预登记顺序 Q0、q5、q6、q6、q5、Q0 全部完成，无跳过，临时凭据已清理。固定官方 CLI 0.155.0、同一透明 native 网关、原题、sol/medium、相同工作目录；每格 fresh home、唯一推理 POST、无自动重试，前后身份及凭据 epoch 检查通过。

| 本轮账号别名 | 两次答案 | 两次 reasoning output tokens |
| --- | --- | --- |
| Q0 | 21、21 | 2406、1552 |
| q5 | 29、29 | 516、516 |
| q6 | 29、29 | 516、516 |

按预先约定 21 评分，本轮确有同期两类答题表现；不是沿用历史标签。六格均 clean、HTTP 200、completed，发送和返回 model 均为 `gpt-5.6-sol`，effort=`medium`，最终 tier=`default`，输入 tokens 均为 **6881**。q5/q6 四次 reasoning tokens 都为 516 是相关现象，不能单凭 token 数证明算力降档、隐藏集群或账号标记；缓存 tokens 也不同，未把缓存差异解释为原因。

## 全部 15 组对照结果

独立脚本逐对重新读取实际 IPC 的字段摘要，核对 15 对 × 7 个 input item，共 **105 个 item 字段对照**，不只读取汇总结论：

- 实际顶层有 12 个字段。除 `input`、`client_metadata`、`prompt_cache_key` 外全部摘要相同，没有新增/丢失字段或其他未分类差异。Responses Lite 将工具定义放进 `input[0].tools`，所以顶层没有 `tools` 不代表没有工具。
- `input[0]` 是 `additional_tools`，`tools`、`role`、`type` 全部一致，仅 `id` 变化。`input[1]` 的内容、role/type、`content_item_kinds` 一致，也只有 `id` 变化。
- `input[2..6]` 的 `content`、`role`、`type` 一致；差异只有 `id` 和 `internal_chat_message_metadata_passthrough`。后者逐键差异仅为 `create_time`、`turn_id`，`content_item_kinds` 一致。所有已记录 input 字段中没有其他未知差异。

完整 input item hash 在每一对中仍不同；没有删掉 ID 或时间后把两份原始请求宣称为相同。

## 两个匿名 metadata 键的源码映射

以固定 CLI 源码中的字面 key 计算 SHA256，与六份记录的 `name_sha256` 精确匹配：

| 已记录名称哈希 | 实际键名 | 源码依据 |
| --- | --- | --- |
| `76c9368c8c09e331f788b4df7e9c22ba6a3d4e85de55931c3cea2e7cbcf8c760` | `root_turn_id` | `core/src/responses_metadata.rs:47` 定义，约 334 行加入 client_metadata |
| `96b60d83140783d55b20a5c38bdce94fd9bfb117e6d4c8f44a1cdc8467b29188` | `x-codex-installation-id` | `core/src/client.rs:152` 定义，`responses_metadata.rs:309` 加入 client_metadata |

这里只映射键名，没有恢复或修改其值。client_metadata 的七个键均随 fresh session 改变：上述两键，以及 `session_id`、`thread_id`、`turn_id`、`x-codex-turn-metadata`、`x-codex-window-id`。其中 `x-codex-turn-metadata` 是 JSON 字符串，此摘要只有完整字符串 hash，没有保存其内部逐键 hash，因此不能进一步断言该字符串内部只差 ID/时间。

## HTTP header 与证据范围

15 对实际 header 的差异并集仅为 `authorization`、`chatgpt-account-id`、`session-id`、`thread-id`、`x-client-request-id`、`x-codex-turn-metadata`、`x-codex-window-id`。同账号两格没有凭据/workspace 差异；header 名称原始拼写没有差异。

`user-agent`、`originator`、`version`、routing hint、`x-codex-beta-features`、`x-openai-internal-codex-responses-lite` 在六格全部相同。后两者实际存在；`x-codex-features` 和 `OpenAI-Beta` 则六格均不存在，不能把“共同缺失”写成“值相同”。

本实验将差异缩小到账号、fresh session 身份/时间元数据及服务端结果，尚未做到**整份请求只改 Authorization/workspace**。六格入站到 IPC 均增加 18 字节，现有入站只记录整体 hash，不能用字段级出站摘要反推入站变化内容。后续固定整份请求的 native 对照可进一步减少这些混杂因素，当前不提前赋予因果结论。

本次只读核对并新增本文，未修改 Go、工具、原始 CLI/IPC 报告。最终 `comparison.json` SHA256：`6b64ee66a052756f1d96615b83936c9f729417b0c8e7c4ae9b64d1940b43ac8b`；对应最终 index SHA256：`ba28b1c7877cb75dc700fa93eb32f8a83fba500e2525dcee1eabc87e88b8455c`。
