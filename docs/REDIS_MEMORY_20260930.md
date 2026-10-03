# Redis 续聊缓存及磁盘回收（2026-09-30）

线上已采用与 Sub2API 推理运行时一致的有限续聊缓存：最多1小时、512 MiB正文字节、32,768条记录；归属证明最多1小时、65,536条。大请求仍可正常处理，超出缓存预算的单条结果不保留正文，但保留归属证明，客户端可携带完整上下文续聊。过期或容量淘汰后，previous_response_id返回既有的历史不可用提示。

不再永久保存每轮完整对话，也不再向PostgreSQL写历史正文。旧CODEX_HISTORY_STORAGE=postgres不会重新开启永久归档。新Redis前缀codex-agent:production:bps:history:v2:的正文、索引、容量账本及归属证明都有TTL。容量由原子Lua同时执行过期清理、计数和字节淘汰。

## 根因

目标是本项目codex-agent-redis（127.0.0.1:26380），不是Sub2API的共享Redis。原代码把“大请求不限尺寸”同时用于“历史永久不淘汰”，每轮按response ID重复保存完整Request + Output。全扫描194,258条永久历史，键内存合计77.24GB、最大单条约96MiB；allocator碎片只有约448KiB，问题是实存数据。

Sub2API普通转发主要保存1小时的response/account路由元数据；其私有推理运行时使用1小时、512MiB、32,768条续聊缓存，没有永久PG正文仓库。这里已区分请求尺寸与缓存保留期。

## 修复与回收

- 预热2,456条近期成功记录，按真实业务完成时间保留剩余TTL，正文约480MiB；原子NX不覆盖新流量，保留正确归属。
- 临时gateway_response_history表已TRUNCATE，实际从57,845,809,152字节降至16,384字节。账单、用量日志和账号数据不受影响。空表及026迁移身份仅保留schema兼容。
- 已移除172,981个旧版历史/归属键，保留v2键及其它业务状态。
- 10:40 UTC实测：Redis已分配657,618,168字节（约627MiB），主机已用约30GiB，可用磁盘约550GiB。所有历史键均有TTL，没有永久历史。
- 旧PG writer及其它旧代已正常退役，只有有限缓存版接收流量。
- 旧AOF/快照单独压缩，删除键不等于AOF立即缩小；最终压缩数字由维护证据记录。

最终维护已完成：旧AOF删除75,362,256,419字节，新AOF为576,372,977字节，最终RDB为576,525,015字节，整个Redis数据卷约1.1GiB。Redis运行及持久配置为maxmemory=2GiB、noeviction、AOF重写阈值100%且最小64MiB；独立root运维凭据仅用于INFO/CONFIG等维护操作，原业务用户ACL逐字保留。

容器另设4GiB内存上限（含持久化子进程余量），运行值及Compose同时验证；新实例引导配置也已更新，避免重新落回128MiB Redis/192MiB容器的旧配置。永久归档工具已禁止upload/start，只保留历史状态查询。

为阻止已有WebSocket继续送入新推理，维护期间先暂停入口进程消费新帧，待实际有效推理租约为0后才冻结应用并重建持久化；随后恢复应用、入口和请求。未停止其它业务。Redis8增量manifest含startoffset字段，检查器已按实际格式修正，完成确认后才删除旧AOF。所有临时冻结状态已经解除。

10:57 UTC最终检查：Redis容器约586.5MiB（波动到约0.6GiB）、主机已用29GiB、磁盘可用619GiB/893GiB（较用户催清理时480GiB增加约139GiB）。空历史表保持0行/16KiB，近5分钟会话存储错误0。维护后再次真实两轮续聊HTTP200/completed，TTL3600秒、正文预算内、PG无新增正文；12项页面和API检查通过。

## 发布与验证

最终缓存release：
afec9ae0ba677ba6c7d4245e0a0d48c8bc199b7c155d93c3fd2f848fb548745d

运行二进制SHA256：
5f915b60e312c8868a9700c36eb27c8ddf7ce4bad2852285fe0f70646e68bd35

以逐个匹配线上1,547个源码文件和55个前端产物的ws-release-tree为基线，在.local/redis-memory-20260930/release-source隔离构建。主工作区其它未发布修改没有上线。

最终38项真实Linux PG/Redis用例全部通过、零跳过，涵盖三模式TTL、新实例续聊、32MiB以上请求、字节/条数预算、归属冲突与过期、预热剩余期限/NX、旧格式截止、零PG历史写入。go vet及最终相关Go回归通过。全量Go回归只有一个原有HTTPHop的5秒超时，针对该项连同历史用例复跑通过，保留原始失败日志。

线上真实两轮续聊HTTP200/completed，TTL3600秒、总正文小于512MiB，PG无新增正文。旧格式临时兼容于10:39:29 UTC截止，不会按请求延长。

## 复查

项目根目录执行：python tools/redis_memory_audit.py --sample 5000

仅读取元信息，不输出提示词、正文或密钥。MEMORY STATS可用；业务用户INFO/CONFIG权限不扩大。不要重新启动早期redis-history-archive永久归档任务；它已按用户要求退役。

详细证据在.local/redis-memory-20260930/；有限缓存测试报告为.local/redis-history-storage-test/20260930T102845.362757Z/report.json。

主机其余内存主要是Sub2API PostgreSQL约8.8GiB、8个WARP约7.5GiB及其它运行服务。PostgreSQL共享映射RSS不能直接相加；PSS调查没有发现大量闲置推理worker。未清空Linux页缓存或停止其它业务来降低显示数字。
