# 工作台 AT-only 桥接审查

审查日期：2026-09-18（Asia/Shanghai）。范围：本项目 `deploy/workbench_bridge.py`、本项目导入/重新授权接口，以及完全合成的本地验证。没有读取真实凭据、请求真实模型、执行支付测试或修改 18876 服务。本审查不能替代部署后验证。

## 已确认并修复

1. **退款禁用未检查账号所有权。** 原逻辑在源记录不再符合资格时直接禁用映射账号；操作员移除桥接标签或接管 RT 后，桥仍会修改该账号。现在禁用前与令牌更新一样检查 `workbench-18876` 标签及 `has_refresh_token=false`，否则返回 `destination_ownership_changed`。位置：`deploy/workbench_bridge.py:157`。
2. **显示名替换存在并发覆盖窗口。** `accounts_pg.go:762` 根据显示名查询待替换账号。桥 GET 映射账号后，若账号改名且旧名被另一账号使用，`replace=true` 可能先覆盖另一账号，再被桥的事后检查发现。现在所有初次导入固定 `replace=false`；已映射账号更换 AT 只调用稳定 ID 的 `POST /api/accounts/{id}/reauthorize`，携带读取到的 `version` 和 `allow_identity_change=false`。位置：`deploy/workbench_bridge.py:188`、`:208`。该 API 在 `admin.go:1400` 注册，`admin_accounts_management.go:502` 校验稳定 ID、版本及上游账号身份。
3. **旧运行版本不支持 CAS 时明确失败。** 重新授权返回 404/405 时报告 `credential_update_requires_cas_api`；409 保持失败，不回退到名称替换，也不前移本地 token 哈希。dry-run 对这种更新仅报告 `would_reauthorize`，表示计划操作，没有执行或验证线上 CAS。
4. **初始化可复用指定代理。** 当 state 中存在 `proxy_id`，初次初始化在同一版本校验 PUT 中设置 `proxies:[proxy_id]`；不含该配置时不写代理字段，后续同步不覆盖人工管理配置。位置：`deploy/workbench_bridge.py:222`。
5. **初次提交路由经真实执行纠正。** root 的首次 apply 得到 405，确认提交接口是 `POST /api/accounts`（`admin.go:1410`），不是 `/api/accounts/import`；root 已修正桥接脚本。初轮 fake 同样写错提交路径，因此原先 12 项通过没有发现此缺陷。现已修正 fake，且每次目标 API 调用都与从 `admin.go` 提取的真实 `HandleFunc` 注册表匹配；额外断言明确拒绝旧的错误路径。preview 仍为 `POST /api/accounts/import/preview`。

## 已核实的接口语义

- `accounts.go:208` 的 `mergeImport` 保留管理字段：名称之外的 notes/tags、enabled、groups、concurrency、priority、weight、proxies、quota 与时间窗等。重新授权另在 `admin_accounts_management.go:514` 保留名称。因此 AT 轮换不会被导入行的 `enabled=false` 意外禁用，也不会抹掉人工设置的并发和代理。
- 初次导入的重复 token 返回 `duplicate`，不会写已有账号凭据；桥仍要求目标名称、标签以及无 RT 符合桥接约定，外部账号不进入映射。RT 仅存在于内存中的源导出结果，发送到 Agent 的 credentials 没有 RT。
- 源字段信息由 root 对首轮实际镜像 `ejh-refund-cancel-e2df43c1` 的只读调查提供：查询 `recharge_status=paid` 返回行值 `success`；授权过滤等价于 `has_refresh_token`，没有 `codex_status` 行字段；列表不含 `has_active_refund`，详情包含此字段。因此 bridge 接受 paid/success，并在每个候选导出前重新请求详情。root 另确认源导出内部校验 paid AT 和退款资格。本子任务未再次访问源服务。root 后续观察到源运行镜像已变化；每次实际执行使用报告中的完整 `source_image` 绑定版本，不把首轮镜像当成永久事实。

## 本地验收

统一重跑命令：

```powershell
python integration/gateway_environment/bridge_smoke.py
```

最终 13 项测试全部通过，0.088 秒。测试替换 source/destination API、私密 key 路径，并阻止真实 HTTP 与 docker 调用；目标请求的 method/path 额外与真实管理端源码注册表核对。

