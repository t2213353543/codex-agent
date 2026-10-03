# 真实 CLI 文件编辑流程

本地验收使用未修改的官方 Codex CLI 0.155.0，经过 Go 网关、封存 Rust
helper 的 UDS 和 loopback TLS HTTP/2 上游。容器为 `network none`，认证为
合成数据，使用新的 HOME、CODEX_HOME、XDG 目录和工作目录；没有读取桌面认证、
真实账号或模型接口，也没有部署。

## 实际执行

合成上游只返回确定性的工具指令，文件操作由真实 CLI 执行。工具从客户端实际
tools schema 递归选择，保留名称、类型和 namespace，不用 shell 冒充 apply_patch。
本次实际提供的是无 namespace 的 custom `apply_patch` 和 function `exec_command`。

1. apply_patch 创建含待修正内容的文件，随后核对真实工具输出和磁盘内容。
2. exec_command 读取文件并校验目标内容，实际返回预期退出码 `7`。
3. 只有对应 call_id 的失败回传被验证后，才发出 apply_patch 修正指令。
4. exec_command 验证修正后的文件字节和 SHA256，实际退出码 `0`。
5. 首次 CLI 进程完成；新进程 resume 同一线程，重新从磁盘读取、校验文件，退出码 `0`。

整个过程共 7 次 Responses 请求、5 个不同工具 call_id、2 次 custom 调用和
3 次 function 调用，5 个结果都按原 call_id/type 对应验证。最终文件为 41 字节，
SHA256 为 `de812bc619265d3eb6422bf3270009fa14262822545f4b139458d2127be561aa`。
这是顺序多工具、失败纠正与续接，不是并行工具或真实模型推理能力测试。

## 保真与统计

同一 CLI 进程内 instructions、工具 schema 和已发送 input 前缀保持一致，
prompt_cache_key 跨该线程和 resume 保持稳定；已有工具输出在后续完整 HTTP
历史中只有一份且内容不变。此证据只说明上下文与缓存标识没有被网关无故改写，
不代表实网缓存命中率、费用或模型质量提升。

报告 `tool_outputs_not_replayed` 的具体边界是：本路径没有额外请求、重复发出的
call_id 或重复/改变的已返回工具结果，且两次 patch 的磁盘效果符合预期。
它不是所有故障条件下的 exactly-once 执行证明；例如独立重复执行一个幂等 shell
命令、却只返回同样结果，并不能单靠这些输出判别。断连/模糊写入不重放另有专项测试。

7 条 Responses usage 记录和准入释放检查通过，合成用量总计 input=280、output=7；
预期 shell 失败不会被伪装为工具成功。Go 上游 fallback trap 为 0。报告只保留
计数、退出码、文件哈希和布尔断言，不保存文件正文、工具输出内容、提示词或凭据。

## 固定证据

- Go 源码 SHA256：`5a9eddb2d5b1ec882cfd83e62bd58576ed6de33a1fc6f7571a6631d37222ff4b`。
- 测试文件 SHA256：`ce486ee06b8aa4c9cf781e8c842ac8e76b17471eff444d19be0d67dbc53480af`。
- CLI SHA256：`660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`。
- helper SHA256：`6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`。
- 报告：`.local/native-transport-build/gateway-compare-20260918T072935-9c540a/gateway-edit-cli.json`。
- 报告 SHA256：`6008d0476a1efffaa40d39982206db4a7ee26ec9f7e8bb38833d11ba5e77e041`。

21 项文件编辑断言全部通过，单项测试耗时 11.38 秒。该次固定配对的 22 个本地
联调顶层测试均通过、零跳过、无 OOM，容器已清理。耗时为一次本地合成样本，
不构成生产延迟或吞吐声明；TLS 使用本地自定义 CA，也不补足生产 SNI/出口验证。
