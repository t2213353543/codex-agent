# 上游认证错误区分与真实流中热更新

2026-09-29 07:27（Asia/Shanghai）上线 0.4.23，07:27:54 完成接口、用量及导出验收。默认开启的缓存计费兼容设置继续有效。

## 已修复的问题

Sub2API 唯一指向本中转18879的接入账号146849，曾因底层OAuth `token_invalidated` 401 被永久停调。保存的中转Key实际有效；此前“网关鉴权失败”和“网关使用的上游账号鉴权失败”共用HTTP401，使下游误把整个中转凭据判为撤销。

新版只对已确认有效客户端Key、已选择真实上游账号、且尚未输出的最终上游401返回 `503 / upstream_authentication_unavailable`。内部保留真实诊断与每次上游401，操作日志的逻辑HTTP状态为实际对外503。真实无效客户端Key仍返回401；403和已输出的流保持原语义，不增加重放。

覆盖 BPS 原生JSON、SSE、compact、模型目录、附件及WebSocket。WebSocket中的单次请求失败不会关闭连接，下一次请求可正常完成。账号尝试顺序、优先级、权重、并发与不透明历史保护没有放宽。

## 验证

- 完整Go测试5000项通过（含子测试），481项环境相关跳过；静态检查通过。
- 22项独立PostgreSQL测试、实际Redis并发与Linux race通过。新增真实producer测试确认public503、actual/observed401、最终错误类别及未知用量在新Metrics读取、列表、统计、CSV/JSONL中保持一致。
- 12组真实客户端回归通过，包含缓存计费开关对照、工具、图片、会话、压缩、结构化输出、SSE及WebSocket。
- 生产切换时旧版仍有22条活动请求。唯一标记SSE在首文本增量后切换，切换确认后仍收到88个文本增量，4.117秒后正常completed并包含结束标记。消费线程没有暂停或人为制造长流；没有重发请求，旧进程与入口PID/二进制不变。
- 上线后2次真实生成、1次预期本地非法参数拒绝、3份管理CSV对照全部通过。缓存创建按读取返回仍为true，内部真实读写量未合并，设置版本27与账号配置保持。
- 上一共享版本0.4.22在真实连接、handler、刷新和待持久化任务全部排空后，由生命周期校验通过再退役；旧版0.4.20仍保留，未强制退出。

## 发布身份

- 发布ID：`37450c1bc61bacd9d216581c6fa08432afae12360056815651766134953aa9e8`
- 二进制：`bad6026f65d6addd0123bffa95fd14c35a58ef39070bb02a625a41a4a5aef5f8`
- 活动实例：`codex-agent-runtime-bad6026f65d6addd0123.service`，PID3337873，内部端口18891，公共入口18879不变。
- 固定入口PID2857465，本次未重启。
- 冻结源：`49b7473457087faa133c21474ad94956af8e5a196f24445c1fdfabf50c2ea61c`，目录 `../codex-agent-auth-release-20260929`。
- 数据库仍为000–005六项迁移；其它协作任务的性能迁移未混入本次发布。
- 回执保存在 `.local/upstream-auth-release-20260929/`：`actual-sse-cutover.json`、`postdeploy.json`、`previous-shared-retirement.json`、`delivery.json`。

继续使用 `python tools/hot_release.py --release <package/manifest.json>` 更新，具体流程见 [HOT-UPDATE.md](HOT-UPDATE.md)。

每分钟监控继续工作。已观察到的不透明历史绑定账号503、`invalid_encrypted_content`及不支持的`service_tier`需分别处理，本次没有将这些错误统一重试或声称已全部解决。
