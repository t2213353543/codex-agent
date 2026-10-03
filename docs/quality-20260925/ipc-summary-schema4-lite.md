# Responses Lite 多账号输入：schema 4 分层摘要

2026-09-25。已读取 `.local/research-20260925/multiaccount-abba/01-q0/ipc/` 和 `02-q4/ipc/` 的真实 schema 3 派发记录：每条实际 POST 有 12 个顶层字段，instructions 为空字符串，input 包含 1 个 additional_tools 和 6 个 message。七个完整 item hash 全不同；前五项各自大小相同，后两项相差 1 字节。**这些摘要本身不能证明正文相同，也不能证明模型指令不同。** 本轮 Q0 完成 21；q4 在 HTTP 200 后上游失败，没有可评分答案，不能称该格错答。

## 为什么正常 CLI 不同运行会产生不同 item hash

固定官方 0.155.0 有明确的正常构造机制：

1. [client.rs:798](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:798) 的 Lite 分支用当前 thread ID 构造 UUIDv5 namespace。additional_tools.id 再以 tools JSON 为 name 构造 `at_<UUIDv5>`；base instructions 以文本为 name 构造 `msg_<UUIDv5>`。两个新 thread 即使 tools/指令完全相同，ID 仍会不同。Lite 返回的顶层 instructions 为空、tools 不在顶层，而是前置 input 项。
2. [session/mod.rs:3400](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/session/mod.rs:3400) 对无 ID 的普通历史项调用 `ResponseItemId::new(prefix)`；[response_item_id.rs:21](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/protocol/src/response_item_id.rs:21) 使用 UUIDv7。保留已有 ID，新运行的新消息会获得新 ID。
3. [session/mod.rs:3353](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/session/mod.rs:3353) 为新历史项盖 turn_id 和 fractional Unix create_time；[models.rs:949](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/protocol/src/models.rs:949) 还定义 internal metadata 的 content_item_kinds、cell_id、executed_tool_calls、tool_calls_complete。固定 cwd 不会固定这些身份/时间字段。时间数字的序列化精度可影响字节数，但现有旧 hash 无法验证本轮 1 字节差异是否就是时间。
4. [client.rs:912](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:912) 发送前只清除未带 prefix 的 legacy ID；合法带 prefix 的 ID 不被统一删除。internal metadata 也不是可以随便移除的无意义字段，官方明确要求保留其协议含义。

这些源码解释了“完整 item hash 不同”的可能来源，**没有将它当作实际七项都只差 ID 的证明**。旧 schema 3 没有保存内容或 item 子字段摘要，无法离线恢复缺失证据；不得回填或从相近长度猜造字段相等。

## 最小观察增强

新 schema 4 仍保存每个 item 的完整 `content_sha256`、规范 JSON 长度、index/type；新增：

```json
{
  "field_summary": {
    "fields": {
      "id": {"content_sha256": "...", "canonical_json_bytes": 42, "json_type": "string", "name_sha256": "..."},
      "content": {"content_sha256": "...", "canonical_json_bytes": 100, "json_type": "array", "name_sha256": "..."}
    },
    "field_names": ["content", "id"],
    "field_count": 2,
    "fields_captured": 2,
    "complete": true
  }
}
```

对每个 object item 捕获全部顶层 field 摘要，不只挑 id/content。标准 type/role/tools/content/body/input/output/arguments/encrypted fields/phase/call_id 等保留字段名；未知名只保存 name hash。`internal_chat_message_metadata_passthrough` 整体仍作为 item field 摘要，若为 object 再增加相同结构的 `internal_metadata_summary`，按其 turn_id、create_time、content_item_kinds 等逐字段记录。所有字段值均不保存明文。

这让下一轮能够具体报告：例如 full item 不同，同时 tools/content/role/type 相同，id 不同，metadata 的 turn_id/create_time 不同；或者 content/tools 自身已经不同。**没有提供“去掉全部 ID 后相等”的总 hash**，也不宣称 id/metadata 不影响上游处理。比较器必须保留完整 item 差异及每个已观察字段结果；未知/截断/旧 schema 仍写未知。

schema 3 的数字精确词法、任意深度 duplicate-key 拒绝、UTF-8/非有限数值拒绝、规范长度语义全部继承。每 item field/metadata field 上限仍为 512；截断会令 body_summary_complete=false，并保留完整 item hash及总数，不冒充字段已全比对。缺失 id、id=null、id="" 分别记录；metadata 缺失或 null 不伪造子摘要。

## 验证、版本与证据限制

新增三项测试：模拟两次合法 Lite tools+message 构造，验证 full item/hash 不同但各内容字段独立相同；真实内容/工具参数大整数变化可被识别；metadata/未知 key 不泄露及 missing/null/空 ID 区分；field 限额不丢全项 hash。旧 38 项全部保持，合计 **41 项，Windows 35 pass、6 个 Linux/POSIX skip、0 fail**，Python 3.9 grammar 与 py_compile 通过。日志 `.local/quality-20260925/ipc-schema4/windows-tests.log`。

```bash
python3 -m unittest discover -s tools -p test_quality_native_ipc_capture.py -v
```

主任务用真实 helper 环境变量在独立 network-none Linux 容器补验 41 项，再启动新版本实验；本专项不 SSH/调用模型。旧 schema 3 已由主任务在第一实验前验证 **38/38、0 skip**，原始日志 `.local/research-20260925/multiaccount-abba/capture-linux-tests.log`。

schema 4 工具 SHA-256：`240ea1b64116c68f9d6ba343ce61550104c562403b3c8e27ff22ace80e680719`；测试：`f26103a2be01e398a3cdf85ac1779c39564731a279612908aaad42a375137814`。schema 3 旧工具和测试独立保存在 `.local/quality-20260925/ipc-schema3/frozen/`；原实验 artifacts 和已固定远端 tool 路径不修改。新版本部署到不同实验目录，避免旧记录来源随源码更新而模糊。

此改动只增强实际 Go→Rust IPC 摘要；不修改请求正文、原字段、模型、TLS、头或转发。它缩小下一次比较的不确定范围，不改变旧实验结果，也不是原账号质量恢复结论。
