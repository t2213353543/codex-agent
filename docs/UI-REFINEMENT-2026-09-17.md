# 字体与色彩细化 · 2026-09-17

用户反馈上一版字体不好看、页面过于单调。本轮仅调整前端字体与视觉层级，保留全部 21 张指标卡、12 个默认账号列、21 个可选账号字段、原交互和透明居中加载圆环。

实际调整：

- 固定界面文字使用自托管 Noto Sans SC 子集（字体名 Codex Admin Sans）。正文 400、导航 500、标题/数字 600，统计数字采用比例字体和等宽数字特性；token/ID 保留等宽显示。
- 蓝色主色配少量青绿、靛紫、琥珀辅助色。资源卡增加图标底座与角落淡色修饰，KPI 增加细色线，小指标使用色点，导航图标和品牌图标增加配色。
- 卡片增加浅阴影，背景改为轻蓝灰，深色主题采用蓝灰层次。浅色小字在各页面背景的对比度至少 4.599，青色数字对白底对比度 4.982。
- 字体共 136,812 字节（133.6 KiB），含 693 字符，覆盖当前所有固定界面文字；动态名称超出子集时使用系统无衬线字体。字体随静态资源发布，无外部字体 CDN 请求。来源与复现见 [FONTS.md](FONTS.md)。

## 验收与发布

统一命令仍为 `python tools/ui_acceptance.py`。本轮冻结证据为 `.local/ui/runs/20260917T113103Z/report.json`，32 项单测、19 组管理页面、10 组图表/日志和 9 组真实 API 检查通过。覆盖四主题与 320/390/768/1440px；128 张合成截图，另外保留真实合成 PG/Redis 页面截图。

Chrome CDP 实际确认标题、副标题、页签、表头使用下载的自托管字体（`isCustomFont=true`），并非只检查 CSS 字体列表。32px 中心圆环、透明背景、减少动态效果模式的可见圆弧继续通过。实际 DynamicUser 进程成功读取字体，`Content-Type: font/woff2`，内容 SHA 与构建一致。

JS 总量 1,383,338 → 1,384,302 字节；CSS 253,950 → 259,898 字节；另增加上述字体文件。详见 `.local/ui-refinement/bundle-comparison.json`。没有为视觉修改增加运行时库。

**2026-09-17 19:33:57（Asia/Shanghai）已部署**。UI 发布 ID：`7ca36331d1089e9a039f0949526dcb1a65fc28b2985a0f0521620646442f207b`。只变更本项目 unit 的 `--web` 路径，后端 binary 和 PG/Redis 环境文件保持一致，本轮没有数据库迁移。新版首页、live、ready 均返回 200；发布失败自动回退、重复发布和上一版恢复均在独立演练中通过。

正式地址：<http://43.225.196.170:18879>。本轮发布清单为 [UI-REFINEMENT-RELEASE-2026-09-17.json](UI-REFINEMENT-RELEASE-2026-09-17.json)，公网复核结果保存在 `.local/ui-refinement/production/browser-report.json`。

回退至本轮之前的 UI（保留当前数据库）：

```powershell
python tools/ui_release.py rollback --release .local/ui-releases/7ca36331d1089e9a039f0949526dcb1a65fc28b2985a0f0521620646442f207b/ui-manifest.json
```

发布流程见 [UI-RELEASE.md](UI-RELEASE.md)；初次 PG 迁移记录和此前 UI 发布证据均保留，不覆盖。
