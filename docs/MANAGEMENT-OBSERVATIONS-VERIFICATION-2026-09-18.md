# 链路观测与外部倍率验收

2026-09-18。本轮完成有来源的使用记录增强、原始倍率迁移和实际边界修复，属于持续目标的实质进展。**候选已验证但未上线，完整对齐目标仍未完成。** 用户恢复的旧蓝色壳、旧候选和本机预览均不被覆盖。

## 本轮实现

- 请求、实际发送、真实返回模型分开展示/筛选，兼容upstream模型来源保留；入站和上游标准路径各自查询，两个明确模型对的不一致筛选只有两端均已知才匹配true/false。
- 新四维分布与三费用域保持同查询/同快照/同只读事务；三个真实阶段单独返回样本数和平均值，0是样本、缺失是未知，重叠阶段不能累加成总耗时。
- 详情及可选列展示最多四项实际派发/OAuth/WS握手尝试，保留nullable状态和耗时、来源、分类、截断和schema标志。派发不代表上游接受，WS握手不复制到后续轮；历史记录不回填。
- CSV末尾增加12项观测字段，JSONL保留可选结构，全部字符串继续脱敏。高级条件贯穿URL/收藏/统计/清理预览/导出。修复合法较长模型名被500字符旧逻辑截短的问题，支持采集器的512字节边界并明确拒绝超后端UTF8范围输入。
- 原始Sub2API rate_multiplier默认不迁移；显式开启后端读取完整外层JSON number，前端保留原词法，精确归一化为十进制字符串。0有效、null/缺失不伪造1；超精度逐行拒绝而非舍入。导入后独立成本回执/策略确认，不改账号weight，不自动创建有序策略，没有目标策略明确跳过。
- 同凭据账号可明确只准备成本或租期/成本共同确认，单行回执不跨文件混合，缺回执/目标变化/结果未知均与账号导入结果分开，不自动重放。

契约：[管理观测](MANAGEMENT-OBSERVATIONS-API.md)、[外部采购倍率](MANAGEMENT-EXTERNAL-COST-IMPORT.md)、[导出字段](MANAGEMENT-PARITY-USAGE-API.md)。本任务没有编辑网关，只读取其他源码任务新增的真实观察字段和管理持久数据。

## 验证结果

| 检查 | 最终固定候选 |
| --- | --- |
| Linux管理竞态，真实隔离PG16/Redis7 | 125顶层、302含子测试通过，0失败/跳过/缺失 |
| 同源码完整Go | 454顶层通过、100环境跳过、0失败；1191通过事件、111跳过事件 |
| go vet与Windows/Linux构建 | 全通过，220文件前后哈希相同 |
| 前端统一单测 | 182通过，0失败/跳过 |
| 账号现有流程 | 58组通过 |
| 原外部租期 / 新外部倍率 / 自家租期 | 5 / 6 / 3组通过 |
| 使用记录全回归 | 57组通过，0页面异常 |
| 视觉与旧壳 | 141账号截图、309日志图和额外原始倍率截图；最终54资源中48基线保持原字节 |

共129组页面回归，最后的模型边界修复后在同一个构建完整重跑，没有沿用先前JS变更前的截图当行为证明。

后端不仅用手填记录：`TestManagedObservationRuntimeToQuery`及PG对应专项让真实网关handler对本地假上游走请求401→OAuth刷新→重发，再从管理查询/详情/统计/导出读到已持久化模型和三项尝试。响应字节和派发数不变，提示词/私有query/刷新token未记录。这证明所测本地HTTP合同，不泛化成生产或所有native/WS分支。

外部倍率实测三种输入、保真数值边界、默认关闭、null缺省、0、来源origin无伪策略版本、无目标策略、同凭据、双配置回执及忙状态。所有DB测试是一次性隔离环境，临时容器/内部网络/凭据已清理；没有生产账号或上游推理。

## 固定交付

- Go集合 `307b183ce71b3582488fa57de28906a361b0d431d1ee646003778dc7d43ef490`，220文件。
- [Go归档](../.local/management-parity/local-race-20260918t050711-9d1fa7/source.tar.gz)，SHA-256 `b07c51d3cf200c6595c8b62fadd972ee21645e4ca79b8f6d1e489ac4c60ed91a`。
- [Linux程序](../.local/management-parity/local-race-20260918t050711-9d1fa7/codex-agent-linux-amd64)，SHA-256 `3f29284dc60db35b6911356f810ed05a68dc97942b31f54b2cf4c050a67f2f6b`。
- [54资源前端包](../.local/management-parity/observations-20260918/frontend-449c449b6f55.zip)，SHA-256 `d24829d43044d13e4b941ce6f711ad2ac5caebb22dbb6d250a31de6d21ff6a13`。
- [前端源码](../.local/management-parity/observations-20260918/frontend-source-449c449b6f55.tar.gz)，构建manifest `449c449b6f55278521dfac20fe0bc558bea56122bfb036b85c732eb6b44be24d`，Accounts-BqsbixQG/Logs-h8bkz_3y。
- [机器总表](MANAGEMENT-OBSERVATIONS-VERIFICATION-2026-09-18.json)，以及race目录management-evidence不可变副本，覆盖全部独立外部导入、租期和日统计报告。

前端重跑：`node frontend/scripts/verify-management-unit.mjs observations-20260918`。

## 本机真实预览

[本轮独立预览](http://127.0.0.1:56257/#/login)已启动，使用固定Windows程序与上述前端副本、空PG/Redis，登录密钥在本机 [admin-key.txt](../.local/management-parity/observations-preview/admin-key.txt)。原预览和生产不受影响。

实际Cookie登录/退出、账号/日志、外部双选项默认关闭、观测七筛选/可选列/未知样本及价格目录共11检查通过；1440/390共20截图，页面/静态/API/外部请求错误均为零。没有导入账号、提交恢复或发送推理。[真实浏览器证据](../.local/management-parity/observations-preview/browser-verification.json)单独保存，不与合成行为套件混用。

后台为用户试用保留；停止 `.local/management-parity/observations-preview/stop.ps1` 只删除自身服务和容器，并清空该预览数据。重启start.ps1后以state.json的新地址和新密钥为准。此本机预览不代表上线。

## 剩余范围

[最新剩余审查](MANAGEMENT-REMAINING-AUDIT-2026-09-18.md)确认一个新的实际纯管理差距：渠道销售自定义多段**输入上下文**区间，逐桶绝对价/倍率/继承，以及平价部分继承。当前采购区间基数=input+output，与该销售功能不同，不能作为等价替代。该项具有真实参考服务/UI调用链，下一阶段继续实现，不用本轮测试全绿缩小原目标。

每账号网关策略/模型映射、恢复冷却与自动调度、Spark/私钥认证等运行时边界仍保留；管理接口不能以写admin文件的名义改变被要求保留的网关行为。未观测的auth/DNS/TLS独立时序和多模态细桶不能从现有阶段或text用量推断，也不回填历史缺失字段。

本管理候选继承独立源码owner的网关/native更新，归属差异在机器报告单列，不能据管理通过宣布其所有分支发布就绪。生产003迁移与协同发布门禁不变。本轮无生产迁移、切换或模型调用。
