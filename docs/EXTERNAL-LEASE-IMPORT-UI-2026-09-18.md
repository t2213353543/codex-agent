# 原始 Sub2API 租期导入界面

2026-09-18。独立候选 `portability-20260918`，不覆盖商业或 completion 冻结产物，不修改既有本机预览服务。

## 行为

`Accounts.vue` 普通“导入账号”流程新增默认关闭的 `include_external_leases` 选项。勾选只让后端识别完整 `platform=openai,type=oauth,credentials={...}` 账号外层的管理租期；账号提交本身不写租期。外层 `expires_at` 与凭据内部令牌到期时间保持独立，`auto_pause_on_expired=false` 不被默认值覆盖；缺省自动停用来源明确标注为 Sub2API 默认。

文件、JSONL、粘贴、多文件和逐行处理保持既有流程。解析器为每项保留原始 `rawSource`，预览和实际提交都发送此完整对象；只在实际单行提交时加入服务端预览确认的名称。嵌套 credentials/tokens 包装、原平台/type、外层租期不经前端扁平化丢弃。

本地格式错误不提交；服务端有效行按本批 `results.index` 与 `external_leases.account_index` 映射回文件、原行号和全局 sourceIndex。租期预览验证来源、时间格式、布尔值和回执一致性；不匹配时阻止账号提交。无效租期只使该账号行失败，其他行仍可选择导入。

已有相同凭据账号默认跳过。仅在用户已选择外部租期且该项有有效租期预览时，提供“保留账号，仅准备租期确认”：发送原账号行且 `replace=false`，保留现有账号和凭据，取得稳定原账号 ID 的租期回执。

## 独立确认

账号仍逐项提交，每条实际响应的租期回执独立保存、独立展示文件/行号上下文。单行请求的 `account_index=1` 不改成 UI 全局行号，也不把多个 receipt 合成一个恢复请求。回执固定 ID、账号版本、租期版本与该账号导入结果核对后，复用 `AccountLeaseRestorePanel`：

- 先预览租期恢复，再单独确认。已过期且自动停用需额外确认；已有租期覆盖沿用原面板独立确认。
- 恢复期间锁定原始文件/选项、其他租期面板与关闭动作，避免改变提交目标。
- 版本冲突显式重新核对；结果未知不自动重发。一个租期失败不撤销账号导入，也不影响另一独立回执。
- 账号导入成功但回执缺失/不一致，账号结果仍正确展示，并单独标出未确认租期；不提供不完整目标的恢复按钮。
- 关闭窗口清除敏感原始内容和临时回执引用。脱敏导入报告不包含原始包装、凭据或服务器回显的秘密。

## 验证

`node --test frontend/tests/import.test.js frontend/tests/external-import-leases.test.js frontend/tests/account-secrets.test.js`：27 项通过，含新增 6 项外层租期/独立回执/重复账号选项验证。

`node frontend/tests/management-external-import-browser.mjs` 默认使用 `.local/management-parity/portability-20260918/dist`。旧运行时新 5 组实际浏览器行为通过，页面异常 0；网络使用隔离本地 mock，不能替代后端真实 file/PG 租期恢复证明。16 张预览/独立回执四主题 1440/390 截图及 320 过期确认截图已目视，无溢出。报告只记录请求计数，不保存原始账号或凭据请求体。

实际后端外部租期识别与应用由管理后端 owner 的 file/PG API 测试独立证明。最终构建与全业务回归的精确哈希以 portability 目录机器清单及 root 最终归档为准。