| 边界 | 断言 |
| --- | --- |
| AT-only 与脱敏 | 不转发 RT；state/report 不含 AT、RT 或管理 key |
| dry-run | 不写账号、不落 state；AT 更新明确标记未验证 CAS |
| 幂等 | 同一合成 AT 第二次 apply 无账号写操作 |
| 退款 | 桥接账号只禁用一次；标签移除或拥有 RT 的账号不修改 |
| 源故障 | HTTP 503 保留目标账号与已有 state |
| 重复凭据 | 不接管外部账号，也不改变外部账号启用状态 |
| 详情复查 | 列表缺少退款字段时，详情的活动退款会阻止导入 |
| CAS 更新 | 使用稳定 ID、版本、禁止身份变化，保留停用状态和人工并发 |
| CAS 缺失/冲突 | 404、405、409 不回退导入、不更改映射哈希 |
| 可选代理 | 只在初始化写入指定代理，重跑不重复更新 |
| 真实路由契约 | 所有 fake 目标调用匹配 `admin.go`；错误的 `POST /api/accounts/import` 被拒绝 |

验收脚本是语义 fake；源码核对支持其接口假设，但它不等于真实数据库事务测试、运行中 API 验证或真实源幂等验证。

本次核对哈希：

```text
deploy/workbench_bridge.py
d9ea77d9702186c335b4dab1011c4ae23ea8b4e09d2ef3f3f07f7fb1247b76f9
integration/gateway_environment/bridge_smoke.py
56a6d6ceba0f2be115d578db2d22c248156739447e5a99c80d5b0b818e692c65
```

## 保留的边界

- 源接口返回 HTTP 错误或无法解析的 JSON 会停止操作；当前没有完整详情 schema 校验。如果将来详情以 HTTP 200 返回 `{}` 或缺少资格字段，会被当成不符合资格。建议后续把“已明确退款/失效”与“详情字段不完整”分开；后者应报告 `source_schema_mismatch`，不禁用账号。这不是本轮观察到的线上返回。
- 本地 state 尚未保存，但初次 import 已提交的极短崩溃窗口中，如果源 AT 同时轮换，下次会遇到固定名称冲突；当前选择明确失败并保留已有账号。不要为自动恢复打开 `replace=true`。可人工按稳定 ID 核实后重建映射。
- API 暂时故障不进行立即自动重试；可由有限次数定时执行重试。CAS 冲突应在下一次重新读取版本后处理。初次初始化已保存映射后发生中断，可以在下次恢复初始化。
- 实际上线的旧版本若没有 reauthorize API，只能完成新建及同 AT 幂等同步，不能宣称已支持安全 AT 轮换。需由 root 将运行版本、桥接版本和实测结果分别记录。
- `Pro20` 来源目前仍采用 `Pro5-workbench-` 名称前缀；资格判断同时允许 pro5/pro20，功能不受影响，但显示名不能作为套餐证据。真实资格以导出的 JWT plan 为准。

状态区分：本文件独立确认 **本地合成测试通过**。root 已回报修正提交路由后 5 条 `added` 实际入库，但本子任务没有独立访问生产复核；**已真实导入** 与 **已部署** 的正式结论仍由 root 的现场执行记录决定。


## 04:18 运行时复核与修正

此前“详情包含 has_active_refund”的假设不符合实际序列化。实际容器 /app/repositories/registrations.py（SHA256 b9b9f786fd635ed5578d0d8ed56292744a352b4088bcf02db958b80426796eb4）移除了内部布尔字段，公开 can_delete 与 delete_block_reason。适配器现接受明确布尔字段，或经验证的两种原因/可删除状态组合；缺失、未知原因、错类型、详情身份不匹配均报 source_schema_mismatch，保留现有账号。

已补 Pro20 显示名、目录 fsync。18 项合成回归通过；真实只读 preview 当前 4 个符合资格、1 个应停用、4 个待 CAS 更新，源状态随业务变化，不能再把初次导入5个当作当前可用数量。当前部署后端仍缺 CAS API，不宣称持续 AT 更新通过。

当前文件 SHA256：
- deploy/workbench_bridge.py: `811b2e21c1a46a11406b6ba8ce26359f6edac2ed7e59a8ecdc4b4896de11e724`
- integration/gateway_environment/bridge_smoke.py: `5743f5afe58c0274f87c86a60b172852f1f46ac7a950d602dbbcf10c874d498a`
