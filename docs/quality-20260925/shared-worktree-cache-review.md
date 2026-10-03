# 共享工作区缓存及兼容处理审计（2026-09-25）

**2026-09-25 后续上线更新：** 当前后端 `bb92216f…aca277ad` 已完成生产验收。导入识别官方 `user_id` fallback；刷新按当前 AT/ID 重建身份、保存旋转令牌并阻断旧身份会话；WS 连续链保持缓存键；HTTP 改键后超限在绑定前拒绝。最终 Linux 946、Windows 822 顶级通过，零失败，另有完整官方 CLI 7 项通过。八账号目录 28 对相同、官方外部令牌本地装载均成功；原 24 次答题结果保持，质量恢复仍未证明。[完整修复与验收](../CODEX-GATEWAY-IDENTITY-REFRESH-2026-09-25.md)。下文旧版本号及测试数字按各自阶段保留。

下文“未部署／只读”描述原始审计阶段；最终四个缓存修复生产文件已通过当前组合全套并上线，源码已按摘要核验后合入共享工作区。

审计对象为当前共享工作区与 `.local/research-20260925/profile-admin-validation/verified-build-source` 冻结源码之间的增量。业务源码只读；复现仅在 `.local/research-20260925/native-auth-provider-v4/cache-review/` 的独立快照执行，使用本机 loopback 合成上游，未部署、未调用模型、未访问账号凭据。

结论：新增功能有意改写官方 CLI 已提供的默认 `prompt_cache_key`，因此不满足原始请求逐字一致。没有发现该改写跨账号复用 session、认证或设备字段；两个实际复现的边界问题是 **WS 开关切换改变增量请求的 cache key** 和 **HTTP 最终校验失败后保留新建会话绑定**。这些是新增工作区变化的行为，不构成先前 24 次账号实验的根因证据。

## 源码锚点与修改范围

| 文件 | 本次读取 SHA-256 | 冻结基准 |
| --- | --- | --- |
| `gateway.go` | `2158468a04a6bd3d90e32b0f33123964307730d7fd93e177458640feaf4d33b4` | `aa1f1346143cf67e962e51f13191fd69c7a86ec1b54cca945b5924594cc9ce80` |
| `gateway_websocket.go` | `390e725b17a37d9b53155bd281721a4041292beee118823d47f711c492fc8058` | `1681184d0dc7984f57456d95448c69e6f816eeeb572d76646cf6169d908484e8` |
| `gateway_compatibility.go` | `ae92cf36503f9987d075c284dc98ce87b10b4bed29d8f5c3061351a5849a0f83` | `4ab127973a1495a296a956dddc45f8b52bc16f1aeca1f2db17d850b3dbb2075f` |
| `gateway_prompt_cache.go` | `bc3192261b4b958dd5d6ec987adb6ccd9c62f1de1763fabc55c4625672bfd610` | 无此文件 |

`gateway_sessions.go`、`gateway_native_identity.go`、`gateway_policy.go` 与基准字节相同。完整 353 个文件的独立快照摘要见 [snapshot-manifest.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/native-auth-provider-v4/cache-review/snapshot-manifest.json)。报告完成前再次计算上述四个当前源码摘要，未变化。

实际增量：

- [gateway.go:659](C:/Users/Administrator/Desktop/项目/codex-agent/gateway.go:659) 把账号选择移动到最终请求压缩之前；选中账号后调用 `prepareGatewayAccountPromptCache`，再进行压缩。请求准备耗时因此包含账号选择区间；已有观测合同允许区间重叠，不能据此认定计时错误。
- [gateway_websocket.go:468](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_websocket.go:468) 在每轮授权后、占用推理准入之前进行相同改写，使用这一轮最新设置。
- [gateway_prompt_cache.go:16](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_prompt_cache.go:16) 仅在开关开启、账号及网关键存在、非空 key 精确等于 `client_metadata.session_id` 时替换 key。新值为 `SHA256([版本, keyID, gatewayAccountRef(account)])`，相同账号及 key 的多个默认会话共用该值。
- [gateway_compatibility.go:15](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_compatibility.go:15) 新增删除顶层 `user`；`background=false/null` 删除，`true` 及非法类型本地拒绝；仅规范 message 角色中的 message-level `author` 被删除。工具数据、未知角色、author-only input 不归一化。该处理受既有兼容开关控制，没有改变 HTTP 身份头或 TLS。

