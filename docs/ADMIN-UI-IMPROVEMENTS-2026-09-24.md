# 管理控件与仪表盘修复

本次保留留白 / 夜航和紧凑布局，处理用户提出的复选框、聚焦输入框、工作空间卡片与仪表盘数据。

- 复选框错位来自旧的 `left: 5px; top: 2px` 与当前 Element Plus 的百分比平移同时生效。恢复到控件中央，禁用项使用其自身颜色，保留键盘焦点提示。
- 输入框原先被通用 `[tabindex]:focus-visible` 绘制内部边框，外部容器又带 3px 光圈。改为整个输入容器的单层 1px 聚焦边界，覆盖普通输入、数字输入、选择器、日期、文本域和组合输入。
- 删除日间“管理工作空间 / Codex Agent”卡片以及夜间对应的重复工作区标签。
- 仪表盘新增当前进程处理请求数、失败率、耗时样本数、今日请求 / Token、模型使用分布、失败类别分布；最近错误补充 HTTP / 模型 / 协议标签。模型和错误类别可跳转到相同时间段日志。
- `GET /api/overview?include_breakdown=true` 可选返回 `traffic_breakdown`。PG 通过一个分组查询只读取模型和错误类别；旧的对比时段只读取原有聚合。模型分布基于保留的请求明细，有独立分母及覆盖说明；未知 Token 不显示假零。

验证结果：172 项前端单测通过，生产构建通过；Dashboard 9 组检查通过，含 320 / 390 / 768 / 1440px 双主题、全部原指标、6 个 ECharts 生命周期与新指标。控件浏览器检查覆盖双主题 checkbox / 输入 / 数字 / 文本域 / select / 日期与键盘焦点。Go 文件统计和 API 可选维度测试通过；PG 用例需带 `TEST_DATABASE_URL` 在集成验证执行。

本地证据位于 `.local/admin-ui-improvements`：`before` 是原控件截图，`after/results.json` 和 `after/*.png` 是修复后控件，`dashboard/results.json` 和 20 张截图是仪表盘检查，`fingerprint.json` 为完成时源码及 dist 指纹。所有浏览器 fixture 均为本地合成数据，产品代码只使用真实 API。

重跑：

```powershell
cd frontend
node tests/controls-browser.mjs
node tests/charts-browser.mjs
```

```powershell
go test ./... -run 'TestOverviewBreakdown|TestWebOverviewAndLogs' -count=1
```
