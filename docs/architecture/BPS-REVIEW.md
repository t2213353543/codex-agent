# BPS 网关独立审查

日期：2026-09-27。范围：`internal/server/gateway_bps*.go`、`internal/bps/` 及其依赖的账号授权、历史存储和流事件处理。此次审查只读取源码并运行 loopback/纯函数测试，没有调用生产或读取真实凭据。

审查测试集中在 `internal/server/gateway_bps_review_test.go`。测试断言真实 handler 输出、实际 loopback 请求、独立存储状态和 deadline controller，不使用模型自述作为执行证据。

## 已发现并修复复验的问题

| 问题 | 触发与影响 | 当前复验 |
| --- | --- | --- |
| 延迟取消误杀复用 stream | 同一个 stream 完成旧 response 后启动新 response，旧 `response_id` 的 cancel 只按 stream 查找，会中止新任务 | `TestBPSReviewStaleCancelDoesNotCancelReusedStream` 通过；取消现在同时核对 response identity |
| legacy compact 把失败当成功 | HTTP 200 的 JSON 仍可能含 `status: failed` 或非 null `error`；原分支只确认存在 compaction 项便记为成功 | `TestBPSReviewLegacyCompactFailureIsRejected` 通过；失败结果被拒绝 |
| legacy compact 绕过交付授权复核 | 账号在 compact 运行中禁用，原 JSON 分支不经过 `OnComplete`，仍交付加密结果 | `TestBPSReviewLegacyCompactRechecksAccountAuthorization` 通过；该分支纳入完成前验证与存储 |
| incremental 缓存身份反复 hash | 把已按账号/key 隔离的 outbound body 存为客户端 history，下一轮 `previous_response_id` 恢复后再隔离一次，cache key 每轮变化 | `TestBPSReviewIncrementalHistoryKeepsPromptCacheIdentity` 通过；保存隔离前的 `historyBody` |
| WebSocket 被 HTTP SSE 开关控制 | WS turn 复用 HTTP handler 且强制 stream，错误受 `HTTPStreamEnabled` 限制；已打开 socket 也绕过后续 WS 设置变更 | `TestBPSReviewWebSocketSettingsRemainIndependentAndRefreshPerTurn` 通过；transport 分离并逐轮重验设置 |
| 新 SSE 路径缺下游写 deadline | 直接写客户端 ResponseWriter、主 HTTP server 无总 WriteTimeout，慢消费者可占住 handler 与 admission | `TestBPSReviewHTTPStreamUsesBoundedDownstreamWrites` 通过；每次写入/flush 设置 deadline，结束清除 |
| history 比 routing 活得久时错误换号 | history 保留一小时，而 routing TTL 可以一分钟；恢复前先普通选号会选中更高优先级账号并报 owner 冲突 | `TestBPSReviewHistoryRestoresOwnerAfterRoutingExpiry` 通过；利用持久 history owner 恢复 binding，再走权限与并发准入 |
| 刷新残留 BPS 身份头 | 通用 401 retry 原来只替换 bearer/ChatGPT-Account-ID，BPS 的额外 account/user 头仍来自旧 credential | dispatch 现在重建 BPS 头；`TestBPSReviewCredentialHeaderRewriteClearsRetiredIdentity` 通过旧头清除检查。真实 refresh 端到端由主任务另行验证 |
| 工具并发策略到最终事件才检查 | `parallel_tool_calls=false` 时，第二个可执行 done 原先已经发送给 CLI，随后拒绝 completed 无法阻止其执行 | `TestBPSReviewParallelFalseRejectsSecondCallBeforeDelivery` 先复现为红，增量调用身份计数修复后通过；第二个调用交付前被阻断 |

## 复验结果

当前九个 `TestBPSReview` 已全部通过；本审查新增的已复现问题均已修复。纯本地三套 manifest（acceptance、gateway audit、Go snapshot）再次核对：当前 417 个根入口及 internal 包 Go 文件全部被纳入，未包含 `.local/` 或 `data/`。该数字是本轮检查时的源码数量，会随新增实现变化。

## 存储与兼容边界

- History 的 Redis key 按客户端 key ID 与 response ID 摘要隔离，值经过现有数据库加密；记录含 account owner 与 session，恢复后还需要当前权限与 owner 检查。代码审查不替代真实 Redis 的跨进程、重启、密钥轮换与故障验收。
- 缺失、过期或不可解密 history 应失败关闭，不能换号继续发送旧的加密 reasoning/compaction。
- 当前 request/tool 支持应以明确识别的字段为准。例如旧 CLI 某些模型会声明 `tool_search`；若实现拒绝该 family，应作为兼容限制记录，而非宣称所有 Codex 工具形态都可用。
- Stream 维护者已补充 `TestStreamTerminalMustIncludeEveryPreviouslyCompletedItem` 与 `TestStreamCompletedWithNonNullErrorCannotSucceed`：前序 done 项使用有界摘要按 index/id 与 terminal 对照，缺项或变化被拒绝；completed 中非 null error 不能记为成功。该改动由其负责本地及 Linux race 复验。
- 发布时使用新源码指纹和测试记录；此前 Python buffered proof 的结果不能替代当前 Go streaming/WS 实现的验收。

复跑审查测试：

```powershell
go test ./internal/server -run '^TestBPSReview' -count=1 -timeout 20s
```

## 实际 Redis 专项

新增 `gateway_bps_redis_test.go`，仅在显式 `TEST_REDIS_URL` 下运行，不需要 PostgreSQL 或真实账户。每个测试使用独立 AES-GCM 合成 key 与 `codex-agent:integration:bps-test:<随机>:` 命名空间。清理只对测试明确登记的 key、index、sizes 执行 `DEL`，不扫描或清空 Redis。

四个用例覆盖新 Proxy/新 Redis 连接恢复历史、密文不包含明文、跨客户端 key 隔离、篡改/过期/缺失失败关闭、TTL、512 条容量上限、过期索引清理和字节预算分支。字节预算分支通过合成 size ledger 触发，不宣称实际写入了 128 MiB 数据。

本机编译时四项因缺少 `TEST_REDIS_URL` 跳过。随后隔离 Linux Redis race 首次实测发现：同秒 expiry score 相同、缓存达到 512 条时，新插入的较小 hash key 被当成 oldest 自我驱逐。保留首次失败报告后，Lua 淘汰逻辑已排除当前插入项；再次实际 Redis race 四项全部通过，无 DATA RACE。联合 BPS race 共 55 个主测试通过（含子测试 84 pass），两个 live 凭据门控测试跳过。

实际报告：`.local/powerpoint-migration/race/20260927T005409.307964Z/report.json`；该证据验证的是新实例/新 Redis 连接的恢复，不等同于真实操作系统进程重启。

```sh
go test -race ./internal/server -run '^TestBPSRedis' -count=1 -v -timeout 90s
```