## 与 CLI 0.155.0 和账号隔离的对照

官方 [client.rs:497](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:497) 的优先级是显式 override、内部任务 `source:parent_thread_id`、默认 `responses_metadata.session_id`。本次新增账号级聚合不属于该官方默认算法。入口值等于 session 只能作为默认值的启发式识别；协议中没有标志能区分“官方默认值”与“调用者恰好显式选择同一个值”。

没有观察到以下新增污染路径：

- 本地会话绑定仍在改写前按真实 header 或原始 key 解析，见 [gateway_sessions.go:28](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_sessions.go:28)。合并上游 cache key 不会把本地两个 session 的绑定表直接合并。
- 账号引用包括管理记录、工作区、用户、OwnerRevision；未知所有者还加入凭据版本，见 [gateway_sessions.go:61](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_sessions.go:61)。不同用户即使共享工作区，也不会因本函数得到同一个账号摘要。
- 新函数只替换一个 JSON 标量，不改变 `client_metadata`、`session-id`、`thread-id`、`previous_response_id`、历史、认证、UA、device 或 turn-state 字段。其本地现有测试验证了这些保留规则。
- native HTTP 和 WS 的 helper 隔离仍使用 [gateway_native_identity.go:16](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_native_identity.go:16) 的账号作用域；本次增量未修改 cookie jar、TLS 会话缓存或 helper pool key。这里是源码一致性核对，不是本轮重新抓取 TLS 的证明。

共享 cache key 是服务端缓存路由输入，不能单凭相同 key 推导两条提示词的 KV、回答或历史发生混合，也不能单凭 key 不同推导被标记或进入不同算力集群。本轮没有服务端路由标签证据。

## 离线实际复现

复现代码：[cache_review_reproduction_test.go](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/native-auth-provider-v4/cache-review/source/cache_review_reproduction_test.go)。结果：[reproduction-summary.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/native-auth-provider-v4/cache-review/reproduction-summary.json)，原始 Go JSON 日志同目录 `reproduction.jsonl`。2026-09-25 04:42 UTC 执行，5 个测试项通过、零失败；这里的通过表示预先声明的异常现象确实被观察到，并不表示问题已修复。

### 1. WS 运行中切换缓存开关，增量上下文仍引用旧响应

关联位置：[gateway_websocket.go:386](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_websocket.go:386)、[gateway_websocket.go:468](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_websocket.go:468)。每轮重新读取缓存开关；已有连接的重连校验只检查指纹设置。

实际执行两轮请求：第一轮含完整输入并收到 `review-response-1`；修改缓存开关；第二轮客户端仍提供相同原始 session key，带 `previous_response_id=review-response-1` 和 `input=[]`。

| 设置变化 | 第一轮上游 key 长度 | 第二轮上游 key 长度 | 第二轮 previous/input |
| --- | ---: | ---: | --- |
| true → false | 64 | 14 | 原 response ID / 空增量，均保留 |
| false → true | 14 | 64 | 原 response ID / 空增量，均保留 |

连接没有要求重连，合成上游确实收到第二条请求。为排除“CLI 本来就会变化”及探针自身影响，独立目录恢复冻结基准的三个转发文件，仅将观测断言改成“key 不变”，再次运行同一真实 WS 流程；两种开关方向都为 **14 → 14**。对照 3 项通过，见 [baseline-control-summary.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/native-auth-provider-v4/cache-review/baseline-control-summary.json)。

官方契约依据：[client.rs:361](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:361) 把 cache key 相等列为续用前提；[client.rs:1255](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:1255) 在属性变化时返回无增量，[client.rs:1307](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/core/src/client.rs:1307) 因而不会带旧 ID 发送空增量。本次网关在 CLI 做完该判断后改变 key，确实破坏这一前提。

建议：对已经开始的 WS 响应链冻结 cache-key 改写策略；或在策略变更后拒绝旧响应的增量续接并明确要求客户端重新发完整请求。不要由网关伪造完整 history。是否真实服务端容忍、拒绝或重新路由这种变化仍未测；该复现不能证明回答质量下降。

### 2. HTTP 最终扩展超限时，未发送的失败请求仍留下新绑定

