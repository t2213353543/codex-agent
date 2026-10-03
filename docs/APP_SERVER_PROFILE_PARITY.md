# App Server 元数据与上下文差异补齐

`resolveAppServerRequestProfile(appServerProfileInput)` 是纯补全层，返回 `Request`、逐字段 `Sources`、`Missing` 和从客户端消息提取的 `Environment`。它不保存对话、不读取服务器环境、不注册额外工具。轻量 handler 已接入它：每个客户端请求只分配一次身份，在失败重试间保持稳定；客户端已有身份优先。未知环境文本保持原文并记为缺少可推导 metadata，不因此拒绝有效会话。

最小调用可传已解析的 `appServerRequestInput`，以及本请求开始前只分配一次的 `Defaults`。安装 ID 应为服务生命周期内固定 UUID；用户提供的会话、线程、turn、window ID 优先。只有确知新消息边界时才传 `NewInputStart` 和对应 `NewItemIDs`。没有边界就保持所有原 item，记录缺项，不能给每次重传的旧历史重新随机分配 ID。

启动 WSS warmup 应对生成快照调用 `appServerPrewarmProfile`，再由 transport 构造空 input 的 `generate:false` 请求。helper 只投影 metadata：`request_kind=prewarm`，flat/canonical `turn_id` 明确为空字符串，删除 root_turn_id 与 turn_started_at_unix_ms，保留窗口、安装、已有环境、模型和未知字段，不改变 generation 快照。固定源码 `session_startup_prewarm.rs:269` 使用空 `INITIAL_SUBMIT_ID`；`:296` 先 capture_step_context 构造工具快照，`:318` 后读 prewarm metadata，因此已有 tool_namespaces_info 保留于 body、仍从兼容 header 排除。真实 capture 的 generation→prewarm 逐字段对照测试通过。

已经构造好的 native Lite / `previous_response_id` 请求可独立调用 `appServerMirrorNativeMetadata(raw, headers)`：仅从已有 canonical turn metadata 删除 `thread_source`，据 body canonical 更新兼容 header。body 中工具 inventory、未知值、大整数保留；不增加 defaults/harness/tools，不碰 input/store/stream/delta/response ID/turn state。仅 header 有 metadata 时 body 完全不变；两侧均无 metadata 时原 body bytes 完全不变。两侧冲突时 body 是权威，不把 header 投影反向覆盖 body 的工具 inventory。

## 已实现的来源顺序

1. 客户端 `client_metadata` 的 canonical turn JSON 和显式扁平身份字段；两者身份冲突返回错误。
2. HTTP 兼容 header 补缺；显式身份 header 冲突返回错误，不静默更换会话。body 中 canonical metadata 优先于握手 header 中可能较旧的 metadata。
3. 已解析请求的 session/thread 和调用方分配的 UUID/时钟快照。
4. 可直接推导的 window、root turn、明确新增 input metadata、实际模型目录字段、客户端环境 XML。

未知 flat metadata 与 canonical JSON 字段保持存在，含大整数和未知 `thread_source` 值。默认不删 `thread_source`；仅 `MirrorAppServer=true` 显式选择 App Server 镜像时删除 canonical 字段。`tool_namespaces_info` 保留于 body，但按官方兼容 header 投影规则不复制到 header。

## 字段账本

