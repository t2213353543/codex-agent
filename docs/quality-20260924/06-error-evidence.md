# 429 / 403 证据与推送桥交付状态复核

日期：2026-09-24。本文新增工作均为本地代码、离线历史证据与隔离测试；实际生产快照和桥热更新由主任务独立执行，本文不冒称已经部署或恢复账号质量。

## 已确认并修复的桥接回归

上轮 `list_files()` 加入凭据失效识别时，把 `account.status == error`、通用 `credential_status` 失败以及可选快照读取失败一并纳入 `ready=false`。该边界过宽：管理端 `accountManagementStatus()` 也会由最后一次普通推理失败产生 error，429、503、容量/网络错误不能证明已交付 AT 失效。

实际源代码链路已核对：

1. `../loophub-mother-checkout-20260920/webui/team_rotation.py::_destination_clone_accounts_healthy()` 接收 CPA `disabled`、`unavailable` 或非 active 状态即返回 false。它只对特定过载文案保留映射；桥输出的固定 `destination_unavailable` 不符合这些标记。
2. `_sync_child_to_sub2api()` 健康检查失败后进入重新同步路径；普通路径可能进入既有 workspace OAuth 处理，`reuse_saved_only` 分支则仅复用保存凭据。不能断言每次都会刷新，但确实会从只读健康查询进入推送流程。
3. 桥 `upload()` 即使相同 AT，仍写映射并 PUT 元数据。管理 `applyManagedAccountPatch()` 对 enabled 账号清 `LastError`；元数据写增加 version。它不会因此清除真实上游限流。上传后的源健康检查仍可能失败并再次补推。

**这证明了代码路径上的回归和无效补推条件，不代表已观察到现网发生风暴，更不能把它当作模型错答的已证实原因。** 上轮“只要 needs_reauth=false 就不会制造凭据处理”的判断不充分，因为源还有交付健康路径。

最小修复在 `deploy/loophub_codex_bridge.py`：

- `ready` 依据 enabled、初始化完成、当前目的端有效期、分组和确证凭据失败。
- 通用推理错误、refresh_failed 文案、403/429/502/503、容量与网络错误不单独让已交付凭据 unavailable。
- 可选 `/upstream` 快照读取失败保留交付状态，同时返回固定 `credential_observation_unavailable` 提示；此提示不会被源健康谓词判为失败。
- 确证当前 401/撤销/过期仍 `needs_reauth=true`；显式禁用、移出组、未初始化仍不可用。凭据版本改变后的二次读取和 ownership 冲突拒绝行为保留。

修复后 SHA256：`1cecef42c03a1379d9d03e4c62374055c07ffaf480d2bc458e8d3443c566fae1`。部署以主任务新生成的运行态哈希为准。

`tools/test_loophub_bridge_quality.py` 15 项通过。新增 `tools/test_loophub_bridge_source_contract.py` 使用 AST 提取真实源的健康方法和同步 hash 方法，未 import 源应用、未启动调度、未连接数据库。6 项通过：临时 403/429/502/503、旧回归 shape、快照不可读、真实 401、显式禁用、读取间凭据替换。旧 shape 对真实源谓词确为 false；修复后临时失败为 true；401 和禁用仍为 false。可用 `LOOPHUB_SOURCE` 指向运行容器只读导出的 `team_rotation.py` 重验。现有 Go/Python 真管理 API loopback 契约 `TestLoophubBridgeQ01RealManagementContract` 的 5 个子场景也通过（0.664 秒）。

## 真实错误记录能够证明什么

离线输出：`.local/quality-20260924/failure-evidence-q06.json`。输入为 `paired-q2.jsonl`、`astra-q1.jsonl`、`source-route-q1.jsonl`、`official3-q1/events.safe.jsonl`。

- Go 记录 12 条 HTTP 429，没有保留错误 JSON、Retry-After、额度 reset。
- 官方 q1 记录 5 条带准确 CLI 状态模板的 403 错误事件，随后 2 条 429 事件。其中终端 `error` 与 `turn.failed` 可能描述同一个最终失败，**不能算作两个独立 HTTP 请求**。
- 启动配置告警两条是 unclassified_error，thread/turn started 两条是 unknown；它们没有被当成上游错误。
- 上述 14 条限流证据都没有能支撑等待截止的字段。因此不能从这些历史资料宣布“再等 N 分钟”“已冷却恢复”或“账号必然永久不可用”。

CLI 0.155.0 对照依据：本地 `.local/codex-source-0.155.0/codex-rs/codex-api/src/rate_limits.rs`、`api_bridge.rs`。

| 官方边界 | 本工具处理 |
|---|---|
| HTTP 429 + error.type=usage_limit_reached | 限流，读取 error.resets_at 的 Unix 秒 |
| HTTP 429 + insufficient_quota / credit / spend-limit 错误 | quota_exhausted，不宣称普通等待可恢复 |
| x-codex-active-limit | 从相应 metered limit 家族提取信息 |
| x-codex-primary/secondary-used-percent、window-minutes、reset-at | reset-at 是 epoch 秒；不是相对延迟；超限窗口才支持额度等待提示 |
| codex.rate_limits event | 保留窗口观测，不自动把一条额度更新当模型请求错误 |
| HTTP 403 | forbidden，与 401 认证失败、429 限流分开 |
| Retry-After | 秒数从实际观测时间起算，或解析 HTTP 日期；重复冲突值视为未知 |

