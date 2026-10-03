# 缓存统计边界协作检查 — 2026-09-28

记录时间：2026-09-28 20:19 +08:00。执行者：协作 A / cache_boundary_review。已先读取 `docs/CACHE-AND-STREAM-RELIABILITY-20260928.md`。本报告基于共享源码、本地临时文件统计和 loopback 合成上游；没有访问生产或真实上游、修改账户/模型/调度配置、部署服务。

## 已复现并修复

`managedUsageTotals.add` 的缓存创建分支与 PostgreSQL 对应聚合没有使用 inference eligibility 过滤。带 `cache_write_tokens` 的 `models` 或 `websocket_warmup` 记录会进入管理统计的创建 token 与样本数；总览的 `OverviewCacheUsage` 已排除这些记录。读取统计已经使用过滤，因此页面之间会出现“读取一致、创建不一致”。即使控制请求只报告创建 0，也会虚增创建样本数。

父会话先批准独立复现，收到失败结果后明确认领并批准只修两处运行实现：

- `internal/server/admin_usage_management.go:151`：创建计数增加 `cacheObservationEligible(r)`。
- `internal/server/admin_usage_statistics_pg.go:77`：创建 `count` 与 `sum` 增加 `cache_eligible`。

没有根据成功状态、`UsageKnown` 或读取字段过滤创建：失败请求的实际用量仍保留，主用量未知但独立报告的创建仍保留，显式创建 0 仍计为已测量。请求总数、成功失败数量、legacy token 总量均未修改。

原始四条合成记录（两个推理请求、一个 warmup、一个 models）修复前结果：

| 控制请求各自 creation | 总览 samples/tokens | 管理统计 samples/tokens | 两边 read samples |
| --- | --- | --- | --- |
| 0 | 2 / 30 | 4 / 30 | 2 |
| 100 | 2 / 30 | 4 / 230 | 2 |

这是实际执行失败断言所得，不是线上数据；证据文件 `.local/collab-a-20260928/cache/reproduction-before.log`。

## 测试与边界

新增测试仅位于 `internal/server/collab_cache_boundary_20260928_test.go`。复现阶段曾用显式 opt-in 避免干扰并行发布；修复后已移除 opt-in，正常回归默认执行。

- `TestCollabCacheBoundary20260928ControlCreationExcluded`：文件后端；控制请求报告创建 0 或正数均排除，正常及失败推理保留。后续按父会话要求补入推理创建已知 0，以及 `UsageKnown=false` 但创建 7；最终六条记录的总览和管理统计、model/series 分组均为创建 4 个样本 / 37 tokens，读取 3 个样本 / 170 tokens、未知读取 1 条，原始请求仍为 6 条 / 失败 1 条。实测 PASS。
- `TestCollabCacheBoundary20260928FailureReadKnowledge`：分别对 `ok`、`error`、`aborted` 验证显式 0、缺失读取、显式未知及不可能的读取数。均为读取 80/200=40%，2 个读取样本、4 条未知请求；创建独立为 120，实测 PASS。
- `TestPGCollabCacheCreationControlExclusion20260928`：与文件后端共用完整 fixture、使用既有随机 schema 的 `newMetricsTestDB`。本机没有 `TEST_DATABASE_URL`，两个 PG 子用例实际 SKIP，不能把父测试显示的 PASS 当成数据库验证。Docker CLI 存在但本机 Linux daemon pipe 未运行。父会话将用隔离 integration PostgreSQL runner 验证；本报告写入时尚未收到该结果。
- 另外运行已有 namespace/API key/account/model 隔离、token refresh 稳定性、compaction 排除、最终发出 key 观测、known/unknown 持久化与导出、读取/创建独立性等限定测试。初版回归共 35 个顶层测试 PASS，`go test` 包执行时间 0.863s；证据 `.local/collab-a-20260928/cache/regression-after.log`。扩充 fixture 后新增测试再次 PASS（PG 子用例 SKIP），包时间 0.050s；证据 `.local/collab-a-20260928/cache/regression-expanded-after.log`。

最小本地复跑：

```powershell
go test ./internal/server -run '^(TestCollabCacheBoundary20260928|TestPGCollabCacheCreationControlExclusion20260928)' -count=1 -v
```

完整已执行限定命令：

```powershell
go test ./internal/server -run '^(TestCollabCacheBoundary20260928|TestPromptCacheObservation|TestBPSDefaultCache|TestBPSSharedCache|TestGatewaySub2CacheNamespace|TestResponseCacheCreationAndReadAreIndependent|TestCacheReadCreationStatisticsFile|TestOverviewCacheReadAndCreationContract|TestManagedUsageCacheKnowledgePersistenceAndExport|TestBPSFinalRequestCacheObservation|TestBPSRejectedParameters|TestBPSTerminalCache|TestBPSTerminalUsage)' -count=1 -v
```

## 只读审查结论

