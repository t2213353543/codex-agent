# 网关环境合成压力验收

在项目根目录运行：

```powershell
python integration/gateway_environment/run.py
```

输出在 `.local/gateway-environment/<UTC时间>/report.json`，同目录保留 Go 原始 JSONL 与 overlay 映射。每次使用新目录，不覆盖历史报告。运行约 3 秒（不含首次编译）。需要项目要求的 Go 1.26 和 Python 3。

此套件通过 `go test -overlay` 将 `environment_test.go.txt` 映射为根包的虚拟 `zz_gateway_environment_overlay_test.go`，不写根目录 Go 文件。使用 `.txt` 扩展名是为了避免普通 `go test ./...` 将其误当独立 Go package 编译。所有账号、token、请求内容、代理均为合成数据；上游是 `httptest` 创建的 `127.0.0.1` 随机端口 HTTP 服务，不接受生产地址，也不请求真实模型。连接中的 `synthetic.invalid` 是本地 HTTP 代理看到的绝对形式请求目标，本地 handler 直接响应，不进行 DNS/外部转发。

## 五个验收边界

| 测试 | 负载与故障 | 断言与观测 |
| --- | --- | --- |
| SlowBoundAndReuse | 64 个同时启动的请求，单请求延迟 15 ms，显式每 route 上限 8 个连接 | 实际 accept ≤ 8、同时上游请求 ≤ 8、连接复用 ≥ 56 次、只创建 1 个池条目；记录 P50/P95 |
| FailuresNoRetry | 64 请求并发进入真实 `Proxy.ServeHTTP`，各 16 个 429、502、503、TCP RST | 每个请求实际到达本地上游恰好 1 次；429 的 `Retry-After: 7` 原样返回；RST 映射客户端 502；账号 in-flight 归零 |
| CancellationReleasesConnections | 4 个活动慢请求 + 16 个等待连接请求，一起取消 | 20 请求均得到取消；上游活动请求清零；相同池后续请求成功；记录取消到恢复耗时 |
| AccountRouteIsolation | 2 个合成账号各绑定独立代理，64 次请求交错并发 | `pickProxy` 选路后真实本地 socket 收到对应账号凭据和身份头；两 route 没有串用；每 route accept ≤ 4；池条目恰好 2 个 |
| PoolCapacityAndDefaults | 创建池上限 + 17 条合成 route（当前 145 条），不实际拨号 | 元数据条目上限符合实现（当前 128）；独立记录产品默认 `MaxConnsPerHost` / idle 配置，避免将条目上限误认为连接上限 |

首轮普通测试证据：`.local/gateway-environment/20260917T193454.904941Z/report.json`（北京时间 2026-09-18 03:34）。五组全部通过，执行前后根包 Go 文件、模块与 harness 哈希一致。

| 指标 | 首轮观测 |
| --- | --- |
| 慢请求实际连接/峰值活动 | 8 / 8 |
| 请求连接复用 | 56 / 64 |
| 本地慢请求 P50 / P95 | 67.444 / 127.532 ms |
| 故障客户端 HTTP 分布 | 429 × 16、502 × 32（包括 RST）、503 × 16 |
| 故障上游请求总数 / 单请求最大尝试 | 64 / 1 |
| 取消请求 / 取消并恢复 | 20 / 0.535 ms |
| 账号身份串用 | 0 |
| 两 route 实际连接 | 4 / 4 |
| 池条目数 | 128 / 145 创建 |

**本轮产品默认 `MaxConnsPerHost=0`，不具有 Transport 级单 host 活动连接上限。** 测试中的 8/4 是有意注入的配置，证明池克隆保留并执行上限，不能据此宣称产品默认已经限制连接。默认 `MaxIdleConns=100`、`MaxIdleConnsPerHost=0`（Go Transport 使用其默认值）。该事实已反馈源码 owner；这里不擅自修改 Go 实现。账号并发限制是不同层面的约束，不能用 idle/池条目数替代活动连接预算。

## Race 与限制

工具链具备 CGO 和 C 编译器时运行：

```powershell
$env:CGO_ENABLED='1'
python integration/gateway_environment/run.py --race
```

当前 Windows 的实际尝试没有执行 race 测试：默认 CGO=0 报 `go: -race requires cgo`；显式 CGO=1 后报 `cgo: C compiler "gcc" not found`。原始结果分别在 `.local/gateway-environment/20260917T193518.529957Z/`、`.local/gateway-environment/20260917T193526.287673Z/`。报告保持非零退出码与 `race_executed=false`，不把环境失败当作 race 通过。可在已有 Linux CGO 工具链上执行同一入口，不需要新增账号或生产依赖。

