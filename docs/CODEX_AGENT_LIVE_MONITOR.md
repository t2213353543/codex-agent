# Codex Agent 只读运行监控

已安装独立 `codex-agent-observer.timer`，每 60 秒触发 `codex-agent-observer.service`。首次实际触发 2026-09-27 21:52:38 CST，退出码 0、`Result=success`，timer 为 enabled/active。仅观察 `codex-agent.service`、专属 `codex-agent-postgres` 的 `codex_agent` 数据库；应用端口 18879，与其他 Sub2API 服务分开。

脚本为 [codex_agent_live_monitor.py](../tools/codex_agent_live_monitor.py)。每次 PostgreSQL 连接强制 `default_transaction_read_only=on`；当前 `admin_settings` 只在服务器内用现有 decrypt helper 解密，立即按字段白名单输出。不读取旧 client-key，不解密或输出 admin key，不调用推理接口，不改账号、不自动重启或修复业务。

详细状态保存 `/var/lib/codex-agent-observer/latest.json`。每日 `YYYYMMDD.jsonl` 只追加聚合和新错误，不重复写六小时错误列表；只清理这个 observer 目录内超过七天的同格式日志。目录 0700、文件 0600。systemd unit 使用 `ProtectSystem=strict`，只有 observer 状态目录可写。

观测项包括当前运行 PID/二进制 SHA、settings version/mode、最近 1/20/60/360 分钟请求与错误、按 model/账号哈希/User-Agent/input token 区间分组的缓存、滚动 RPM/TPM、成功请求缺 usage 数量。缓存比例为 `sum(cached_tokens)/sum(input_tokens)`；没有分母时为 null。成功请求没有 usage 时 TPM 也为未知，同时保留 `reported_tpm`，不把它伪装为真实零用量。

只有明确 `serverOverloaded` 等协议 code 才归为上游繁忙，普通 502、模式绑定冲突、工具协议、图片、加密上下文、账号选择及其他错误继续保留。监控分组按实际 request_type，不把当前 BPS 设置倒推到历史 App Server 请求。

截至首次 timer 触发，线上 settings version 20、`powerpoint_bps`。当时最近二十分钟 BPS `http_sse` 174 请求/51 成功/123 错误，`http_json` 28 请求/10 成功/18 错误；61 个成功请求尚未报告 usage，缓存比例未知。切换后的主要故障是旧 session 的 `different inference mode` 409；此结果已交给主任务修复，不能用混入旧 App Server 流量的缓存百分比代表 BPS。

复跑一次（不安装 timer、不修改业务）：

```powershell
python tools/codex_agent_live_monitor.py --remote
```

检查分类逻辑：

```powershell
python -m unittest discover -s tools -p test_codex_agent_live_monitor.py -v
```

服务器查看状态：

```sh
systemctl list-timers codex-agent-observer.timer --no-pager
```

0.4.5 上线后的监控修订：上传脚本使用同目录临时文件后原子替换；timer 与手动采样用 `flock` 共享单一游标，快照使用唯一临时文件，避免并发写入冲突。运行时间由 `TZ=UTC systemctl show` 读取，避免 `CST` 缩写被解析成不同地区。报告新增 `since_process_start`、`since_process_start_types`、`errors_since_process_start`，发布结果不再混入旧进程窗口。原单次失败没有稳定复现，不能仅凭 stderr 长度断定其根因。

2026-09-27 22:27:35 CST 的真实 timer 触发已再次成功，退出码 0。新业务进程 PID 416736，启动于 14:21:32 UTC；截至 14:27:36 UTC 为 7 请求、2 成功、5 错误。两个成功请求均有真实 usage（输入 74401、输出 37、缓存 0），样本不足以判断目标缓存水平；五个错误为 3 次 usage-policy HTTP 403 和 2 次 unsupported reasoning effort `max` HTTP 400，均不归类为上游繁忙。此快照证据在 `.local/live-monitor/postdeploy-monitor-verified.json`。
