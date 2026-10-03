# 原始 Sub2API 账号采购倍率迁移

2026-09-18，observations 管理增量。默认关闭，只把明确来源的账号外层 rate_multiplier 识别为待确认的本地采购策略账号覆盖，不映射 Weight、凭据字段、实际扣费或网关调度。

## 参考与空值

本机 Sub2API `backend/internal/handler/admin/account_data.go` 的 DataAccount 使用可选 `*float64 rate_multiplier`；ExportData 写出，ImportData 传给 CreateAccountInput。`backend/internal/service/account.go` 的 `BillingRateMultiplier` 对 nil 使用运行时1倍，但0是有效零倍率。因此外部字段缺失/null属于**未配置**，本站不把这个默认行为伪装成文件显式1倍。只有完整 `platform:"openai",type:"oauth",credentials:{...}` 账号封套的外层数字才参与适配，不读取 credentials 内部同名字段。

## 请求与数值

`POST /api/accounts` 和 `POST /api/accounts/import/preview` 的 raw、name+credentials、accounts 三种原请求形式可增加 `include_external_costs:true`。默认false保持旧导入与 warnings；选项只影响本次导入，不写系统设置。

rate_multiplier 必须为 JSON number，使用 Decoder.UseNumber 保留词法值，再通过 big.Rat 精确归一化，不经过 float64。支持普通小数和有界科学计数法，例如 `25e-2` 得到字符串 `"0.25"`，`0` 保持 `"0"`。目标范围沿已有管理采购倍率：0–9999999.99999999，最多8位**有效小数精度**，尾随0可以消除；负数、负零、字符串数字、布尔、NaN、超范围或需舍入的精度返回逐行 `invalid_external_cost`，该行账号不导入。数字词法最长64字符、指数最多3位，先限制再使用任意精度计算。

前端必须保留原始数值词法，不得先用普通 JavaScript Number 解析后重序列化倍率，否则可能把超精度原值舍入为合法值。使用原JSON来源或保真数值解析；后端响应倍率统一为十进制字符串供展示。

## 来源与回执

显式选项开启后返回：

```json
{
  "external_costs": [{
    "account_index": 1,
    "multiplier": "0.25",
    "source": "sub2api_outer_account_rate"
  }],
  "cost_restore_source": "sub2api_outer_account_rate",
  "cost_restore": {
    "dry_run": true,
    "items": []
  }
}
```

items 为空仅示意 envelope，实际每个识别项目有同本次请求 account_index 的成本恢复回执项。其 configuration 为：

```json
{
  "account_index": 1,
  "rules": [],
  "policy_multiplier": {
    "multiplier": "0.25",
    "source": "sub2api_outer_account_rate",
    "origin": "sub2api_outer_account_rate"
  }
}
```

外部来源不是原有采购策略，故不返回虚构的 policy_id、policy_version 或 effective_from。现有自家bundle策略来源仍要求这些真实字段；新 origin 分支只接受固定来源，禁止混入非零策略版本/时间或伪策略ID。恢复回执顶层 item.policy_version 表示**本地目标策略**，不等于源字段历史。

预览只读取与展示，pending_import无可执行回执。实际账号阶段完成后，复用成本回执的固定新ID、账号版本、操作者和30分钟TTL；账号导入不自动写采购倍率。已存在同Token账号可以显式选择保留账号、仅准备管理配置确认；同名不同Token未明确替换则失败，多来源条目指向一个账号则不能猜归属。

实际提交仍用 `POST /api/accounts/import-costs`，要求 confirm_restore 和独立 confirm_policy_update，先 dry_run 可见目标影响。只有目标已有启用的有序采购策略时才允许追加该账号覆盖；无此策略显示 skipped/ordered_policy_required，不自动建立或启用全局策略。写入追加未来有效版本，保留目标其他规则/默认倍率，账号/目标倍率变动拒绝覆盖，重复回执不再写一次。

没有确认策略更新时，倍率明确跳过；没有规则且仅倍率的外部条目不能声称费率已恢复。结果未知不自动重试，GET回执核对路径沿已有成本恢复合同。

## 与租期同时识别

include_external_leases 与 include_external_costs 相互独立，可以同时开启。每个实际请求返回各自回执，不混合源索引和目标。建议先恢复成本再恢复租期，因为已过期租期可能停用账号并增加账号版本，原成本回执会按CAS正确拒绝旧版本。

只有实际识别的rate字段会减少其未映射警告；未选择租期时保留外层expiry/auto_pause警告，额外 extra、模型映射、分组/代理等警告不被删除。有效项目新增 external_cost_pending_confirmation；null/absent不生成项目或覆盖，不误称配置已迁移。

## 文件与验证

改动限定新增 `admin_account_external_costs.go`/测试、admin.go 导入开关/回执接入，以及 `admin_account_bundle_costs.go` 来源结构与校验的最小扩展，未改变其事务/claim执行路径。无新增数据库迁移。

本地9个顶层/61个含子测试通过（新精度/来源/API/无策略4组，既有外租2组与成本bundle3组）；真实一次性 PostgreSQL 16.15 验证5个顶层/10个含子测试通过。包含三种输入形式、数值边界、null/absent、0、双选项警告、Token/Weight不变、显式确认/重复提交、无策略跳过、模糊重复目标防护及旧bundle兼容。

证据 `.local/management-parity/observations-20260918/{local,local-docker-pg}-external-cost-{summary.json,tests.jsonl}`。PG使用现有本机镜像、随机回环端口和内存密码，容器已删除；没有真实账号、生产访问或上游请求。前端保真词法与交互由Accounts负责人独立验收，旧预览与冻结包没有修改。
