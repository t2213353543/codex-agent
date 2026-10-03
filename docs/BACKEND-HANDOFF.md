# 管理后端与 PostgreSQL / Redis 交付说明

> 当前推理入口已于 2026-09-27 改为 PowerPoint BPS，维护入口见[当前架构](architecture/POWERPOINT-GATEWAY.md)。下文保留 2026-09-17 存储改造交接；其中网关状态及目录位置按当时版本理解，不能代替当前推理验收。

本文件对应 2026-09-17 管理后端改造。生产迁移与部署归集成任务执行；后端任务只操作独立合成测试数据库。接口对接见 [API-CONTRACT.md](API-CONTRACT.md)。本项目没有 Git 元数据，交付以文件清单和构建校验为准。

## 存储和运行边界

- 服务模式必须配置 PostgreSQL 和 Redis，不自动回退 JSON。旧 file store 保留用于导入、回退导出与兼容测试。
- PG 保存账号、分离的 AES-256-GCM 加密凭据、分组/代理关联、加密代理密码、请求明细、持久事件、聚合、审计和设置。旧网关 API key 文档作为加密设置保存在 PG，维持原认证行为。
- 账号使用随机稳定 ID；原名称路径兼容；列表不读取凭据表。旧网关账户枚举仍按原导入顺序，未修改网关实现。
- `request_logs` 是可清理的明细；`usage_events` 是持久幂等事件。累计使用量不依赖明细保留期。`usage_legacy_baselines` 保存旧快照中无法由日志恢复的可验证统计差额。
- Redis 保存短期管理会话以及无筛选概览的 10 秒缓存。缓存故障回源 PG；cookie 会话校验故障返回 503，不能用同请求的 Bearer 自动绕过。未引入异步任务平台；刷新接口是同步请求。
- 凭据刷新按账号串行，使用独占连接上的 session advisory lock；OAuth 网络请求期间没有普通数据库事务。返回的新令牌保存使用 credential_version CAS。

## 环境变量

| 变量 | 默认/要求 | 说明 |
| --- | --- | --- |
| `DATABASE_URL` | 服务和存储命令必需 | 独立 PG 用户/数据库；支持 pgx URL 参数，包括 TLS 与测试 `search_path` |
| `CREDENTIAL_ENCRYPTION_KEY` | 必需 | 标准 Base64 编码的 32 个随机字节；由部署私有配置注入，不写数据库、仓库或普通日志 |
| `CREDENTIAL_ENCRYPTION_KEY_ID` | `primary` | AEAD 认证数据的 key ID；与密钥一起备份，改变它也需要重加密，不能直接改值启动 |
| `REDIS_URL` | 服务必需 | 支持用户名/密码 ACL URL；存储 CLI 不依赖 Redis |
| `REDIS_KEY_PREFIX` | `codex-agent:` | 所有 Redis key 的命名空间；末尾自动统一为 `:` |
| `REDIS_NAMESPACE` | 无 | `REDIS_KEY_PREFIX` 未设置时的兼容别名 |
| `ADMIN_KEY` | 首次空库启动必需，至少 32 字符 | 只用于初始化。已持久化密钥优先；轮换后不要通过旧环境值覆盖 PG。导入 `client-key` 可完成初始化 |
| `ADMIN_SESSION_TTL` | `28800` | 会话 TTL 秒，范围 60–86400，整数；不是 `8h` 形式 |
| `ADMIN_COOKIE_SECURE` | `true` | 仅本地纯 HTTP 测试设置 `false`；cookie 始终 HttpOnly、SameSite=Strict、Path=/api |
| `PG_MAX_CONNS` | `12` | 范围 2–100；MinConns=0，连接寿命 30 分钟，连接超时 5 秒 |
| `STATS_TIMEZONE` | `UTC` | IANA 时区；非法时区拒绝启动。二进制内嵌 tzdata |
| `TEST_DATABASE_URL` | 无 | 显式设置才运行 PG 测试，每个测试建立独立随机 schema 并只清理自身 schema |
| `TEST_REDIS_URL` | 无 | 显式设置才运行 Redis 集成测试，使用独立命名空间子前缀 |

