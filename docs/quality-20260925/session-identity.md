# Session 身份与上游实际字段的一致性修复

日期：2026-09-25。仅本地源码研究及合成 loopback 回归；没有 SSH、真实模型请求或账号切换实验。

## 已复现的两个边界

`gatewaySessionKey` 先读取可被转发的 session/thread header，再按既有产品规则把客户端明确提供的 `prompt_cache_key` 作为 fallback。`applyGatewayPolicy` 使用精确的 JSON map 字段，但旧 `gatewaySessionKey` 使用 Go struct 解码，因此未知的 `Prompt_Cache_Key`、`PROMPT_CACHE_KEY` 会覆盖标准小写字段。结果是原样发给上游的 canonical cache key 不变，本地黏性身份却改变。

新增测试通过真实 TCP 网关和上游，设置两个合成账号的优先级实现确定性选择：首轮 A 优先、标准小写 cache key 建立 A 绑定；第二轮把 B 的优先级调高，但 A 仍启用且空闲。第二轮保留相同标准 key、`previous_response_id` 及一个 opaque history sentinel，仅加入未知大小写扩展。修复前 3 个场景都派发到 B，上游合成 owner 校验返回 400；捕获到的标准 cache key 和 opaque input 均未变化。这个 owner 校验只是精确账号关联 fixture，不是真实加密解密或模型推理。

另外，HTTP header 的 `strings.TrimSpace` 会移除 Unicode 空白，范围大于 HTTP framing 的 SP/HTAB。真实 TCP 测试证明：首尾 ASCII SP/HTAB 在线路上会被 HTTP 层移除，绑定到同一 session 合理；首尾 NBSP (`U+00A0`) 与全角空格 (`U+3000`) 则原样到达上游，旧本地逻辑却把它们和不含这些字节的 session 合并。按相同优先级切换控制，两个独立 wire 身份错误复用 A 绑定。修复前日志：`.local/research-20260925/session-identity-red.log`，包括 3 个 cache-key shadow 失败、2 个 Unicode header 合并失败，以及通过的对照。

补充证据是只带未知 `Prompt_Cache_Key`、没有标准 cache key 或 session header 的无状态请求：旧代码会建立错误黏性，第二条独立请求仍走 A。策略生成的标准应用 cache prefix 不同于会话身份，未知字段也不应成为身份。该失败以及 canonical null/类型错误/重复成员的定位用例见 `.local/research-20260925/session-identity-fallback-red.log`。

## 修改

只修改 `gateway_sessions.go` 的 `gatewaySessionKey`：

- header 值仅裁掉 HTTP 可选空白 SP 和 HTAB，保留其他实际 header 字节。
- body 解码为 `map[string]json.RawMessage`，仅读取精确的 `prompt_cache_key`；未知大小写扩展不参与路由。
- 无效/非字符串 canonical 值不产生 fallback 身份，错误请求的既有 policy 校验保持原处处理。

没有改请求正文、未知扩展字段、Session-Id 转发值、账号选择算法或 session TTL。原有 header 优先级、Connection/hop header 过滤、重复标量 header 选择、租户 namespace 和 WS 长度限制均保留。

两个精确同名 `prompt_cache_key` 成员继续按现有 policy 的 JSON map 语义使用最后一项；大小写不同的成员是未知扩展，不等价于精确同名成员。测试覆盖顺序变化、canonical null/数字/对象、malformed JSON，以及明确 header 始终优先的对照。

## 产品边界与 CLI 对照

这次修复维持产品已有的“客户端显式 cache key 作为缺失 header 时的黏性 fallback”，不把所有共享 `prompt_cache_key` 都宣称为同一真实会话，也没有改变 fallback 架构。自动生成的共享应用 prefix 仍只用于缓存，不能被反推成会话绑定；测试确认两条无身份请求具有相同生成 key，但按新优先级独立走 A、B，且 session binding 数为 0。

固定 CLI 0.155.0 的 `core/src/client.rs` 在约 1164 行通过 `build_session_headers` 传递 session/thread 身份。测试中的未知大小写成员和 Unicode header 是协议边界输入，不声称官方 CLI 正常产生这些内容。修复提高网关本地路由与实际传输字段的一致性，不能由此推断历史模型错答的原因、服务端账号标记或算力状态。

## 验证

新增文件 `gateway_research_session_identity_test.go` 包含：

1. 两账号真实 TCP 的 canonical cache key continuation，4 个 unknown extension 情形；另有显式 opt-in 的 Linux Rust native transport 分支。
2. 实际上游 header 字节与本地绑定的 ASCII OWS/NBSP/U+3000 对照。
3. 9 种 canonical 成员/未知成员/重复精确成员/非法输入及 header 优先级。
4. generated prefix 相同、未知扩展原样保留，但无身份请求不建立黏性的两轮验证。

最终 Windows 针对性 session/ownership/cache/policy/CLI 回归：**50 个顶层测试通过，含子项 130 pass、1 个 native 分支明确 skip、0 失败，0.859 秒**。日志 `.local/research-20260925/session-identity-green.jsonl`。此前新增的实际 TCP red 均转 green；native 分支需由根任务在 Linux 提供固定真实 helper 后运行，不把 Go transport 证据算作 Rust 成功。

复跑：

```powershell
go test . -run '^TestResearchSession' -count=1 -timeout=90s -v
```

冻结 SHA256：

- `gateway_sessions.go`：`4491e016ebd0f6f974d8bdc9f201c14a416139a184eaaae0a890d190c50bd3cc`
- `gateway_research_session_identity_test.go`：`fd5c359e532929eeaa040955f54a645d6ebfb76995c0f8f79efc1f44575c1ef8`

最终主任务回填：真实 Rust 双账号续接分支已在 Linux 全套执行；race/PG/Redis/native **873 项通过**，CLI 独立 **7 项通过**，已部署 `cdc07d09…39752`，HTTP 200/WS 101 正常。详见[完整上线证据](../CODEX-GATEWAY-CONTROL-RESEARCH-2026-09-25.md)，不将本次状态归属修复宣称为原题质量恢复。
