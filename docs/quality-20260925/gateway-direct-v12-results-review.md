# v12：原生直连与当前网关的独立结果核验

2026-09-25 11:17:31 UTC 预登记的 8 格镜像实验已完成，独立核验通过，未发现证据冲突。8 次请求均 HTTP 200、`response.completed`；无自动重试、无刷新。new2 的原生直连两次回答 21，网关两次正文也明确回答 21；new1 的原生直连和网关各两次均回答 29。本轮观察到的账号间差异同时存在于直连与网关路径，不能归因为只有网关才会触发的问题，也不能据此识别服务端内部原因。

实验使用当前 a52ad225… 网关源码的隔离单账号测试实例，保持生产设置，仅关闭自动刷新和 401 重放；不是生产监听端口。原生直连使用相同 9110b869… Rust helper，每格独立进程，模型路径前不做目录 GET。每账号、每路径两次，按固定顺序镜像排列。完整组 usage 预检发生在第一格前，逐格另有 usage 与凭据快照前后检查。new1/new2 的 token、稳定用户及 workspace 哈希保持预登记值；元数据版本分别是 10/8，未将元数据版本当作凭据代次。

| 格 | 路径 | 账号 | 冻结自动答案 | 独立人工注释 | input | cached | reasoning | output |
|---|---|---|---:|---|---:|---:|---:|---:|
| 01 | 原生直连 | new2 | 21 | — | 6881 | 0 | 1924 | 2238 |
| 02 | 网关 | new1 | 29 | — | 6926 | 0 | 516 | 660 |
| 03 | 网关 | new2 | null | 21 | 6926 | 0 | 1034 | 1256 |
| 04 | 原生直连 | new1 | 29 | — | 6881 | 0 | 887 | 1140 |
| 05 | 原生直连 | new1 | 29 | — | 6881 | 5888 | 900 | 1147 |
| 06 | 网关 | new2 | null | 21 | 6926 | 6784 | 1232 | 1540 |
| 07 | 网关 | new1 | 29 | — | 6926 | 5888 | 763 | 1078 |
| 08 | 原生直连 | new2 | 21 | — | 6881 | 6656 | 3406 | 3698 |

自动结果仍是 2 个正确、4 个错答、2 个未评分，两个 `null` 未改写。审查器只加载实验 `model-tooling-manifest.json` 绑定的旧评分器，SHA `85f00490fa2d54980356662e5529c169be11fd8dbe9f6f610acf6dcb0ec31763`，不回退到已修复的工作区评分器。03 的正文首句和末尾方框均为 21，旧评分器把“20 颗或更少”的下界说明误当条件答案；06 的正文首句及末尾 `9+12=21` 一致，旧评分器未接受方框后的单位后缀。两份人工注释分别绑定原始 final SHA `3d71b039…`、`4938edd8…` 及原 report SHA，保存在 `review/human-annotations.json`。把人工注释单列纳入解读后，是 4 个 21、4 个 29。

原题 SHA 固定为 `cd771ba38f8f75696ba703c7d3e91e5273588c91cb8c4145e3ce4b077c075c3f`，本批新官方 CLI 捕获正文 SHA 为 `cae65d9006542a2d4d53336bdd17502cc978e019492edcd6857ebba2a34e23d5`。四次 direct 的实际 IPC 正文字节与捕获一致，非认证头值也一致。四次 gateway 在 IPC 边界的题目、input 各项、模型、推理强度、工具等非策略字段均与捕获摘要一致，但请求不完全相同：

- 新增 `instructions`，内容精确等于冻结生产设置里的 232 字节内置提示词。
- `prompt_cache_key` 与 `client_metadata` 会话身份按账号命名空间投影；同账号的两次 gateway 投影摘要相同。
- 头部 `session-id`、`thread-id`、`x-codex-turn-metadata` 改变，新增 `session_id`、`conversation_id`；其他非认证头保持原值。
- 正文及头部各自的嵌套 turn metadata 有 17 个已观察字段，实际仅 `session_id`、`thread_id` 的字段摘要改变；`installation_id`、turn/root-turn、window/context-window、sandbox、request-kind 等其余字段保持原摘要。
- direct 保留捕获的 zstd 请求压缩；gateway 本地入口传未压缩正文，实际 native upstream 观察为未启用请求压缩。本轮未把压缩方式宣称相同。

这些区别是当前生产策略及实验入口的实际差异，逐格原始摘要保存在 `upstream-evidence.json`、IPC JSONL 及 `capture/original-field-evidence.json`，独立报告列出每个字段的前后哈希。它们不构成 TLS 握手字节抓包，也不证明未知设备或服务端状态完全一致。

本批同时存在 cached=0 的 21 和 29，也存在 cached>0 的 21 和 29。缓存未命中不能单独解释错答。样本只有每账号、每路径两次，时间与服务端状态未随机化，不能作为独立同分布样本推断原因。生产日志只读查询显示八格期间已完成且持久化的重叠请求均为 0；该证据不覆盖未完成请求和外部使用。

清理核验显示实验 unit 的 MainPID=0、inactive/dead、退出码 0，临时账号库、凭据文件、原始捕获均已删除。独立核验同时检查了 32 份远端公开文件 SHA 与本地一致。审查本身未触发模型、刷新或账号写操作。

主要证据位于 `.local/research-20260925/gateway-direct-controls-v12/`：`plan.json`、`execution.json`、`review/results-final.json`、`review/human-annotations.json`、`cleanup-verified.json`。结果审查器在读取真实报告前通过了独立合成报告故障测试，包含正文/认证/嵌套身份篡改、未完成响应、错误自动答案、清理失败，以及归档评分器被修改后禁止回退。

只读复核命令：`python .local/research-20260925/gateway-direct-controls-v12/review/review_results.py --require-complete`