PG 会话设置 UTC、statement_timeout=15 秒、lock_timeout=5 秒。Redis 连接池 10、dial timeout=2 秒、read/write timeout=1 秒，无自动无限重试。环境变量不可用时不降级为未认证模式。

## 命令与可重复执行的迁移

以下命令均在项目根目录运行，连接环境由部署/测试私有配置注入。

```powershell
go build -o .local/codex-agent-candidate.exe .
.local/codex-agent-candidate.exe migrate
.local/codex-agent-candidate.exe import-legacy --source C:\private\legacy-copy --dry-run
.local/codex-agent-candidate.exe import-legacy --source C:\private\legacy-copy
.local/codex-agent-candidate.exe --listen 127.0.0.1:28879 --web frontend/dist
```

`migrate` 按文件名执行嵌入的 SQL，每个版本独立事务；schema 级 advisory lock 串行化并发迁移，`schema_migrations` 记录 SHA256。重复运行跳过相同版本；已应用文件内容变化拒绝继续。服务启动只校验 migration 状态和 checksum，不自动执行 DDL。后续 schema 调整必须添加新的 SQL 文件。

`import-legacy --source` 支持旧目录（accounts/groups/proxies/keys/settings/stats JSON、logs.jsonl、client-key），或单个完整 JSON / JSON 数组 / JSONL / 纯 token 文件。支持 tokens、credentials、平铺凭据。损坏 JSON 不降级为 token。

dry-run 在连接数据库之前完成，只读源文件，不执行任何 PG/Redis 写入。输出 `accounts/groups/proxies/keys/account_groups/account_proxies/logs/log_requests/log_tokens/stats_requests/stats_tokens/manifest/metadata_hash`，检查名称、凭据形状、重复、关联、活动账号和到期字段；不输出令牌、密码、管理 key。metadata_hash 包含稳定 ID、名称、到期与关联，manifest 用于精确识别源文件版本。统计差额在实际写入持久事件后再次核对。

首次导入要求业务表为空。账号与管理配置在一笔事务中写入；日志按稳定 event_id 幂等导入，失败可使用完全相同源文件重跑。不同 manifest 拒绝覆盖。导入未完成时服务拒绝启动；不要同时启动服务和导入。完成后再核对账号/关联数量。只在这个离线导入校验中使用精确 COUNT，不用于日志翻页。

旧日志可能比 stats.json 更新：持久事件已经覆盖的快照不补计；旧快照存在但明细已丢失时，按时间桶和维度保存可验证差额。无法解释的混合差额报错并要求核对源文件。基线可以恢复累计/趋势/账号日用量，但不能凭空恢复原始错误正文、模型、IP、key 维度或每次请求明细；这些缺口不能标为完整日志恢复。

## 保留、聚合与健康检查

```powershell
.local/codex-agent-candidate.exe rebuild-usage
.local/codex-agent-candidate.exe prune-logs
.local/codex-agent-candidate.exe prune-logs --before 2026-06-17T00:00:00Z
```

`rebuild-usage` 在同一事务内从 usage_events 和历史基线重建聚合，与事件写入通过共享/独占 advisory 锁协调。`prune-logs` 按小批清理明细，默认 92 天；不删除持久事件、历史基线、分钟/日聚合或真正累计。`tokens_retained` 是最近 92 天的统计窗口，并不声称其他聚合已经物理删除。没有自动定时清理任务，集成可按维护窗口调用该命令。

