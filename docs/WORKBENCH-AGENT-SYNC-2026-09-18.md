# 18876 → Codex Agent 同步与环境验收

## 12:10 当前状态

已通过现有Clash香港节点恢复SSH并发布固定956/a66后端，003迁移、47前端保留、重启对账和生产网页均通过，详见 [发布记录](GATEWAY-DEPLOYED-2026-09-18.md)。旧版缺少reauthorize端点的阻断已由版本替换解除；同步timer已恢复active，观察3个正常周期均为2个合格账号unchanged、无错误、RT转发0。源AT在窗口内没有变化，因此自然轮换仍明确未观察，不把接口支持等同轮换现场通过。下文账号数、a9旧binary及连接故障等为此前时间点记录。

更新于 2026-09-18，Asia/Shanghai。仅涉及独立 18876 工作台的只读管理 API、Codex Agent 的账号/代理/分组 API，以及本项目新增的同步定时器。未改变工作台支付流程、购买调度、Sub2API、accounts 或 28765。

## 最近现场状态

以下健康/版本来自 09-18 04:13 +08 的成功核验；同步状态最近核验于 04:18。10:16 恢复工作后服务器连接不可用，不能把历史健康状态当成此刻探测结果。

| 项目 | 结果 |
| --- | --- |
| 工作台 | 主节点 `43.225.196.170:18876`，Compose `workspace-transfer-tool`；实际镜像按每次同步报告记录，不能使用早期本地目录版本代替运行证据 |
| Agent | `http://43.225.196.170:18879`，`codex-agent.service` active，live/ready HTTP 200 |
| Agent 当前二进制 | `a9bf68d1899ec1d58381c788b84cc63123c88cb8196d1acb0b4faafa1b65831d`；本轮尚未切换网关二进制或前端 |
| 新账号 | 初次导入 5 个不同身份的已付费 Pro5，源令牌计划为 `prolite`；04:18 源资格变为 4 个，实际同步停用 1 个不再符合资格的自有账号，不替换原有账号。之后的当前数量尚未重新核验 |
| 刷新凭据 | 转发数量为 0。只同步 AT、身份、真实到期时间；现有系统继续持有 RT，避免两个进程同时轮换 |
| 出口 | 新账号绑定工作台授权时使用的既有 SOCKS5 出口，在 Agent 内新增独立代理记录；未修改该出口服务。代理诊断 HTTP 200、76 ms |
| 测试分组 | 单账号组 1 个、账号池组 4 个；另留 1 个账号给官方 CLI，CLI 与压力池不重叠；所有测试 Key 限定分组 |
| 自动同步 | `codex-agent-workbench-bridge.timer` 已部署并 active，约每 60–65 秒运行一次 |
| 持续 AT 更新 | **未通过**。旧版网关缺少 `POST /api/accounts/{id}/reauthorize`，明确报 `credential_update_requires_cas_api`；定时器已部署不代表该功能通过 |

新增账号的并发配置为 100、优先级 -100，绑定工作台分组与固定出口。配置值不是已经验证的上游容量；不修改原有账号的并发、优先级或代理。

04:18 运行时代码核对纠正了原测试夹具的退款字段假设：实际 API 移除内部 `has_active_refund`，公开 `can_delete` 和 `delete_block_reason`。适配器已接受经过核实的组合；详情缺失、未知原因或字段错类型明确报错，不据此误停用。该修正已上传独立同步服务，18 项本地合成回归通过，不能拿这个数字代替 PG/CAS 真实事务验证。

## 验证证据

首次导入报告：`.local/gateway-environment/20260917T195209.156841Z/bridge-apply.json`，5 added、0 RT。再次执行已识别源 AT 更新；旧服务缺 CAS 端点，拒绝按名称覆盖，见 `20260917T195234.427715Z/bridge-apply.json`。

