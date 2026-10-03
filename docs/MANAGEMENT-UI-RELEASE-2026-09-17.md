# 管理后台最终界面发布

> 当前状态：用户已要求恢复旧页面，2026-09-17 22:32 已执行。本文为此前改版记录，当前 UI 见 [恢复记录](UI-RESTORE-2026-09-17.md)。

2026-09-17 21:59（Asia/Shanghai）已上线，入口 [Codex 管理后台](http://43.225.196.170:18879)。发布版本 `d2b3b78936d031cc127c004f8c9ad1519bf919276a9fdf456b6bfafd786a9b9a`；[机器可读发布清单](MANAGEMENT-UI-RELEASE-2026-09-17.json) 记录实际哈希、备份和检查结果。

全部 55 个静态文件与最终验收包逐项一致，包含完整 10 个管理页、登录、四主题、快速访问、资源批量管理、手机操作记录卡片、诊断和接入指南。现网后端二进制保持 `ec340e64246c47e50fca074eadcbfc7e7b8ed797c60189800c42ce6532b64cf0`，与发布前完全一致；本次没有改变 Codex 网关实现。

上线后验证：live/ready/首页均 200；10 个管理 GET 接口及匿名 401 正常；PostgreSQL/Redis 正常；管理设置和账号数量与发布前一致。真实浏览器完成 UI 登录、HttpOnly `/api` Cookie、Ctrl K、10 页 × 桌面/手机共 20 组合、字体加载、无整页横向溢出、无 JS 错误或资源失败、退出和会话失效检查。仅创建并删除验收临时会话，未导入账号、调用模型或修改管理配置。

本地前端验收 53 项单元、132 个截图组合和全部行为回归已通过。后端、依赖及迁移 82 个文件和重新构建的二进制与前次发布一致，复用对应 Go / Linux race / 实际隔离 PG/Redis 证据，不冒充本次重复执行。

沿用 `tools/upgrade.py` 的 stage/preflight/activate 流程；切换前保存完整 unit、私密环境和一致 PG dump；没有 schema 迁移或重复旧数据导入。前一发布 `1244c39ae6cd413c42b8d5deeaa42c738be4160eb6a936607f5b88bbc077dbc1` 保留，可执行以下命令恢复前一程序/界面并保留当前数据库：

```powershell
python tools/upgrade.py rollback --release .local/management-deploy/source/.local/upgrades/d2b3b78936d031cc127c004f8c9ad1519bf919276a9fdf456b6bfafd786a9b9a/upgrade-manifest.json
```

直连 SSH 不通，沿用本项目此前已授权的临时代理连接；未变更系统代理、网络入口或其他项目。
