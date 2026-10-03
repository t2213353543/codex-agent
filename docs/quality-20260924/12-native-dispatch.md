# 原生 HTTP 实际 IPC 观测（仅测试工具）

日期：2026-09-24。工具：`tools/quality_native_ipc_capture.py`；回归：`tools/test_quality_native_ipc_capture.py`。

本补充解决旧实验报告 `predicted_upstream_body_sha256` 只是根据策略重算、没有独立观测真正调度字节的问题。不修改生产 Go/Rust 源码、生产配置或账号状态；不调用真实模型，不复制真实账号凭据。实际账号试验由根任务独立执行并记录。

## 观测边界

已读取当前 `gateway_native_http.go`、`gateway_native_startup.go`、`gateway_native_identity.go`、`gateway_native_ws.go` 和 `native-transport/src/main.rs` 确认以下协议：

1. Go 通过 stdin 发送 4 字节大端长度加 JSON 启动配置。真实 Rust helper 回传一行 JSON readiness；Go 检查 Unix socket、0600 权限、生命周期、能力和并发约定。
2. 每条 HTTP 请求通过独立 Unix socket 连接发送 4 字节大端长度、JSON 请求头、精确 `body_length` 字节请求体。请求头的 HTTP header 值采用 base64，以保留重复项及原始字节。
3. 正文发送后客户端保持写端打开；EOF 或其他后续字节具有取消意义。响应是独立 framed byte stream。
4. `accountScope` 仅存在于 Go 的 helper pool key，不在启动配置或请求 IPC 中。传输边界不能观测这个值。

工具生成一个 0700 launcher，供测试环境 `CODEX_NATIVE_HTTP_TEST_BIN` 使用。launcher 固定真实 Rust helper 路径及 SHA256，并固定工具文件 SHA256。它把启动配置中的本地 `unix_socket` 从 Go 生成的私有目录下 `s` 改为同目录 `r`，让真实 helper 监听 `r`，自身在 `s` 提供透明中继；readiness 仅恢复这个 socket 字段。启动配置的其余值、子进程环境及真实 helper 的能力字段保持不变。

请求与响应均按原始字节转发，不重新序列化请求头或 JSON 正文。只在 backend socket `send` 返回成功接受的字节之后更新观测；短写循环处理。两条方向独立传输，客户端 EOF 会传给真实 helper，不会在正文完成时提前关闭写端。没有增加 Go HTTP fallback、TLS 代理、证书替换、HTTP header 或推理提示。

## 脱敏记录与可证明内容

每个 wrapper 实例生成独立 0600 JSONL，目录必须属于当前用户且为私有目录；多连接写入经锁串行化。原始请求头、正文、bearer token、workspace ID 和完整启动配置不落盘。

| 字段 | 直接证据及范围 |
| --- | --- |
| `actual_ipc_body_sha256` | 已被真实 helper 的本地 socket 接受的 HTTP body 原始字节 SHA256。必须同时检查 `request_forwarded_complete=true`；截断时只代表部分 body。 |
| `actual_ipc_request_sha256` | 4 字节前缀、原始 IPC head、body 的 SHA256；不包含 body 之后的取消字节。 |
| `authorization[].token_sha256` | 实际 IPC Authorization 的 Bearer 部分哈希；重复 header 分别保留。 |
| `workspace_header_sha256` | 实际 IPC `ChatGPT-Account-Id` header 值哈希，表示发送的 workspace 声明，不表示服务端已认可。 |
| `headers.x-client-request-id` | 仅安全的 `quality-...`/UUID 格式可保留值，同时记录值哈希，供同一实验结果关联。 |
| `model`、`reasoning_effort`、`service_tier` | 完整 body 内观察到的字段，采用限制字符/枚举；不推断服务端实际使用的模型、算力或 effort。 |
| `instance`、`helper_pid`、`connection` | 本次 wrapper、真实 helper 进程及本地连接的关联；不是 Go `accountScope`。 |
| `configured_helper_sha256` | 执行前校验的 helper 文件 SHA256。 |
| `runtime_executable_sha256` | Linux `/proc/<pid>/exe` 的运行时可执行文件 SHA256；`runtime_executable_matches_helper` 区分 ELF helper 和合成 Python fixture。 |
| `ca_environment` | 三个 CA 环境变量的存在性与值哈希，环境本身不修改。不能单凭这个字段宣称已抓到 TLS ClientHello。 |
| `native_ipc_helper_exit` | 子进程退出码、是否已 reap、是否被强制终止，以及两个本地 socket 是否已清理。 |

普通 header 只保留显式允许集合；session/conversation 等只记录哈希。已知 token/workspace 若被放入允许明文字段也会被剔除。HTTP JSON body 的输入、instructions、工具参数及模型输出不写入记录。WS 流可按原始字节透明传递，但观测只覆盖 handshake，显式记为 `handshake_only_no_message_capture`，不把它计作 WS message 证据。

