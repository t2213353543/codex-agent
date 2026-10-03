# B 组 0.4.14 发布复核

2026-09-29 00:19（Asia/Shanghai）：0.4.14已由主会话部署并完成现场验收，B独立复核通过。

- release：`3b80aef4b4cefa029acc6d3c3ae02b599f8dd82589aef04493faaf45bc8c5c2a`
- 运行PID：1562231
- binary：`68c5a18cbe7eecb555e45a946ce454200c403f49d1968ce1a0a93fac7c64722f`
- 最终source：`1f688fef14baa5ef40eb0dd59aee1337102c3ab4c666c40f98cc1fc3fa5f579b`

## 正式门禁与包

Go/vet通过，4563含子PASS/458环境SKIP/0FAIL。新增ToolPolicy与terminal共21顶层全部实际PASS；新PG在普通Go的SKIP由独立真实PG验收补齐。

PG160442.016102共14顶层/22含子PASS、0skip/fail，包含ToolPolicy与OAuth roundtrip。race160442.069959的新21顶层/85含子项、协作20、Redis4均PASS，无竞态或源码漂移。三门禁697执行源码逐项匹配最终freeze。正式live由C独立核对单run完整8top+18必需子例PASS，本次无需组合补验。

封包调用包内原始validate，90制品/30证据/压缩包91文件的名称和SHA完全一致；696 backend scope加既有embedded JSON构成697执行源。Windows本地核验不替代Linux权限检查；主会话真实preflight/activate已另通过。

## 上线验收

16:17:55 UTC的postdeploy、activate和manifest同PID/binary/version。SSE gpt-5.5→gpt-5.6-sol、JSON codex-auto→gpt-6-astra各一次生成，均completed，max→xhigh、schema2、逻辑/真实HTTP200及精确用量合同通过；实际2request/0refresh。

无工具成功记录的ToolPolicy恰5个Prepare字段，计数为整数0、布尔false；本地非法include_usage请求400、零派发，ToolPolicy字段确实未出现。三条标记记录CSVv4/77列导出通过。B的最小postdeploy判据加固已被主会话采用，严格区分0/false/省略，脚本SHA为c7376827…，未增加生成预算。

settings27、api_key cache scope、cooldown=false、全部账号配置前后保持，已有六项迁移无增项。B只读既有执行回执，未重新发送生成、负例或导出请求。

## 限制与后续

现场smoke未触发真实工具拒绝或OAuth refresh，不以此宣称自然错误全消除。真实工具拒绝/usage保留由已完成本地、race、PG及独立HTTP/WS边界支持。精确清单A/B的6对小样本支持本次取舍，不能据此保证长期零误调用。

主会话下一步继续stream options与structured mixed问题。B已将旧生产6条bound503定位到候选扫描尾分支：旧绑定无法匹配当前授权候选。禁用、删除重建、组过滤或owner/快照差异在现有聚合中无法区分，未宣称修复；最小后续证据建议见 `bound-owner-next-evidence/REPORT.md`。

## 回执与复跑

全部B证据在 `.local/cross-session-20260928/01a0e7ed/`：

- `v0414-release-audit/package-3b80aef4-audit.json`、`deployment-audit.json`。
- `tool-policy-gate-coverage/completed-pg-go/`、`completed-race/`。
- `terminal-metadata-boundary-review/owner-after/`及`terminal-usage-accounting-review/`。

重新核对已保存部署回执（零HTTP/SSH/SQL）：

```powershell
python -B .local/cross-session-20260928/01a0e7ed/v0414-release-audit/check_deployment.py
```
