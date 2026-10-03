# 表格与运行诊断紧凑布局

已热发布前端 `c1e2930238bb476706bc110084adbb7bfab76f3eeda3307da85e4772b3643aaa`。后台仍为0.4.0，PID 1304514及二进制、systemd配置、启动环境均未改变，没有重启服务。

## 修改

- 删除“显式连通测试”下截图中的硬编码PowerPoint说明，保留标题及测试控件。
- 单个及批量编辑账号移除本地Token预算、窗口、重置入口。编辑请求不提交`quota/window_days/reset_window`，已有后台配置不会因编辑其他字段而被清空或重置。
- 官方额度每个窗口改为一行使用率、细进度条、费用及总额；完整重置时间和估算解释放在可点击浮层，官方详情入口保留。大金额不截断，平板宽度取消遮挡中间字段的固定列。
- 分组、密钥、代理、使用记录、操作记录及对话框内表格统一缩小单元格留白；原生日期统计表也压缩行距。
- 运行诊断合并顶部服务信息、PG/Redis状态与推理配置；六个KPI桌面同排，两张趋势图、分布、12项资源、双连接池及错误表全部保留。错误表可直接进入对应请求记录。

参考本机Sub2API的`AccountUsageCell.vue`、`UsageProgressBar.vue`、`DataTable.vue`与`OpsDashboard.vue`，采用同行数据与可展开详情，而非缩小到不可读或删除指标。

## 实测

同一合成数据、1440px视口，表格首行高度：

| 表格 | 原高度 | 新高度 |
| --- | ---: | ---: |
| 双窗口账号 | 177.8px | 64px |
| 分组 | 66.7px | 50.3px |
| 密钥 | 84px | 70px |
| 代理 | 84px | 70px |
| 使用记录 | 66px | 55px |
| 操作记录 | 41px | 33px |

诊断页同数据滚动高度2527px降至1417px，顶部概览约180px。浅色/深色及1440/768/390/320宽度验证金额完整、浮层可操作、页面不溢出。诊断另验证刷新失败保留结果、空值状态、时间范围、自动刷新偏好及全部指标数量。

188项前端测试通过，构建通过；账号编辑请求字段、批量入口及硬编码说明删除有浏览器断言。发布执行器3项事务检查通过。生产HTTP逐项核对新首页及全部静态资源摘要与候选一致，状态为active。

证据：`frontend/test-results/compact/`、`.local/operations-monitor-20260927/browser-report.json`、`.local/compact-layout-acceptance.json`、`.local/compact-ui-release/c1e2930238bb476706bc110084adbb7bfab76f3eeda3307da85e4772b3643aaa/`。

本次为可回滚静态覆盖：原后台发布清单保留，UI journal记录旧首页与新增文件；保留旧哈希资源供已打开页面继续使用。刷新页面后加载新界面。

复跑布局：`node frontend/tests/compact-layout-browser.mjs`。发布状态：`python tools/compact_ui_release.py status --release .local/compact-ui-release/c1e2930238bb476706bc110084adbb7bfab76f3eeda3307da85e4772b3643aaa/manifest.json`。