- `/health` 保留旧 liveness 响应。
- `/health/live` 仅检查进程存活。
- `/health/ready` 检查 PG、Redis 和未解决的持久化失败；失败返回 503。
- 日志写入失败明确记录脱敏错误，健康状态失败，保留内存待重试事件；每 30 秒及优雅退出重试。数据库故障期间若进程被强制终止，尚未提交事件无法保证恢复；没有把 JSON 或 Redis 作为隐式第二事实来源。
- 退出顺序：停止接收 → 等待 HTTP 排空（55 秒后关闭连接）→ 等待实际 handler 结束（含正在保存的刷新结果）→ 最终 Metrics.Save/Keys.FlushLastUsed → 关闭连接。保存错误使进程返回错误。部署停止超时应允许请求取消、刷新持久化和最终刷写完成，不能在第 55 秒立即 SIGKILL。

## 凭据轮换与回退

管理 key 轮换单独事务提交密钥与幂等结果，再更新内存 key；与普通设置修改混合的请求返回 400。客户端生成并保存 request_id，24 小时内重放返回相同结果。原响应丢失时，可用原管理会话重试；旧 Bearer 仅能以完全相同 request_id 重放该轮换操作，不能访问其他接口。GET settings 只给 masked key，账号接口永不回传 AT/RT，代理接口只返回 has_password。

切换之前保留旧二进制、完整停写 JSON 快照、PG 备份和独立保管的加密密钥。新旧进程禁止同时刷新同一套凭据。尚未发生新写入/刷新时可以恢复旧快照；新进程开始写入后，旧快照不再是可用回退数据。

```powershell
# 先停止新服务，确认刷新/写入已排空；保持 PG 可读。
.local/codex-agent-candidate.exe export-legacy --target C:\private\rollback-current
# 核验导出的账号、关联、日志和统计，再用旧二进制指向该新目录。
```

export-legacy 从 PG 当前事实导出最新凭据、代理密码、密钥、关联、日志和旧版统计格式，不打印敏感内容；文件以私有模式创建，目标目录必须不存在，完成后从临时目录原子改名。Windows 还需由部署维护私有目录 ACL。账号存在同名歧义时拒绝导出，不能生成旧版无法正确区分的数据。

导出必须停写，跨表多次读取不是在线一致性备份。已清理明细的事件只能导出其持久的时间、状态与用量，缺失的错误/IP/模型不会伪造。统计基线直接导出到 stats.json，不依赖已缺失的日志。回退后再次切到 PG 需要重新规划停写导入，禁止未经设计的双写。

## 测试入口与归属

```powershell
go test ./... -count=1
go vet ./...
# 在已有测试私有配置的本地工作区运行完整PG/Redis测试，禁止输出配置：
$config = Get-Content .local/integration/environment.json -Raw | ConvertFrom-Json
$config.PSObject.Properties | ForEach-Object { [Environment]::SetEnvironmentVariable($_.Name,[string]$_.Value,'Process') }
go test ./... -count=1 -timeout 20m
```

未设置 TEST_DATABASE_URL / TEST_REDIS_URL 时相应测试明确 Skip，不能把纯单测通过当作数据库验收。独立库需允许测试用户 CREATE/DROP 自身 schema、测试 trigger/function；不需要超级用户。race 需要 CGO 和本机 C 编译器，运行结果以实际日志为准。

主要变更：accounts.go/accounts_import.go/accounts_pg.go、store.go/store_pg.go、admin.go/admin_extra.go、stats.go/stats_pg.go/stats_legacy.go、main.go/database.go/sessions.go/migration_cli.go、go.mod/go.sum、migrations、后端测试和本 docs。未修改 frontend、tools、deploy、proxy.go、socks5.go 或 web.go 的实现。gateway.go 仅调整 ensureReady 的凭据读取标识和 PostgreSQL 刷新锁入口：使用稳定 ID 并交由 Store 的账号锁串行化；旧文件兼容路径保留原锁。没有改动转发协议、调度选择、负载、计费、限流或模型路由。

最终测试数字、增量复测要求与环境阻断记录见 [BACKEND-TEST-RESULTS.md](BACKEND-TEST-RESULTS.md)；本说明不代表已迁移或已部署生产。