本套件只证明受控本地 HTTP/1.1 场景，没有覆盖真实上游容量、TLS 指纹、HTTP/2、SOCKS5、WebSocket 或真实模型推理。账号隔离测试覆盖选路和连接池/头部隔离，不替代 API key 多租户授权测试。P50/P95 是本机回归观察，不是生产容量承诺。所有测试前后会记录根包源文件哈希；如果其他会话同时改文件，即使测试本身通过，报告也会标记失败并列出变更，要求对最终冻结源码重新验收。

## Linux 实际 race 验收（2026-09-18）

在已配置本项目 SSH 连接的工作站运行：

```powershell
python integration/gateway_environment/linux_race.py
```

本轮通过用户明确授权的 `CODEX_AGENT_SSH_PROXY=http://127.0.0.1:17897` 连接当前主节点；不改全局代理或技能。脚本只使用已存在的 `codex-agent-integration-race` 编译镜像，在 `/opt/codex-agent-integration/gateway-environment-race/` 新建唯一快照目录；不安装宿主包，不改生产服务。根 Go、go.mod/go.sum、migrations 与 `.txt` overlay 按逐文件哈希冻结；依赖从工作站现有 module cache 离线 vendor 后打包，远端上传 SHA256 复核。容器仅只读挂载该快照，2 CPU / 2 GiB / 128 PID，`--network none`，GOPROXY/GOSUMDB=off，仅运行五组 `TestGatewayEnvironment`。容器退出后删除，源文件与证据保留。`--prepare-only` 只生成本地离线快照，不连接服务器。

测试适配了已落盘的新准入/429 语义：故障矩阵的 64 个请求使用独立合成模型 scope，保证每个受控 HTTP 错误都真正到达本地上游；另追加一次同模型请求，必须被已有冷却拦在本地。准入泄漏检查改用稳定账号 ID 派生的 `gatewayAdmissionKey`。没有关闭或绕过产品冷却，也没有改 Go 实现。普通测试先通过，再对冻结版本执行 race。

实际结果：**5/5 PASS，race_executed=true，无 DATA RACE，exit=0，未 OOM，临时容器已删除，冻结前后源文件未变化**。工具链 Go 1.26.8 linux/amd64、GCC 15.2.0、CGO_ENABLED=1；包含依赖打包、上传和冷编译共 59.910 秒。生产请求数 0。

```text
报告：.local/gateway-environment-linux-race/20260917T200319.786588Z/report.json
原始输出：同目录 go-test.jsonl
逐文件清单：同目录 source-manifest.json
snapshot_sha256=bee29a4dd87fd9bb278ff2a7548d42b8dded90104d7f803715dc4cee160bd462
archive_sha256=9f8d3f9c31126b2e8249870f20c724c6fa7f234730ba7ddaa28741ce3f7e83bf
image_id=sha256:eb1d21756fc64fbd95138b52362d6952f31898a666d57b5bdc02eb22bdf4f556
```

| 指标 | Linux race 实测 |
| --- | --- |
| 慢请求 P50 / P95 | 67.971 / 127.266 ms |
| 连接 / 峰值上游活动 / 复用 | 8 / 8 / 56 of 64 |
| 故障 HTTP 分布 | 429×16、502×32（含 RST）、503×16 |
| 故障矩阵上游请求 / 单请求尝试 | 64 / 1 |
| 同模型冷却后续请求 | 本地 429，上游无新增请求 |
| 20 请求取消及恢复 | 6.597 ms |
| 账号头部串用 / 两 route 连接 | 0 / 各 4 |
| 池条目 / 创建 route | 128 / 145 |
| 默认 MaxConnsPerHost / MaxIdleConnsPerHost | 0 / 100 |

这些数据带有 race 检测开销，只作为合成回归证据。默认 `MaxConnsPerHost=0` 仍不能当作 Transport 活动连接有界；8/4 是 harness 显式配置。该结果属于上述快照，不能替代后续变更复验、全量 Go race、真实上游容量或部署验收。

## 已授权的真实 30 / 50 / 70 / 100 并发工具

`live_load.py` 是独立 Python 标准库工具，可直接复制到服务器本地运行，不依赖 pip 包，不修改网关、账号或代理配置。**编写此工具时只完成本地 stub 自测，没有执行真实请求。** 实际运行由 root 在确认同步账号与当前网关版本后进行。

推荐服务器本地通过 loopback 调用本项目网关，排除工作站网络或本地代理对负载的影响。由 root 提供现有私密 key 文件及包含已核对版本哈希的 metadata 文件：

```bash
python3 integration/gateway_environment/live_load.py --base-url http://127.0.0.1:18879/v1 --model MODEL --key-file /private/existing-gateway-key --levels 30,50,70,100 --waves 2 --timeout 120 --cooldown 10 --max-cooldown 120 --metadata-file /private/load-metadata.json --output /private/load-result-NEW
```

