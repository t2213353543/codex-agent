# 账号状态与使用记录观测补齐

2026-09-18。本轮延续无计费范围，补齐实际参考管理能力与已复现故障，不改变网关转发、调度、协议或指纹。上一轮属于实际进展，本轮不依赖历史报告标签判定完成。

## 账号排序与身份

参考 Sub2API `account_repo.go` 的 `applyAccountListOrder` 确实支持 ID、状态排序。本站新增 `sort_by=id/status`，列表、全筛选选择、元数据和凭据导出使用相同服务端顺序。状态依据本站实际状态键 `disabled/error/expired/ok/unverified` 排序，而不是移植参考中不存在于本站的状态；并列以稳定 ID 同方向排序。具体契约见 [账号排序](MANAGEMENT-ACCOUNT-SORTING-2026-09-18.md)。

文件模式原摘要和状态过滤读取按名称、受 recent ring 容量限制的最新请求，改名可能丢状态、旧名称复用可能串状态。当前管理层完整读取留存记录并只匹配稳定账号 ID，同时间以记录 ID 决定最新；PG 使用同 ID 的持久 account_request_state。摘要、过滤、排序共享同一状态规则，不改累计用量或网关状态写入。缺少身份的旧记录不猜测归属。

元数据导出允许最多 10000 账号，但内部 PG 用量摘要一次只接受 1000；此前超过 1000 会报存储错误。本轮按 1000 分批读取摘要后合并，保留完整导出顺序与数量。测试实际覆盖 1003 账号。文件用量读取不再附带每账号重复扫描最近请求 ring。

日志路径意外变成目录时，原文件扫描可能把零长度目录当空日志；现在明确拒绝非普通文件，避免读取失败显示为无记录或健康状态。

## 预热与查询

`RequestRecord.WebSocketWarmup` 已由独立网关任务真实采集，本轮只消费现有字段。管理过滤 `websocket_warmup=true/false`、详情及 CSV/JSONL 支持该 nullable bool：true 为已观测预热，false 为已观测普通生成，缺失未知不能当 false。来源为 WebSocket `response.create` 的 `generate`，不从 Token 数或 HTTP 状态猜测。

筛选贯穿 URL/收藏、列表、快照、统计、导出、错误列表和清理预览。CSV 在现有 `codex-usage-v3` 最后追加 `websocket_warmup`，目前共 59 列；消费者应按字段名读取。

实际保存和后端筛选允许 1024 字节的请求 ID，但前端原通用 500 字符裁剪会导致长 ID 查不到。当前保留合法完整值，超过 1024 UTF-8 字节明确报错，不静默裁剪为另一个标识。

另修复清理后端白名单遗漏已支持的实际发送/返回模型、入站/上游路径、模型差异与错误截断过滤。此前这些条件在列表可用，清理预览却 400；现在保持完全相同条件。真实 PG 测试确认只删除一条目标明细，保留其余四条明细和五条持久用量事件。

## 证据

新增文件/PG专项：`TestManagedAccountSummaryStableIdentity`、`TestManagedAccountLargeMetadataExport`、`TestManagedAccountIdentityStatusSort`、`TestManagedUsageWebSocketWarmup`、`TestManagedCleanupObservationFilters` 及各 PG 对应测试。

固定 Go `3b8d4b0ed15c677469fa69deb907d2ecd3a7f4de910cd2a1d615d5f578c3a29a` 已通过 104 项管理 Linux PostgreSQL16/Redis7 race，311 事件通过，0 失败/跳过/缺失，测试后源码未变且临时资源清理完成。证据目录 `.local/management-parity/local-race-20260918t071407-edcbb1`。

同一归档 234 文件校验通过；全库 Go 463 顶层通过、110 环境依赖跳过、0 失败（含子测试1213通过/123跳过），vet 与 Windows/Linux 构建通过。跳过原因原样单列。独立网关任务新增被动预热观测等变更随共享快照继承，本管理报告不替代其运行时发布验收。

前端固定 manifest `4a29cbad22816256db72146b5cf2674b14114afbda8708ac9aeb91b6c29adf11`，账号 `accounts-managed-CpksNcOR.js`，日志 `logs-managed-bq7eOFdN.js`。账号55组、日志42组浏览器及150项前端单元通过，无页面异常，包含本轮 ID/状态排序、原租期提交锁定、预热三态、长ID完整查询/超限拒绝断言。账号48张和日志146张截图保留桌面、手机和四主题证据。联合最终归档索引见 [机器验收](MANAGEMENT-ACCOUNT-STATUS-VERIFICATION-2026-09-18.json)。

## 未完成边界

相同源码/前端的真实空库预览位于 `http://127.0.0.1:64810/#/login`，证据在 `.local/management-parity/account-status-preview/api-verification.json` 和 `browser-verification.json`。真实 Cookie、ID/状态/最近请求双向排序、预热筛选、59列CSVv3、退役计费路由404通过；12项桌面/手机浏览器检查和22张截图无页面/API/静态错误。root 独立 ready 检查为200。预览未导入真实账号或发送上游请求，旧57451/50666服务与数据保持不变；预览证据为固定包之外的独立补充。

当前连接能力逐项审查见 [账号覆盖](ACCOUNTS-NONBILLING-CURRENT-COVERAGE-2026-09-18.md)。不会用本轮通过宣称所有参考功能已完成：参考错误工作区的 severity 管理分级仍未接入；清除冷却/自动恢复/独立可调度状态/模型映射/Spark 与 Agent Identity 签名仍涉及网关运行行为；部分多模态 Token、独立客户端请求 ID、细分网络阶段仍缺实际采集来源。可选 TOTP step-up 是参考多用户认证架构差异，不将其默认关闭行为误报为本站导出故障。

不重新引入计费，不回填没有来源的历史数据，不覆盖旧固定候选与本地预览，不部署生产。
