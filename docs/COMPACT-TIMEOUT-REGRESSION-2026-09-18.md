# Compact 自定义 Transport 总超时回归

## 结论

对冻结候选 `5a818844f613ad35602ac466c80d8949a2e7f7a180e96ae21c4cbd1573ce6d22` 增加源码 owner 的最新最小回归测试，**确认旧候选存在已知缺陷**。只通过 overlay 替换 `gateway_client_pool.go` 为 owner 的单文件修复后，最小回归通过。未修改冻结候选、共享 Go 实现、现网 binary 或配置，未执行 SSH、代理恢复或真实推理。

报告：`.local/compact-timeout-regression/20260918T023719.775475Z/report.json`，schema `codex-agent-compact-timeout-regression-v1`，status `reproduced_and_fixed_in_overlay`。

该状态是“缺陷已复现，单文件修复 overlay 已验证”，**不是旧 binary 已修复，也不是整个共享源码通过联合发布**。候选 Linux binary SHA 仍为 `6ea3efc086d510399fe149def6b3e56941462f5531f8875967a7ed0c43e28918`。

## 精确影响

触发条件同时包括：

1. `Proxy.Client.Transport` 是注入的自定义 `RoundTripper`，不是 `*http.Transport`。
2. 使用直连 route，没有绑定代理。
3. 调用 `gatewayRequestClientWithHeaderTimeout(..., gatewayClientCompact, explicitTimeout)`。

旧 `gatewayClientForConfigured` 在此分支提前返回 `newGatewayProxyClient`，没有将显式期限赋给新客户端 `Timeout`，保留了默认 20 分钟。owner 新增测试要求显式 30 秒期限且原始 custom client 的 1 秒期限保持不变；旧候选在这条真实断言失败，不是编译失败或缺少数据库前提。

影响不能外推为“所有线上 compact 忽略总超时”：原生 `*http.Transport` 的直连/代理总期限已实际通过；冻结候选 `gateway.go` 的正常 compact 转发还调用 `doCompactRequestWithTimeout`，该函数会再次设置总期限。已确认的是自定义直连 client 工厂的显式超时合同漏项。

## 实际结果

| 测试 | 冻结候选，仅新增测试 overlay | 冻结候选 + owner 单文件修复 overlay |
| --- | --- | --- |
| `TestGatewayTimeoutPoolDefaultsAndCustomRoundTripper` | FAIL：custom compact deadline was lost or its transport was replaced | PASS |
| `TestGatewayTimeoutPoolCompactTotalTimeoutLeavesHTTPBodyAlive/proxy=false` | PASS | PASS |
| `TestGatewayTimeoutPoolCompactTotalTimeoutLeavesHTTPBodyAlive/proxy=true` | PASS | PASS |

第二项测试实际让 compact body 超时，同时确认相同 header timeout 的普通 HTTP 流 body 不被截断；父测试也在两次运行中通过。两次运行均无 skip，未执行 race 或 PG/Redis。

版本绑定：

- 冻结 `gateway_client_pool.go`：`27b924c5a46e18945a6c1b6a8b4927d66c372e39f8f5eb39259d737343b18bc1`。
- owner 修复文件：`234a6bebee0550a68d053f1efa0da145805fb47fff210769fe5f46d54e42e6ba`。
- owner 最新测试文件：`8aaf92538a63e00a7c62beb6164ce9e767349fee3af4fcfe90d6cd8a63d20205`。
- `baseline.jsonl`：`b71cb7abf77843409cb9e6102972377740404372c0853c1bdb8c68bd36078fe6`。
- `repaired.jsonl`：`26263e8e88d7251c5ff1b9e89ae90f83bf8a34d50cbd9c9aadb08a6a118d9832`。

工具严格核验新旧实现 diff 只能是提前返回分支增加 `client.Timeout = *headerTimeout`，拒绝把更多共享树变更混入此次修复验证。owner 实现与测试在开始时复制为本轮不可变 `.txt`，整个候选源文件清单执行前后哈希一致。

## 复跑

```powershell
python integration/gateway_environment/timeout_regression.py
```

入口使用现有离线 vendor 的冻结副本 `.local/frozen-gateway-race/20260918T022116-6f87b7/source`；`GOPROXY=off`、`GOSUMDB=off`，只运行上述两项顶层测试，本地合成监听，不访问外部网络。

每次产生新证据目录，不覆盖原报告。返回 0 表示预期的“旧候选断言失败 + 修复 overlay 通过”完整成立，并不表示旧候选可发布。root 的总门禁应将旧 binary 从只有缺失验收升级为含已知缺陷，等待新候选冻结、构建及相应联合验收；不能只引用 repaired overlay 报告为旧 binary 解锁。
