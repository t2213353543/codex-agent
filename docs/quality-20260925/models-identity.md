# 模型目录身份、ETag 与显式会话路由

日期：2026-09-25。只做本地源码对照和合成 HTTP loopback；没有 SSH、真实模型或真实账号调用。

## 实际发现与反证

发现一个正常显式 Session-Id 请求可触发的目录/推理账号不一致：`forwardModels` 原先始终 `pickAccount`，即使该 header 已绑定推理账号也重新选目录账号。首轮推理和目录都来自 A，之后 B 优先级升高，携带相同 Session-Id 的目录刷新从 B 返回；紧接着同线程的推理仍来自 A。两次目录可以携带不同 instructions、default service tier 或模型能力，因此不应在已有明确绑定时忽略身份。

真实 TCP 上游和下游测试保存了实际 Authorization 对应的账号别名、Session-Id、If-None-Match、client_version。普通目录 GET 与条件 GET 两个场景修复前均失败：实际目录账号变为 B；条件 GET 不是 A 的 304，而是 B 的 200 新目录。原始证据 `.local/research-20260925/models-identity-red.log`。

**未证实“切账号一定产生错误 304”**。对照测试使用正确区分账号/版本内容的 ETag，结果符合预期：同账号同目录返回无正文 304；无 session 切到不同目录账号时返回 200 和新 ETag；客户端版本改变，以及 fingerprint profile 覆盖 client_version 后，正确上游也返回新表示。不能为了制造失败，假定不同目录必然使用错误相同强 ETag。

## CLI 0.155.0 缓存行为核对

本地固定源代码给出的证据：

- `codex-api/src/endpoint/models.rs:45` 的 `list_models` 读取响应 ETag，解析 `ModelsResponse`；本身不生成 If-None-Match。
- `model-provider/src/models_endpoint.rs:116` 以 `HeaderMap::new()` 调用目录 client，默认不传 Session-Id，也不传条件 ETag。provider 配置仍可能附加自定义 header，不能把默认路径说成所有配置。
- `models-manager/src/manager.rs:408` 收到 `x-models-etag` 后，仅当当前 provider/auth identity 和缓存 ETag 都匹配时续 TTL；否则执行完整目录刷新。
- `models-manager/src/cache.rs` 的 cache entry 有 client_version 与 identity；TTL 更新同时检查版本、identity 和 ETag。
- `model-provider/src/models_identity.rs` 对 provider 名称、base URL、query、认证方式、账号/user/email/plan 等做长度前缀哈希。具有完整稳定 ChatGPT owner 时不把轮换 access token 当新身份；显式/无稳定身份 API 凭据参与摘要。

因此本次显式 session 目录修复针对 gateway 的兼容请求契约，**不是官方 CLI 默认目录请求的已知触发条件**。网关内部多账号被隐藏在一个 API key/provider 后，客户端无法凭这一个稳定 identity 自动识别每次网关选出的账号；这个架构差异仍存在，不能用本次局部修复声称完整解决所有无 session 目录与推理账号的差异。

`ETag`、`x-models-etag` 仍按原值转发，不注入网关生成的伪 ETag，不重写模型 metadata，不让目录 GET 消耗一次真实推理。目录成功体已有 8 MiB/读取错误边界及 304 独立分支；本次回归包括既有 truncated/oversized models 检查，没有修改这些错误体规则。

## 最小修复

仅修改 `gateway.go` 中 `forwardModels` 的账号选择前段：

1. sticky 开启且请求具有可转发的明确 session/thread header 时，先读取当前 key namespace 下的已有绑定。
2. 仅当绑定存在，才把该 session 放入 route context，并调用既有 `pickSessionAccount`。目录与推理因此沿用相同 owner 及 session proxy 选择规则。
3. 没有 header、没有既有绑定、sticky 关闭时使用空 session，维持现有独立目录账号/代理选择；GET `/models` 不建立新会话绑定。
4. 绑定存储不可读时在账号选择阶段失败；已绑定账号被停用或不可授权时拒绝，不偷偷改走备用账号。

此行为意味着已有显式绑定账号暂时不可用时，目录也会反映该不可用状态，避免拿另一账号的能力表替代。相同文字 session 在不同 API key 下仍隔离。

## 验证

新增 `gateway_research_models_identity_test.go`：

- 真实 TCP 双账号、已有 inference session、普通/条件目录 GET，最后再发同 session 推理确认原绑定没有丢失。
- 无 session：A 200 → A 304 → B 200，新旧目录与 ETag 区分，且没有新绑定。
- fingerprint 关闭时保留 caller client_version，开启时使用 profile version；缓存 validator 随上游实际表示更新。
- unbound header、sticky 关闭、bound owner 停用、另一 key 使用相同 header 四种边界。
- 缺少共享绑定存储时直接调用目录选择边界，确认失败发生在任何账号选择/上游请求之前；不把不完整 PG fixture 假装成完整认证 ingress。
- 显式 opt-in 的 Linux 真实 Rust helper 分支使用同样的 loopback，不访问模型端点。

最终 Windows 针对性回归包含新测试与既有 models、304、header、session authorization、fingerprint、catalog read failure 检查：**47 个顶层测试通过、1 项明确跳过、0 失败，1.071 秒**；含子项 162 pass、2 skip。日志 `.local/research-20260925/models-identity-green.jsonl`。真实 native 子项须由根任务在 Linux 设置固定 helper 后验证，不能用 Windows skip 代替。

复跑：

```powershell
go test . -run '^TestResearchModels' -count=1 -timeout=90s -v
```

冻结 SHA256：

- `gateway.go`：`97a95cb3dec35206c1c8ef8940548a1a36134a7a26fdf82d1d274a9bb80daf36`
- `gateway_research_models_identity_test.go`：`aa419ae6736b04f517974e5718435d9cb31cdfbd1a88345fca27e7570a08992b`

本次修复不能证明糖果题错答来自模型目录，不能证明账号被标记或被发送到特定算力集群；实际证据范围是已有显式会话请求中目录与推理 owner 的一致性。

主任务最终回填：Linux race/PG/Redis/native 880 项通过，真实 native 目录身份分支已执行；CLI 独立 7 项通过，后端 `33f4535f…cdbe5e` 已上线。HTTP models 200、WS 101 正常，详情见[最终集成记录](../CODEX-GATEWAY-AUTH-CATALOG-RESEARCH-2026-09-25.md)。