关联位置：[gateway.go:660](C:/Users/Administrator/Desktop/项目/codex-agent/gateway.go:660)、[gateway.go:676](C:/Users/Administrator/Desktop/项目/codex-agent/gateway.go:676)、[gateway_sessions.go:218](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_sessions.go:218)、[gateway_prompt_cache.go:46](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_prompt_cache.go:46)。

实际构造 **33,554,400 字节** 合法请求，距离 32 MiB 上限 32 字节，原 cache key 为 `s`，metadata session 同样为 `s`。`applyGatewayPolicy` 和 `prepareBody` 均成功；key 从 1 字符变 64 字符会增加 63 字节。实际通过完整 HTTP handler 得到：

- HTTP **413**，真实 loopback 上游 **0 次**调用。
- 请求前 session 无绑定，请求后绑定已存在且指向被选账号。
- 账号准入正常释放，未发生并发槽泄漏。

这里的确定缺陷是无上游发送的最终本地拒绝产生持久路由副作用，可能占用 sticky session 容量并影响下一次合法重试的账号选择。官方 CLI 不实现网关多账号绑定，不能把“不应绑定”的要求伪称成官方源码要求；这是网关失败原子性及资源边界问题。当前默认绑定 TTL 为 7 天。

建议：在选择并安装绑定前，按确定的 64 字符输出长度完成此类变长校验；或把“预选账号”和“提交新的会话绑定”分开。不要在失败后无条件删除 session，因为并发请求可能已经合法使用同一绑定。

### 3. 开关来源及管理说明

没有新增独立“账号级收敛”配置项，新增行为复用了原有 `PromptCacheKeyEnabled`。

- [store.go:638](C:/Users/Administrator/Desktop/项目/codex-agent/store.go:638) 的默认值为 true，历史配置未记录该字段时也使用默认值。
- [store.go:739](C:/Users/Administrator/Desktop/项目/codex-agent/store.go:739) 区分字段缺失与显式 false。`validateSettings` 不会把 false 改成 true。离线验证显式 false 经过 `Set`、实际落盘、全新 Store 重读后仍为 false。
- [admin.go:1287](C:/Users/Administrator/Desktop/项目/codex-agent/admin.go:1287) 仅在请求明确含该字段时修改它；未传字段沿用当前配置。因此现有 true 会自动启用新收敛逻辑，不能据此声称用户本次另外开启了收敛。
- 新 [GATEWAY-SETTINGS.md:54](C:/Users/Administrator/Desktop/项目/codex-agent/docs/GATEWAY-SETTINGS.md:54) 和 [CACHE-OPTIMIZATION-2026-09-25.md](C:/Users/Administrator/Desktop/项目/codex-agent/docs/CACHE-OPTIMIZATION-2026-09-25.md) 已说明该行为。
- 当前 [Settings.vue:365](C:/Users/Administrator/Desktop/项目/codex-agent/frontend/src/views/Settings.vue:365) 仍展示“保留已有缓存键”，与新后端默认值改写冲突。界面文字需要同步；若严格 CLI 对齐与账号级缓存优化需要独立控制，应使用显式独立策略开关，避免旧“补缺失字段”开关的含义被隐式扩大。

## 验证范围与重跑

共享源码现有 cache/compatibility 小套件：54 PASS、3 native SKIP、零失败。3 个 skip 为需要 Linux helper 的兼容 HTTP 测试，本次没有把它们计为 native 验证。

原始观察探针保存在 `cache-review/original-observation-probe.go.txt`；同一独立快照后来加入下述候选修复，重跑命令现在验证修复后的合同。PowerShell 单命令：

```powershell
go -C 'C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/native-auth-provider-v4/cache-review/source' test . -run TestCacheReview -count=1 -timeout=60s -v
```

这三类现象与“账号已被服务端打标”的推论严格分开：当前仅证明网关边界行为、UI 合同不一致和与 CLI 增量判断的差异。生产二进制、真实 TLS、历史账号质量及服务端集群归因不在本离线复现的验证范围。

## 独立快照内的候选修复（未合入、未部署）

经主任务要求，在上述独立目录内继续完成补丁及红绿验证。共享根目录业务源码保持原摘要，其他任务的代码没有被覆盖。