| 字段 | 当前 baseline / source | 补全规则和限制 |
| --- | --- | --- |
| `installation_id` / `x-codex-installation-id` | baseline 有，`responses_metadata.rs:306` 投影 | client 优先，否则调用方一次分配的安装 UUID。与 native 安装 ID 数值相同需客户端提供或共享那份安装状态 |
| `session_id`, `thread_id` | baseline 均为 runtime UUID | body/header 优先，后用既有请求值与显式 defaults；只缺一项时可由另一项补。不能覆盖用户显式的不同身份 |
| `window_id`, `window_number` | `session/mod.rs:4327` 使用 `thread_id:number` | 已有 window ID 可解析 number；已知 number 可拼 window ID。`CompleteHistory=true` 可选按该完整客户端历史中的 compaction 数构造客户端窗口计数；它不覆盖 native 的手动新 context window 状态，不能声称等同全部 runtime 窗口生命周期 |
| `context_window_id` | runtime 独立 opaque UUID | 保客户端提供值；只有 window 已确立才接受调用方分配值。无 state 不能证明新分配值与上次 HTTP 同一窗口；需要客户端回传稳定 ID 或已有会话状态提供者 |
| `turn_id`, `root_turn_id` | baseline generation 有真实 turn UUID；prewarm turn 空 | 明确 new-input 范围的 metadata 可确定当前 turn；否则接受调用方分配的本请求 turn。无 parent 且普通 turn 可令 root=turn。工具结果未带 turn 时，不能宣称新 ID 是原 pending call 的 turn |
| `request_kind` | `turn` / `prewarm` / `compaction` | 保明确 canonical 值；否则由调用方 operation 指定，或看到 `compaction_trigger` 确定 compaction；默认普通 turn。unknown 客户端值保留 |
| `turn_started_at_unix_ms` | baseline 有，本地消息 create_time 与它不同 | 用客户端值或调用方请求开始时钟，不能把消息 create_time 当官方 turn 开始时间 |
| `agent_name`, parent/subagent 元数据 | baseline `/root` 由真实代理树产生 | 客户端声明才保留；仅有 tools 名称不能推出代理树。缺少时在 Missing 中标明，不伪造 `/root` |
| `sandbox` | baseline `seccomp` | 需要实际执行器的证据；read-only XML 只能说明策略，不能推出 seccomp。保持未知 |
| `sandbox_mode` | baseline `read-only` | 从客户端完整 filesystem XML 中严格识别 read-only 或 disabled/unrestricted；不从网关所在系统推断 |
| `auto_review_enabled` | baseline false | 审核策略属于执行器配置；缺少时保持未知，不因用户没有写就当 false |
| `node_repl_auto_review_required`, `node_repl_disabled` | 实际 catalog；`ModelInfo` 两字段有 serde(default) | 现有 metadata 优先，否则取 actual catalog；目录省略时使用固定 0.155.0 明确的 false 默认，不根据模型名字猜 |
| 用户 message `id` | 官方 `ResponseItemId::new` 使用带 prefix UUIDv7 | 仅明确 new-input 范围且调用方给出 `msg_…` ID 才补；保留已有 IDs。opaque reasoning/function/server item 不新造 ID |
| input `turn_id`, `create_time`, `content_item_kinds` | `session/mod.rs:3430` 和 record 阶段 | 仅明确新用户消息补缺，text/image/audio kind 按类型，完整环境消息使用环境 kind；旧历史原样。已有 kinds 会开启 writer 保留行为，不因默认 false 被删 |
| base instructions | writer 从 `model_messages.instructions_template` 及 personality 渲染 | 已在 writer 实现；网关补空与用户明确空由原 body provenance 区分。本 resolver 不叠加第二套 instructions |
| effort / summary / verbosity | writer 有目录默认；客户端可显式指定 | 显式 Responses 控制保持。`ultra` 在官方配置层是别名，会解析为模型支持档位；客户端直接写 `reasoning.effort=ultra` 保原文的 profile 与该配置语义不同，需调用方显式选择别名解析，不能同时宣称两者逐字相同 |
| `tool_namespaces_info` | 有条件的 runtime 工具注册表遥测 | 仅凭声明可知工具名，却未必知道 dispatch owner、CodeMode 名称、defer 状态和配置开关。原值保持；不伪造 runtime registry 或隐含工具能力 |

## 上下文集成可用的可靠输入

`Environment.contextProfile(explicit)` 能提供 cwd/shell/date/timezone/workspace roots 和可识别 filesystem 模式。调用方仍须给真实 approval、network、collaboration 与 multi-agent 配置，之后可调用已通过 capture 验证的 `buildAppServerInitialContext`。现有工具表没有证明执行器支持 delegation，因此不能仅凭模型目录的 multi-agent 提示模板自动添加 collaboration/clock/goal 工具。

要完整镜像 baseline，还需要：真实客户端安装/线程/窗口/turn 生命周期；明确的全量历史与新 input 边界；执行器 sandbox engine、approval/auto-review 与 network 策略；代理路径/parent/root；工具注册表及每个工具执行方；CodeMode 执行能力；模型目录与 profile 配置；真实 trace/routing/turn-state。仅复制 baseline `/root`、`seccomp` 和服务端 cwd 会给模型错误环境。完整 baseline 里有未声明 builtin；客户端若未实现这些工具，则“所有工具和执行语义完全镜像”与“仅客户端现有工具执行”直接冲突。可选择客户端补齐对应执行器，或明确采用只暴露已声明工具的 profile；不能将后一种说成完全复制 native runtime。

## 证据与验证

`TestAppServerProfileCapturedMetadataReconstruction` 从真实 `appserver_writer_pilot.json` 移除可推导冗余字段，再用实际 flat metadata/window ID 重建，完整 canonical JSON 与捕获一致。其余测试验证 unknown metadata、大整数、opaque reasoning、明确 mirror、完整/裁剪 history 区别、输入边界、模型 flags、client XML、身份冲突。原 capture 保留在私有服务器。

`appserver_native_metadata_pair.json` 来自真实 paired pilot `20260927T102700.613738Z` 的 native App Server 和 `codex exec` 首次生成 metadata，仅按字段名映射动态身份和时间，没有删除 metadata 字段。归一化后唯一差异为 CLI 的 `thread_source`；native mirror helper 处理后与 App Server 侧 canonical/header JSON 相等。这个测试不声称整份模型请求或模型质量相等。

复跑：`go test ./internal/server -run '^TestAppServer(Profile|RequestMetadata)' -count=1`。