路径只是命令模板，`18879` 和 `MODEL` 必须由执行者核对实际目标后替换；输出目录必须不存在，避免覆盖证据。也可以用 `--key-env VARIABLE_NAME` 从现有环境变量读取，不能直接把 key 作为参数传入。脚本不会把凭据、完整目标地址、账号身份、模型输出或上游错误原文写入报告。`request_id` 是工具生成的合成关联 ID；`observed_request_id` 与 `observed_response_id` 只保留通过格式白名单的上游关联 ID，供服务端日志核对。模型名与版本 SHA 属于报告的一部分。

metadata 的允许字段如下，不接受其他字段或明文注释，以免误将密钥写进报告：

```json
{
  "binary_sha256": "64位十六进制哈希",
  "source_sha256": "64位十六进制哈希",
  "snapshot_sha256": "64位十六进制哈希"
}
```

字段可按已有证据提供子集。工具只绑定执行者给出的哈希，不声称自行核验远端运行二进制；root 应将其与 server-local 实际文件哈希核对。

默认先执行 1 次单请求 sanity，只有收到结构有效且 `response.status=completed` 的 `response.completed` 才进入负载。随后每档 2 波，每波通过线程屏障同时开始指定数量的请求；四档共 **500 次负载请求 + 1 次 sanity**，每请求仅 1 次尝试，不做任何自动重试，固定合成输入要求简短回复 `OK`。

- 每波结束后默认冷却 10 秒；不同波之间不重叠。单请求默认 120 秒期限，活动 socket 由独立 watchdog 在期限结束时关闭，SSE 读取亦检查总期限。建议使用 loopback/IP，避免操作系统 DNS 解析时间成为未测量的外围因素。
- 遇到 429，先让当前已发出的有界波结束，按该波最大的 `Retry-After` 与配置冷却值取较大值冷却，默认随后停止，不进入下一波/档。显式传 `--continue-after-429` 才会冷却后继续剩余有限波；这不是失败请求重试。
- `Retry-After` 支持秒数与 HTTP date。如果要求等待的时间超过 `--max-cooldown`，记录 `next_allowed_after_seconds` 并停止，绝不缩短服务端要求后继续请求。
- 遇到 401/403 或整波全部 timeout，结束当前波后停止。502/503/SSE 失败继续既定有限波，便于观察并发分布。无无限循环、无无限重试。
- 每次成功、失败终态或每波完成都会形成结果；报告在每波结束后原子写盘。`completed` 表示全部负载终态成功，`completed_with_failures` 表示计划波次执行完但有失败，`stopped_*` 表示因明确边界提前停止。CLI 只有全成功返回 0。

报告区分以下边界：

| 字段/指标 | 含义 |
| --- | --- |
| `level` / `client_peak_inflight` | 计划并发 / 工具实际处于拨号、等待、读取中的请求峰值，不等于网关 admission 或上游活动推理数 |
| `observed_gateway_local_rejections` | 根据现有 `proxy_error` 与 busy 错误形状识别的本地账号并发拒绝 |
| `upstream_peak_inflight` / `upstream_admitted_requests` | 默认 `null`；必须另取服务端证据，不能从 HTTP 200 数反推 |
| `origin=gateway_transport` | 网关生成的 upstream connection failed，与上游 HTTP 502 不混淆 |
| `origin=upstream_payload_inferred` / `unknown` | 供应商形状错误正文仅作推断；仅凭 HTTP 502/503 无法确认来源 |
| `success` | HTTP 200、SSE 且完整 `response.completed`、response.status=completed 同时满足；`response.failed`、`response.incomplete`、`error`、缺失终态、损坏 SSE 均失败 |
| `ttft_ms` | 首个非空 `response.output_text.delta` 到达的时间；不是 response.created/HTTP 头耗时；无文字 delta 则 null |
| P50/P95 | 总延迟覆盖全部负载请求；TTFT 分位只统计 completed 且有 delta 的请求，并显示样本数 |
| `observed_usage` | 只累计收到的终态 usage；中断/缺失终态仍可能产生未观测的计费量，不能用作账单总量 |
| `summary_scope` | 聚合仅覆盖负载波次；sanity 的单独结果和 usage 在 `sanity` 字段 |

单事件上限 1 MiB、单响应上限 16 MiB；原始 SSE 只在内存中有界解析，不落盘。每请求独立 HTTP/1.1 连接，避免测试端共享小连接池将标称 100 并发变成串行，但这也意味着测试不代表官方 CLI 的连接复用或 TLS 指纹。

真实工具的本地自测命令：

```powershell
python -m unittest discover -s integration/gateway_environment -p test_live_load.py -v
```

