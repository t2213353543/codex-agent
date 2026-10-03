# Frontend Release Compatibility

本审计只读本机已捕获产物，未访问服务器、读取凭据、构建或改动产品。生产证据时间是 `2026-09-18T03:49:22.825002Z`，不代表此刻线上状态。

生产来源是 `.local/runtime-frontend/d87032e071e7c72343ccb178857d4e56a0200ea7af87aa3b262083e73d15d1c6/capture-20260918T034922Z.json`，其实际 web 路径为 `/opt/codex-agent/releases/4d4c4dfd91de306b6d1f79bfba232df49c85d3a1dfdda06bbf199538a7322e38/frontend/dist`，捕获时 HTTP 与磁盘 SHA 一致。捕获二进制 SHA 为 `a9bf68d1899ec1d58381c788b84cc63123c88cb8196d1acb0b4faafa1b65831d`。

较晚的本地部署记录位于 `.local/upgrades/35624e0cafd9f60cae88f61d772464de2f0b891638c6a950f5f0a17668ebee70/upgrade-manifest.json`。其 `frontend` 全部 47 项已与上述捕获清单逐路径、逐 SHA 完全相等。`status.json` 记录部署于 `2026-09-18T04:00:54.850574Z`（北京时间 12:00:54），重启后的 `postdeploy-after-restart.json` 也指向此 release。最近这次已记录部署的二进制是 `a66c7be3de344221d6433a6bc7140523d95a6a068fd2312757c80f2dc50c073d`，而不是早期捕获的 `a9bf...`；记录的 web 路径相应迁到该 `35624...` release 下。

该部署的验收记录 `evidence/upgrade-acceptance.json` 将源码 owner 的 compact Go 快照标为 `956fc90be91a922ac1593b283102bc5149674dac337564e6023479ec8e60a6f9`；发布清单自身为 `source_sha256=a1d958c3a3deab35278baa65c4c30553b117c4d9ecdd76263b80994637655abe`。已逐项核对两者的文件映射完全相同，差异来自摘要计算时的JSON序列化格式，不是源码范围不同。机器报告分别保存两个摘要，不混为同一字串。上述补充只建立已有记录的时间线，不声称当前服务器仍运行该二进制。

候选为 `.local/management-parity/error-severity-20260918/dist`，manifest SHA 为 `c3368f22919950b4ae2ca6de744714ccebeaaa21cb16bf4abc3eb9523fd37648`。它比生产 47 文件多 7 文件：46 个原资源同路径同 SHA，只有 `index.html` 同名变化，未删除原文件。

| 路由/模块 | 生产实际使用 | 候选实际使用 |
| --- | --- | --- |
| Accounts | `Accounts-CO5sQjFD.js` | `accounts-managed-BXbY_rsM.js` |
| Logs | `logs-improved-ByrZMFc4.js` | `logs-managed-COfEPxyG.js` |
| Settings | `Settings-CU7oOkU4.js` | `settings-restored-DJLGQsl3.js` |
| 共享运行时 | `index-D--MHP3C.js` | `index-management-19b7cda49f82741e.js` |

候选所谓“48 个 baseline 资源不变”是相对本地 49 文件的 Settings overlay，等于生产的 46 个非入口资源加新增 Settings JS/CSS。不能把它表述成与生产 47 文件完全一致。Settings 恢复页的 JS/CSS 原样继承 `.local/settings-restore/build-manifest.json`；该页在上述生产捕获中尚未启用。

共享运行时经过完整字节比较，严格等于生产 `index-D--MHP3C.js` 加六个已有绑定的导出，没有改写 request 实现。新增导出包括统一 request、clearSession、authGeneration 与三个下拉组件。Import map 把旧页面和新页面都重定向到同一个模块实例，避免两套登录状态或 Vue。原旧模块留在包中，不等于路由继续使用它们。

不能仅以静态资源兼容判断可以单独替换前端：

- Logs 首次加载就要求 `POST /api/logs/snapshot`，随后同 `as_of` 查询列表与统计；错误级别、预热、全量导出、错误处理和清理还依赖当前完整管理契约。
- Accounts 首次加载要求 `/api/accounts/manage` 返回精确分页与稳定身份，新增 ID/status 排序、额度批量、版本化动作、OAuth、定时检查、租期和备份回执均要求配套管理后端。
- Settings 恢复页要求有效的 `settings_version` CAS，传输/策略开关、提示词与超时/TTL、指纹配置字段必须由后端真实支持。已有生产 UI 不足以证明新字段可保存。
- Cookie/request/401 会话基础逻辑保持原实现，但这不使新接口在旧后端自动可用。候选已验证的后端源码为 `ff4ce018b14afdc1f1549cb6e1881f36c029fdd58a80da8f9a9238dbf877f10e`；最近记录的部署二进制 `a66...` / owner 源码 `956...` 不在该候选验收范围，需按独立发布合同核对。

应把完整 54 文件候选和已验后端作为配套发布对象。若要保留旧后端，必须另有这些接口及字段的兼容证据，本次离线检查不作该承诺。全路径、旧/新 SHA、import map、来源和接口族见机器报告 `.local/management-release-audit/frontend-compatibility.json`。旧候选和冻结证据均未改动。
