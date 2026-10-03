# Session 503 / store 错误分类只读交接

2026-09-28。本轮遵守 20:31 源码冻结，不修改运行源码、测试或工具，不读取客户正文或凭据，不访问真实模型/部署。B 组已认领合成矩阵，A 组不重复矩阵，转入独立 structured-output 投影验证。

## 实际证据

`.local/cache-scope-release-20260928/natural-errors-before-scope.json` 的 12:20:20 UTC 快照关联运行 SHA `3d00cac386b7104d20f2adfccde5f583984b5206c60dfd92bb41c9e1c9b1607f`，进程起于 11:45:21 UTC，502 请求、56 错误；这不是尚在验收的新版本。输入文件 SHA256：`B7A3182CE3793C8A1D88C32E16C10B53EFCD0DA80C89F95BDE67176DCE0AF824`。

| 组 | 数量 / 时间 UTC | 可确认分类 |
| --- | --- | --- |
| HTTP 503，`session account is no longer enabled or allowed for this key; start a new thread` | 13，11:59:54–12:12:07 | 绑定 owner 不在当前候选集合，属于会话账户可用性/授权边界；source/phase 均缺失 |
| HTTP 400，`bps: persistent store is unsupported; use client-managed history` | 1，12:19:01 | 本地 `request_validation`，不是 Redis/PG 存储故障 |

旧 11:49 审计另有 7 条 bound-session 503，时间跨 08:56–11:06，早于本次激活；不能加进上述 13 条或用来证明新版本自然失败。

## 代码路径与准确边界

`gateway_sessions.go:328` 是含完整 `; start a new thread` 文本的唯一源码落点。`pickSessionAccount` 已取得已有绑定，`candidatesForContext` 取得 enabled 且与 key groups 有交集的账户，扫描没有找到 exact owner，也未找到可识别的旧 owner 记录，才到该末端错误。可能前因是 disabled、账户/key 分组变更、删除等；聚合中没有对应时刻账户状态或操作审计，不能逐条判定是哪一种。

这条路径不同于：

- Redis 失败：`session routing unavailable`；设置读取失败：`session routing settings unavailable`。
- 账户刷新失败：`session account unavailable; restore its credentials or start a new thread`。
- owner 替换可被识别：`gatewayOwnerChangedError`，映射 HTTP 409。
- 并发忙或 cooldown：类型化错误映射 HTTP 429。

因此当前 13 条不能称为 Redis/PG、CPU、连接池或泛化上游 503 故障；也不能仅凭文本断言 13 条全部包含加密内容。当前代码对完整明文请求已允许受控账户 failover，而带 previous_response_id、encrypted_content、encrypted_function_args、file_id 或 compaction 的请求不可跨 owner 重放。哪一条实际被此保护阻止，需 B 组矩阵和额外脱敏状态证据，而非客户正文。

`internal/bps/request.go:142` 的 store 检查在 `bps.Prepare` 内发生，早于账户选择和上游派发：`store:true` **或不能解码为 bool 的值**都会得到同一错误。`false`、缺省及 JSON null 当前通过，并对上游固定 `store:false`。所以聚合的 1 条不能确定是合法 `true` 需求还是客户端类型错误；它也没有证明服务器存储不可用。网关的 TTL 续聊历史缓存不等于完整 Responses 持久存储 API，直接删掉 store:true 不是保真修复。

## 独立发现及发布判断

可改进的非阻断问题：`gateway_bps.go` 账户选择失败只填写 Status/Error 后调用 `writeGatewaySelectionError`，没有设置 `ErrorSource/ErrorPhase`；聚合确实出现 source/phase=null。更细的 machine code、`account_selection` 阶段与“可恢复分组/账户授权”和“原 owner 已不存在”区分可改善诊断，但不影响已经拒绝越权/跨 owner 派发的安全边界，不能当成本次新发布阻断证据。

store 参数拒绝亦可未来改成带 param=store 的类型化错误，并把非法类型与不支持 true 分开；本轮仅提出诊断契约改进，不冒充已实现持久化支持。现有聚合没有可证实的新增协议损坏，也没有足以阻断当前冻结发布的回归证据。不存在“所有 503 同根因”的结论。

## 本轮只读源码 SHA256

| 文件 | SHA256 |
| --- | --- |
| `internal/server/gateway_sessions.go` | `64B6867321D5287B498CF2C9FE8294FB24D68E9C1CF5748BB077A7FB84DAC935` |
| `internal/server/gateway.go` | `896F29DD3206BE4BA06D91290E80E65BF540A80DF0D702384229099F0F34665F` |
| `internal/server/gateway_cooldown.go` | `937E996FFCF5F75526A8794F54CFC23737B513864C050B92E68CE4B9DCC413DF` |
| `internal/server/gateway_bps_mode_migration.go` | `9756164E48258275A4EBE6DF4CA58912499F047C787A711C1A4BFF218F418ECE` |
| `internal/bps/request.go` | `0DC20648396C75C52A600755806C80F0DFAD7BAEE28FFFC380E9D38206CF1FF5` |
