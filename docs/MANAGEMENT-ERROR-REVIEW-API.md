# 使用记录错误处理状态

2026-09-18 后续增量，尚未部署。参考本地 Sub2API `frontend/src/api/admin/ops.ts` 的 `resolved/resolved_at/resolved_by_user_id` 与 `updateErrorResolved`。这是独立的管理员处理标记，不把历史失败改成成功，不重新发送请求、不恢复账号、不影响计量。

## 接口

| 路由 | 行为 |
| --- | --- |
| `GET /api/logs/{id}/review` | 按当前记录 ID 或 EventID 读取状态；默认未处理、version=0。 |
| `PUT /api/logs/{id}/review` | `{version,resolved,note}`；版本及布尔值必须显式提供，备注最多1000个Unicode字符。 |
| `GET /api/logs/errors` | 当前留存失败/中断记录的独立列表；通用日志过滤和排序，另有 `resolved=true/false`，省略为全部。 |

详情响应为 `{review,record,scope:"management_review"}`。`review` 包括 `record_ref/version/resolved/resolved_at/resolved_by/updated_at/updated_by/note/history/history_total/history_truncated`；时间为 Unix 秒。`record` 只含 ID、EventID、时间、账号 ID、模型和原始状态。

没有终端用户域，因此操作者以管理员会话/认证指纹记录，不冒称真实用户姓名。备注采用密钥/JWT/带密码代理地址脱敏，正常Unicode备注不会被日志错误的2000字节上限静默裁剪；浏览器只作为文本渲染。

详情更新采用 CAS。过期版本返回409，保留草稿、显式重新读取后核对；不存在或已经清理的记录返回404；成功请求拒绝错误处理写入。取消编辑不会保存。重新打开为未处理时清除当前 `resolved_at/by`，原处理过程保留在历史里。

错误列表与主使用记录的统计快照分开：固定业务结果为 `status=failed`，拒绝外部 `as_of` 和矛盾状态；`total/total_kind/has_more/next_cursor/page/page_size/checked_at` 明确当前查询事实。处理状态筛选发生变更后，旧游标返回409 `review_query_changed`，需要回第一页刷新。返回列表不包含具体历史内容，详情最多显示最近50次，累计历史数和截断标志分别提供。

## 持久与一致性

状态存放于已有加密 `management/usage-review` 文档，不新增迁移。PG 的状态、版本、处理历史与 `usage_review.update` 审计同事务；审计失败不允许保存成功。文件模式状态与有限历史同一私有原子文件，跨进程锁与 CAS 仍生效。

新记录按稳定 EventID 绑定，旧文件无 EventID 时按去行号、统一脱敏后的记录指纹绑定，避免 prune 后新的记录占用旧行号而继承错误状态。请求明细清理不改处理历史或累计事件；已删除请求不会继续出现在留存错误列表中，也不能再通过其旧行号修改状态。

当只筛已处理/未处理时，PG 先把明确已处理事件的标识作为参数化条件，由SQL过滤再分页计数；文件模式使用同一已处理集合做二分查找。状态说明和列表一致读取当前已保存管理文档，后续他人更新会由下次刷新体现，不把它称为永远固定的处理状态快照。

## 验证

`TestManagedUsageErrorReview` 与 `TestPGManagedUsageErrorReview` 验证默认状态、HTTP200业务失败、稳定标识、处理/重新打开、多个操作者竞争、旧版本/旧游标、输入边界、重新创建Proxy后仍可读取、审计失败回滚和累计事件不变。`TestManagedUsageReviewHistoryAndLegacyIdentity` 验证历史上限、1000字中文、旧文件行号变化与错误脱敏后的相同绑定。

真实隔离PG执行记录：`.local/management-parity/followup-review-pricing-pg.jsonl`；这是新增三个专项的成功日志，不将它代替最终联合冻结验证。
