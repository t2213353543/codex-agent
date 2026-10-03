# 同期账号对照：实际 IPC 摘要 schema 3

仅增强 `tools/quality_native_ipc_capture.py` 的 test-only 观察摘要及已有测试。没有改变请求/响应转发、TLS、HTTP 头、native 生命周期、错误隔离或模型输入。旧 schema 1/2 结果保留原样；新记录 `schema=3`，不能对旧结果补造不存在的字段。

实际边界仍是 **Go→Rust helper 的本地 socket 接收字节，位于 Rust 压缩/TLS 之前**。完整 IPC 写入不独立证明上游接收。WS 仍只观测握手，不记录 WS 请求消息或响应内容。

## 正文摘要

完整 HTTP body 原始摘要继续保留在 `actual_ipc_body_sha256`，新增 `body_wire_sha256/body_wire_bytes` 对应同一原始 body。`body_fields` 为每个顶层字段记录：

- `content_sha256`：规范 JSON 内容摘要。
- `canonical_json_bytes`：该规范 JSON 的 UTF-8 字节数，**不是原始字段字节区间长度**。
- `json_type`、`name_sha256`。

规范规则：对象 key 排序、JSON 字符串 escape 归一、数组顺序不变；数字通过独立词法类型保存，不经过 float/int。`9007199254740993`、`9007199254740992`、`-0`、`0`、`1e1000`、`10e999`、`1.2300e+009`、`1230000000` 分别产生不同摘要。保留数字词法差异并不声称数值相等的两种表示具有不同模型语义。

`body_field_names/body_field_count/body_fields_captured` 明确字段集合与完整性，缺失字段与 null、空字符串、空数组不同。instructions、tools、include、reasoning、text、input 等标准字段保留名称；未知顶层名称只用 `sha256:<namehash>` 表示，防止自定义 key 中携带正文或身份内容。

`input_summary` 除整体摘要外，记录完整 item 总数、每个捕获 item 的 index/type/hash/规范字节数，以及类型计数。不保存 message content、tool arguments/output、encrypted reasoning。未知 item type 只保存类型值摘要；无 type 的 message 记 `missing`，不伪造 wire 中不存在的 type。类型计数范围明确为 `captured_items_only`。

`client_metadata_summary.keys` 按每个 metadata key 保存同样的内容摘要和长度，`key_names/key_count/complete` 明确集合。标准 session/thread/turn 等键保留名称但值只有摘要；未知 key 名也哈希化。

`stable_controls` 是以下字段组成的**报告用控制集合**：model、instructions、tools、tool_choice、parallel_tool_calls、reasoning、store、stream、stream_options、include、service_tier、text、access_programs。它不包含 input、client_metadata、prompt_cache_key、previous_response_id 等动态/上下文字段，因此相同 `stable_controls` 不代表完整请求等价。完整配对必须再比较有关字段和正文来源。

## 严格不完整性标记

任意深度出现重复 JSON key（包括 Unicode escape 解码后重名）均拒绝内容摘要，`body_summary_complete=false, body_summary_error=duplicate_json_member`，不静默采用最后一项。NaN/Infinity、非法 UTF-8、孤立 surrogate、非 object、尾随 JSON 和过深结构也明确拒绝，不输出部分字段摘要冒充完整。

观察器仍记录完整原始 body SHA，并原样传输重复成员或不合法正文；解析器不是新的网关验证器，也不能中断发送。`request_forwarded_complete=true` 与 `body_summary_complete=false` 可以同时成立，分别描述传输和可比较摘要。

顶层字段/metadata 上限 512，input 明细上限 8192；超限记录总数、捕获数及 complete=false。完整 input 字段摘要仍覆盖整个 input，但 item 明细只覆盖已捕获部分。比较器不能把不完整字段集合当成全部相同。摘要完整也仅表示已按规则描述 body，不代表请求满足远端模型 schema。

## 全部实际请求头

保留原有有限白名单 plaintext（UA/originator/version/routing hint 等）。新增 `all_headers` 按每个实际值出现顺序记录：规范 lowercase name、规范 name SHA、`original_name_sha256`、`value_sha256`、`value_bytes`。Authorization、Account-ID、Cookie、未来未知 header 也只有摘要；`header_names` 是规范名集合，`header_value_count/header_summary_complete` 标记完整性。无值、一个空值和多个值可以区分。

Header 名称只在合法、长度有界且不含已知凭据时展示，其他名称哈希化。名字大小写归一仅用于报告比较，原始大小写摘要及完整 IPC head SHA 仍在；实际发送没有被重写。既有 Authorization token/workspace 专项摘要继续供身份守卫核验。

## 验证与版本

新增 10 项测试，覆盖敏感内容不泄露、全部 header 值及顺序、整数/小数指数词法、对象/转义归一、missing/null/empty、input 顺序、全树重复拒绝、非法 Unicode/nonfinite、summary 截断标记与 stable-controls 作用域。实际 socket relay 测试确认重复 JSON 被摘要拒绝时，原始请求字节和 native 响应仍完全一致。

Windows 执行 **38 项，32 通过、6 个 POSIX/Linux 限定跳过、0 失败**；旧 28 项也全部保持其既有结果。测试日志 `.local/quality-20260925/ipc-schema3/windows-tests.log`。两个 Python 文件通过 Python 3.9 grammar 解析和 py_compile。

```bash
python3 -m unittest discover -s tools -p test_quality_native_ipc_capture.py -v
```

Linux 验证须提供 `CODEX_NATIVE_HTTP_TEST_BIN` 指向真实 helper，才能执行全部 launcher/permission/真实 Rust loopback 分支；可在 network-none 容器中运行，不使用真实账号。主任务负责执行，Windows pass 不代替这些分支。

主任务已于同日完成 schema 3 的 Linux `--network none` 验证：38 项全部通过、0 跳过，3.255 秒，包含真实 helper 回环和 POSIX 分支。证据为 `.local/research-20260925/multiaccount-abba/capture-linux-tests.log` 及 `capture-linux-state.json`。随后扩展的 schema 4 独立记载，不能把它倒填进既有 schema 3 样本。

冻结 SHA-256：工具 `bdb2370549a4aa7a03ff6b7d79f51348028400e80f5a1b1ba465cc2fb8768c86`；测试 `61a955d59453c70e5658b01efbd2156fdebf4e4d8a974311fb886b76c43e7219`。新 launcher 会按工具与 helper SHA 固定运行版本，旧 capture 目录和旧报告不修改。
