# 准入前独立错误事件

这是非计费的独立运维观测。仅记录尚未建立业务 `RequestRecord` 的固定早期拒绝，不写入 `request_logs`、`usage_events`、Token、请求累计或账号状态，不修改响应正文、响应头、HTTP 状态、路由或重试。

## 来源与存储

早期 hook 由网关 owner 接入；事件只接受以下状态与原因配对：401/`invalid_api_key`、503/`settings_unavailable`、405/`method_not_allowed`、404/`unknown_endpoint`、426/`websocket_disabled`。端点仅保留已知 Responses、compact、models、guardian 固定路径；任意原始路径、query、body、认证头、IP、账号和 KEY 均不进入事件。

字段为 `id`、持久递增 `sequence`、毫秒时间戳 `time_ms`、`http_code`、`reason`、可选 `endpoint`、可选 `client_request_id`、可选 `authentication_ms`。关联值仅接受 1–128 ASCII 字符 `[A-Za-z0-9._:-]`；无效值省略，不截成另一个 ID。明确 0 毫秒保留，缺失或负值不伪造为 0。关联来源由网关统一规范化，此接口不从 OAuth client_id 推算。

每个进程只有一个 worker，队列最多 256 项、worker 批次最多 256 项。拒绝路径不执行数据库/文件 I/O，不等待队列空位，不为每个事件创建 goroutine。满队列增加当前进程 `queue_dropped`。

worker 约每秒批量写 `management/ingress-events/recent`，PG 沿用加密管理文档与事务锁；文件模式使用可取消的进程/目录锁和原子替换。单存储最多保留最近 1000 项，跨进程序号及淘汰计数在同一更新内提交。不是按天完整保留，也不声称全部准入错误均已记录。

提交报错可能已成功或未成功，因此该批不自动重发，增加 `persistence_unconfirmed`，不计成确定丢弃。后续批次可以恢复；`last_error` 只有固定 `storage_unavailable`，不会返回原数据库/文件错误。常规成功清除该错误标志，但不清累计不确定计数。异常进程退出可能丢失内存队列，不声称持久 exactly-once。

## GET 接口

`GET /api/ingress-events` 受现有管理员认证保护，`Cache-Control: no-store`。只读；没有修改、删除、重放或推理接口。

参数：`from`/`to` 为 Unix 秒半开范围；`status` 为 400..599；`reason` 为上述五个固定枚举；`client_request_id` 精确匹配；`limit` 默认 50、1..200；`cursor` 为上次返回值。非法、重复及未知参数拒绝。

响应：

```json
{
  "items": [],
  "scope": "retained_ingress_events",
  "source": "gateway_ingress_v1",
  "total": 0,
  "total_kind": "exact",
  "has_more": false,
  "next_cursor": "",
  "checked_at": 1800000000,
  "retention_limit": 1000,
  "evicted_total": 0,
  "collector": {
    "configured": true,
    "running": true,
    "queue_capacity": 256,
    "queued": 0,
    "pending": 0,
    "queue_dropped": 0,
    "persistence_failures": 0,
    "persistence_unconfirmed": 0,
    "last_persisted_at": 0,
    "last_error": "",
    "unpersisted_at_shutdown": 0,
    "counter_scope": "current_process"
  }
}
```

按 sequence 降序返回。第一页固定 ceiling/floor、存储 epoch 与全部筛选及页大小；后续新事件不插入该游标集合。若淘汰已跨过最初 floor，或文档被重建，返回 409 `ingress_retention_changed`，要求重新查询；不会静默跳过已被淘汰的页。`total` 是这一留存集合内符合筛选的准确数量，不是业务请求总量。cursor 不是无限期快照。

`evicted_total` 属于持久文档累计，重建文档后重新开始；`collector` 仅是当前服务进程，重启清零，不能与集群全局事件数量混为一谈。未启动 sink 时 `configured:false`，仍可读取已配置存储内历史事件；完全未配置存储返回 503。存储不可读或损坏时沿用通用管理存储错误，不返回空成功结果。

## 生命周期与验证

`startGatewayIngressEvents()` 在处理 HTTP 前初始化并且拒绝重复启动/未配置存储。HTTP drain 与所有 handler 完成后调用 `Close(ctx)`；调用方提供 5 秒截止。关闭停止接受新事件并尝试排空，提交失败或未排完会返回错误，不谎称全部持久化。文件锁等待和 PG I/O 服从 context；操作系统文件系统调用本身不被宣称有硬实时保证。

本地 6 项专项通过：真实管理认证/留存/分页筛选/淘汰与重建冲突、生命周期与白名单隐私、256+256容量/模糊失败不重放/恢复、锁取消/并发文档更新、损坏存储拒绝、关闭失败/截止/最长值。`TestPGManagedIngressEvents` 对同样管理合同、PG事务存储及业务两表零新增提供独立 PG 验收入口；PG 和最终 race 结果由联合冻结报告记录，不把本地文件测试冒充 PG 通过。
