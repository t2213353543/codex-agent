# PowerPoint + Codex 迁移交付

本项目已改为 PowerPoint BPS 推理上游，Codex保留客户端、本地执行和会话能力。服务版本0.3.0。生产启动不再选择纯Codex推理，旧协议实现只保留为兼容回归材料。

## 主要改动

- 新建 `internal/bps`：原生请求/工具/模型目录适配，真正逐事件输出SSE，压缩JSON转换，完整性校验和取消处理。
- 新建 `internal/server/gateway_bps*`：账号、代理、OAuth恢复与BPS绑定；WS预热、增量历史、分流、取消；加密Redis会话缓存。
- 把397个根目录Go实现和测试迁入 `internal/server`，根只保留薄入口与项目说明；SQL独立嵌入包保持原迁移键和checksum。
- 更新Vue设置、诊断、账号测试及接入指南为PowerPoint；移除不生效的旧指纹/缓存控制入口。
- 统一源码枚举、打包、overlay和测试路径，避免目录迁移后漏测；新增可复跑BPS live/race和独立发布工具。
- 旧根二进制移入私有构建目录；过时交付说明归档于 `docs/history`，现有用户数据、账号凭据和私有调查证据保留。

## 验证结果

| 范围 | 结果与边界 |
| --- | --- |
| 全仓Go单测与vet | 3262项测试及子测试通过，vet通过；388项依赖外部环境的测试明确skip，不冒充已执行 |
| Vue单测/构建 | 175项通过，构建成功 |
| 设置与接入浏览器 | 12项行为验收，320/768/1440px无横向溢出 |
| Linux race | 最新冻结版本64个主测试、含子测试93项通过，无DATA RACE；真实模型用例在此环境明确skip |
| 实际Redis | 4项通过：密文、跨实例恢复、篡改与过期拒绝、容量与TTL |
| 隔离PostgreSQL | 20项通过；嵌入SQL与数据库5条迁移ledger匹配，生产schema checksum未变 |
| 实际Go网关＋官方CLI HTTP | 文件、恢复、原生压缩、apply_patch、搜索、MCP实际并行、两账号糖果，共8项通过 |
| 实际Go网关＋官方CLI WS | 文件、恢复、原生压缩，共3项通过；禁止HTTP回退，有101与WS指标证据 |
| 发布执行器 | 22项离线验收覆盖产物、运行基线、健康、失败回滚与外部变更保护；包含systemd StateDirectory软链接和失败诊断 |

真实模型测试固定 `gpt-6-astra/xhigh`，使用授权测试账号，令牌不交给CLI也不返回工作站。两账号糖果题均回答21并给出正确策略。HTTP/WS11项完整通过记录：`.local/powerpoint-migration/20260927T011730Z/live.log`。

先前失败记录也保留：终态推理密文和搜索action与已经发出的done事件不同、慢写入、迟到cancel、Redis同秒自驱逐等均先复现再修正。最终流历史采用Codex已经消费的原生done项中的对应加密状态/搜索action，只在同身份、其余字段一致时复用；不解密、不编造内容、不忽略工具参数变化。

## 发布和复跑

```powershell
go test ./...
npm --prefix frontend test
python tools/bps_validate.py
python tools/bps_race.py --with-redis
```

发布入口 `tools/powerpoint_release.py` 按 `prepare → stage → preflight → activate → status` 执行。发布包绑定源码、二进制、前端和验收文件hash，切换前复核旧运行二进制/服务单元/环境/SQL。切换失败只恢复服务产物，不能恢复或覆盖用户数据库。

## 生产发布结果

2026-09-27 已发布并完成线上复测，版本 **0.3.0**，release ID：

`26f88f12bbb9ef7627e6bfe4e3f06653b21c3206503c28ce0fd600b13ef21edc`

实际进程二进制 SHA256 为 `aa9f3d84d58a996cb667e76431ab1ae58ad3c8f3369c664f5712ca6a73ebc411`，与发布清单一致；实际前端路径和首页摘要也与发布包匹配。管理 API 返回 `backend=powerpoint_bps`、`profile=powerpoint`、`codex_fallback=false`，WebSocket 已启用。

| 线上复测 | 结果 |
| --- | --- |
| 存活、就绪、前端首页 | 通过，实际进程和前端文件匹配发布清单 |
| 实时模型目录 | 6个模型，包含 `gpt-6-astra` |
| HTTP SSE，`gpt-6-astra/xhigh` | 收到真实文本delta与成功终态，回复符合验证标记 |
| 原生compact → 继续生成 | 原生加密compaction返回正常，下一轮准确恢复随机标记 |
| WebSocket预热 → 增量续聊 | 连接成功；`previous_response_id` 与 `stream_id` 正确续接，恢复随机标记 |
| 状态与数据 | 发布状态active；2条可见账号的身份/启用状态一致，设置版本仍为4，环境文件摘要与5条SQL迁移checksum不变 |

证据保存在 `.local/powerpoint-release/26f88f12bbb9ef7627e6bfe4e3f06653b21c3206503c28ce0fd600b13ef21edc/`：`activate.json`、`status.json`、`production-smoke.jsonl`、`production-smoke-result.json`、`production-final.json`。烟测退出码0，无stderr。测试用管理员凭据仅在服务器内读取，不返回工作站。

首次候选发布因执行器拒绝 systemd `StateDirectory` 的父目录软链接而未通过健康校验，自动恢复旧服务成功，未恢复或覆盖数据库。修复专用只读凭据路径校验，并增加失败阶段记录及回归测试后，发布上述最终版本。失败候选 `4ef3bd50…` 的回滚记录保留供追溯。

以上上线结论依据当时的运行进程和真实请求；后续变更应重新读取状态与复测，不能只依赖本静态文档。

## 使用边界

真实功能已经通过，但不能把它描述成上游协议完全等价。BPS不接受的非默认服务档位、后台生成、结构化输出等被明确拒绝；会话缓存有TTL/容量限制；BPS办公配置仍存在。标准客户端工具、搜索和压缩走已验证路径，用户实际模型权限以实时账号目录为准。

部分 `include`、`text.verbosity` 和非 `effort` 的 reasoning 控制仍存在适配差异，局部字段保留不代表原上游语义完全等价。

没有任何单题能够保证账号长期“满血”。本次确认的是指定账号、模型、推理档位和调用路径下的实际成功结果。

维护入口：[架构与边界](architecture/POWERPOINT-GATEWAY.md)、[目录迁移](architecture/STRUCTURE-MIGRATION.md)、[独立审查](architecture/BPS-REVIEW.md)、[工具索引](../tools/README.md)。