已通过 9 组压力工具自测：完整四档两波（501 次本地 HTTP 请求）、SSE 分块/多行/EOF、HTTP 200 失败终态、429 冷却停止、Retry-After 超预算停止、502/503 来源区分、断流/reset/timeout、metadata 白名单及错误码/关联 ID 脱敏。测试 server 只监听 `127.0.0.1`，不读取环境中的真实 key。

`error_code` 只保留以下类别中的明确白名单值：`token_revoked`、`invalid_token`、`invalid_api_key`、`authentication_error`；`rate_limit_exceeded`、`rate_limit_error`、`usage_limit`、`usage_limit_reached`、`insufficient_quota`；`overloaded`、`overloaded_error`、`server_error`、`internal_server_error`、`service_unavailable`、`temporarily_unavailable`、`upstream_error`、`proxy_error`。其他有 code 的错误仅记 `other`；绝不把错误 message 当作 code 保存。聚合报告另有 `error_codes` 分布。SSE 终态中的明确限流/配额码即使 HTTP 为 200，也会触发冷却逻辑。

关联 ID 限制为 8–128 字符：请求 ID 仅 `req_` / `req-` / `request_` / `request-` 前缀加字母数字下划线连字符，或者 hex / UUID；响应 ID 仅 `resp_` / `resp-` 前缀加同类字符。邮箱、空白、换行、Bearer 和超长值都丢弃。字段带 observed 前缀，保留来源 header 名，不凭 ID 单独断定故障来源。

## 服务端只读 admission 与资源采样

`monitor.py` 是独立标准库脚本，需在目标 Linux 主机运行。root 已确定此次目标是本项目 loopback `18879`，执行时仍需核对该服务 PID 与私密管理 key 文件：

```bash
python3 integration/gateway_environment/monitor.py --pid ACTUAL_GATEWAY_PID --admin-base http://127.0.0.1:18879 --key-file /private/existing-admin-key --interval 0.2 --duration 600 --request-timeout 1 --stop-file /private/load-monitor.stop --output /private/monitor-result-NEW
```

先启动 monitor，再运行负载；负载结束后创建 `--stop-file` 指定的文件即可停止采样。若文件已存在，立即停止且不采样。最长 duration 为 7200 秒，每次 API 请求有 socket watchdog，默认最多 1 秒；stop-file 每次采样前及等待期间检查。使用已有服务 PID，不扫描/改写其他项目进程或配置，不读 `/proc/<pid>/environ`、cmdline 或凭据字段。

采样范围：

- 每次 `GET /api/accounts?limit=500` 只读取账号 ID（缺失时 name）用于 HMAC 匿名别名，以及 `in_flight`、配置 concurrency、enabled；不保存原响应和其他字段。HMAC 使用本次运行随机内存密钥，不持久化，别名在同一运行内稳定、跨运行不保证相同。
- `gateway_admitted_peak_observed` 和各别名峰值反映账号侧 `in_flight`，**包括刷新凭据、等待连接或其他网关内工作，不代表已到达上游的并发推理**。`upstream_peak_inflight` 始终 null，必须另取上游证据。采样可能错过短峰值；各账号独立峰值不能相加当全局峰值。
- API 返回 `has_more=true` 或缺少 in_flight 时标记 `scope_complete=false`，只给可观测下界，不把当前页当全部账号。当前五个账号应在一页内；认证 401/403 时采样立即停止。
- `/proc/<pid>/stat` 的 CPU ticks、RSS、线程数；`fd` 数量；以该 PID 的 socket inode 过滤 TCP/TCP6 状态，只记录状态计数，不保存连接地址或端口。socket 数包括入站、代理和其他连接，不能解释为上游生成数。`cpu_one_core_percent=100` 表示使用约一个 CPU 核，可能高于 100。
- 从该 PID 的 cgroup 找 CPU `cpu.stat`，记录累计与差分 `nr_periods`、`nr_throttled`、`throttled_usec`、usage 等。cgroup v1 的 throttled_time 纳秒转换为微秒。cgroup 不可读时显式标记 unavailable，不伪造零。
- 检查 `/proc` start_ticks，PID 消失或被复用时停止，避免把新进程混入同一组指标。

输出 `samples.jsonl` 为脱敏逐样本结果，`report.json` 是原子更新摘要。默认 0.2 秒采样间隔，相当于最多 5 次管理列表 GET/秒；API 变慢时串行采样，不叠加请求，`sample_duration_ms` 可判断实际频率。不会修改 Go 或生产配置。

统一验证压力工具与 monitor 的命令：

```powershell
python -m unittest discover -s integration/gateway_environment -p 'test_*.py' -v
```

共 14 组本地自测：压力工具 9 组、monitor 5 组，覆盖匿名稳定性/不输出秘密、分页不完整标记、合成 proc/cgroup 解析、stop-file 退出和管理认证失败停止。此次 monitor 自测使用本地 stub 和合成 `/proc` 文件，尚未执行生产采样。
