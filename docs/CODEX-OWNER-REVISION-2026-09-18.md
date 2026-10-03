# 持久化 Owner 代次

本轮仅本地实现和验证，不再进行用户已取消的真实账号测试，没有生产请求或部署。目标是修复已证实的 A -> B -> A 会话复活：正常 Import/reauthorize 恢复相同凭据后，旧摘要和用户/workspace 又相同，旧线程可能重新获得授权。修复前的 JSON 实际 Store API 反例保存在 `OWNER-REVISION-ABA-RED-2026-09-18.json`。

## 实现

每个新增账号和每次实际 owner 变化生成一个新的 128-bit 随机 `OwnerRevision`。这是内部 incarnation 标识，不是递增数字、不是 CLI 的 wire 字段，也不是令牌。已知完整 user/workspace 的正常 AT/RT 轮换保持原值；未知 owner 的 AT/RT/ClientID 变化生成新值。名字、备注、期限、管理版本、禁用/启用本身不当作 logout。

所有引用在原静态 owner / 未知凭据摘要后附加非空 revision。A -> B -> A 不再回到旧引用；删除后通过 legacy 路径重建相同管理 ID 也生成新 incarnation，不接受恢复文件携带的旧值。外部凭据中的 `owner_revision` 不被解析为内部状态。

持久化在现有统一写入口，不新增表或改旧迁移：

- PostgreSQL：revision 放在加密 accountSecret 中，由 `pgInsertAccount` / `pgWriteTokens` 生成、保留及写入；与原凭据 CAS/事务同提交。失败 CAS 或事务不提交新值。
- JSON：内部 accountState 的 `owner_revisions` map 按稳定 ID 保存；`Store.modify` 在回调前保留深独立身份快照，成功后比较并统一落盘。回调失败不写入，原地修改 Identity 指针也能检测。
- `Account` 的字段使用 `json:"-"`，常规 API/凭据导出不输出该值。旧数据没有 revision 时仍可读取，元数据修改及已知同 owner 刷新保持空值，首次真实 owner 变化才建立新值。
- JSON Refresh 返回提交后的精确 ID 账号，避免返回写入 revision 之前的快照。内部读取明确按 ID，不允许另一个账号的显示名遮蔽它。

已有 HTTP 线程和已连 WS 每次续用时，还会在调度缓存之外读取当前持久化 owner。账号缓存未失效且仍在五秒有效期内，也不能延迟新代次生效；已知同 owner 刷新则读取新凭据继续，不误断会话。

## 本地证据

测试通过真实 JSON Store 导入、替换、Refresh、ApplyAll、重开文件，以及隔离 PG/Redis 的 reauthorize CAS 和新 Proxy 实例，不是只比较内存结构：

- 已知与未知 owner A -> B -> A：旧线程、旧 WS 拒绝，原 Redis 绑定未被改写。
- 原地修改 Identity、回调回滚、同 owner 刷新、未知凭据不变的元数据刷新。
- 旧格式数据兼容、忽略伪 revision、相同 ID 删除重建、JSON 返回值和重新读取一致。
- 预热缓存后不手动 invalidate：HTTP 用新 AT 保持正常同 owner 轮换，身份 ABA 后不再发送；WS 后续轮被拒绝。
- PG stale-version reauthorize 不推进持久 revision；导入、管理账户合同及停机轮换测试一起回归。

最终验证源码目录 `.local/source-20260918/gateway-overlay-20260918t052835-6936e4/source`，SHA256 `57a7ded3c036485f44d7b96d8e659d2579cb05c15f49db10dc73ac6afe67ae90`。它以固定管理 5a818 为基线并纳入本轮最小 accounts 中央入口变更，不覆盖其它会话在途管理代码。具体测试和产物哈希见同名 JSON。

该快照全库测试、vet 与 Windows/Linux 构建通过。Linux 真实隔离 PG/Redis `-race` 263 顶层、560 通过事件，零失败/跳过/遗漏，108.847 秒；临时容器、网络及凭据已清理。配对现有 release helper 的 15 项本地联调全部通过，包括真实 CLI 程序面对合成上游的工具流程。Linux gateway binary SHA256 为 `4f452dca7161477ba95768917c25a7a225a091a0f480b6eb1790e5e6e70112c9`。

## 升级边界

旧 binary 读取加密 secret 或 JSON 状态后再写入时，会丢弃它不认识的 revision 字段：旧 PG 凭据更新会重建不含该字段的 secret；旧 JSON 的任意账户写入（包括元数据）会移除整个 owner_revisions map。因此不支持新旧版本同时写同一账号存储，也不能把回退旧 binary 后的后续写入声称继续保有此代次隔离。当前 schema 没有 writer 版本栅栏；启用必须先排空旧写入进程并统一所有 writer 版本，不能继续让旧 binary 读写已启用此状态的存储。回滚还必须处理相关旧会话失效，不能只换回程序。生产部署与回退需要联合管理/环境 owner 明确版本门禁；本轮没有切服务、迁移会话或操作账号数据。

本修复保护通过当前版本存储 API 发生的 owner 变化、凭据变更与重新创建，不拦截授权操作者直接编辑/回滚整个底层数据库或文件；没有持久痕迹的外部 logout 也不是已观察事件。禁用再启用只是管理调度动作，不能擅自当作 CLI logout。并发请求在最终校验之后发生的管理修改仍有正常在途边界，本轮不宣称全局锁住身份至请求完成。

release helper 沿用已验 0.155.0 包，本轮只更改 Go 身份与存储边界。真实模型效果、美国设备环境和生产部署不在本轮验证中。