[candidate.patch](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/native-auth-provider-v4/cache-review/candidate.patch) 的最终 SHA-256 为 `ff91f571ef9f1c0af892caf70bf661ce1960255d816cf9292c04482ff2acae11`；逐文件前后摘要见 [candidate-manifest.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/native-auth-provider-v4/cache-review/candidate-manifest.json)。该 patch 的父版本是本报告审计的共享工作区缓存增量，**不能当作部署版本上的独立小修补直接应用**。

候选改法：

- HTTP 只在距离上限不足 63 字节时预检查默认键改写的确切长度；不需要实际账号即可知道新值必为 64 字符。该检查发生在账号选择之前，因此 413 不会创建绑定，亦不删除任何既有绑定。日常小请求直接跳过检查。
- WS 为每个已观察到的 response ID 记住首次使用的缓存开关与出站 key 摘要。续接原 response 使用原策略，新完整请求使用最新设置；新链之后仍可回到旧响应分支。账号仍固定在已有连接的认证身份，不增加账号切换。
- 记录复用原有 256 个 response ID 的有界 FIFO；pending 的 `response.created` 同样可查。只保存摘要，不持有原始 key 或额外历史。key 摘要读取使用原始 JSON span，避免为大 input 再分配整份 `RawMessage` 副本，计算发生在 turn mutex 外。
- 默认键链如果最终实际 key 不同，返回可恢复 400，要求客户端重新发送完整输入；不能伪造 history。未知/已淘汰的 prior 仅在缓存改写启用、策略准备后的请求确实匹配默认键且可能发生账号改写时返回可恢复 400，避免猜测另一连接先前采用的聚合策略；完全禁用改写时，跨连接未知 prior 原样透传，不因缺少本地历史而拒绝。这是明确的兼容性边界，需要合入前审阅。未参与默认键收敛的其他既有兼容流不会新增该摘要约束。
- 本地校验失败或准入失败的请求不创建新的 response policy；不把失败首轮的设置当成之后合法首轮的冻结值。

红测 [candidate-red.jsonl](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/native-auth-provider-v4/cache-review/candidate-red.jsonl) 在修复前实际报出双向开关续接失败及 HTTP 413 留绑定。首版候选的相关套件 **205 PASS、6 native opt-in SKIP、0 FAIL，`go vet .` 通过**，见 [candidate-green-summary.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/native-auth-provider-v4/cache-review/candidate-green-summary.json)。新增验证还包括客户端显式换 key 的增量请求被拒绝、新完整链采用新策略、旧链分支保持原策略、失败首轮不冻结、未知 prior、pending/completed 查找与 FIFO 淘汰、413 不删已有有效绑定。

期间一次扩大后的回归发现原有“没有默认 metadata、按模型 prefix 自动补缺失 key”的兼容测试被过宽摘要校验拒绝；候选已把摘要强约束限定为默认键收敛链，原测试未改且最终通过。最终补丁尚未经过 Linux/race/native 或真实模型验证，也没有声称它提升糖果题正确率。


### 协调者追加边界后的最终修订

协调者指出首版候选对“未知 prior + 默认 key”拒绝过宽。新增真实 WS 红测实际确认：完全关闭缓存改写时，第二条新连接的合法续接仍被 400；另一红测确认入站缺失 key、`applyGatewayPolicy` 补出的 key 恰好等于 metadata session 并被账号聚合后，原实现没有为这条链设置 tracked，下一轮显式改变 key 会被放过。红测记录 `candidate-second-red.jsonl`，两项都实际失败。

最终候选把默认键识别移至策略准备后的 bytes，记录实际进入账号聚合器的条件；未知 prior 的拒绝还要求改写开关开启。补充验证：关闭改写的两条独立 WS 连接逐字透传原始 key/metadata/previous_response_id；策略补齐再聚合的链能够拦截显式换 key 的错误续接，原样正确续接仍完成。

最终增量验证 **14 PASS、0 FAIL、0 SKIP**，覆盖全部 `TestCacheReview` 以及此前易受过宽策略影响的 `TestGatewayWSSessionAuditValidationWarmupAndContinuation`，见 [candidate-second-green-summary.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/native-auth-provider-v4/cache-review/candidate-second-green-summary.json)。前一轮的 205 项通过及 vet 属于上一候选摘要 `1d75eaba…`，本次没有重复整个大套件，不冒充最终补丁完整回归。最终摘要、测试范围与父版本均已写入 `candidate-manifest.json`。这份复杂候选继续保留独立目录待协调者审阅，不修改生产或共享业务源码。