## 验证

Windows 本地冻结时执行：

```powershell
python -m unittest discover -s tools -p test_quality_native_ipc_capture.py -v
```

结果：23 项，18 项通过、5 项因 POSIX/Linux 能力明确跳过，无失败。通过项涵盖：

- 短读、短写、目的端中途失败时只观测成功发送部分。
- 前缀/head/body 多种截断、非法/超限长度、错误 JSON，以及任意分片下原字节 SHA256 一致性。
- 大请求与大响应双向传输、12 条并发连接不串数据、客户端关闭取消传播、不提前 half-close。
- 非法观测内容仍原样传给真实下游协议端，不以观察器解析结果改写请求。
- 明文凭据/正文不进入脱敏记录、已知凭据混入允许字段的剔除、并发 JSONL 完整性。
- launcher 使用参数列表和 Python 字面量固定路径，不经过 shell 插值，并校验工具与 helper 哈希。

POSIX 专项包含完整 launcher 的合成 native 协议、startup 无请求 EOF、stdin 仍打开时 SIGTERM、双 socket/子进程清理、原始启动配置与 CA 环境值保持不变。真实 Rust 专项只访问本地 `ThreadingHTTPServer`，比较本地服务器收到的 body 与 IPC 实际 body，校验 `/proc/<pid>/exe` 身份；不发模型请求。以下命令启用这个真实 helper 专项：

```sh
CODEX_NATIVE_HTTP_TEST_BIN=/absolute/path/to/codex-native-transport python3 -m unittest discover -s tools -p test_quality_native_ipc_capture.py -v
```

根任务已在 Linux Python 3.9 上执行本次真实实验所用的 schema 1 冻结源码：**23 项全部通过、0 skip，2.526 秒**，包括真实 Rust helper 的本地 HTTP、startup 无请求 EOF 与 SIGTERM 子进程/双 socket 回收。日志为 `.local/quality-20260924/ipc-tests-linux.log`。随后既有 `TestGatewayNativeActualRoutesAndOAuth` 通过，SSE、JSON、compact、models 四个本地真实 Go→wrapper→Rust 子项全部通过，2.39 秒；日志为 `.local/quality-20260924/ipc-go-loopback.log`。

## 一次真实原题 IPC 结果（schema 1 历史证据）

根任务于 `2026-09-24T12:35:17Z` 对 q4 执行一次有界透明网关原题请求，模型 `gpt-5.6-sol`、effort `medium`。结果 HTTP 200、`response.completed`，最终答案 **29**；不将这次请求计为原题恢复成功。

原始脱敏材料位于 `.local/quality-20260924/dispatch-live-q4/`；独立离线复核为 `.local/quality-20260924/dispatch-live-q4-review.json`，18 项完整性检查通过。实际请求 925 字节，以下三个 SHA256 完全相同：输入 body、旧的 predicted body、实际发送给真实 helper 的 IPC body：

```text
763f3380b4807815c1dab11c2baf7586e4466e3bcc4a4e668c658f0be69df7f5
```

实际 IPC 的 token/workspace 哈希与服务器内私有快照一致，`x-client-request-id` 与该次 Go 结果一致；请求前后账号身份及 credential epoch 一致。这里的 workspace 核对是服务器本地对快照的比较，不是读取提供方内部账号路由。请求 model/effort 与预期一致，没有额外指定 tier，实际 routing hint 正确，三个自定义 CA 环境变量均不存在。

共观察到 2 个真实 Rust helper（含 startup validation）和 1 次实际请求。两个 helper 的 `/proc/<pid>/exe` 哈希均匹配固定 Rust ELF；两组 start/exit 一一配对，helper 退出码均为 0、已 reap、没有强制终止、内外两个 socket 均已删除。根任务随后确认 2 个 wrapper 与 2 个 helper 的 `/proc` 均不存在，临时凭据快照已删除。

这次观测把“网关是否实际改动了这份正文或换了凭据”的证据，从预测提升到发送边界核对；原题仍然答 29，不产生账号恢复、服务端隐藏集群或算力标记的证明。

## 后续 schema 2 修正：观测失败不得中断请求

独立复核发现 schema 1 工具有一个诊断健壮性问题：若 `EventSink.emit` 写盘抛出 `OSError`，异常会从 `observer.feed` 进入传输失败分支并提前 `SHUT_WR`。已完成的真实请求没有触发此错误（完整记录和两组正常退出均存在）；但这个失败分支必须修复，不能把工具稳定性问题带入未来实验。

