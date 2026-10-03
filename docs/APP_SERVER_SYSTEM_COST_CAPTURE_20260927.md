# 系统费用查询与认证检查：官方进程离线捕获

2026-09-27，正式 run `20260927T125419.084184Z`，固定官方二进制 `codex-cli 0.155.0`，SHA-256 `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`。两种认证 case 并行运行在 `unshare --net` 创建的新网络空间；核验仅有 loopback 接口和路由。没有读取线上账号、没有真实模型调用、没有真实费用。启动探测 pilot 与正式 150 秒轮询证据分开保存，不能把 probe 当成真实 turn 查询。

复跑命令：

```powershell
python tools/appserver_parity_system_loopback.py --remote
```

脚本保留官方 `POLL_INTERVAL=150s`，不修改可执行文件或计时器。固定 App Server 发起一个完整合成 turn，后台 collector 等待实际费用查询与 OTLP 费用点；每 20 秒打印运行进度。`--probe-only` 只用于快速排查配置，导出正式 oracle 会拒绝该模式。仅重新验证已有记录：

```powershell
python tools/appserver_parity_system_loopback.py --export-run .local/appserver-parity/system/20260927T125419.084184Z
```

## 捕获结果

| Case | 真实官方出站 | 官方轮询时间 | 消费合成响应后的实际 OTLP |
| --- | --- | --- | --- |
| API key + custom provider | `POST /v1/analytics/codex/turn-costs`，`{"turn_ids":["<actual_turn_id>"]}`，Bearer 为合成 provider key | 150242 ms；此前还有一次随机 turn 的启动探测 | `codex.turn.cost_microusd` 的 `asInt=125001`，conversation.id/turn.id 与实际 RPC/模型请求匹配 |
| ChatGPT auth + OpenAI provider | `POST /backend-api/wham/usage/thread-estimates/query`，threads 内含 thread_id/turn_ids，`include_settled_response_ids=true`；Bearer 加 ChatGPT-Account-Id 均为合成值 | 150207 ms；此前还有一次独立随机 thread/turn 的启动探测 | 同样 `asInt=125001`，对应实际 thread/turn；证明官方执行了 ChatGPT 金额转换与已结算 response 覆盖检查的成功路径 |

普通模型请求的 `x-codex-turn-metadata` 中 `thread_id`、`turn_id` 与后续费用查询一致。API-key 费用请求本身**没有 session/thread header**，只发送 turn ID；它的 User-Agent 是 `codex-cli`。ChatGPT 查询的 User-Agent 会随 initialize 前后进程身份变化，因此不能用探测阶段头值替代正常 turn 阶段头值。

API-key 夹具返回的形状：`turns:[{turn_id,status:"priced",total_usd:"0.125001",event_count:1,responses:[{response_id,total_usd}],model,speed,reasoning_effort}]`。ChatGPT 夹具返回的形状：`threads:[{thread_id,turns:[{turn_id,model,estimated_usage_usd_micros:125001,settled_response_ids:[response_id]}]}]`。这些返回值是**合成 backend 响应**；真实证据是官方进程写出的请求，以及它接收该响应后输出的精确费用事件，不是上游真实账单。

机器可读、带原始 body SHA-256 的向量：[appserver_system_cost_vectors.json](../internal/server/testdata/appserver_system_cost_vectors.json)。向量导出器额外断言实际 OTLP 金额为 125001，并检查实际模型 metadata 与费用 turn/thread 相同；不会只凭某个 metric 名字出现就认定成功。

## 同次运行验证的客户端认证检查

| 相同行为触发 | API key case | ChatGPT case |
| --- | --- | --- |
| RPC `account/rateLimits/read` | `-32600`，`chatgpt authentication required to read rate limits`；没有 `/wham/usage` HTTP | GET `/backend-api/wham/usage`，带合成 bearer/account，RPC 返回夹具额度窗口 |
| 完成普通 turn 时的 git attribution 设置 | 没有 settings/user 请求 | GET `/backend-api/wham/settings/user`，带认证 |
| RPC `plugin/list`、插件发现 | GET `/backend-api/plugins/featured?platform=codex`，**没有 Authorization** | 相同 featured 路径，带认证 |

