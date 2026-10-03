# Codex 网关修复、性能与源码一致性审计

**最新凭据干预证据：** [v8 固定请求刷新前后对照](CODEX-QUALITY-CREDENTIAL-REFRESH-2026-09-25.md)完成八个 POST 和一次 g7 正常管理刷新，七次 21、一次 29。g7 在刷新前已经两次 21；刷新后的同 AT、同完整正文及头摘要出现 21／29。新凭据持久化、同用户／工作区、对照账号不变均核验，但质量改善不能归功于刷新；隐藏算力或固定账号标签仍未证明。

**最新同账号对照：** [v7 八格镜像](CODEX-QUALITY-GATEWAY-DIRECT-2026-09-25.md)全部完成：new2 直连／保留生产设置的隔离网关各两次均答 21，g7 两路各两次均答 29。实际 native 出站核验表明原题、input、模型、effort、metadata 与共同非认证头保持，网关差异为配置提示词及缓存键；本批结果随账号保持，不随这两条路径改变。此证据不能等同生产多账号负载全验收，也未证明服务端内部机制。

**2026-09-25 后续现场核验：** 当前组合后端 `a4777428…e20136a` 已包含 Lite 缺失 instructions 的修复；已核对实际运行 SHA、源码归档与 35 个 Lite 测试终态。独立旧候选未覆盖组合发布。[Cookie／先行 GET 对照](CODEX-QUALITY-COOKIE-CONTROL-2026-09-25.md)实际执行 7 POST 后因 q0 被停用停止，五个完整答案均为 29、两次明确限流。q0 同期存在生产流量，完整镜像未完成；不据此宣称 Cookie 或隐藏集群根因已确定。

**2026-09-25 14:36 后续对照：** 新增三账号 HTTP／WS 共 12 格已完成：HTTP 四格完成、q0 两格 429；WS 六格完成。十个完整答案人工均为 29，未观察到切换 WS 恢复 21。两次 429 单列失败，不计错答。[原始 CLI 请求与传输对照证据](CODEX-QUALITY-HTTP-WS-2026-09-25.md)。下文保留前序批次与部署记录。

**2026-09-25 后续上线更新：** 当前后端 `bb92216f…aca277ad` 已完成生产验收。导入识别官方 `user_id` fallback；刷新按当前 AT/ID 重建身份、保存旋转令牌并阻断旧身份会话；WS 连续链保持缓存键；HTTP 改键后超限在绑定前拒绝。最终 Linux 946、Windows 822 顶级通过，零失败，另有完整官方 CLI 7 项通过。八账号目录 28 对相同、官方外部令牌本地装载均成功；原 24 次答题结果保持，质量恢复仍未证明。[完整修复与验收](CODEX-GATEWAY-IDENTITY-REFRESH-2026-09-25.md)。下文旧版本号及测试数字按各自阶段保留。

日期：2026-09-24。20 个专项 agent 分批执行，最多 4 个并发槽（含主 agent）；主任务完成集成、真实 CLI/TLS 验证和最终复核。**首轮审计结束时**代码修复已经写入工作区，候选版本尚未部署。以下追加后续状态，保留首轮的测试数字、产物和范围，避免把历史快照当作当前结论。

**2026-09-25 12:25 最新状态：** 当前网关后端 `020fa045…2065e1` 已上线。在此前请求 JSON、usage 字段与缓存边界、真实 CentOS 应用身份修复基础上，本次补齐管理 settings POST 的平台一致性校验，禁止先保存冲突 profile 再让实际请求失败。最终构建快照 Linux race/PG/Redis/native **921 通过、0 失败、23 跳过**，Windows **797 通过、0 失败、148 跳过**，vet 通过；生产实际冲突 POST 返回 400 且设置未变，HTTP models 200、WS 101/正常关闭、usage 往返 200、health ready。仅发布已测试快照中的管理校验补丁，共享工作区其他并行变更未进入该版本。此前官方 CLI 7 项及真实 Rust HTTP/WS 身份抓取属于各自有记录的验证批次，没有冒充本次全套重跑。28765 `auth-device-v2` 保留；详见[环境与设备修正](CODEX-QUALITY-ENVIRONMENT-2026-09-25.md)。

