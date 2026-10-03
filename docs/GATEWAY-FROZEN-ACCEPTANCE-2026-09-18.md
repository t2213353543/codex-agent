# 冻结候选网关验收：2026-09-18

当前被验候选为管理后续交付的不可变归档，不使用仍在修改的共享源码。

**后续新增已知缺陷：** 最小最新 compact timeout 回归确认此旧候选在“自定义直连 RoundTripper 的显式 compact client 总期限”分支失败；owner 单文件 overlay 已通过，但旧 binary 未更新。详见 `docs/COMPACT-TIMEOUT-REGRESSION-2026-09-18.md` 及 `.local/compact-timeout-regression/20260918T023719.775475Z/report.json`。因此本页较早的本地专项通过不能替代新回归，也不能将当前发布状态仅理解为缺少远端验收。

| 绑定对象 | SHA-256 |
| --- | --- |
| Go-only source | `5a818844f613ad35602ac466c80d8949a2e7f7a180e96ae21c4cbd1573ce6d22` |
| source.tar.gz | `52b8e1be2c67a85a787387fef156267d9e43e58b145a5c884f53137dad45cd11` |
| 候选 Linux binary | `6ea3efc086d510399fe149def6b3e56941462f5531f8875967a7ed0c43e28918` |

归档目录 `.local/management-parity/linux-race-20260917T214152-2d29dc/`。已审读同目录管理专项报告：真实隔离 PostgreSQL/Redis Linux-race **73 个顶层 / 124 个事件通过、0 fail、0 skip**。该证据原样复用，不重复执行这 73 组，也不把它当成未覆盖的网关 race。

## 本次结果

独立新增网关验收结果：`.local/frozen-gateway-race/20260918T022116-6f87b7/report.json`。

- 本地普通 Go 专项：选中 172 个顶层，**154 顶层 / 337 事件通过、0 失败、18 明确跳过、0 缺失**。
- 同一冻结源与 overlay 的 `go vet`：通过。
- 六项自定义 timeout pool、协议门禁、SSE/取消、被动观察、旧刷新持久化相关本地用例及五项环境故障测试均纳入选择；实际依赖 PG/Redis 的 17 项及 POSIX 进程退出项没有运行。
- 运行前后冻结源哈希一致；没有修改项目根 Go、SQL、前端、生产服务或配置，没有真实推理请求。
- 总状态为 **blocked**，不是联合发布通过：该快照网关专项的真实 PG/Redis 与 Linux race 仍未执行。Windows 当前没有可用 gcc/clang/cc，未安装额外编译器。

首个远端尝试仅在连接阶段失败，容器未启动：`.local/frozen-gateway-race/20260918T021921-d23c92/report.json`。本地 `17897` 无监听；收到停止恢复代理的明确边界后，未代起进程或换方式绕行。后续使用纯本地 `--local` 路径完成可行验证。

## 本地合成观测

| 场景 | 数据 |
| --- | --- |
| 64 慢请求，显式 8 连接上限 | 实际 8 连接、峰值 8、复用 56；P50 61.594 ms、P95 122.844 ms |
| 64 混合 429/502/503/RST | 上游共 64 次、每请求最大 1 次；429×16 / 502×32（含 RST）/ 503×16；Retry-After 保留；后续冷却请求本地拒绝 |
| 20 个活动/排队取消 | 20 个取消，恢复约 0.513 ms，后续请求成功 |
| 两账号代理路由 | 每 route 4 连接，0 身份头串用 |
| 创建 145 route | 池项 128；默认 MaxIdleConnsPerHost 已为 100；MaxConnsPerHost 仍为 0 |

这些是本地合成回归值，不能换算成真实模型并发容量或生产延迟。显式 8/4 连接上限只证明 Transport 保留测试配置；产品默认活动连接上限仍未设置，账号准入与池元数据上限是不同约束。

## 复跑入口

本地可运行且不会连接 SSH/PG/Redis：

```powershell
python integration/gateway_environment/frozen_gateway.py --local
```

返回 2 表示本地专项通过但必须的远端 gate 尚缺；返回 1 表示实际本地失败。完整 skip 名单及每个测试的 Go JSON 事件均保存在报告旁，不把 skip 计入 pass。

只检查归档与测试选择：

```powershell
python integration/gateway_environment/frozen_gateway.py --plan
```

待网络与操作前提恢复后，root 可安排不带 `--local` 的同入口，在既有受限 Linux 测试容器运行 race 与独立 PG/Redis；不在本次网络受阻时改用其他代理或恢复路径。远端工具只使用已有编译镜像、2 CPU / 2 GiB / 128 pids、独立 Redis prefix 和随机 schema；测试不调用生产账户或模型。

003 的独立迁移报告已通过，但精确旧 binary 回退兼容尚 blocked，见 `.local/migration-003-rollback/network-blocked.json`。旧 binary 只读复制与 SHA 已验证，旧进程未启动、没有写测试库或生产库；合成 schema/fixture 按 root 要求保留用于后续浏览器兼容检查。这些状态不能合并为“已迁移”“已部署”。
