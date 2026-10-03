# 附件网络尝试观测合同

**0.4.16已于2026-09-29 01:48:01（UTC+8）完成现场验收。** release `e145f1ba35394071be855149b9f4931dee4511a3d60ece0b59aa4190663fda65`，PID1911991、binary `70b42e593ba0e86d02c10ab625978988c356603a4f2256e5cb0f44ca8e609de4`。`/attachments`路由、转义路径producer与UI白名单已随同源正式Go、PG、race和完整实网验收发布。两条正常生成、一次top_p本地拒绝及三条精确CSV导出通过，账号与设置保持；现场没有额外制造上传失败或账号绑定错误。[上线核对](../.local/collab-a-20260928/transport-release-review/postdeploy-review.json) · [关键包证据](../.local/collab-a-20260928/transport-release-review/final-source-package-review.json)

初始源码对比固定于01:38:54，保留了相对旧0.4.15三处新增观测的[指纹回执](../.local/collab-a-20260928/attachment-doc-review/source.json)，不把旧版自然错误归因到新版。

BPS 的 schema 2 `attempts` 是实际网络账本。附件上传沿用 `phase=request`（HTTP 请求派发），**不等于模型 generation**；`endpoint=/attachments` 与 `/responses`、`/responses/compact` 区分用途。OAuth 是 `refresh`，不会因上传或外层 handler 再虚增一次 generation。缓存命中、锁等待和本地凭据重读不产生网络尝试；共享上传的等待方也不能凭等待推断自己发起了上传。

只发生附件上传、尚未派发 generation 的记录，`SentModel`、`ReturnedModel`、`UpstreamPath` 为空；原入站 model/endpoint 仍按自己的来源保存，不能用它们填充发送字段。附件 HTTP 429/401 属于上传结果；不能把附件-only 失败填成 `FinalUpstreamErrorCode/Source` 的 generation 失败，也不能凭 HTTP 状态添加额度耗尽原因。已知零值与未观测值分别保留：真实零耗时有效，未收到响应头时 HTTP/header timing 为 unknown；附件-only 无模型 usage，不能设成已知零。原账号身份/凭据版本等字段继续采用实际派发来源，本补丁不改其语义。

账本最多保留 **前 4 项**，后续真实尝试只将 `attempts_truncated` 置 true，不把后续状态覆盖进前四条。`observed_http_status_code` 来自实际响应头，`status_code` 保留逻辑/协议结果合同；二者不能互相代替。最终 generation 的有限机器错误来源独立于截断数组，不能从“最后保存项”猜测。既有本地 loopback 补证：

| 实际顺序 | 保存结果 |
|---|---|
| 4 次上传 200 → generation 429 | 前四条仍为上传 200；truncated=true；独立 final generation 错误保留 |
| 第 4 次上传 429 → 换 owner 后上传及 generation 成功 | 原上传 429 不被成功覆盖；最终 generation 错误字段清空 |
| 4 次上传 200 → 第 5 次上传 429，generation=0 | 前四条仍 200、truncated=true；发送模型/路径和 final generation 错误仍空 |

因此截断记录不能恢复任意请求的完整网络次数。上述 3 项补证及日志见 [A ledger](../.local/collab-a-20260928/attachment-ledger-review/REPORT.md)。

路由只保留规范固定值：识别 `/v1`、`/backend-api/codex`、`/basispoints/api` 前缀并允许 `/attachments`。结构化 URL 解析不保存 host、userinfo、query、fragment、file ID 或任意路径。真实 producer 现传 `r.URL.EscapedPath()`，避免先解码 `/basispoints%2Fapi/attachments` 后误标成合法固定端点；这只修观测，不改真实 URL、上传内容或发送次数。旧 backend 转义前缀也有同类既有问题，不将其描述为新发送漏洞。[A route](../.local/collab-a-20260928/attachment-route-review/REPORT.md) 的真实 callback 链补证为 4 顶层/24 子项通过；当前 route/producer 规范化文本与受测候选一致。

当前 UI **全局 `observedPathOptions` 包含 `/attachments`**，同时用于 attempt 端点、顶层入站/上游路径显示及 Logs 路径筛选选项；不能声称合入的是 A 的 attempt-only 两行实现。全局选项也不意味着后端有 attempt.endpoint 查询筛选，或附件-only 的 `UpstreamPath` 会被填成 `/attachments`。A 的[5 组 helper/实际 SFC SSR 证据](../.local/collab-a-20260928/attachment-ui-candidate/README.md)属于此前私有 attempt-only 候选，不能替代当前 main 的完整前端/浏览器门禁。

[root 上传换号证据](../.local/stream-options-release-20260929/attachment-failover/REPORT.md)已覆盖完整 plaintext + data URI 换授权 owner 重上传，以及 file_id/加密状态保持 owner 绑定；属于 2 顶层/16 子场景的合成 loopback。本页所述端点观察补丁只改善观测和展示，不改变这些资格、缓存归属、排序、并发或冷却设置，也不证明自然上传错误已经消失。

主任务同版另有共享上传取消恢复修复：健康等待者遇共享上传返回context取消/超时类错误后可重新竞选一次，HTTP401/429/503不走该重入分支。触发范围不限于leader父请求被客户取消；独立本地上传测试也覆盖父ctx健康但HTTP响应头等待超时的路径。它是独立行为变更，应以主任务相应源码及[C范围实测](../.local/coordination-20260928/team-c/phase28/attachment-retry-boundary/run-20260928T174457.053786Z/test.log)证明，不将本页端点显示检查作为其验收。本次文档复核新增测试、SQL、模型调用均为0。

后续 **0.4.17 已于 2026-09-29 02:24 完成上线验收**：release `49c77636fd630acd…`、source `0636f98e359a4a3d…`、PID 2058745、binary `4cf31b8c…1f9ad3`，见[实际部署核验](../.local/collab-a-20260928/session-release-review/postdeploy-review.json)。该版另加入 TTL 清理优化：完整过期扫描改为最多每分钟一次机会清理，每个被查询项仍即时检查自身到期；命中不续 TTL，没有新增容量上限或后台清理线程。未访问的过期项可能等到后续清理访问才删除，但不能作为有效 fileID 返回。[A 同 fixture 前后语义对照](../.local/collab-a-20260928/attachment-expiry-review/REPORT.md)已验证固定 TTL、多 key 隔离及过期后同 key 并发一上传；受测附件源码 SHA `7c265cac…` 已与实际发布包绑定。普通现场验收不证明真实 TTL 已到期；命中微基准不代表端到端延迟，全表清理的那次访问仍为 O(N)。
