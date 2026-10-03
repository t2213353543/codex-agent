# 固定请求实验响应摘要增强（工具 v2，未运行模型）

本轮只修改 `tools/quality_exact_request_probe.py` 及对应测试。旧顶层 `codex-quality-exact-request-v1` 保持兼容，新增 `response_output_evidence`，其子 schema 为 `codex-quality-response-output-v1`。旧冻结工具、报告和本轮修改前副本均保留；不能从历史报告反推未保存的 reasoning 摘要。

`done_items` 保存按到达次序的 `response.output_item.done` 摘要，保留 output_index 是否存在及 null/0 的区别；`terminal_output` 独立保存终态 response.output，不把已收到 done 项补入明确空数组。每项保存完整 JSON 值 SHA、canonical UTF-8 字节数、已知 type 标签、字符串 ID 的 UTF-8 SHA、各顶层字段值 SHA/字节数/JSON 类型。content、summary、encrypted_content 只存摘要，不保存隐藏推理、摘要正文或密文。未知 type 不输出原值，未知字段名以 SHA 标识；完整项摘要仍包含动态 ID，不将它忽略后宣称相等。

摘要沿用 IPC capture 的 exact-number JSON tree：对象键排序、字符串转义规范化，数字原词法完整保留；大整数和长小数不经过 float。`1`、`1.0`、`1e0` 仍可不同，不声称数学等价或 RFC 8785。字段字节数是该规范化表示长度，不是原 wire span。重复键在任意深度拒绝，避免覆盖后错误宣称完整。

每个来源最多展开 128 项，每项最多 32 个字段，已知字段优先；记录总数、已捕获数和 `complete=false`，终态整个 output 的 hash 仍覆盖未展开项。总响应原有 8 MiB 限制保持。缺失 output、显式 null、空数组，缺失 summary、null、空数组分别可见。

`terminal_event_data` 的 SHA 覆盖 SSE data 行以 LF 拼接得到的原 JSON 字节，**不是包含 SSE framing 的全 wire hash**。`final_text` 分别保存 extract_final 提取文本在脱敏前后的 UTF-8 SHA/字节数，明确 source 和是否输出脱敏文本，不混同于完整 output tree。外来 response ID 在摘要生成之前仍被拒绝。相同 token 数不证明 reasoning 相同；不同 encrypted_content hash 也不证明隐藏推理不同，可能只是加密随机性。

新 6 项测试先失败，原日志 `.local/quality-20260925/exact-response-evidence-v2/fail-before.log`。修复后共 21 项：本机 19 pass、2 项 POSIX/真实 Rust 环境 skip、0 fail（0.149s）。覆盖相同 reasoning/不同最终答案、相同 token/不同 reasoning、动态 ID、字段缺失/null/空数组、精确数字及键序规范化、未知敏感字段、1 MiB 项与项/字段数量边界、外来 reasoning 拒绝；原真实 Rust loopback 测试已增加输出摘要与隐私断言，等待 Linux 运行。`py_compile` 通过。

四个旧报告差分样例移除新增字段后逐值相等。AST 核对 `load_capture/load_at_only/exact_head/deadline_io/native_request/main` 完全未变，证据 `compatibility.json`；未修改发送、TLS、headers、凭据选择、deadline、重试与进程回收路径，也没有修改 Go 生产或 remote runner。

```bash
python3 -m unittest discover -s tools -p test_quality_exact_request_probe.py -v
```

最终 SHA：probe `1e448cff75e9f02b688bbe665d999e502a658797e5c87b7433e06b624d604749`；tests `e7dcabf344ce3cda0a77052ca3c2e0be93c20b85910b90452e0828d77d939b0f`。Linux network-none/实际 Rust helper 验证由主任务对独立 v2 快照执行；本报告不预先记作已通过。

后续独立 Linux 验收已完成：同 SHA 工具在 network-none 容器实际运行 21 项，全部通过、0 跳过（0.846s），包含真实 Rust helper→loopback 输出摘要与隐私检查。与 usage/account-control 合计 47 项全部通过，模型请求数 0，见 [环境工具验收](environment-tooling-validation.md)；历史 exact-account-control 保持原样。