**本轮多账号结果：** [八账号固定请求与官方 CLI 复测](CODEX-QUALITY-EIGHT-ACCOUNTS-2026-09-25.md)已执行 16 次固定 native IPC 请求和 8 次完整 CLI 直连调用，全部完成；自动为 3 正确、15 错误、6 无法判分，人工为 3 次 21、21 次 29。user/identity/token 各 8 个独立值，共 6 个 workspace；q0/g7 均有对错变化。Go 不在本轮推理路径上，官方直连也错答，但历史环境影响及私有服务端分配机制仍未知。旧额度阻塞已不代表本轮状态。以下部署 SHA、计数及结论按原阶段保留。

## 后续状态追加：已上线，质量问题仍在调查

截至同日运行时 TLS 纠正验收，网关主体、账号原生状态隔离及推送桥修复已经上线，且已执行真实账号 HTTP/WS 请求。[账号隔离上线报告](CODEX-QUALITY-ISOLATION-DEPLOYED-2026-09-24.md)记录部署与完整回归；[运行时 TLS 续查](CODEX-QUALITY-RUNTIME-TLS-2026-09-24.md)记录随后发现并修复的实际环境差异。后文的 821 项、候选产物及“未做真实账户验证”等表述专指首轮快照，不代表当前仍未部署或仍只有合成模型响应。

固定源码和 CLI 参考仍为 **Codex CLI `rust-v0.155.0`**，没有将 PATH 中其他版本或后续发布当作同一基准。下表为此前账号隔离与 TLS 纠正阶段的已部署产物和证据；当前版本见本文顶部：

| 项目 | 后续已核验状态 |
|---|---|
| 网关运行 SHA256 | `84ca77af5c1d9a49d293180f4ccf38396df2e8ed3a1108d73df0c868315985f0`；[激活记录](../.local/quality-20260924/activation-result.json)、[TLS 纠正后运行记录](../.local/quality-20260924/tls-final-runtime.json)均一致。 |
| 原生 helper SHA256 | `f72e5c0ff8798c02f430f7aa36f74009dad40fade6111dad722390891239b556`；TLS 环境纠正未替换网关或 helper 二进制。 |
| 最新完整 Linux 回归 | **836 顶级通过、0 失败、21 opt-in 跳过，2365 个含子测试终态，0 DATA RACE**；race / PG / Redis / native 批次耗时 264.225 秒，vet 通过。[summary](../.local/gateway-audit-20260924/go-quality-isolation-full/summary.json)及[原始测试日志](../.local/gateway-audit-20260924/go-quality-isolation-full/tests.jsonl)已重新核对；跳过不计为通过。 |
| 生产源码与测试关联 | [测试快照](../.local/gateway-audit-20260924/go-quality-isolation-full/source-manifest.json)摘要 `a9f4f0f989d45b01991904ea9f896309b8af555149876f87b8ee857d572e4268`；[部署构建清单](../.local/quality-20260924/deploy/build-manifest.json)的 105 个生产源码文件与该快照、当前工作区逐文件一致。此快照范围为 Go/SQL/module 文件，不包含前端。 |
| 运行环境纠正 | 旧部署自动 `SSL_CERT_FILE` 会触发官方 HTTP rustls 分支；现已移除该条有来源核验的自动覆盖，三个 CA 覆盖变量均不存在，readiness 正常。部署脚本不再自动注入，保留用户显式 custom CA；Linux 对应部署测试 **37/37 通过、无跳过**。 |
| 真实请求验收 | TLS 纠正后原糖果题 HTTP 200、`response.completed`、答案 21；[糖果验收](../.local/quality-20260924/tls-candy/summary.json)记录无自动重试、未向模型发送预期答案。另有[WSS 验收](../.local/quality-20260924/tls-ws/summary.json)：101，同连接两轮成功。 |

后续核验更新了“一致”的适用范围，不能只凭源码 hash 推断实际请求指纹：

| 分类 | 当前结论与限制 |
|---|---|
| 已验证一致 | 固定 0.155.0 官方库、依赖和既有同平台 HTTP/WSS 对照仍有效；生产环境已纠正默认 HTTP backend 的覆盖偏差。同生产 UID、相同 helper 对真实上游，默认系统根及仅 `SSL_CERT_DIR` 均正常验证证书后返回 401 / HTTP/1.1，旧 `SSL_CERT_FILE` 返回 401 / HTTP/2。backend 判断结合源码分支和进程环境；401 只证明传输可用。WS 保持官方 `ExplicitCodexTls`，未改为 OpenSSL。 |
| 已上线的网关专有修复 | 可信账号身份隔离 helper/HTTP cookie 状态，同 owner 正常刷新保持连续，避免跨账号串用；适用请求补齐 CLI 路由提示；桥不再因容量/网络错误误报已交付凭据失效。它们是明确的网关逻辑修复，不是已证实的隐藏集群切换手段。 |
| 保留差异 | 多账号鉴权/调度/准入、固定账号出口和受控重放、有界 IPC/32 MiB message、显式 `pooled` 模式、JSON 聚合及旧 compact 适配、固定 profile OS/terminal 值仍有下文列出的差异；Linux 实测不能代替 Windows/macOS TLS。 |
| 仍未验证或未证明 | Windows/macOS 原生构建和运行、所有代理下同平台 CLI 抓包、HelloRetryRequest、原始头顺序、压缩 wire 字节完全一致、TCP/IP 指纹及生产负载表现仍未完整覆盖。已做真实账户推理，但异常账号 q1/q4 的稳定恢复、隐藏集群分配机制及网关与错答的因果关系尚未证明。 |

