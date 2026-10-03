# 企业风格 UI 整改与上线记录

本次三项整改已验收并于 **2026-09-17 19:15:40（Asia/Shanghai）上线**。访问地址：<http://43.225.196.170:18879>。PG/Redis 迁移已在 18:42:53 完成；本次仅切换前端，不再导入数据。

## 实际变化

- 顶栏 56px、页签栏 36px；普通页签约 98px，标题与关闭按钮间距 4px，去掉宽大的胶囊和多余留白。中文经 Chrome CDP 确认真正使用 Microsoft YaHei，标题使用其粗体。
- 按 Element Plus 原生组件尺度调整为官方蓝、灰白中性背景、细边框和克制圆角。深色、海蓝、翡翠保留，海蓝/翡翠使用同一中性布局，仅强调色不同。参考[官方设计原则](https://element-plus.org/en-US/guide/design.html)、[布局容器](https://element-plus.org/en-US/component/container.html)和[加载组件](https://element-plus.org/en-US/component/loading.html)；没有额外引入另一套 UI 框架。
- 对照旧生产静态页面恢复统计内容：账号总数、启用账号、KEY 数、请求/成功/失败、输入/输出/缓存 Token、平均耗时、峰值及实时 RPM/TPM、今日请求/Token、累计 Token。另明确分组、今日平均耗时与保留期数据。页面共 21 张指标卡和实时吞吐条，原图表、排行和最近错误仍保留。今日标注 Asia/Shanghai，累计和保留期分别显示，未知值不伪装为零。
- 默认账号列恢复为 12 项：名称、状态、并发、优先级、权重、分组、用量窗口、请求用量、平均耗时、代理、令牌到期、操作。共 21 项可选字段，补充输入/输出/缓存、凭据状态、备注、标签、最近验证、创建/更新时间、ID/版本、最近错误；详情和编辑配置一并恢复。旧六列默认偏好自动升级，其他用户自选列保留。仅改备注等元数据不会重置额度窗口。
- 加载背景完全透明，不染色、不模糊；显示居中的 32px 中性灰圆环。普通模式旋转；系统“减少动态效果”模式显示可见圆弧，不再缩成小点。

## 验收

统一入口：

```powershell
python tools/ui_acceptance.py
```

前提是独立合成 PG/Redis 环境及 `127.0.0.1:28882` HTTP 隧道可用。当前网络已由用户授权使用项目本机代理。环境准备、发布与回退步骤见 [UI-RELEASE.md](UI-RELEASE.md)。入口不执行生产部署或迁移，每轮新建报告目录，不覆盖旧证据。

| 检查 | 本轮结果 | 证据 |
| --- | --- | --- |
| 单元测试 | 32 通过，0 跳过 | `.local/ui/runs/20260917T111153Z/unit.log` |
| 构建与浏览器 | 19 组管理 + 10 组图表/日志通过；128 张合成截图，四主题 × 320/390/768/1440px | 同目录 `browser.log`、`browser/results.json`、`charts-browser/results.json` |
| 真实接口页面 | 8 组通过：未授权、Cookie、管理页面、间距、四主题加载圆环、筛选、手机、退出 | 同目录 `real/report.json` |
| 真实后端身份 | 合成性能容器 `/proc/1/exe` SHA 与已上线 PG binary 相同 | 同目录 `backend-identity.json` |
| 发布演练 | 激活、重复激活、回退、重复回退、启动后故障自动恢复均通过 | `.local/ui/rehearsal-report.json` |
| 生产运行权限 | DynamicUser 非 root 实际读取新首页、全部新旧静态资源；ready 200 | `.local/ui/permissions-report.json` |
| 正式切换 | 仅 unit 的 `--web` 参数改变，环境文件不变；账号/分组/代理身份及凭据条数一致 | `.local/ui/production/activation-report.json` |
| 公网页面 | 实际登录、恢复的账号列/统计卡、管理页面、390px、退出通过；未导出生产账号值 | `.local/ui/production/browser-report.json` |

最终统一报告：`.local/ui/runs/20260917T111153Z/report.json`。本轮使用合成 1,000 账号和原 100,000 事件规模环境；浏览器截图均为合成数据。首次预跑中的测试环境识别错误（容器为 host 网络）和跨 SSH 默认 5 秒等待超时已修正；这些失败报告保留，不当成通过证据。最终完整命令真实退出 0。

## 包体与版本

恢复功能后 JS 总量由 1,360,807 增至 1,383,338 字节（gzip 469,411 → 476,157）；CSS 252,393 → 253,950 字节（gzip 39,274 → 40,025）。最大 JS chunk 为 426,638 字节；继续使用页面懒加载，未提高构建警告阈值。对照基线从冻结旧发布归档读取并逐文件核对 SHA，见 `.local/ui/bundle-comparison.json`。实际首屏资源清单、传输大小和时间保存于 `real/report.json` 的 `first_screen`；网络经过 SSH，不能作为服务端延迟基准。后端性能和 SQL 执行计划仍见 [PERFORMANCE-FINAL-2026-09-17.md](PERFORMANCE-FINAL-2026-09-17.md)。

| 身份 | SHA256 |
| --- | --- |
| UI 发布 ID | `3cc0f99d2c87ae09aa6888a83bc94eddfba257bd9e9341328da23c78caa84203` |
| 前端源码集合 | `6e682957f039a33cd570d3718132dd2174db6d3c0b7fab32f98f8a5e4af09d9f` |
| 前端 dist 集合 | `14991d6802d1fe2832c0a305a739aa72e16e0fad215b26bd6ec932d6fbd3a311` |
| 后端 binary | `07f958e065f8473e5a21c037d8527943cf71652e8b089f245a2c8aa91f673894` |

实际发布清单见 [UI-RELEASE-2026-09-17.json](UI-RELEASE-2026-09-17.json)。**测试通过、已部署；本次数据库迁移为 false（未重复迁移）**。进程 live、ready 和首页为 200，公网页面复核通过。静态目录为 `/opt/codex-agent/ui-releases/<UI发布ID>/frontend/dist`，保留 24 个旧 hash 文件供未刷新的浏览器完成懒加载。

仅前端回退命令（在项目根目录）：

```powershell
python tools/ui_release.py rollback --release .local/ui-releases/3cc0f99d2c87ae09aa6888a83bc94eddfba257bd9e9341328da23c78caa84203/ui-manifest.json
```

回退保留当前 PG 数据及后端。不要为 UI 回退调用旧 JSON 迁移的 rollback。现有已知边界保留：公网入口仍为 HTTP；账号排序只作用于当前页；后端未提供的独立手动验证、异步刷新进度、全量日志导出和代理诊断历史没有虚构入口。