这三项把先前源码中的 gate 转成了实际可执行证据。API-key case 还尝试过两次外部 CONNECT，由本地夹具返回 403；独立网络空间没有外部路由。它们与 curated 插件发现相关，不是失败的模型请求。此处没有安装、发布或修改任何远程插件。

## 费用映射增量实现

本阶段新增协议投影、最小映射与消费者费用查询 handler；完整 HTTP/WS 发送 hooks 由主任务集成验收，不能仅凭纯组件测试声称线上已切换：

- `newAppServerTurnCostLedger(db *Database)` 创建映射存储。`Record(ctx,keyID,clientTurnID,mapping)` 原子合并实际 response IDs；`Lookup` 读取。映射为 owner 引用、实际 upstream thread/turn IDs 和 response IDs，**不存凭据、prompt 或响应内容**。外层索引用 API key ID 与 client turn ID 的 SHA-256；生产 Redis 用同槽 hash/set 的单个 Lua 原子操作，内存仅供本地兼容和测试。
- 默认保存 7 天；过期或不存在只能得到 unknown/pending。相同 key/turn 的 owner、thread 或 upstream turn 改变时原子标记为冲突，保留原始归属数据但后续 Lookup 返回 unknown，旧身份重复写入也不能在 TTL 前复活旧报价。不能把前一账号产生的 turn 查询交给后一账号。这个组件没有替代 handler 的当前 key/账号授权检查。
- `projectAppServerTurnCosts(requested,mappings,backend)` 精确使用整数微美元，按六位小数输出，不经浮点。缺失/负数金额、缺失或未覆盖全部 response 的 settlement、未知/其他 thread、重复歧义条目均为 pending，金额为 null，**不是零**。零金额只有上游明确返回 0 且满足 settlement 条件时才 priced。
- `gatewayAppServerTurnCosts` 只接收精确 `/v1/analytics/codex/turn-costs` 的 POST。来自单个 owner 的后台查询响应与该 owner 的映射分组独立投影，查询只发送 ledger 中已记录的上游 IDs；客户端附加 owner/thread 字段不会进入请求。未知 probe、别的 key 的 turn、冲突映射均 pending 且不触发上游查询。已有 turn 遇到 401 只允许同账号认证恢复，不能换账号查询旧账单。
- `newAppServerCostObservation` 仅接受原始客户端的真实 turn ID；`BindSent` 记录实际 writer 出站的 thread/turn，`ObserveResponse` 只收已验证的 `response.completed` ID，预热不计入，缺失 ID 不临时编造关联。它不保存请求文本，最后由主流程在成功和 owner CAS 确认后提交 ledger。

本地检查 `go test ./internal/server -run '^TestAppServerTurnCost' -count=1` 覆盖金额边界、最大 int64 精度、多 response 覆盖、并发合并、冲突标记、跨 key 隔离、过期、实际 writer 向量、发送 metadata、不同 owner 使用相同 upstream thread/turn IDs 的分组隔离，以及 401 不跨 owner 查询。真实 Redis 测试已通过现有 SSH 隧道连到隔离集成 Redis，使用随机 namespace 验证两个 ledger 实例的原子并发、冲突标记、7 天 TTL，并清理该测试的全部键。证据为 `.local/appserver-parity/system/20260927T125419.084184Z/redis-ledger-test.log`；本地无 `TEST_REDIS_URL` 的普通调用仍会明确 skip 这一项。

源码依据：`app-server/src/turn_cost_worker.rs:25,101-126,205-300,429-454`，`turn_cost_worker_chatgpt.rs:35-84`，`backend-client/src/client/{turn_usage,chatgpt_turn_cost}.rs`。静态负向金额/settlement 条件来自官方源码和对应纯单元测试；本次官方进程在线程费用上的实际 capture 覆盖的是成功结算路径。