正常账号原先也可能答 21；异常账号在官方/原生直连中也有错答或 403/429/容量错误。因此这次验收不能宣称所有账号“恢复满血”，也不能把无最终答案的失败计为数学错答。账号目录相同同样不证明实际推理算力相同。

专项索引复核结果：`WORKPLAN.md` 中 **01–20 共 20 个唯一编号，20 个目标报告均存在，无漏项或重复编号**；后续 `quality-20260924/` 报告是追加调查，不替换原 20 项。

最新[同账号原题与语义对照](CODEX-QUALITY-SEMANTICS-2026-09-24.md)：q4 原题直连/透明网关均答 29，明确可选择形状后答 21，明确盲抽后答 29；四次均完整结束，身份与凭据前后保持。这补足了可比较正文的原题配对及可见模型元数据证据，但控制题不计为原题恢复，亦未证明隐藏集群机制。

随后[完整官方参考与实际 IPC 核验](CODEX-QUALITY-DISPATCH-2026-09-24.md)进一步确认：q4 完整官方 CLI 无告警完成原题仍答 29；单次透明网关实际交给 Rust 的正文、认证、model/effort 与预期一致，HTTP 200 完成也答 29。实际 IPC 证据限这一样本，不等同网络逐字节抓包或隐藏集群证明。

## 基准与结论

本轮对齐项目固定的 **Codex CLI `rust-v0.155.0`**，使用完整本地源码、官方发布二进制和实际构建依赖，不将系统 PATH 中的 `0.153.4` 当作相同版本证据。

- 官方 Linux CLI SHA256：`660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`。
- 本轮 Rust helper SHA256：`f72e5c0ff8798c02f430f7aa36f74009dad40fade6111dad722390891239b556`。
- 独立来源校验：265 个锁定包，其中 261 个外部依赖；30 个官方 Rust 源文件保持原样，1 个明确记录的 WS 固定代理适配补丁可完整重建。上游 Cargo.lock、workspace、依赖 allowlist、TLS features 与本轮构建快照均通过校验。

原生 HTTP/TLS 与 WSS/TLS 的所测字段、默认连接生命周期及 CLI 工具链路已经对齐并验证。网关仍承担多账号授权、限流、有限内存和旧 API 适配，因此**不能描述为所有输入、平台、设备指纹均与 CLI 完全相同**；保留差异和未验证范围列于下文及 20 份专项报告。

## 修复内容

| 范围 | 本轮主要改动 |
|---|---|
| HTTP/TLS 与 IPC | 严格校验 Content-Length、控制字符及头元组；保留真实超时原因；总期限覆盖上传；处理短写；body 只关闭一次；单连接 accept/peer 错误不终止整个 helper；修复等待启动请求被提前回收。 |
| WebSocket/TLS | 默认每个新 socket 使用新的官方 TLS connector；恢复官方 16 MiB frame 上限；分离控制帧/message 限额；补齐半条 IPC 命令、关闭等待超时、ACK/序号/UTF-8/关闭码校验。 |
| WS 会话与限流 | 命名 stream 错误仅结算对应 FIFO；忽略失配/已完成 response ID 对后续请求的污染；按活动 lane 计空闲期限；取消/关闭释放准入；运行时设置变更重新检查；未知 scoped 429 不扩大为全连接冷却。 |
| 指纹、头和 URL | 大小写别名确定性合并、单值身份覆盖、保留列表和未来扩展头；身份改写不重放原 attestation；过滤 Connection 指定逐跳头；HTTP/两种 WS 共用安全 URL 构造，保留编码路径；修复裸 IPv6 来源解析。 |
| SSE 与输出 | BOM/UTF-8/CRLF/EOF 处理；原始流继续透传；JSON 重建检测 index、item ID、call ID 冲突，安全补齐已知终态子集；防止跨 response ID 合并；本地观察失败不截断原流；完整事件才重置空闲预算。 |
| 压缩与 compact | 保留 Guardian 两个端点的 zstd 选择；重复编码头、取消和大小上限一致处理；最多复用两个小请求编码器；校验 compact 输入/输出和 encrypted item。 |
| OAuth、授权和调度 | 严格读取完整有限响应；接受合法 2xx；过期字段损坏不丢轮换令牌；RT-only 并发只刷新一次；重放前授权和取消复查；修复权重溢出、busy 重试时间、已取消会话仍续期、错误 key 身份首次绑定。 |
| 代理、运行时与观测 | SOCKS VER/RSV/port/zone/short write、远端 DNS、半关闭和取消；保留 HTTP query/trailer；关闭基础连接池；拒绝损坏持久配置与版本溢出，同时允许已认证管理 API 修复字段；修复写失败误成功、usage 零/负数/溢出、矛盾终态及 models 超限静默截断。 |

