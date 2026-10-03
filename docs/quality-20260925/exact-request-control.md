# 固定完整请求跨账号对照：捕获与单次 native 重放

本工具链为下一步实验准备，未在本专项发送真实模型请求。目的是固定官方 CLI 已生成的完整 body（包括 Lite tools/content、全部 item ID、turn_id、create_time、cache/session 字段），跨账号只替换 Authorization 和 ChatGPT-Account-Id，以进一步区分账号因素与每次新会话生成字段的因素。不能从四次 reasoning_tokens=516 直接推断服务端预算或算力集群。

## 捕获一次官方 CLI 请求，但不向上游发送模型 POST

新增 `gateway_quality_exact_capture_test.go::TestQualityExactRequestCaptureOnly`。GET `/v1/models` 交独立 catalog proxy，走真实 Rust helper 取得当前账号目录；POST `/v1/responses` 交另一 capture proxy，经实际网关 policy、prepare、request headers builder、native RoundTripper 和 IPC writer。

capture proxy 的 IPC peer 是同一 Go test executable 的专用合成模式。它只有本地 listener，没有 HTTP client、net.Dial 或上游连接；读取真正 native IPC writer 发出的完整 head/body，保存一份私有文件后返回明确的 `response.failed / synthetic_capture_only`。CLI 因合成错误退出是预期捕获结果，不能评分、不能当作一次模型质量调用。入口只接受一个 POST，后续 POST 一律 409，不因 CLI 重试再捕获或放行。GET 仍可并发，且不会使用 capture peer。

配置由 `CODEX_QUALITY_EXACT_CAPTURE_CONFIG` 指定，结构：

```json
{"credential_file":"/private/at-only.json","output":"/private/new-capture","settings":{}}
```

实际 settings 需使用完整有效网关 settings；fixture 强制四项透明策略、关闭自动刷新/401恢复。`output` 必须不存在，启动以 0700 创建。凭据为 `access_token/account_id/exp`，不提供 RT。`CODEX_NATIVE_HTTP_TEST_BIN` 指真实 Rust helper，仅供 catalog GET；Go fixture 使用现有 opt-in server 方式启动。

ready.json 返回 `base_url`、`model_post_forwarded=false`、`capture_request_limit=1`。把官方 CLI HTTP provider endpoint 指向该 loopback URL；请求前保持原题、model/effort、完整官方包和固定 cwd。捕获结束写 `output/stop` 并等待进程退出。

产物：

- `request.body`：实际发送到 capture peer 的原始 prepared JSON bytes，位于 Rust zstd/TLS 之前。
- `request-head.json`：schema=`codex-quality-captured-request-v1`，method/path/完整 no-auth header pairs/base64 值、native request_compression、body SHA/bytes。只从实际 IPC head 剥离 Authorization 与 Account-ID；其他身份/会话字段保留固定值。
- `capture-report.json`：必须同时确认 `capture_posts=1`、`extra_posts_refused=0`、`model_inference_requests=0`、`synthetic_result=true`、`capture_complete=true`，并固定 body/head SHA。

raw body/head 是专门授权的服务器私有 0600 文件，会包含提示词和会话 ID；不是可公开的摘要。文件 O_EXCL 创建，不覆盖历史。捕获 credential 自身不写入 head/body，清理由主任务执行。不要把这些私有 raw 文件复制成本地公开报告。

## 读取捕获文件，执行一次真正 Rust native 请求

`tools/quality_exact_request_probe.py` 入口示例：

```bash
python3 tools/quality_exact_request_probe.py --body-file /private/capture/request.body --head-file /private/capture/request-head.json --credential-file /private/q0-at.json --helper /private/codex-native-transport --expected-body-sha256 BODY_SHA --expected-head-sha256 HEAD_SHA --expected-helper-sha256 HELPER_SHA --output /private/results/01-q0.json --alias q0 --timeout 240 --allow-live
```

这条命令会发送一个模型 POST，因此当前只作为经主任务审阅后的执行路径。未给 `--allow-live`、无有效 pins、凭据/捕获文件不是私有普通文件、输出已存在均在发送前失败。输出父目录需私有。capture 必须 full first turn、stream=true/store=false；不跨账号搬运 previous_response_id。接受内容重复键拒绝只针对本实验是否可精确比对，不改变网关产品的兼容性。

每个 cell 启动新的真实 helper，固定 SHA 并从 `/proc/<pid>/exe` 复核运行文件。配置 cli_request，隔离环境只留 PATH/LANG；无自制 HTTP/TLS、无 cookie/Chrome 模拟、无隐式刷新、无自动 retry/fallback。原 capture 的 zstd 仍由 Rust 原 encoder 执行，Python 不重新序列化 body、也不自行压缩。non-auth headers 的顺序、base64 bytes、session/cache/turn 字段原样保留；只追加当前 AT/Account-ID 及 IPC 管理字段和实际 body length。