真实单请求：`.local/gateway-environment/20260917T195324Z/real-sanity.json`，`gpt-5.6-luna`，HTTP 200 且 `response.completed`，TTFT 1398.136 ms，总耗时 1959.834 ms，输入 64 / 输出 5 token。

另一个源码对话的官方 CLI 验收见 [CODEX-CLI-WORKLOAD-DEDICATED-2026-09-18.json](CODEX-CLI-WORKLOAD-DEDICATED-2026-09-18.json)：修复、续接、历史记忆任务通过，12 个请求全部有成功终态，5 次 shell、4 次文件变更，缓存输入 104704 / 124319，即 84.22%。该链路是候选本地网关 → 当前生产专组网关 → 固定出口；不能当作新候选已部署或高并发通过。

PG 独立核对：新同步账号累计 13 条低负载请求均成功，延迟 P50 4022 ms、P95 24526 ms。这是短 sanity 与代码任务的混合样本，**不是四档并发性能数据**。

历史错误也不能混为一个根因。48 小时查询中的 270 条 429 匹配上游限流文字；37 条 503 匹配本地无启用账号；5 条 502 中 4 条匹配 overload，另 1 条尚未分类；还有 13 条 HTTP 200 但流内 overload。原始错误、账号名、令牌未导出。服务采样时无 OOM 或 CPU throttling，不能推断高峰同样没有资源瓶颈。记录：`.local/gateway-environment/20260917T200031Z/inventory.json`。

## 四档真实并发状态

| 档位 | 真实执行 | 成功率 / P50 / P95 |
| --- | --- | --- |
| 30 | 未执行 | 无数据 |
| 50 | 未执行 | 无数据 |
| 70 | 未执行 | 无数据 |
| 100 | 未执行 | 无数据 |

启动四档的命令被自动审批以 `blocked by policy` 拒绝；缩减为单轮 30 并发也被拒绝。两次均在执行命令前被拦截，未启动压力进程。系统没有返回更详细原因。本会话没有重试其他执行通道、要求其他 agent 代跑，或将合成 501 次请求当作真实数据。

工具已写好：`integration/gateway_environment/live_load.py`、`monitor.py`，部署编排为 `tools/gateway_load.py` / `deploy/gateway_load_remote.py`。默认零自动重试、完整 SSE 终态判定，429 尊重 Retry-After；客户端并发与网关 admission 分开，真实上游并发未知时保持 null。服务器编排的实际启动尚未通过验证，不标记为部署成功。

## 统一合成验收

```powershell
python tools/gateway_acceptance.py
```

只运行合成账号/本地假上游，不请求生产模型。覆盖桥接幂等、退款禁用、源错误不误删、ID+版本更新与错误路由；SSE、错误来源、冷却、deadline、关联 ID；只读采样脱敏；连接池、取消释放、账号路由隔离。源码在并行修改时会记录前后哈希并使验收失败，不能用旧报告批准新快照。

联合候选使用显式冻结源码，不能重新取共享在途目录：

```powershell
python tools/gateway_acceptance.py --snapshot .local/management-parity/linux-race-20260917T214152-2d29dc --release-audit
```

10:21 最新候选 `5a818844f613ad35602ac466c80d8949a2e7f7a180e96ae21c4cbd1573ce6d22` 的独立网关检查 154 顶层通过、18 明确跳过，vet 通过，报告 `.local/frozen-gateway-race/20260918T022116-6f87b7/report.json`。源不变；同版本网关 PG/Redis 与 Linux race 没有执行，整体状态仍是 `blocked`。管理 owner 的同归档管理 PG/Redis race 73/73 可用于管理部分，不能替代网关部分。

本机合成 64 请求在显式 8 连接上限下新建 8、复用 56，P50/P95 为 61.594/122.844 ms；20 次取消后恢复耗时 0.513 ms。没有自动重试放大、账号路由串头为 0。这些是本机回归数据，不是上游容量或部署收益。默认 Transport 的 `MaxConnsPerHost=0` 仍不代表有全局活动连接上限，128 条客户端缓存上限与连接上限不同。