- `gateway_bps_default_cache.go` 的默认/共享 key 使用 tenant key ID、已确认 owner namespace、模型；相关 fixture 已验证跨 tenant/user/workspace/model 不合并、同 owner token refresh 稳定、缺 owner 不合并显式 session hint、compaction 不填 key。本次没有发现这部分新的实测隔离失败。
- `gateway_prompt_cache_observation.go` 的 scope 包含 tenant，比较包含 account/cache key/model 变化；有限 LRU、盐化摘要、输入前缀与状态化续聊边界已有本地测试通过。它记录发出请求的观察，不是成功创建缓存的证明；失败请求仍能更新最近观察，所以统计其前缀相同不能证明服务端命中。
- `tools/cache_rate_report.py` 的控制请求过滤与服务端口径差异已报告父会话，并由协作会话 B 交接给会话 C `01a0e7ed-5abb-7690-8d5d-c11b4273b156`。本分支不修改该脚本，也不重复线上探测。
- 生产流量中究竟存在多少带创建字段的控制请求，本任务未查询；没有宣称生产影响量或命中率改善。

## 文件 SHA-256

完整前后快照：`.local/collab-a-20260928/cache/source-before.json`、`source-after.json`。下表是 20:19 +08:00 写报告前读取到的值。

| 文件 | 修复前 | 当前 |
| --- | --- | --- |
| admin_usage_management.go | ECF3A41A8DD3DCEE68416EF9CBAB5186434C734A42FAB3361E24449D70022E3E | 0BC40496CDC2D8CF9442EAF9EDB5C2C5D40AD5AB05238F0A2B0F224ECAF2AEBB |
| admin_usage_statistics_pg.go | 55307846E869E1E7B016B158F3A90F13976493978A4BAE70E9D13E5E10431461 | 5500E17421D8CD57CDB852E3821B5D51275FB1562F8095FA796491A59BB307A3 |
| collab_cache_boundary_20260928_test.go | 6E526F603E25C1464ADD482F1EDEAD796D99B4D235B01910B222F0FCE3A9776A（初始复现版） | F2AF1515826B70101D027064A73D60A0E195944BBB0E7F0D19B1B49A01892826 |

以下已审查文件在前后快照间未漂移：

| 文件 | SHA-256 |
| --- | --- |
| stats_cache.go | 787499E3F75A1BA03AB1D97A0DC3078BC3AC30AE682ABF8B7F255E3C45D445B2 |
| gateway_bps_default_cache.go | A782083144F1CD5A5030FA962060EC008BB2A7EC491A6125DB495C2C8451EB76 |
| gateway_bps_cache_scope_test.go | DF575C4A7E2F98DBBEB181C4D34B43CA324898664D1EE47E0ECC3E590AE1FE0D |
| gateway_prompt_cache_observation.go | 26A35F9735F30DEC6BDA71EDB815C2641177ACA2FA82C9890F552ACEB6013191 |
| tools/cache_rate_report.py | 0B0BC16DCB46EB34E9728C3A73EB266DD69534644DCADE79C8168DA9962A581F |

共享目录没有 Git 历史，本检查没有全仓库冻结；此处稳定性仅涵盖上列快照，后续其他会话修改以各自证据为准。没有部署回执。

## 根任务补充：隔离 PostgreSQL 已实际执行

20:23 +08 收到独立 Linux 测试结果：冻结源码 `f30b30105a38cc0d3a665f581122978d945a44543f82d94036bbe88e33e3c6dc`；测试二进制 `f61e854c890f0ff5980cf13eabe19ab3e1b6fb939cb6da6d4729fed243de8682`。三个选定测试全部实际 PASS，没有 skip/fail，测试进程退出码 0、执行耗时 735 ms：

- `TestPGCollabCacheCreationControlExclusion20260928`（创建 0、正数两个子用例均 PASS）
- `TestCacheReadCreationStatisticsPG`
- `TestPGManagedUsageAggregationParityReview`

测试使用既有独立 `codex_agent_integration` 数据库和 `codex_agent_test` 用户，容器端口核对为 loopback `25432`，每个 fixture 使用随机 schema。没有连接生产数据库或真实模型，临时远端二进制已删除。

原始报告：[report.json](../.local/cache-observation-pg-linux/20260928T121930.783972Z/report.json)，原始事件：[test.jsonl](../.local/cache-observation-pg-linux/20260928T121930.783972Z/test.jsonl)。**整树报告仍保持 fail**：冻结后另外的 JSON 错误测试增加用例，错误 helper 增加删除 Content-Length；这两个非缓存文件发生变化，严格全树校验不能通过。缓存两处运行修改、独立 fixture 与既有统计测试的摘要均与受测快照相同，见 [专项范围核对](../.local/collab-a-20260928/cache/pg-scope-check.json)。本补充仅证明冻结版本的缓存 PG 专项通过，不覆盖当前全部源码的发布门禁。

一条命令复跑相同隔离测试（不修改共享 runner）：

```powershell
python -c "import sys; sys.path.insert(0, 'tools'); import cache_observation_pg_linux_check as c; c.TESTS=['TestPGCollabCacheCreationControlExclusion20260928','TestCacheReadCreationStatisticsPG','TestPGManagedUsageAggregationParityReview']; raise SystemExit(c.main())"
```
