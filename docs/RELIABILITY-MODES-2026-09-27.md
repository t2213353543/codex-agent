# BPS 修复、官方运行模式与用量统计

本轮目标为修复线上可复现 BPS 错误、增加官方 CLI/App Server 两种模式及客户端/服务器两种执行方式，并对齐 Sub2API 官方窗口美元估算和耗时统计。

## 线上错误及处理

读取当前生产使用记录，按时间区分 BPS 流量与此前 Codex 流量，没有把旧的429限流统一归为 BPS。

| 线上类别 | 记录数 | 复现与处理 |
| --- | ---: | --- |
| 历史函数调用缺少命名空间 | 15 | 原请求400，按唯一的真实声明恢复namespace后200；歧义、冲突和替换身份仍拒绝 |
| tool_search 未支持 | 1 | 官方完整声明可以成功；补原生调用/结果、发现工具注册和命名空间恢复 |
| 非默认 service_tier | 16 | priority 实际 BPS 返回422；改为可操作的 unsupported_parameter，不丢字段或改档位 |
| 严格结构化输出 | 2 | json_schema 实际 BPS 返回422；明确指出参数限制，可显式选择官方模式 |
| 请求体422 | 9 | max一项已最小复现并明确返回参数错误；另8项旧记录不含请求正文，无法仅凭状态码证明同一根因 |
| 模型访问403 | 7 | 对应不在账号BPS实时目录的模型，返回实际目录/能力诊断；不自动换模型，也不重复生成请求 |

本轮已复现的 max、priority 和 json_schema 限制没有通过静默删除、降档或更换账号/模型伪装成功。既有“客户端参数兼容”仍会规范化部分通用参数，不将它描述为所有字段透明保留。历史大整数、未知字段和加密状态保持原值。会话和加密历史增加模式归属检查，旧记录空模式兼容为PowerPoint。

证据：`.local/reliability-modes/bps-errors-48h.jsonl`、`bps-errors-current.jsonl`、`live-controls.jsonl`、`live-namespace-final.jsonl`、`live-capability-discovery.jsonl`。这些上游能力是本轮观测，并非永久服务契约。

## 三模式实现

管理设置新增持久化、版本校验的 `inference_mode`。模型目录、账号测试、请求路由和实际状态显示均跟随所选模式。PowerPoint保持默认，不自动失败回退。

官方CLI模式支持现有客户端完整工具循环；独立的官方Rust HTTP/WS组件负责传输。服务器托管任务另行启动真实官方 `codex exec --json`，提供创建、状态、事件重放、取消、工作区文件和恢复任务。AppServer支持现有Responses客户端工具以及原生双向JSON-RPC WebSocket入口。详情与例子见[接入说明](INFERENCE-MODES.md)。

真实发现并修复了 App Server 默认代码工具模式产生“内部exec调用ID与动态函数ID不同”的兼容错误。Responses客户端工具桥从官方进程取得真实模型目录，只修改 `tool_mode=direct` 后启动独立运行时，保留模型、推理档位及其他目录字段；托管CLI和原生AppServer保持官方默认。custom grammar工具使用字符串函数包装，不宣称字节级协议等价。

同账号凭据更新时更新运行时登录态，保留会话及待回传工具；失败清理旧进程。CLI退出码0不代替成功终态。修复了恢复任务与凭据清理竞态、失败启动遗留凭据、超大事件等待以及运行时关闭时未终止工具子进程的问题。后台任务持续复查授权；原生连接在活动任务期间断开会取消进程。

## 官方用量窗口与耗时

按本机 Sub2API 当前源码提取128个模型价格，执行其原始计价函数做588组对照，涵盖缓存读写、图片、服务档位和长上下文边界。使用上游模型、账号统计费用语义和默认账号倍率1；没有引入本项目不存在的自定义商业售价。

官方七天窗口的总美元额度估算为：`窗口账号统计成本 × 100 ÷ 官方使用百分比`。窗口时间使用实际上游边界，缓存刷新不会冻结本地费用。未知使用率、零使用率和过期快照不编造总额度；未定价或历史记录缺失会注明覆盖范围。估算不是官方美元余额。

新请求冻结计价成本与价格版本。耗时新增 `latency_known`，区分真实0和历史未采样；平均值以实际采样数为分母，保留小数。前端数字完整显示，耗时统一为ms、秒、分、小时、天，移除K/M缩写。新增005仅增加nullable字段、不回填历史未知值；发布必须迁移后执行 `rebuild-usage`，保证旧聚合分母也被修正。

## 验证证据

- 全仓Go：3510测试/子测试通过，414个外部或平台条件测试明确跳过；vet通过。
- 前端188测试通过，设置浏览器13项验收通过，320/768/1440px；官方窗口桌面/手机及61.234秒、缺失耗时和0ms渲染通过。
- Sub2API原函数对比588/588，报告 `sub2api-cost-parity.json`。
- 真实隔离PostgreSQL两项通过、0跳过，涵盖迁移、耗时采样、剪枝、聚合重建、费用窗口和导出。
- Linux race：195测试/子测试通过，29项官方模式主测试及4项实际Redis测试通过，无数据竞争；4项外部BPS测试在此单独跳过。
- 官方模式真实7项通过：原生协议生成、AppServer续聊/原生压缩、函数工具回传、托管CLI文件及恢复、两种模式真实下游CLI本地工具、原生AppServer WebSocket。
- BPS真实HTTP/WS原有11项通过，涵盖文件、恢复、原生压缩、apply_patch、搜索、并行MCP及两账号糖果题。最终发布已绑定与候选完全一致的复测快照。

## 生产结果

已发布 **0.4.0**，release `cd2b4b1b075735e468759f196044d4930989db1ef07cad1b74356c9babd218a3`，实际进程二进制 SHA256 `3c4a40bcaaa0fbd9e74b0162485f87b9ecd3c1ca2f07e053d22ccefa3f4a76c4` 与清单相同，实际前端匹配发布包。

三种模式线上模型目录和生成均通过，服务器托管CLI实际写入并读回文件通过；测试结束以版本CAS恢复原先PowerPoint模式，账号身份/启用状态/版本均未变化。迁移005已应用，使用统计已重建。线上单一数据库快照核对17,174个有效耗时样本，总耗时42,518,712ms，事件加历史基线与全局聚合完全一致。

首次切换因误用只具日常读写权限的runtime数据库角色执行迁移失败，自动回滚成功且账本仍5条。修正发布器使用既有专用migrator执行迁移与聚合重建后成功上线；未扩大日常服务数据库权限。发布器21项回归通过。

上线后托管任务发现旧systemd资源上限128进程/线程、512MiB不足，cgroup记录实际触顶。已持久调整该服务为2,048进程/线程、2GiB，并在不重启服务的情况下重新完成全部线上验证；后续发布器也已纳入该配置和生效检查。这是部署配置补充，不改变已验证的应用二进制。

证据在 `.local/reliability-release/cd2b4b1b075735e468759f196044d4930989db1ef07cad1b74356c9babd218a3/` 的 `activate.json`、`status.json`、`production-smoke.jsonl`、`production-smoke-result.json`、`production-final.json`；线上复测退出0、stderr0。资源调整证据另见 `.local/reliability-modes/production-resources.json`。原始复现、失败样本和测试入口均保留在 `.local/reliability-modes/`。

复跑：`python tools/reliability_live.py`；真实存储：`python tools/validate_usage_storage.py`；计价对照：`python tools/verify_account_window_parity.py`；Linux并发：`python tools/bps_race.py --with-redis --include-official`。
