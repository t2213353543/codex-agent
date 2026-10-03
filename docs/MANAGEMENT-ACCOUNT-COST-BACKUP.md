# 账号绑定成本配置备份

2026-09-18。本功能只迁移账号绑定的采购配置，不迁移全局渠道/分组/销售目录、有序规则顺序或默认倍率，不修改网关、不产生实际扣费。

## 明确导出

`POST /api/accounts/export/credentials` 可增加 `include_costs:true`。默认关闭；普通 metadata 导出不能使用这个选项。原 v1 bundle 新增可选 `management.costs`，没有成本配置的账号不产生条目：

```json
{
  "management": {
    "version": 1,
    "leases": [],
    "costs": [{
      "account_index": 1,
      "rules": [],
      "policy_multiplier": {
        "multiplier": "0.5",
        "source": "Administrator procurement agreement",
        "policy_id": "costpolicy_source",
        "policy_version": 3,
        "effective_from": 1789680000
      }
    }]
  }
}
```

rules 保存该账号 `target_type=account` 的所有旧单目标规则历史版本，包含模型、服务层、四桶单价、倍率、阶梯、启停、生效时刻和来源。policy_multiplier 仅保存当前已生效且启用的有序策略中该账号的**显式覆盖倍率**；没有覆盖、只有默认值或只有未来策略时不伪造配置。该快照不携带整份策略，也不改变源数据。

导入严格验证版本连续、同序列身份一致、生效时刻递增、每个模型/服务层只属于一个序列、价格/阶梯与来源合法、索引唯一且对应账号数组。最多2000个规则版本，body及导出仍遵循既有大小限制；源规则 ID 和版本只用于核对，不能作为目标实例的配置身份直接复用。

## 三阶段恢复

账号、租期和成本分别执行。账号 `import-bundle` 的 dry_run 不创建账号/成本，实际账号导入也**不自动应用成本**。成本响应为 `cost_restore`，包含每个条目的实际新 ID、account_version、rule_count、显式倍率存在性、策略观测版本、状态和纯配置 `configuration`。

configuration 可以在页面展开核对模型/历史/四桶/阶梯/倍率/来源，不包含 credentials。不是凭账号名推测目标。多个备份条目落入同一目标时跳过，失败账号不恢复成本，实际凭据/ID已变化时报告 account_changed。

实际导入产生30分钟、绑定管理员的持久 `cost_restore_*` 回执。GET `/api/accounts/import-costs/receipts/{receipt}` 仅读取事实。POST `/api/accounts/import-costs`：

```json
{
  "receipt_id": "cost_restore_example",
  "items": [{"account_index":1,"id":"new_account_id","account_version":1}],
  "dry_run": false,
  "confirm_restore": true,
  "confirm_policy_update": false
}
```

每批1–100个固定目标。必须与回执的索引、ID和版本完全一致，不能重绑。界面先独立成本预览、核对配置、选择条目，再明确恢复；可停止后续批次，已完成项不撤销。

## 两类配置结果

费率历史：目标相同模型/服务层已有任何本地成本序列时拒绝覆盖，返回 cost_rule_exists。否则完整复制该账号的版本历史，生成新的规则/版本 ID，保留价格、启停和原生效时刻，target_id 改为回执新 ID。一个账号的规则追加原子提交，不部分插入一半历史。

显式账号倍率：只有目标已存在启用的有序策略才可恢复；不自动创建或启用全局策略。必须另外 `confirm_policy_update=true`，新版本复制目标当前目录的其他字段，仅更新该账号倍率，生效于当前时刻或已有未来版本之后一秒。账号覆盖值自预览后变化会失败，不覆盖其他管理员对该账号的修改；目录中其他账号更新会保留。没有确认则标记 `skipped/policy_update_not_confirmed`，费率仍可独立完成。无启用策略为 `skipped/ordered_policy_required`，不是假保存。

两类配置分开提交，因此每项有 rules_status 和 multiplier_status，最终 applied/partial/failed/unknown。部分完成就是事实，不宣称整项回滚。真实提交先持久 claim；已经领取或终态的条目不会重放。写入或结果落库不确定显示 unknown，读取回执并人工核对，不自动重试。

PG 在目录 advisory lock 和目标账号 FOR SHARE 下核对版本，配置及审计同一事务。文件模式保持账号锁直到管理文档写完。恢复不会改变账号凭据、token expires_at、版本或启用状态。独立审查未发现反向持锁嵌套。回执最终状态用独立短 context 保存，超过90秒仍无确定结果时只读显示 unknown；TTL后返回冲突，后台清除 cost-restore 文档。

## 证据与边界

`TestManagedBundleCosts` 及 PG 对应测试覆盖所有版本导出、新ID映射、预览零写、并发领取、完整历史、回放幂等和管理员/TTL边界。`BundleCostPartialAndValidation` 覆盖无目标策略的部分恢复及审计故障回滚；`BundleCostTargetConflicts` 覆盖未确认策略更新、目标倍率变化和账号被修改。

浏览器独立验证导出选项、成本配置可读、账号/成本分阶段、确认/跳过、部分失败和未知禁止重放，并保留旧壳移动端布局。最终验证结果绑定 portability 候选报告。

本功能不自动适配原始 Sub2API 的账号 rate_multiplier 到目标有序策略，原始格式成本映射需明确来源与目标模式，不能靠新增一个账号字段伪装完成。Sub2API 外层租期的 opt-in 适配另见 [外部租期合同](MANAGEMENT-EXTERNAL-LEASE-IMPORT.md)。已有预览服务和历史候选未被此实现覆盖，生产未部署。