最终独立复审额外复现了“坏配置不能通过管理接口修复”的交互回归，并用真实管理路由的冷/热缓存、授权、CAS、PG 用例修复验证。详细变更不以数量充当正确性依据，逐项证据见专项报告。

## 首轮最终验证（历史快照）

| 验证 | 实际结果与范围 |
|---|---|
| Linux 完整 Go + race + PG/Redis + 新 helper | **821 个顶级测试通过，0 失败，19 个明确 opt-in 跳过；2297 个含子测试终态，0 DATA RACE**。260.891 秒；`go vet` 通过。快照中的所有 Go/SQL 文件与最终工作区一致。 |
| 固定版本官方 CLI 最终批次 | **13 个顶级测试通过，0 失败**；普通批次的 OAuth 压缩测试按网络隔离要求另跑。覆盖 HTTP/WSS、实际工具、并行工具、文件修正、恢复、compact、426 回退、无效终态、不重放及 TLS。 |
| 合成 OAuth 压缩 | 独立 `--network none` 容器检查实际网络接口，验证官方 CLI 压缩、真实工具后续及 native helper，未使用真实账户。最终结果索引见下方证据目录。 |
| Rust 原生传输 | Linux musl / Rust 1.95.0 release：**35 项 Rust、26 项 HTTP/UDS loopback、8 项新增 HTTP/TLS/HTTP2 测试全部通过**。 |
| HTTP TLS 实测 | 默认 native 首个 ClientHello、自定义 CA rustls 完整握手比较通过；另 6 个同平台场景各 4 次请求，自定义 CA 的 HTTP/2 SETTINGS、WINDOW_UPDATE、priorities 与恢复序列相符。默认 native 未协商 h2，不计为 h2 对齐证据。 |
| WSS TLS 实测 | 两种有效信任模式共 12 条成功连接、24 条消息；SNI、ALPN、cipher、归一化 Hello、deflate 和每 socket 多轮复用相符；三条 gateway 连接保持同一 helper 进程。未知 CA/错误 DNS 双方拒绝，记录 4 个 TLS 证书告警，零 Go fallback。 |
| Windows 一键验证 | 完整 Go、vet、来源校验通过；外部依赖/原生 Linux/CLI 用例明确跳过。报告为 `passed_with_skips`，不将缺少运行条件计为通过。 |
| Fuzz 与验证工具 | 四个 fuzz target 实际 mutation 运行通过；分片、输出身份、IPC 限额及新增 stateful splitter 差分验证。来源校验 7 项、验证 runner 9 项测试通过。 |
| 构建 | Windows amd64 与 Linux amd64 网关构建通过；原生 helper 为上述实测 Linux musl 产物。候选产物及摘要在 `.local/gateway-audit-20260924/release-candidate/`。 |

首轮真实 CLI 的工具由 CLI 执行 shell/apply_patch，检查实际文件、输出和退出码；该批上游模型回复仍是合成 fixture。这证明协议与工具链路，未证明真实账户可用性或远程模型推理质量。首轮完整批次跳过项在 JSON 中逐项列出；多项 CLI 测试由独立批次补跑，该批真实账户用例未运行。后续真实账户验收见本文追加状态，不回写为该批已运行。

## 性能实测

同一 Windows amd64 / Go 1.26.7 机器，从原始源码快照提取旧实现做对照：

