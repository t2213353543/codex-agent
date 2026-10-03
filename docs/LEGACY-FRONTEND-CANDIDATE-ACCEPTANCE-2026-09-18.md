# 新候选与保留前端：真实管理 API 浏览器验收

## 版本与结果

**13 项检查全部通过**。测试只使用本机独立 PostgreSQL/Redis、禁用的合成账号及浏览器真实 API 调用，不使用 mock，不发模型推理，不部署生产。

| 对象 | 绑定值 |
| --- | --- |
| 候选 Go source | `956fc90be91a922ac1593b283102bc5149674dac337564e6023479ec8e60a6f9` |
| 实际运行 Linux binary | `a66c7be3de344221d6433a6bc7140523d95a6a068fd2312757c80f2dc50c073d` |
| 前端 | `.local/runtime-frontend/d87032e071e7c72343ccb178857d4e56a0200ea7af87aa3b262083e73d15d1c6/frontend/dist` |
| 前端文件数量 | 47，逐文件本地 SHA 与候选 HTTP 返回字节 SHA 均匹配原 capture |
| 前端 canonical 文件清单 SHA | `0ebb8355f15c810cb6b6eb0703827d3dbfc82807a4ae9b579cdcb3a964edbf3e` |
| 运行报告 | `.local/legacy-candidate-browser/20260918T025248-5f0e36/report.json` |
| 浏览器详细报告 | 同目录 `browser/report.json` |

使用的是保留运行版的 47 文件，包括其原 `index.html`、字体、图表模块和 Logs importmap；没有构建共享 `frontend/dist`，没有替换为待交付的新 Accounts/Logs 54 资产包。测试结果因此只证明这 47 文件与此候选的兼容，不覆盖其他前端候选。

## 真实验证范围

1. `/` 和全部 47 个静态资源返回 200，内容逐字节匹配原 capture。
2. 未认证管理 API 返回 401；直接访问旧 Accounts 路由进入登录页。
3. 登录使用实际 Redis 会话，cookie 为 HttpOnly / SameSite Strict，浏览器不在 localStorage 保存管理 key；重载保持登录。
4. 通过真实 PG 导入一条 `enabled=false` 合成账号；响应和渲染内容不出现 access/refresh token 等凭据字段。
5. 两个真实旧 UI 页面同时打开同一账号编辑，首个保存成功，第二个保存收到 409 并重新读取最新版本；后者没有覆盖前者的备注。
6. 新 `POST /api/accounts/{id}/reauthorize` 按 stable ID / version 成功更新合成凭据，并保持禁用状态和管理字段；旧版本请求返回 409。
7. 旧运行总览、Logs importmap 模块、分组、代理和设置页面均使用真实候选 API 加载。
8. 1440×960 桌面和 390×844 手机页面无 document/body 横向溢出；宽表格使用内部滚动。
9. 旧账号筛选无结果/恢复列表、删除确认实际可用；删除后详情返回 404。
10. 原合成 fixture 的日志 1 条、累计 17 tokens 在账号 CRUD 前后保持一致。
11. 页面未捕获 JS 异常，无缺失资源，无意外外部资源或模型 API 请求。
12. 注销实际撤销 Redis 会话，刷新受保护页面重新进入登录。

报告按检查编号拆为 13 项；HTTP 401/409 是上述明确的未授权/并发冲突正向断言，不是被忽略的页面错误。实际 UI 沿用原标签 `管理密钥`、`搜索账号名称`、`编辑账号`、`保存`、`删除` 等，不套用其他被撤回 UI 的选择器。

## 截图

详细目录保存 11 张含 SHA 的截图：桌面 accounts / overview / logs / groups / proxies / settings，双页面版本冲突，以及手机 accounts / overview / logs / settings。截图只显示合成数据；已人工查看概览数据、日志记录、手机布局和冲突弹窗。普通页面截图等待操作消息消失，冲突弹窗保留实际错误提示。

## 生命周期与复跑

先确认旧 binary 测试进程已停止，再启动候选，二者没有并发使用同一套 fixture 凭据。候选容器限制为 0.5 CPU、256 MiB、64 pids、只读根文件系统、无额外 capabilities、非 root 用户。测试地址为 `http://127.0.0.1:28887`，只发布到 loopback；测试结束已正常停止并移除该候选容器。独立 PG/Redis 及私密 fixture 按 root 要求保留，没有停止其他会话的容器。

在既有本机 fixture 保留的前提下复跑：

```powershell
python integration/gateway_environment/legacy_candidate_browser.py --environment-file .local/migration-003-local/20260918T024916-72594cd8d7/environment.json
```

工具只读取私密环境配置，不在报告输出 DSN、管理 key 或合成令牌。它不会再次迁移数据库；fixture 已由独立 003 验收工具准备。执行后会重新停止并移除自身候选容器。

本验收是“新候选 + 保留旧前端 + 合成持久化环境测试通过”，不是生产已迁移或已部署，也不能替代生产切换后的网页、认证、日志统计和重启恢复检查。