工作区 schema 2 将观测异常隔离在观察器内部：失败时立即释放正文/head 的内存副本并停止该实例的后续记录，传输方向继续转发原始字节，不提前 EOF。实例尽力创建 0600 `.invalid` 标记；若文件系统连标记都无法写入，仍永久保持内存失效状态，正常请求结束及自然清理后 wrapper 返回 1，并不生成健康的 helper_exit 记录。成功的 schema 2 helper_exit 额外包含 `observation_healthy=true`。

schema 2 的有效证据必须同时满足：完整请求记录、匹配的正常 start/exit、`observation_healthy=true`、wrapper 正常退出、无 `.invalid` 或不完整 JSONL。不能只看到请求的 `request_forwarded_complete=true` 就认可整次捕获；在失败之前写出的旧行也不能替代最终健康性验证。缺少标记本身不是健康证明。

新增 5 项故障注入：完成请求时写盘异常、观察器处理到一半异常、磁盘部分写后失败、停止重试/永久失效，以及完整 launcher 在观测失效后仍完成第二个请求并清理子进程。双向大字节传输测试还在后端检查 **没有提前 EOF**，避免“请求字节刚好发完”掩盖取消信号。Windows schema 2：28 项，22 通过、6 POSIX 明确跳过。

schema 2 在独立 Linux Python 3.9 目录的结果：**28 项全部通过、0 skip，2.984 秒**，日志 `.local/quality-20260924/ipc-v2-tests-linux.log`；使用新 launcher 和同一固定真实 Rust helper 的 `TestGatewayNativeActualRoutesAndOAuth` 四子项也全部通过，2.33 秒，日志 `.local/quality-20260924/ipc-v2-go-loopback.log`。这些是新工具的离线验证，不与上面的 schema 1 23 项历史结果混写，也不增加真实模型请求。

schema 1 原始工具与测试完整保留于 `.local/quality-20260924/capture-live-v1/`，真实实验所用 remote launcher 与脚本没有替换。schema 2 仅在新隔离目录做离线回归，不重新调用模型，不覆盖真实实验材料。

## 接入现有隔离试验

先在测试主机生成 launcher；路径都应为绝对路径，输出目录是新建的私有测试目录。示例中的 helper SHA 是本次已部署的固定 Rust helper：

```sh
python3 /absolute/tools/quality_native_ipc_capture.py install --helper /absolute/codex-native-transport --expected-helper-sha256 f72e5c0ff8798c02f430f7aa36f74009dad40fade6111dad722390891239b556 --launcher /absolute/quality-native-launcher --output /absolute/capture-output --python /usr/bin/python3
```

原有 Go probe 只需把 `CODEX_NATIVE_HTTP_TEST_BIN` 指向此 launcher。示意单命令（原有 private config 仍留在服务器）：

```sh
CODEX_NATIVE_HTTP_TEST_BIN=/absolute/quality-native-launcher CODEX_QUALITY_PROBE_CONFIG=/absolute/private-config.json /absolute/probe.test -test.run '^TestGatewayAccountQualityLive$' -test.v -test.timeout 5m
```

launcher 的参数固化是必要的：`gatewayNativeEnvironment()` 仅把 PATH 和三个既有 CA 环境变量传给 helper，不能依赖新增的 capture 配置环境变量。先跑 Linux 离线专项与既有 Go 原生路由/启动测试，再用同一 launcher 执行一次有界真实账号实验。通过 `x-client-request-id`、body SHA、token SHA、workspace SHA 和 helper instance 关联结果；保留原题，不注入预期答案。

## 明确不能证明的内容

这不是 upstream wire 抓包，body 哈希边界在 Rust helper 压缩、HTTP 序列化和 TLS 之前；`send` 完成表示本地 socket 已接受字节，不等价于服务端接收或接受。真实 Rust 的 HTTP/TLS 实现与运行时身份仍需用固定 helper、环境和已有协议/TLS 对照共同解释。

不能从该观测声称获得 Go `accountScope` 值、服务端集群位置、账号被标记、服务端实际推理算力、模型权重，或仅凭算题答案就证明上述因果。测试 wrapper 会增加本地进程/内存与 IPC 复制开销，因此只用于隔离诊断；不用于生产性能数据，不部署为生产 helper。

真实实验 schema 1 冻结源码 SHA256（保存在 `capture-live-v1/`）：

- `quality_native_ipc_capture.py`：`2bd345da020e8749bec42bb6759a5583a9fa4d6abdd3a98477b4774dc699de68`
- `test_quality_native_ipc_capture.py`：`230c8b358f84998a0296647cc3f0bb76dce7b5ce722374b7b935d0e69440d663`

工作区 schema 2 冻结源码 SHA256：

- `quality_native_ipc_capture.py`：`43a78d93687172f6e5c1000e2f20bea07381a3c3ff7f3c47d48fba4d6350bc54`
- `test_quality_native_ipc_capture.py`：`f99e761f1a162752ee19919b0baefebc22894a400ba1d5c278e4e6120459bdfb`
