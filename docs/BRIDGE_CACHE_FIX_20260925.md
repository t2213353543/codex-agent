# 28765 推送恢复与 Sub2API 缓存对齐

2026-09-25。本次以 `sub2api/current-source` commit `49783993f3b8d3df50a4a3da67d589d106c4ccae` 为缓存参考，不再运行缓存率对照或等待业务请求。

## 推送

排查时两个活跃子号均已有成功推送记录；真实私网接收接口和 Agent 账号均存在。近期业务日志曾出现 `no enabled account in the key's groups`，说明“已入库”并不等于“业务 Key 的分组里有账号”。两个接收器原先只固定评测组，轮换新子号不会继承额外业务组。

已新增并启用每个 workspace 的 `additional_group_ids`，使后续子号保留当前授权的业务组 `g_e96ca29b0ffd`；既有账号分组、Key 权限和 28765 源配置不变。所有必需分组齐全才向源端报告 active。

另已复现并修复接收端账号删除后的补推死循环：旧 receipt 指向已删除 ID 时，以同来源标签及稳定名称重新查找或导入，并核验实际 workspace/user/token。辅助 metadata 或 credential-export 的 404 不被当作账号已删除。撤下已删除账号可幂等完成，存在账号的撤下需确认凭据身份。

两个接收器已部署 SHA256 `21f7e27543ccfe3805f16d28c30318db4e0150ac893a03023c8e347f5dd215a6`。用 28765 当前活跃子号的已有 AT 经实际 multipart 接口重推，两路均 HTTP 200、active；实际存储 token SHA256 与源一致，无 RT 转交、OAuth 刷新或模型调用。验证回执：`.local/bridge-fix-20260925/deployed.json`。

轮换后，包含旧账号加密历史的会话不能直接改绑新账号；`session account is no longer enabled or allowed for this key; start a new thread` 继续要求客户端新建会话。

## 缓存

- 缺失 `prompt_cache_key` 保持缺失；已有原始 key 分别映射为租户、真实上游 owner、kind、原值的稳定命名空间，取消把不同会话压成账号公用 key 的行为。
- HTTP 和 WS 分别遵循 Sub2API 的原始会话头优先级；body 与嵌套 header 的 session/thread 一致；真实设备、installation、turn、window、request 字段保持不变。
- 没有显式会话时，用配置和首条用户输入建立本地内容粘性，TTL 不超过一小时；不把内容摘要写成上游 key。保留现有所有权、授权和加密历史约束。
- WS continuation 保留原 key kind，HTTP/WS 重试只投影一次。保留 JSON 未修改区域、精确数字和压缩后的实际请求一致性，写入前精确检查大小增长。
- 兼容模式移除 OAuth 不支持的 retention；仅 generic 客户端移除 options，native Codex 保留显式 options。诊断缺失键标 `absent`，兼容历史 `generated_*` 记录。

缓存率依赖实际重复前缀和上游状态；本次交付验证源码行为和协议，不宣称实时命中率达到 80%–90%。缓存后端与设置说明均已上线：二进制 SHA256 `a52ad225ce147692d6b0355b460e0ad2b63d59e8cc19ddbaac4a849ef32672a6`，回执为 `.local/sub2-cache-align-20260925/release/activation-result.json`。公网 ready 和首页 HTTP 200，首页 hash 匹配发布包，认证 WS 握手 101，临时 Key 已清理。

本轮共享源码同时保留凭据状态修复，需要 migration 004 的新增观察表。首次启动因该表未迁移而自动回滚成功；最终切换前已经核实全部五项 schema checksum 与冻结源码一致。迁移不改历史日志；独立结果位于 `release/migration-result.json`。最终激活动作本身不执行 DDL，因此激活回执的 `database_migrations:false` 仅指切换步骤。

## 复跑

```powershell
python -m unittest discover -s tools -p 'test_loophub_bridge*.py' -v
go test ./... -count=1
```

Python 离线回归 66 项通过，包括 18 项独立恢复测试；旧线上代码运行新测试可稳定失败。最终 Windows 全套 895 项通过，152 项因平台或依赖跳过；Linux 全套 1023 项通过，23 项真实 CLI/live opt-in 跳过，零失败，race/vet 通过。Linux PostgreSQL、Redis、native transport 使用既有隔离环境；三个跨 Python/Go 桥接契约实际通过，未因缺少 Python 跳过。源码 manifest SHA256 `374d772b0e7db08df6b0422db799772238940091fcbb4de93027bdb1d37dccc9`。最终 Linux 证据位于 `.local/gateway-audit-20260924/go-sub2-cache-align-final-20260925/`。