| 微基准 | 修改前 | 修改后 |
|---|---:|---:|
| 流读取计时器 | 约 143 ns，248 B，3 次分配/次 | SSE 约 56 ns，0 B，0 次分配/次 |
| 128 KiB 随机正文 zstd | 约 84 μs，1.93 MB 分配/次 | 约 32 μs，303 KB 分配/次 |
| 32 MiB 单行，64 KiB 分片扫描 | 约 2.482 s | 约 15.85 ms（约 156 倍） |

第三项消除了 Scanner 分片输入反复扫描已检查前缀的平方级开销，保留 CR/LF、EOF 和大小上限。压缩缓存容量为两个，仅用于不超过 1 MiB 的请求。以上是特定函数微基准，不能换算为生产吞吐、端到端延迟或缓存命中率提升。大正文的有界分配和 JSON 聚合成本仍然存在。

## 明确的一致与不一致

| 边界 | 结论 |
|---|---|
| 默认 HTTP backend / 自定义 CA / WS rustls | 直接复用锁定官方 crate；HTTP 随平台 native TLS，自定义 CA 切 rustls；WS ExplicitCodexTls。已验证 Linux 所测行为，不能将 Linux OpenSSL 当作 macOS/Windows 指纹。 |
| 默认连接生命周期 | HTTP 每请求、WS 每新连接创建客户端/connector；同 socket 多轮继续复用。`pooled` 为显式性能选项，允许不同 TLS session 恢复行为。 |
| 应用指纹 | 原生入站身份按确定规则保留；开启 Linux/macOS profile 使用固定参考 OS/terminal 值。它不是实时宿主探测、移动设备证明或 TLS 跨平台模拟。OAuth 短 UA/额外 Version 等既有差异保留并记录。 |
| 代理与重放 | 网关固定账号出口、不读环境代理、不自动跨出口重试；401 仅一次受控恢复重放。CLI 可使用系统/PAC 路由、更多 retry/reload 阶段及 TLS fallback。这些没有为了字节相同而复制。 |
| 资源边界 | CLI fork 默认 message 64 MiB，网关 message 32 MiB，frame 均为 16 MiB；网关消息/IPC 队列有界。写期限及背压下的计时有明示差异。 |
| HTTP JSON 与终态 | CLI 原生消费流；网关额外提供 JSON 聚合，接受部分旧 `response.done`/缺失字段兼容形状。CLI completed 必须有字符串 ID，网关部分旧响应仍较宽松。冲突输出对 JSON 拒绝，原始 SSE 不重写。 |
| compact | 官方 CLI v2 请求可直接流式通过；旧 JSON compact 适配缺少 CLI 本地的 token/image/agent-message 元数据，保留上下文仅为有限近似，不能宣称完全相同。 |
| 多账号、权限、配置 | 调度、黏性、冷却、Redis/PG 授权属于网关增加的行为；新授权或重新启用可能受 5 秒候选缓存影响，撤权有权威重查。 |
| SSE 修复 | 完整 data 事件才续空闲期限；首 BOM 安全处理，不复制上游旧依赖按错误字节切片的 panic。排除下游等待的计时与 CLI 纯 event 等待计时存在明确差别。 |
| 首轮尚未验证（历史范围） | 当时尚未做真实生产 SNI/账户推理、Windows/macOS 原生构建、所有代理下 CLI 抓包、HelloRetryRequest、原始 HTTP 头顺序、压缩后逐字节身份、TCP/IP 指纹、生产负载及部署。后续已有生产 TLS/真实账户请求和部署证据；它们未覆盖全部原始指纹比较和剩余范围，见本文追加状态。 |

## 复验与交付

项目根目录一条命令：

```powershell
python tools/verify_gateway_audit.py
```

可显式添加 `--native-bin PATH --cli-bin PATH --race --rust --fuzz-seconds N`；不同平台或缺少条件的用例会标记 skip。工具记录每个测试状态、源码 SHA256、命令和日志；执行期间源码变化会使验证失败。来源证据也可独立运行 `python tools/verify_native_provenance.py`。

完整本轮证据：`.local/gateway-audit-20260924/`。主要索引为 `final-default.json`、`go-full-final/summary.json`、`go-cli-final/summary.json`、`go-cli-compression-final/summary.json`、`native-provenance-final.json`、`http-fingerprint.json` 及 `cli-reports/`。原始源码快照、逐文件哈希、修改包与候选二进制均保留，可复核和回退。

20 个专项的逐项源码行号、测试和限制：[分工索引](audit-20260924/WORKPLAN.md)。首轮候选冻结时未发布到生产，彼时现网配置只读核对与候选源码验收分别记录；当前部署状态以本文追加说明和后续上线/TLS 报告为准。
