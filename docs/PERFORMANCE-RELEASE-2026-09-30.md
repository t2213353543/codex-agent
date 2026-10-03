# 管理接口性能发布（2026-09-30）

用户要求清理本轮测试现场并上线当前优化。发布范围为独立 Codex Agent，公开入口 `43.225.196.170:18879`，使用现有稳定 ingress；不操作同机 Sub2API 服务。

## 产物与验证

- Release ID：`68137d6584193005e6d32453ef182ccc3516ebd6bf3d8add102b49b00328e3fa`。
- 完整源码 SHA256：`84c9b43cb8623596f9e6c3d388bbf018d23308bd8e30fe2b63257d2b751a9b08`。
- 本地包：`.local/performance-release/packages/<release-id>/package/manifest.json`；文件、前端、21 份 SQL 及功能/性能证据均按哈希绑定。
- `go test ./... -count=1`、`go vet ./...` 通过。修复了测试夹具遗漏 `PreferWebsocket=true` 导致的 native startup 屏障超时；产品代码不涉及此修复。
- 隔离真实 PostgreSQL 定向回归报告 `.local/performance-phase6/pg-release-passed/report.json`：55 个通过记录、无跳过；覆盖 pipeline 独立提交、并发、取消、损坏凭据回退、选择性导出和统计重建。随后仅修改上述 test fixture，非测试 Go 源码相同。
- 前端 237 项测试和生产构建通过；发布工具及控制器 80 项测试通过。
- 当前依赖保留 pgx v5.7.6；没有发布未完成的依赖升级。

## 性能边界

最新 10k 导入：group 894.131ms，proxy 1186.409ms，mixed 1065.547ms；每组持久行数、审计数、独立事务数均为 10,000。报告 `.local/performance-slo/20260929t162335-e587ad/report.json`。

此前 10M uniform 与复杂过滤测试通过，源身份和完整结果在 `.local/performance-phase5/http-uniform10m-current2/report.json` 与 `http-complex-current2/report.json`。10M 指日志/事件，账号夹具为 1,000。尚未证明所有路由 <1s，proxy/mixed 10k 导入仍超 1s；release acceptance 明确记录 `slo_met=false`。

## 发布流程与清理

原发布器只接受 000–005 迁移；新增独立性能包校验器，旧发布分支保持原规则。先执行候选二进制显式 `migrate` 并核对 000–020 ledger 与权限，后由 `hot_release.py` 在新私有端口启动候选、校验共享状态和 readiness，再切换 ingress。迁移不删除业务行；汇总回填会短暂占用写锁。

生产预检：173,858 条历史日志的数字、布尔、groups 字段均可转换；migrator 拥有源表，runtime 默认已有新表 SELECT/INSERT/UPDATE/DELETE 权限；迁移后补齐新增汇总表 TRUNCATE 维护权限。

远端已清理 3 个旧合成测试容器、11 个 `perf_slo_*` / `perf_uniform_*` schema、166 个测试产物目录。测试库由 65,574,893,247B 降至 78,116,543B，测试文件另释放 6,035,566,694B。报告、日志和源码清单保留。

本地临时二进制删除被自动审批以 `blocked by policy` 拒绝，尚未删除；未改用其他方式绕过该拒绝。

生产迁移与切换结果以同一包目录内 `migration-status.json`、`activate.json`、`verify.json` 为准。


## 上线结果

- 生产迁移完成：新增 006–020，完整 21 项校验一致，耗时 1174.873s（历史汇总回填），新增汇总表权限验证通过。
- ingress 已切换到 `codex-agent-runtime-0ff890eb239bae2d947c3.service`，私有端口 `18892`，PID `3620985`，二进制 SHA256 `0ff890eb239bae2d947c300f80bea86f2e574cde2cbcc4dd30e6ec4cbf73d7c2`。发布时显示版本仍为 0.4.25，实际发布身份以此 SHA 和 release ID 为准。
- 公网 `/health/live`、`/health/ready`、`/` 均 200，首页 SHA 与本次包一致。
- 服务端 loopback 9 个管理请求通过，样本范围 1.108–47.580ms；这不是全路由 P95/P99 结论。
- 上线后新增请求已持久化：日志、事件、分钟总汇总、每日模型汇总、模型条件总汇总均为 173861；无 invalid index。账号出现外部新增，目前账号/凭据均为 4，审计有 2 次 account.import；本次发布没有执行账号导入。
- 新服务 `NRestarts=0`，错误级 journal 行数 0。
- 兼容保留：旧启动参数 `CODEX_NATIVE_WEBSOCKET=1`，此前未存储 prefer_websocket；通过候选 settings CAS 将该值显式设为 true，settings_version 27→28，前后差异仅这两项，inference_mode 保持 powerpoint_bps。
- 两个已验证排空的 shared 世代退出；legacy `codex-agent.service` 因缺少排空证明保留，不强制停止。
- 新增上传临时目录已清理；旧隔离 `codex_agent_performance` 测试数据库已删除。
- 打包后仅更新本地运维验证器 `tools/performance_rollout.py`，以服务器内存中的当前加密管理密钥执行复验，避免使用已轮换的旧 client-key 文件；不改变已发布二进制或不可变包。

复验命令：

```powershell
python tools/performance_rollout.py verify --release .local/performance-release/packages/68137d6584193005e6d32453ef182ccc3516ebd6bf3d8add102b49b00328e3fa/package/manifest.json
```
