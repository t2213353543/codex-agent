# 28765 OAuth 设备绑定修复

2026-09-25 已部署 `20260925-oauth-device-binding-v1`，镜像 SHA256 `2433ec9df8ce5384c7abcf6ad585810ba39a87837dd831a8bb00592a5945d634`。线上健康检查返回 PostgreSQL 正常；调度开关与切换前一致。此修复独立于此前已上线的[新凭据仍显示旧 401 修复](CODEX-REAUTH-STATUS-FIX-2026-09-25.md)。

## 修复的实际问题

重新授权通过 `AuthFlow` 产生新的 OAuth 设备 ID，立即验证时使用该值；原 `update_registered_oauth_tokens` 只写 token，丢弃设备 ID。后续重新读取、补推前验证会退回浏览器会话设备，无法延续本次 OAuth 事务的设备身份。

直接覆盖原 `registered.device_id` 不正确：它与旧浏览器 `session_token`、cookie、CSRF 成套保存。独立运行真实 `AuthFlow.from_existing_credentials` 方法证明，单独覆盖会把旧 session 与新 OAuth 设备组合。该候选在上线前拒绝。

最终修复只改变三个源文件：

- `credential_utils.py`：选择与当前 AT 摘要匹配的独立 OAuth 设备绑定；硬件 profile 的设备目录键不作为认证 DID。
- `webui/db.py`：同一事务保存 token 与 `extra.oauth_device_id`、`oauth_device_token_sha256`，保留浏览器字段及其它 metadata。新 AT 无明确 DID 时不推断继承；同 AT 可保留已有绑定。损坏的历史 metadata 保留原字节，并继续保存新 token，跳过设备绑定、记录固定告警。
- `webui/team_rotation.py`：区分新 OAuth 返回与持久化数据库行；单飞等待后的上传路径只返回 token 和有绑定证据的 DID，防止验证正确后，二次保存又把浏览器 DID 写成 OAuth DID。旧数据的兼容回退不升级成已观测到的 OAuth 来源。

## 验证与发布

作者新增 16 项测试，独立存储和消费者 14 项，独立完整补推及导出链 16 项均通过。真实隔离 PostgreSQL 的 5 项测试覆盖原子回滚、损坏 metadata、设备与 AT 绑定、等待行锁后合并最新 metadata；合成 schema 已清理。

使用线上原版本的 208 文件源码清单构建，只替换上述三个文件，其余 205 文件保持原始字节。原 Dockerfile 完成前端构建和产物校验。新镜像在无网络、无生产卷条件下执行 6 个既有测试模块：77 项总计，66 项通过、11 项需要 PostgreSQL 的旧测试跳过；新增数据库行为另由上述真实 PostgreSQL 测试验证。完整生产模块导入正常。

原发布脚本确认相关 worker 全部空闲后切换，保留回滚镜像和发布回执。上线核验 207 个实际打包源文件哈希（`.gitignore` 不打包），服务健康，调度开关恢复。14:14:06 UTC 只读检查确认网关仍运行 `a52ad225…`，本次没有触发账号重新授权或模型请求，也没有批量改写已有账号设备。

复核入口：[发布与运行回执](../.local/research-20260925/account-capabilities-v19/source-environment/live-after.json)、[独立审核](../.local/research-20260925/account-capabilities-v19/source-environment/review/final-review.json)、[实际 PostgreSQL](../.local/research-20260925/account-capabilities-v19/source-environment/review/pg-results.json)、[镜像回归](../.local/research-20260925/account-capabilities-v19/source-environment/review/image-results.json)。

## 证据范围

该补丁修复源端 OAuth 设备保存及已审查的验证、补推链。CPA/Sub2API 导出与网关桥接不传递此 DID，不能将它描述成网关 HTTP/WSS 已增加设备字段，或账号错答已经恢复。历史未保存的 OAuth DID 无法凭空回填；RT 刷新未返回 DID 时不假设设备继承。行锁保护本次写入，不代表所有既有 metadata writer 已跨进程串行化。

下一处独立测试已证实的差异是：子号 OAuth 恢复其保存的 profile，而后续 Team 验证、RT 刷新和部分隐私请求选择了母号 profile。该问题在独立后续候选修复，不混入本版发布记录；它是否影响服务端账号状态或模型答案仍未证明。
