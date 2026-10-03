# 最终冻结性能验收补充

源码：`f54e9150b48d3e4503b16921d23955aca16ba7cbacdfabaf8eec6a8f5bf50369`；二进制：`07f958e065f8473e5a21c037d8527943cf71652e8b089f245a2c8aa91f673894`。结果替代 REVIEW.md 中旧候选的延迟数值；旧报告保存在 results/historical-9be0b215。生产迁移/部署状态由主任务独立记录。

1,000 合成账号、76,687 保留明细、100,000 持久事件，应用/PG/Redis 同机、应用1 CPU/512MiB；每路径30样本，12路径×2并发档，共720次HTTP200。真实cookie认证、新HTTP连接、nearest-rank百分位。race/演练完成后开始采样，无SSH往返，无生产SLO承诺。

| 场景 | C1 P50/P95 ms | C4 P50/P95 ms |
|---|---:|---:|
| accounts_50 | 6.992 / 8.837 | 11.575 / 71.502 |
| accounts_500 | 85.571 / 108.776 | 223.896 / 387.951 |
| accounts_filtered | 8.729 / 11.241 | 11.971 / 81.391 |
| logs_50 | 1.743 / 2.097 | 1.838 / 3.097 |
| logs_errors_50 | 1.798 / 1.936 | 2.002 / 3.435 |
| logs_account_50 | 1.559 / 2.026 | 2.523 / 5.331 |
| overview_cached | 1.006 / 1.125 | 3.162 / 5.699 |
| overview_1d | 68.979 / 86.466 | 312.017 / 504.599 |
| overview_7d | 73.943 / 89.502 | 307.344 / 593.836 |
| overview_30d | 104.847 / 176.741 | 405.669 / 690.138 |
| accounts_50_cursor | 7.525 / 8.69 | 10.035 / 73.148 |
| logs_50_cursor | 1.306 / 1.699 | 2.689 / 4.621 |

本轮实际 prune-logs 删除 1 条新跨92日边界的明细；事件100,000与token139,900,000不变。实际重建前/后grand/minute/daily九字段完全一致，retention-check按每个分钟/日桶与账号双向EXCEPT均 missing=0、extra=0。修补了旧候选适配器同名JSON key导致的证据缺口；旧原始报告未改写。

重建耗时 3865.453 / 3731.915 ms，清理 27.529 ms。SQL EXPLAIN 与同机其他测试曾并行，应以计划结构评价；最终HTTP采样避开了这些负载。

固定6次账号页业务SQL、无每页日志COUNT、游标分页、有界PG/Redis池、10秒默认概览缓存已审查。仍有密钥页按key查询统计N+1、概览全量读取账号元数据/关系、旧page最多100次连续查询；未安装pg_stat_statements，查询次数为静态审查。保留期不清理持久事件/分钟/日聚合，容量仍需监控。

完整结果与统一schema证据：integration/performance/results/final-evidence.json；每项status=pass，含artifact SHA256，所有最终报告均绑定本源码与二进制。
