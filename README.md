# Codex Gateway

高级反代系统提供 PowerPoint + Codex 与 Codex 直连。会话、推理流程和本地工具由客户端管理；官方 App Server 运行模式及服务器托管入口已移除。系统同时管理账号、分组、API Key、代理、请求日志和用量。

在管理设置选择运行模式；没有隐式跨模式回退。切换后新建客户端会话，避免混用不同后端的加密上下文。现有 ChatGPT 账号和数据库继续沿用。详见[三模式与托管接口](docs/INFERENCE-MODES.md)。

## 架构

```text
Codex / Responses 客户端
  → 身份校验、账号准入、会话绑定
  → PowerPoint 协议适配
  → BPS Responses / Compact
  → 实时输出、原生工具调用
  → Codex 本地执行 / MCP → 工具结果回传
```

HTTP 支持流式和非流式；WebSocket 支持分流、预热、增量历史展开和取消。加密历史绑定账号身份，增量会话缓存加密保存于 Redis。返回成功终态前先保存续聊状态，存储或身份校验失败不会伪报成功。

续聊正文缓存最多保留 1 小时，共享 512 MiB / 32,768 条预算，超时或超量自动淘汰，不永久写入 PostgreSQL。单条结果超过缓存预算时仍可正常返回，后续需携带完整上下文。归属证明同样有限期保存。排查与维护记录见 [Redis 内存修复](docs/REDIS_MEMORY_20260930.md)。

Sub2API 双节点均可通过同机 Docker 网关 `http://172.18.0.1:18879` 对接；两台 Codex Agent 共享账号、配置与有限续聊状态，详见 [双节点部署](docs/DISTRIBUTED_DOCKER_20260930.md)。设置页支持独立的 [对外缓存读取比例控制](docs/CACHE_READ_RATIO_20260930.md)。

详细行为与限制见 [PowerPoint 网关](docs/architecture/POWERPOINT-GATEWAY.md)，当前 Go 服务验收与 0.3.0 上线结果见[迁移交付](docs/POWERPOINT-MIGRATION-2026-09-27.md)。早期 Python 原型的验证记录在 [实测报告](docs/POWERPOINT-CODEX-VALIDATION-2026-09-27.md)，不代替当前 Go 服务验收。

管理页面的订阅额度来自 ChatGPT 账号信息；模型目录、生成和压缩使用所选运行模式。美元估算按 Sub2API 账号统计成本与官方七天窗口使用率反推，不把估算值视作官方美元余额。旧研究报告与根编译残留的位置见[历史材料索引](docs/history/README.md)，当前构建不要使用历史二进制。

## 目录

| 目录 | 职责 |
| --- | --- |
| `main.go` | 薄启动入口，保留 `go run .` / `go build .` |
| `internal/bps` | 纯协议：请求、工具、模型目录、实时 SSE、压缩结果 |
| `internal/server` | HTTP/WS 服务、账号授权、会话、管理 API、存储和对应回归测试 |
| `migrations` | SQL 与嵌入资源，已有数据库迁移标识保持不变 |
| `frontend/src` | Vue 管理界面 |
| `tools` | 验收、构建、部署入口；先读 [工具索引](tools/README.md) |
| `deploy` | 服务与发布脚本 |
| `integration` | 隔离数据库、协议和性能测试 |
| `docs` | [当前文档与历史记录索引](docs/README.md) |
| `.local`、`data` | 私有验证产物与运行数据，不提交、不作为源码扫描输入 |

## 构建与测试

版本库保留源码、测试、数据库迁移、部署工具、维护文档与必要静态资源。依赖缓存、编译产物、运行数据、浏览器截图和临时发布副本由 `.gitignore` 排除；首次检出后按下列命令安装依赖并重新构建。`native-transport/vendor` 是 Rust 的本地源码依赖，必须保留。

依赖 Go 1.26、Node.js 20.19+；运行服务需要 PostgreSQL 和 Redis。

```powershell
npm --prefix frontend ci
npm --prefix frontend run build
go test ./...
go build -o .local/build/codex-agent.exe .
```

数据库和服务环境的准备见 [数据库部署](docs/MIGRATION-DEPLOYMENT.md)。设置环境变量后运行：

```powershell
go run . --listen 127.0.0.1:18879 --web frontend/dist
```

控制台时间显示和日期输入统一为上海时间（`Asia/Shanghai`）。今日统计默认使用上海日历日；可用 `STATS_TIMEZONE` 显式设置服务统计时区，使用记录的日历分桶也可单独选择。

必需的存储配置包括 `DATABASE_URL`、`REDIS_URL`、`CREDENTIAL_ENCRYPTION_KEY`。不要把密钥写入源码或示例配置。独立 PostgreSQL/Redis 验收与发布步骤见 [工具索引](tools/README.md)。

## PowerPoint 配置

| 环境变量 | 默认值 / 作用 |
| --- | --- |
| `BPS_BASE_URL` | `https://bps.openai.com/basispoints/api`；生产要求 HTTPS |
| `BPS_TOOLS_VERSION` | 当前已验证的 PowerPoint 工具版本，可随发布更新 |
| `BPS_WEB_SEARCH` | `enabled`；可设 `disabled`，同时限制请求和返回工具 |

管理设置仍控制 HTTP、WebSocket、JSON、模型目录、压缩和超时。会话始终绑定原账号及推理模式。官方 CLI 协议使用独立的原生传输；PowerPoint 请求仍使用自身协议。

## Codex 接入

在用户级 Codex 配置中添加独立 provider：

```toml
model_provider = "powerpoint_gateway"
model = "gpt-6-astra"
model_reasoning_effort = "xhigh"

[model_providers.powerpoint_gateway]
name = "OpenAI"
base_url = "http://127.0.0.1:18879/v1"
env_key = "POWERPOINT_GATEWAY_KEY"
wire_api = "responses"
supports_websockets = true
```

`POWERPOINT_GATEWAY_KEY` 使用管理后台创建的网关 Key。`name = "OpenAI"` 用于启用当前 Codex 客户端的原生远程压缩分支，不会把请求改发到 Codex 官方上游。WebSocket 还需在管理设置启用；关闭时支持回退的客户端可继续使用 HTTP。

模型与推理档位以账号实时模型目录为准。答对单道题不代表对所有任务的质量保证。

## 验收与发布

```powershell
python tools/acceptance.py
```

实际 BPS + Codex 验证使用授权测试账号，在隔离进程中运行，不部署生产服务：

```powershell
python tools/bps_validate.py
```

该命令将编译当前 Go 网关测试程序，经既有 SSH helper 在服务器内读取两条已启用测试账号。凭据不返回工作站，临时文件在结束后清理。用量、上线状态与完整检查以对应验收报告为准。

手动运行 live 测试所需的显式环境变量及报告位置见[工具索引](tools/README.md)。`go test` 缺少 live 环境时会跳过该项，源码测试通过不等于真实推理或生产发布通过。