绝对总期限最多 240 秒，read/write 每次短 socket 调用前重新扣除剩余预算，包含等待 header/body；ready 最多 9 秒且不超过总预算。已发送失败不会重新调用。截止或协议错误释放 socket，关闭 stdin 并回收 helper；最多再等 5 秒后必要时 kill，报告清楚记录回收/强杀。这几秒仅为本地清理，不是新增模型请求预算。

## 结果与归因边界

报告记录：完整 body SHA、captured head SHA、实际 IPC head+body SHA、成功写入的 IPC 字节数、所有 non-auth 头摘要、AT/workspace 摘要、schema 4 分层 body 摘要及 helper SHA。`local_ipc_write_complete` 只证明本地 socket 接受，不单独代表上游已处理；HTTP/终态另外观察。

SSE 支持 LF/CRLF/CR 和 UTF-8 分块，created/in_progress/terminal 中记录可见 model/service_tier、reasoning effort/summary/context/mode、已知 numeric budget、max_output_tokens、usage/output_tokens_details 的数字字段；未知 reasoning 值只存摘要。每个阶段分开，不把 auto→default 当作后台降档，也不把 reasoning_tokens 当作隐藏最大预算。

单请求 collector 绑定 response ID，拒绝 top/nested 相互冲突、foreign done 或 foreign terminal。只有 HTTP 200、真正 response.completed、status=completed、非空 response ID、无错误时才可评分；无 final、冲突 final 或上游失败都不能作为错答/正确答案。完整最终 assistant 输出会脱敏保留，原输入正文、AT/RT、workspace 值和上游错误 message 原文不进入报告；错误 code/type/status 保留，message 仅 hash/长度。

每个 cell 的账号来源、前后 epoch/分配身份/额度、捕获来源及预登记顺序仍由外层 runner 负责。把同一 fixed session/id/body 用于多账号后若服务端拒绝，也保留为该条件下协议结果，不按答案重试或修改固定字段补成“成功”。当前已知异常候选额度不足，主任务停止模型调用，先完成离线验证；不以 429 代替数学失败。

## 离线验证与冻结

Python **15 项，13 pass、2 个 POSIX/真实 helper 环境 skip、0 fail**；测试包含 exact body/non-auth header不变、AT-only/JWT expiry/private input、zstd仅标志、跨 response 冲突拒评分、每次短读/写绝对 deadline、SSE失败分类、预算/usage阶段字段和敏感值不泄露。Linux 提供 `CODEX_NATIVE_HTTP_TEST_BIN` 后，额外真实 Rust→loopback 验证一个且仅一个 POST、原字节、原 session、helper 回收，完全无需账号。Py3.9 grammar/py_compile 通过。

Go `TestQualityExactCaptureOffline*` 两项在本机通过：普通与 zstd 入站都经过实际 native IPC writer，到无上游网络的 peer；body等于prepared bytes，压缩意图保留，GET/catalog 与 POST/capture分流，一次POST后409，无认证头落入 capture head。日志 `.local/quality-20260925/exact-request-probe/`。

```bash
python3 -m unittest discover -s tools -p test_quality_exact_request_probe.py -v
go test . -run '^TestQualityExactCaptureOffline' -count=1 -timeout=60s
```

冻结 SHA-256：Python probe `69bb112852a92a253ce5fc32568db9b1bf766e01bed8d7d6575feee89101bcdf`；Python tests `4968b5ae8111b3824aaf0a73c51f19e3d23f924eab510587c7864e560f6ab72a`；Go fixture `c4573190e3d9cfada4f8919d8e0ffffa946a75a6db8670aa5c7dc7368ba351dd`。

本次仅新增 test-only 工具/fixture，没有修改生产网关。最终 Linux真实性验证和捕获执行由主任务完成，本报告不预先声称已取得 fixed-body 真实账号对照结果。

主任务已完成最终 Linux 验证：network-none Python 15 项全过、0 跳过（0.665 秒），Go race 捕获两项全过、0 跳过（3.709 秒），vet 通过。完整官方 CLI capture-only 也通过，实际保存 36,352 字节请求，零模型 POST 外发，私有请求体/头和 AT 已删除；`execution.json` 明确 `max_model_calls=0`。证据 `.local/research-20260925/exact-account-control/`，六格账号对照及后续入口见 [多账号研究](../CODEX-QUALITY-MULTIACCOUNT-2026-09-25.md)。仍没有固定完整正文跨账号的真实质量结果：q5/q6 已达配额 100%，启动脚本在前置检查返回不启动，没有拿 429 填补样本。