官方公共 API 的 Retry-After 表示临时限流或过载时的最短重试等待，但不会证明额度、计费或权限错误只需等待即可恢复。公共 API 说明不能替代 ChatGPT Codex 自定义头的源码契约。[OpenAI rate limits](https://developers.openai.com/api/docs/guides/rate-limits)、[OpenAI error codes](https://developers.openai.com/api/docs/guides/error-codes)。

## 新增离线工具

`tools/quality_failure_summary.py` 零网络调用，读取 JSON/JSONL，允许下列证据形状：

- `http_status`、`response_headers` 和结构化 `error/http_error/response_error/upstream_error`。
- `raw_http` / `rawHTTP` 的 `{status, headers, body}`，或以 HTTP 状态行开头的完整响应文本。
- `safe_errors` / `safeerrors` 结构化错误数组，以及 `codex.rate_limits` 事件。
- 旧 CLI safe events 仅在真正 error 事件内接受固定 `unexpected status NNN` / `last status: NNN` 模板，不通过任意正文中出现 429 判断错误。

输出仅含允许的数值/枚举/时间/匿名标签；不拷贝原始 body、message、cookie、Authorization、URL、邮箱。过期的 reset 产生 `recorded_deadline_elapsed_recheck_required`，不是 healthy。409/403 与嵌套 429 冲突保留为 conflicting evidence，不挑选有利解释。

32 项边界测试已通过，包括 reasoning_output_tokens=429、正文含错误文案、状态/错误类型冲突、HTTP 200 内 SSE 错误、delta/绝对 Retry-After、缺观测时间、已过去 reset、active-limit、无穷/NaN、重复冲突 header、403/401、真实 CLI 固定模板、raw HTTP（含无 header 响应）、敏感字段不泄漏、源身份变化、过期缓存、禁用和容量并非限流等。旧额度缓存即使 reset 仍在未来，也不能证明现在还在限流：期间可能发生手动额度操作。

复验：

```powershell
python -m unittest discover -s tools -p 'test_quality_failure_summary.py'
python -m unittest discover -s tools -p 'test_loophub_bridge*quality.py'
python tools/test_loophub_bridge_source_contract.py
python tools/quality_failure_summary.py .local/quality-20260924/paired-q2.jsonl .local/quality-20260924/astra-q1.jsonl .local/quality-20260924/source-route-q1.jsonl .local/quality-20260924/official3-q1/events.safe.jsonl --output .local/quality-20260924/failure-evidence-q06.json
```

## 28765 / 网关当前状态的只读采集设计

由主任务在当前运行宿主执行，先核对容器 Running、镜像 ID/revision、源文件 hash 和 `/proc/<gateway-pid>/exe`。既有 source key 和网关 key 只在宿主内存读取。不可直接输出 `docker inspect` 环境、owner proxy、完整源 status 或 registered 行。

1. `GET /api/team-rotation/status` 只读当前配置、owners、children。只按既有 control 里的 owner_id/workspace_id/assigned_at 和匿名 child identity 取选定行；身份变更返回 unknown，不能沿用 q1/q2 标签直接比。
2. `children[].last_checked_at` 会在失败与生命周期操作更新，并非专属 quota 观测时间。仅凭它与 primary_used_percent 不能证明额度数据刚更新。
3. 若需要确定新鲜度，在运行容器数据库连接中执行 **只读事务**，显式参数绑定查 `team_rotation_child_history` 的 `quota_observed_at`、`primary_used_percent`、`secondary_used_percent`、`primary_reset_at`、`secondary_reset_at`，并和 child 当前 owner/workspace/assigned_at 复核。SQL 不读 tokens，不 import `webui.app`，不运行 init_db、repair、reconcile、quota probe。
4. 网关仅 `GET /api/accounts/{id}`、`GET /api/accounts/{id}/runtime?model=<exact-model>`、`GET /api/accounts/{id}/upstream`。后两者只读已有缓存，不调用上游。runtime 的 checked_at 与 cooldown.until 是毫秒；upstream.checked_at 是秒，credential_version/stale 按凭据代次核对。
5. 不调用 `/usage` 检测、不请求 fresh/reset credit、不主动 OAuth、不消费 reset credit。旧 snapshot 不因管理 API 刚读取就变成新观测。

建议的只读 SQL（参数从已锁定的所选身份取得，不输出参数原文）：

```sql
BEGIN READ ONLY;
SELECT c.status, c.assigned_at, c.oauth_reauth_status,
       h.quota_observed_at, h.primary_used_percent, h.secondary_used_percent,
       h.primary_reset_at, h.secondary_reset_at, h.quota_state
FROM team_rotation_children c
JOIN team_rotation_owners o ON o.id = c.owner_id
LEFT JOIN team_rotation_child_history h
  ON h.workspace_id = o.workspace_id AND h.child_email = c.email
WHERE c.owner_id = %s AND o.workspace_id = %s
  AND c.email = %s AND c.assigned_at = %s;
ROLLBACK;
```

将脱敏结果投影为 `{identity_matches, source_child, source_history, credential_expires_at, gateway_runtime?}` 后调用 `summarize_source_state()` 或 CLI `--source-state`：

- `testable`（可测）：当前 active、OAuth ready、身份一致、AT 有效、新鲜缓存额度未用完，若附 runtime 则未见冷却/容量阻断。**只是可以进行受控探针，不是上游必接受或答案正确的保证。**
- `rate_limited`（已限流）：新鲜缓存额度达到 100% 且未来 reset，或当前凭据/模型 runtime 有活跃 rate-limit cooldown；给出有证据的 not-before。不能擅自清 cooldown。
- `unknown`：身份变更、缓存过期/缺专属 quota 时间、reset 已过但无新观测、AT 不可用、禁用、容量或共享状态不可读。不会把这些记为糖果错答。

原始 q1/q2 观测缺 reset，无法通过离线重分类恢复丢弃的响应。必须靠当前只读状态、或下一次已授权的受控请求中完整保留结构化错误，才能缩小该证据缺口。
