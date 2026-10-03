# 2026-09-28 生成 403 实测

当前生产 `codex-agent.service:18879` 为 0.4.6，PID 561146，二进制 SHA-256 `856fb63c634221576342e9d736397f1f2843f417253b4bd06a05499cee0569fd`。模式保持 `powerpoint_bps`，两条账号启用、并发各 50，未为诊断切换模式、刷新令牌或覆盖账号。

## 实际请求结果

9 月 28 日 04:46（Asia/Shanghai）使用实际 Go HTTP/2 客户端，直接向上游发送合成最小请求：单条短文本，没有工具、内置提示词、缓存键或加密历史。

| 对照 | HTTP | 耗时 | 上游 request ID |
| --- | ---: | ---: | --- |
| 账号 A，最小请求 | 403 | 323 ms | `d62a5dea-8075-4877-b362-7ad3d555c332` |
| 账号 B，最小请求 | 403 | 222 ms | `f959bdd7-a856-4f4a-83fd-ed2b104ca361` |
| 账号 A，同 body，仅平台字段改官方默认值 | 403 | 269 ms | `11262dfc-7511-4cf3-b631-61023d90d265` |

三次都是 `server_error`、`code=null`，错误正文 SHA-256 相同，未收到生成终态或用量。官方默认 `Client-Platform-Class=OfficeOnline` 已通过执行官方 bundle 的原函数确认；网关原来误用了 runtime 值 `web`。该字段已在本地修正并通过测试，**对照证明它单独不能恢复生成，因此没有为这个改动部署或重启服务。**

随后对同一账号、相同 body/header/token 摘要、相同 Go 二进制做双出口测试：`43.225.196.170` 和 `43.225.196.214` 均返回相同 403（288 ms、965 ms）。两机属于同网段和 LAX 区域，因此仅排除主节点单机特有故障，不能排除共同网络或上游规则。

## 账号与鉴权核查

两个 JWT 均未过期；存储账号标识与令牌一致。现有令牌的公开客户端标识为 Codex 的 `app_EMoamEEZ73f0CkXaXp7hrann`。官方插件使用 `app_fnr0pYvVwwFDocDumLG3H2Bp`，额外请求 `organization.read` scope，并通过 `/oauth/token?unified=true` 换取 token。

原生插件直接使用该 access token 作为 Bearer，没有额外的 BPS token exchange。其目录查询成功并不能证明生成权限正常。已有历史证明 Codex 令牌曾在此入口成功，所以不能只凭客户端差异断言这是根因，仍需同账号原生授权对照。

旧探针的 `credential_epoch=13/17` 实际是管理记录版本；真实凭据版本为 3/4。证据已保留原始文件并更正标签，不能用错误的版本字段推断 token 变更。

## 已修复的监控问题

原观察脚本把 `/models` 成功纳入生成用量缺失统计。现已排除模型目录和无生成预热，保留所有错误总数，并分别统计推理与控制请求。真实只读 PostgreSQL 夹具 8 项通过，脚本已原子同步，定时器继续每分钟运行；主服务未重启。

## 证据与复查

- `.local/latest-operations/bps403-current/runtime-readonly.json`：当前版本、账号及日志分布。
- `.local/latest-operations/bps403-current/jwt-readonly.json`：脱敏 JWT 声明与身份匹配。
- `.local/latest-operations/bps-minimal-policy/20260927T204611.242194Z/`：三次最小请求与单字段对照。
- `.local/latest-operations/bps-egress-policy/20260927T205229.202241Z/`：双出口逐项一致性检查。
- `.local/latest-operations/bps403-contract-audit.json`：官方 bundle 哈希、原函数执行与 OAuth 契约。

离线复查：`node tools/bps_official_contract_audit.mjs`。只读线上复查：`python tools/codex_agent_live_monitor.py --remote`。

**生成尚未恢复，403 的上游具体原因尚未确定。** 不能通过降低推理档位、重复换号、改写错误码或伪造 token 用量声称修复。
