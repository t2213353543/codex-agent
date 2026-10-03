# 管理功能独立发布核验

本轮推进发布兼容性，不修改共享产品源码、生产服务、账号或既有预览。没有将用户此前“只允许已复现网关兼容错误”的范围扩展为任意网关改动；被动采集范围仍待确认。

## 固定输入

- 最后已保存的生产发布：`35624e0cafd9f60cae88f61d772464de2f0b891638c6a950f5f0a17668ebee70`；Go `956fc90be91a922ac1593b283102bc5149674dac337564e6023479ec8e60a6f9`；Linux binary `a66c7be3de344221d6433a6bc7140523d95a6a068fd2312757c80f2dc50c073d`。
- 生产源码目录 `.local/joint-candidate/20260918T024841.878837Z/local-source`，源归档和生产manifest文件映射逐项相符。这里读取的是已保存证据，不是本轮实时线上探测。
- 最新完整管理候选Go `ff4ce018b14afdc1f1549cb6e1881f36c029fdd58a80da8f9a9238dbf877f10e`、前端manifest `c3368f22919950b4ae2ca6de744714ccebeaaa21cb16bf4abc3eb9523fd37648`。完整管理验收通过不等于现网网关行为等价。

## 前端与发布工具

生产47个资源到候选54个资源：46个同路径字节不变，`index.html`改变，新增7个，没有删除。新增项含账号、日志、共享导出扩展，以及此前只在本地验收的Settings恢复模块。因此“保留48个框架资源”是相对本地恢复基线，不应写成相对生产有48个相同资源。

共享JS只在已捕获运行时末尾追加已有绑定导出，没有另起框架实例。但新页面依赖新管理后端查询、快照、数据和操作契约，不能直接只换前端。详情见 [前端兼容报告](MANAGEMENT-FRONTEND-RELEASE-COMPATIBILITY-2026-09-18.md)。

`deploy/upgrade_remote.py:validate_v2_evidence` 要求候选前端等于生产捕获前端；现v2是保留前端的后端升级契约，不能拿旧47文件验收给新54文件背书。v1还只接受000–002迁移，本轮没有篡改这些门禁或编造通过证据。

## 管理层独立组合实验

直接以生产956为基线替换当前管理文件，编译缺少 `ManagedCRSLinks`、13个可选日志字段及尝试摘要类型。利用Go AST精确检查后，只在独立artifact中加入这些数据声明：

- `accounts.go`只增加同账号文档内的管理来源链接字段。
- `stats.go`只增加13个omitempty可选字段，没有采集函数或状态写入改动。
- 新`admin_observation_dto.go`只有`GatewayAttemptSummary`数据类型。
- 生产网关、凭据存储方法、主入口等运行函数保持原字节，不加入OwnerRevision、native或其他新网关实现。

独立候选197文件，摘要 `30e262b91c772e3831d6ab473b8c028d7f69e5c417937ca6039985f6c1d312ab`，位于 `.local/management-release-audit/source`。Windows与Linux编译通过，说明管理代码并不必然依赖新native或新身份代次算法。

AST和字节验证确认108个生产非管理文件中106个完全不变，上述两个结构声明文件之外无修改；431个非管理函数声明原始字节相同，module和迁移文件相同。完整边界清单为 `.local/management-release-audit/release-audit.json`，逐函数证据为 `runtime-function-preservation.json`。

**完整测试未通过**：未排除测试的`go test -overlay ... -count=1 -json ./...`结果为358顶层通过、86环境跳过、1失败。失败是`TestManagedObservationRuntimeToQuery`，明确说明旧生产运行时没有新增实际发送/返回模型、标准路径和尝试等采集，按这些字段查询返回0条。字段声明不是采集实现，不能改断言、补假字段或称该组合全量可用。PG环境跳过未计为通过，未启动这个实验服务。

## 真实降级写入验证

使用冻结956和ff4各自真实`Store.modify`、管理计划decode/update/encode及`accountSecret` JSON编解码，针对合成数据完成了以下对照：

| 字段或事实 | 956旧写入 | ff4新写入 |
| --- | --- | --- |
| 稳定账号ID、普通元数据修改 | 保留并正常修改 | 保留并正常修改 |
| ManagedLeases | 保留 | 保留 |
| JSON OwnerRevisions | 丢失 | 保留 |
| JSON ManagedCRSLinks | 丢失 | 保留 |
| 计划与普通计划修改 | 保留并正常修改 | 保留并正常修改 |
| 计划Requests及删除请求标识 | 丢失 | 保留 |
| accountSecret owner_revision | 丢失 | 保留 |
| accountSecret常规Token/身份字段 | 保留 | 保留 |

PG凭据这一行是**真实加密前JSON类型编解码验证，不是PG运行测试**。生产实际使用PG，因此JSON文件风险与PG凭据/管理文档风险分开；不能把已有ManagedLeases误报成全部会丢。结论是旧writer混写或原样回退会破坏新增保证，需要准确的writer与回退处理，不能仅凭数据库schema没变就放行。

证据为 `.local/management-release-audit/writer-roundtrip/2026-09-18T07-53-51-623Z/report.json`；无真实凭据、无数据库或上游连接。旧与新各项预期行为测试通过，确认的是不兼容事实，不是回滚已获准。

## 可复验门禁

新增只读工具验证源归档、二进制及前端哈希，比较生产与候选范围并明确拒绝把未知发布条件标成通过：

```powershell
python tools/audit_management_release.py
```

结果 `.local/management-release-audit/release-boundary-final.json` 为 `publish_eligible=false`。整个新管理快照相对生产有27个非管理运行文件变化（测试探针不算运行文件）；逐项清单与源/前端差异保存于该报告。重复同输入不会覆盖固定结果，输入变化要求独立输出。

新门禁5项单元与既有升级打包/远端模拟测试合计31项通过；没有真实SSH或部署。完整组合实验失败与writer不兼容都保留原始证据。

## 下一边界

目前已有证据足以排除两种错误操作：直接把完整ff4二进制称作“不改网关的管理发布”，以及把独立组合的“能编译”称作“全部已验收”。下一步需确定被动采集改动范围，完成准确组合的联合验收，并使用支持新前端且具备writer/回退保证的发布合同。目标仍未完成，本轮没有上线，也没有降级已验收功能来宣布成功。
