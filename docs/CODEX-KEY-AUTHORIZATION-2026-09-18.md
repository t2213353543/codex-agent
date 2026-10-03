# 普通 Key 的当前存储授权

本轮修复普通网关 key 的跨实例旧索引授权问题。只在本地使用合成账户、加密测试文档及 loopback 服务，不使用真实账号、不部署、不重建 Rust helper。

## 已复现问题

管理端 KeyStore.Update/Delete 只刷新自己的索引。另一个 KeyStore 即使读取同一个 JSON 文件或 PostgreSQL 文档，仍可能长期用旧索引接受已经停用、删除或轮换的 token，或使用已撤销的分组权限。修复前 JSON 双实例的身份、HTTP 和已连接 WS 三组测试均失败；测试明确确认 reader 索引没有被刷新。

## 实现

新增 `KeyStore.LookupCurrent(ctx)` 从当前存储读取 `keyState`，PG 继续使用现有加密 `gateway_keys` 文档，不新增明文索引、迁移或 Redis 广播。文件读取使用现有存储 API。读取失败、损坏文档或匹配身份歧义不回退旧索引；返回的是分离的当前 key/Groups，不改管理索引和 LastUsed。

`resolveGatewayKeyContext` 在同一授权检查点一次解析身份与分组。入口、账户候选选择、`ensureReady` 前后授权以及 WS 后续业务轮次使用此快照，不再识别当前 key 后读取缓存 Groups，也不在这些检查点之间缓存授权结果。已有管理 Lookup 与 CRUD 保留。HTTP 401 恢复后原有网络重试路径本轮未新增授权检查点，不把它混称为已经覆盖的 `ensureReady` 后检查。

普通 key 不存在、停用、删除或旧 token 被轮换后，HTTP 入口仍为 401；合法 key 被移出账户允许组仍走既有选择/授权拒绝。普通 key 存储错误在入口为固定 503，在已连接 WS 后续轮次也为 503，而不是伪造无效凭据 401/权限 403。入口错误单独记录有限原因 `key_store_unavailable`，不进入业务用量；真正的客户端 context 取消不记成该存储错误。

管理员分支的原先比较顺序和错误处理保留，不据此宣称所有管理员或设置存储故障都统一返回 503。读取是授权检查点，不是与整个网络发送原子绑定，也不会撤回已经发送的模型请求或迁移旧会话。

## 开销与边界

PG key 文档读取有继承调用 context 的 3 秒上限；超时或损坏不会改用索引。此期限不包含前置 Settings/Admin 读取，文件锁与文件读取也不获得可强行中断的保证。

每个检查点读取完整 key 文档并解密、解析和扫描，成本为 O(keys)。单候选 HTTP 通常有四次普通 key 读取，发生 `ensureReady` 刷新时前后检查仍在这四次之内；已连接 WS 每轮通常两次。多候选或重新选路可增加次数，原 Settings/Admin 读取另计。本轮优先保证撤权可见，没有声称数据库吞吐或生产延迟不变。后续优化不能重新引入可放行已撤权 token 的旧缓存。

账户候选发现与排序仍保留其原调度缓存；本轮改变普通 key 的权威读取，不把全部账户元数据、授权、刷新与发送称为原子强一致系统。

## 验证范围

- 独立 JSON KeyStore writer 停用、删除、轮换、改组，reader 索引保持旧值但身份/HTTP/WS 下一轮拒绝；既有绑定不迁移。
- 新 key 不在 reader 索引也能使用，轮换旧 token 拒绝、新 token 生效；修改返回 Groups 不污染后续读取或管理索引。
- JSON 损坏时 HTTP 503、WS 后续错误帧 503，零新增上游请求；入口事件可按新原因查询，不新增业务记录。
- PG 独立 reader/writer 的四种修改、新 token/新 key、实际密文损坏，以及独立单连接池阻塞读取后取消。测试不靠刷新旧索引来掩盖问题。

前端原严格校验不认识新原因，会拒绝整页准入事件，已先复现。修正为新增 `key_store_unavailable` 标签与枚举，保留对未知原因的拒绝；随后实际 320px 截图发现下拉标签裁切，仅将窄屏原因字段跨两列显示，其它管理功能不变。旧静态资产不包含新枚举，未来启用新后端时须配套新前端；本轮不覆盖任何旧发布目录或预览。

## 固定来源

- Go：`5c9f17493b4cc1c8545b45e7c886201753c4513485865afc1b98532e42834cd5`。
- 快照：`.local/source-20260918/gateway-delta-20260918t095018-28debc/source`。
- 基线：已验 b07d3bc3，保留管理完整被动观测 d52 及所有此前网关修复。
- Helper：`6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`，未改变。

同一源码通过 27 项 native 顶层测试加 1 项 Go CLI 顶层测试（3 子场景），共 28 项、0 跳过；六份必需场景报告全部存在且通过，容器已清理。报告 `.local/native-transport-build/gateway-compare-20260918T095033-4efaeb/result.json`。这些 CLI 场景验证兼容性，普通 key 撤权本身由独立 JSON/PG 场景证明。

全库测试、`go vet` 及 Windows/Linux 构建通过。管理与网关联合 Linux PG16/Redis7 `-race`：468 项顶层测试、1143 个事件全部通过，0 失败/跳过/缺失，176.109 秒；343 网关项与 131 管理项去重合并。四个新增 PG 顶层测试均真实执行，包括阻塞 key 文档读取取消。报告 `.local/management-parity/local-race-20260918t095317-1fc658/report.json`。

临时容器、网络、编译镜像和测试凭据均独立确认清理。Linux 二进制 SHA256：`e726f80b02a224e5a6020236133b82510ebfd1dcf83491bfde2685554cd9b04f`。

前端 Node 162 项全部通过，其中准入原因 5 项通过。独立构建在 1440/390/320 三种 viewport 通过真实浏览器验证：503 显示、原因筛选 GET 重取、无校验错误、无横向溢出、标签宽度足够；三张截图已目视核对。所有 API 均本地 mock，无外部请求，临时服务已关闭。报告 `.local/gateway-key-auth-ui/20260918T095804499Z/report.json`，构建在同目录 `dist`；先前裁切截图保留在 `20260918T095600628Z`。这不是已部署或已经和真实 API 联合运行的前端发布包。

## 复跑

```powershell
python tools/native_transport_compare.py --native-run 20260918T054243Z-b43907cb --source-dir .local/source-20260918/gateway-delta-20260918t095018-28debc/source --suite gateway --with-go-cli
```

所有推理回复和用量为本地合成数据，不证明真实账号成功率、缓存收益、美国出口或完整设备指纹。
