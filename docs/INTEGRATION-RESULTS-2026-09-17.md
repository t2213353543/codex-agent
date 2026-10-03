# Codex Agent 集成验收记录 · 2026-09-17

最终 PG 版本状态：**测试通过；生产已迁移；新版已部署**。统一验收 46 项通过、0 失败、0 阻断，Linux race 190 个通过事件、0 跳过，最终 22 项迁移/API 故障验收通过。发布 `07d3784ae9502b4260b6e9c0c6b413fca439ee80b13bf682f156595d4922bf06` 于 2026-09-17 18:42:53（Asia/Shanghai）切换，停写 0.692 秒；18:45 root/live/ready 均 200，公网真实浏览器和 PG 写入/实际重启恢复通过。详见 `docs/RELEASE-2026-09-17.json`、`docs/PERFORMANCE-FINAL-2026-09-17.md`。随后提出的前端视觉、统计卡片和账号字段恢复已于 19:15:40 单独验收上线，详见 [UI-ACCEPTANCE-2026-09-17.md](UI-ACCEPTANCE-2026-09-17.md)。本次只更换前端，没有重复迁移数据库。

下文保留早期中间候选及断网时的历史记录，以便审计；其中未完成/旧性能数值不代表上述最终发布状态。

## 已执行的验收

| 检查 | 实际结果 | 证据 |
| --- | --- | --- |
| PG/Redis 隔离 | 独立容器、数据库/非超级用户；Redis ACL 拒绝其他前缀和 FLUSHALL；回环监听、CPU/内存/PID 上限通过 | `.local/integration/isolation.json` |
| 同机真实 API 与迁移故障 | 22 项通过：JSON/JSONL/token、JWT 碰撞、显式替换、409、会话、脱敏、解绑事务、轮换恢复、Redis 故障、代理 403/407/429、时区/排序、重启/导出 | `.local/integration/remote-migration/20260917T094252/report.json` |
| 前端 | 构建、30 单测、19 组管理/布局 + 9 组图表测试通过；128 张合成数据截图 | `frontend/FRONTEND-HANDOFF.md`、`frontend/test-results/screenshots.html` |
| Go 本地 | 纯单测与 vet 通过；21 项数据库/Linux 条件测试跳过，不计为完整通过；本机 CGO=0 且未找到 C 编译器 | `.local/acceptance/20260917T100400-19228/` |
| 真实页面与 API | 未授权跳登录、真实 HttpOnly 会话和账号列表已观测成功；整套页面联调尚未通过 | `.local/integration/browser-real/report.json` |
| 部署状态机 | 脚本、私有配置、专用 rehearsal 数据库已准备；真实停写/启动失败/回退演练未执行 | `deploy/release_remote.py`、`tools/rehearsal_env.py` |

22 项真实验收运行于候选 `1b1b7012bfaeccf99e978cfdeb1056cd45532ad40755d43a503c532eedb08841`。之后后端新增 PG 刷新中客户端断开与 SIGTERM 子进程测试、settings/key 乐观锁完善等；最终冻结源码必须再次完成同机验收和 Linux race。

账号身份边界：账号内部有独立稳定 ID；完整令牌指纹去重；同名冲突明确拒绝、显式替换更新原 ID。未承诺自动将跨名称、不同 token 的同一上游用户合并，不能把共享工作区 ID 当个人身份唯一键。

## 性能实测

合成规模：1,000 账号、100,000 请求明细/持久事件、92,057 日聚合，跨约 120 日。应用与独立 PG/Redis 同机；应用容器 1 CPU、512 MiB。每路径每档 30 样本，12 路径 × 2 并发档，总计 720 次 HTTP 200，真实 cookie 认证。样本不含 SSH 往返。

| 路径 | 并发 1 P95 | 并发 4 P95 |
| --- | ---: | ---: |
| 账号列表 50 条 | 7.277 ms | 71.729 ms |
| 账号列表 500 条 | 74.897 ms | 231.282 ms |
| 日志 50 条 | 1.494 ms | 3.497 ms |
| 缓存概览 | 1.308 ms | 2.623 ms |
| 30 日概览 | 116.275 ms | 405.893 ms |

