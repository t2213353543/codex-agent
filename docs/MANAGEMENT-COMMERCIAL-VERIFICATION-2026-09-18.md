# 渠道售价与采购阶梯验收

2026-09-18。本阶段完成了实际功能、边界修复、独立测试和固定打包，属于持续目标的实质进展。**尚未上线，不能宣称全部参考管理功能已对齐。** 用户恢复的旧蓝色壳和此前冻结候选保持原样。

## 实际完成

- 渠道售价目录：渠道/计价组/KEY 归属完整编辑、启停和删除引用检查；整份目录原子 CAS 发布，不可变历史、生效时间和只读详情，失败保留草稿、结果不明不自动重发。
- 实际计价：组模型价优先渠道价再回基础目录；精确/前缀/服务层选择；KEY 倍率覆盖组倍率；订阅组高峰；渠道分时和长上下文组合。KEY 明确绑定唯一管理计价组，不从账号多成员组猜测。
- 三费用域：基础参考、账号采购、配置售价在账号统计、日志详情、全量汇总、趋势、分布、排行和 CSV/JSONL 分开显示。缺来源或用量时保持未知，不伪装成实际扣款。
- 采购 Token 阶梯：最多 32 个 `(min,max]` 区间，基数 input+output，输入已含缓存。命中时四桶绝对价替换整次请求价格，再乘采购倍率；空隙和零 Token 回基础价，缺写入桶/非法总量保持未知。历史、预览和导出包含实际命中证据。
- 界面修复：关闭长上下文后再次编辑不误恢复启用；销售目录历史可展开核对全部价格/时段/高峰；来源标签区分“所选价格来源”和目录来源；账号导入的旧“不适用”文案改为“未自动导入”。

契约：[渠道与配置售价](MANAGEMENT-SALE-POLICY-API.md)、[采购阶梯](MANAGEMENT-PROCUREMENT-TIERS.md)、[使用记录字典](MANAGEMENT-PARITY-USAGE-API.md)。固定 as_of 同时保存三类费用规则；PostgreSQL 三域统计共享只读 REPEATABLE READ，文件模式同扫描计算。管理动作不修改网关路由或计量。

## 当前证据

| 验证 | 结果 |
| --- | --- |
| Linux 管理竞态，真实本机隔离 PG16 / Redis7 | 95 顶层、210 含子测试通过；0 失败、跳过或缺失 |
| 同归档完整 Go 测试 | 376 顶层通过、76 环境跳过、0 失败；1020 通过事件、85 跳过事件 |
| go vet、Windows/Linux 构建 | 全通过，190 文件前后哈希不变 |
| 统一前端单测 | 139 通过，0 失败/跳过 |
| 账号浏览器 | 46 组，0 JS 错误；93 张截图，三费用统计也有金额与未知断言 |
| 使用记录浏览器 | 44 组，0 JS 错误；192 张四主题截图及额外 320px 边界 |
| 恢复旧 UI | 54 个静态文件，48 个基线资源原字节不变，无第二 Vue/认证运行时 |

后端手算用例涵盖模型优先级、KEY 覆盖而非相乘、真实零、订阅高峰起止、不同 IANA 时区/周末、跨目录版本删除归属、停用渠道回退、组价排除渠道时段、采购边界/空隙/无上限及微额精度。真实 PG 同时覆盖 CAS 并发、审计失败回滚、冻结价格与整段统计/导出。

浏览器使用合成 API，不是实际账号账单。长表单补拍底部，确认手机下高峰 0 倍、应用按钮和采购区间可见；排行三个格的边界/scrollWidth 检查无溢出，不把截图裁剪误当产品问题。测试自有容器、内部网络和临时密码文件均已清理，不改其他会话容器。

## 固定交付

- Go 源码集合 `5852e08850f5813fc77ada246efcbaca82e0baee2be71a0e838eb15596d74119`，190 文件。
- [Go 归档](../.local/management-parity/local-race-20260918t031440-112ef6/source.tar.gz)，SHA-256 `8a7bea2764442a21b3a3e83ef2dbe2c8de734222b355a6923b1e8d46d647b659`。
- [Linux 程序](../.local/management-parity/local-race-20260918t031440-112ef6/codex-agent-linux-amd64)，SHA-256 `4454acfe35256fad0ada4e0c3aa9ff96aa9345ef4cfa0f91284b690c1300feec`。
- [54 文件前端包](../.local/management-parity/commercial-20260918/frontend-fa845eab012f.zip)，SHA-256 `a15170a28bc519bfe49ce921c660271e88a2c2a81093584c83af9493641c01fb`。
- [前端源码](../.local/management-parity/commercial-20260918/frontend-source-fa845eab012f.tar.gz)，构建清单 SHA-256 `fa845eab012f20603703d31488415b26b9adf9eb594c5d5318cd95c1324ddcb9`。
- [机器验证总表](MANAGEMENT-COMMERCIAL-VERIFICATION-2026-09-18.json)保存全部证据哈希；独立副本在上述 Go 目录的 management-evidence 中，不受后续开发覆盖。

前端重跑：`node frontend/scripts/verify-management-unit.mjs commercial-20260918`。

## 本机真实预览

已启动 [隔离预览](http://127.0.0.1:50673)，使用上面的 Windows 固定程序和前端资源副本，空 PostgreSQL/Redis，不是 API mock。登录密钥仅在本机 [admin-key.txt](../.local/management-parity/commercial-preview/admin-key.txt)，无生产密钥或账号导入。

实际 Cookie 登录/退出、账号/日志空态及销售/采购目录通过，1440/390 共 10 张截图，零 JS、静态资源、API、外部请求或意外业务写入；日志查询自身的只读快照写入正常。证据 [HTTP 验证](../.local/management-parity/commercial-preview/api-verification.json)、[浏览器验证](../.local/management-parity/commercial-preview/browser-verification.json)。这项额外集成证据不修改已固定候选验证报告，不意味着生产已经部署。

预览留作本地试用，停止命令：`powershell -NoProfile -ExecutionPolicy Bypass -File .local/management-parity/commercial-preview/stop.ps1`。启动命令将同目录文件换成 `start.ps1`，实际 URL 以 state.json 为准；脚本只处理自身进程/容器，不影响其他会话工作负载。

## 未完成范围

[剩余能力审查](MANAGEMENT-REMAINING-AUDIT-2026-09-18.md)重新从参考调用链确认四组可独立管理实现的任务：显式跟随售价基数的账号统计；有序多目标采购规则；账号租期随备份往返；账号活跃日/日均/峰值日和费用日趋势。这些不是可用新价格目录替代的同一功能，仍需继续完成。参考未提供的独立渠道价格文件导入导出不额外列为硬要求。

真实网关行为边界也仍保留：每账号指纹/映射、恢复冷却/自动调度、Spark 父子语义、Agent 私钥认证等没有因为管理 UI 存在就完成。完整发送/返回模型、端点链、阶段耗时、重试链/错误截断等仍需真实被动来源。

本管理任务没有编辑网关；本快照继承其他源码会话在途 native 接入文件，机器总表把这些差异单列。管理验收不能代替该会话的 native 最终冻结和专项验证，也不能据此部署共享树。生产环境仍由发布会话协调，现有 003 增量迁移门禁继续；本任务没有生产迁移、推理或切换。