旧快照 `bee29a...` 的五项 Linux race 与 `dc955...` 的 141 项网关 Linux race 已通过，但只证明各自快照，不能给新候选补填通过。

## 部署和回退

本机连接当前主节点使用已读取 `sub2api-production` 技能的凭据辅助工具。用户已授权本项目本地代理例外：

```powershell
$env:CODEX_AGENT_SSH_PROXY='http://127.0.0.1:17897'
python tools/workbench_bridge.py preview
python tools/workbench_bridge.py prepare-proxy
python tools/workbench_bridge.py apply
python tools/workbench_bridge.py prepare-tests
python tools/workbench_bridge.py install
python tools/workbench_bridge.py status
```

`prepare-proxy` 解析源工作台既有 `account-warp` 的容器地址，只写 Agent 代理。地址不匹配时停止，不默默替换既有配置。`prepare-tests` 只创建/关联专用分组和 Key，不发推理。私密 Key 仅保存在主节点 `/var/lib/codex-agent-workbench-bridge/`，不得贴入工单/报告。

同步程序：`/opt/codex-agent/workbench-bridge/workbench_bridge.py`。状态与锁：`/var/lib/codex-agent-workbench-bridge/`，目录 0700、文件 0600。定时器单实例加 flock；内存上限 96 MiB、CPU 10%、Tasks 32、FD 128、单周期期限 180 秒。令牌只在源 API → 进程内存 → Agent 加密存储间传输，不落同步状态文件。状态只持有映射、摘要、到期时间与结果。

停止自动同步的回退命令：

```powershell
python tools/workbench_bridge.py stop
```

该命令停止并禁用新增定时器，保留已导入账号、现有凭据和日志，以便继续使用或手工管理；不回退工作台版本，不重启 Agent，不触碰其他项目。

下一发布门禁：源码/管理 owner 冻结后端候选 → 合成与 PG/Redis/CAS 验收 → 保留当前生产前端原字节的后端发布 → 桥接再次执行验证 AT 更新和幂等。当前仍为“新账号已同步、同步定时器已部署、AT 更新受旧 API 阻断、真实阶梯压测未执行”；不能写成整体部署/验收完成。

## 当前发布限制

- 已读取管理最新冻结交付 `5a818844...`，Linux 二进制为 `6ea3efc086d510399fe149def6b3e56941462f5531f8875967a7ed0c43e28918`。未发布该候选，未发布新 54 文件 UI 包。
- 当前生产前端 47 个文件已通过 HTTP 与 SFTP 逐项哈希校验，保存于 `.local/runtime-frontend/d87032e071e7c72343ccb178857d4e56a0200ea7af87aa3b262083e73d15d1c6/`；发布时不能覆盖为共享 `frontend/dist`。
- 003 实际迁移器的隔离验收已通过，生产尚未执行 003。准确旧二进制的 003 回退兼容只完成复制/hash，首次报告解析错误后尚未完成真实复跑；不能发布。
- `tools/upgrade.py` 新显式 v2 additive 路径及 26 项本地状态机/封装测试通过。该工具不会用新库覆盖旧 JSON、不会回退数据库数据，也不会把 v1 的 schema-preserving 限制放宽。工具通过不等于生产演练通过。
- 10:16 的直连 SSH 超时；原临时代理 17897 没有监听，系统 7897 经 CONNECT 后 SSH banner 超时。10:19 尝试恢复项目独立代理进程被平台以 `blocked by policy` 拒绝，进程没有创建。本会话未改换执行通道或委派绕过，远端验收/迁移/切换暂停，继续本地可执行验证。

后续执行前必须重新核对实际运行版本、源账号资格、固定出口地址、前端 hash、数据库 ledger 与同步服务状态；不能直接重用数小时前的运行事实。