完整 P50/P95、响应大小、样本和执行计划见 `integration/performance/results/http-server-c1-session.json`、`http-server-c4-session.json`、`explain.json`。二进制为 `9be0b2156bf522a52eabf36b0f2762fd9332c6480f34916c2b841b1a907c074e`，不是最终发布版本。未设定或声称生产 SLO。

EXPLAIN (ANALYZE, BUFFERS) 确认修复前 500 账号统计使用 OR 名称兼容条件产生 500 次大量过滤，2,028.685 ms；后端改为稳定 ID 批量聚合后 26.952 ms。50 账号从 194.995 ms 降至 2.334 ms；日志关键路径约 0.29–0.37 ms。未安装 pg_stat_statements，查询数量的 N+1 检查标为源码审查，不冒充计数器测量。

实际 `prune-logs` 删除 23,298 条超过 92 日的明细，剩余 76,702 条；100,000 持久事件、139,900,000 累计 token 及 usage_totals 九字段在清理和实际 `rebuild-usage` 前后不变。清理约 212 ms，重建约 2.35–2.47 s；证据 `integration/performance/results/retention.json`。该报告的 minute/daily SUM 列因测试适配器同名 JSON key 折叠，只保留最后一项，不能证明分钟/日九字段完整一致。工具已补唯一列别名，并准备只读 `python integration/performance/remote_candidate.py retention-check` 逐字段、逐时间桶对照持久事件；尚未执行，不将旧报告的全量聚合通过标记用于放行。

前端 JS gzip 从 791.6 kB 降至 470.2 kB，约减少 40.6%；CSS 原始从 382.5 kB 降至 252.4 kB。构建没有通过提高 warning 阈值掩盖体积。真实首屏资源/整页行为仍需完成真实 API 页面联调。

## 当前停点

本机 WLAN 的 IPv4 地址变为 169.254.*，默认路由消失，直连主节点 SSH 报 `WinError 10051`。一次 DHCP 续租失败：`A device attached to the system is not functioning.`。未修改网卡固定配置、生产服务或其他项目。此环境故障中断了最终 Linux race、真实页面、部署演练和最终发布。

新候选包已本地生成：`.local/releases/9fb65a753ec7bd1738a122b1e8a48189f708ee41bdf6f9675cdb2b2cbc666f5a/release-manifest.json`，明确 `tests_passed=false,migrated=false,deployed=false`。后续工具修订会使该候选过期；最终发布必须重新 prepare，不复用旧验收哈希。

## 恢复执行

联网恢复后先核对直连主节点与测试容器状态，重建 SSH 隧道；统一验收入口为：

```powershell
python tools/acceptance.py
```

此命令保留未具备证据的 blocked 项，不自动部署。完整发布模式、证据合并与部署/回退命令见 `docs/MIGRATION-DEPLOYMENT.md`。服务器侧最终验收使用 `tools/run_linux_checks.py` 和 `tools/run_remote_integration.py`，演练使用 `release_remote.py --rehearsal`。只有最终 `release_ready=true` 报告与候选哈希一致，`tools/deploy.py provision/preflight/cutover` 才允许进入生产阶段。

## 现网与回退

旧版地址：`http://43.225.196.170:18879`。最后成功只读核验为 2026-09-17 09:14:43 UTC：service active，`/health` 与 `/` HTTP 200，运行中及磁盘二进制 SHA256 同为 `96e708aff1430d24a6efa0a584e6f4089a5e03868e6dfd8cc8e35149a8d07aed`。断网后不能声称重新核验了当前健康。

当前没有生产切换，无需执行回退。将来切换后的实际回退入口：`python tools/deploy.py rollback --release <该次本地release-manifest.json>`。它会先停止新版，从 PG 导出最新凭据，再恢复旧服务；导出失败保持停止，不拿旧快照覆盖新令牌。该路径仍须先通过真实隔离演练。
