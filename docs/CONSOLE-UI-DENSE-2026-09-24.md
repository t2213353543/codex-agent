# 留白 / 夜航第二轮：精细布局与信息密度

2026-09-24 14:31:49（Asia/Shanghai）已上线到 [Codex Agent](http://43.225.196.170:18879)。在用户选定留白/夜航之后，本轮按追加要求参考 Sub2API 的用量单元、语义背景色与紧凑表格，整体提高空间利用率。

## 交付内容

- 使用记录：输入、输出、缓存 Token 同行显示为蓝、紫、琥珀三种带图标胶囊，完整数值、真零与未知保留；模型、服务层、推理强度、协议、HTTP 和耗时统一为小标签。日志舒适行约 66px，紧凑行约 56px。排序与结果控制合并视觉区域，统计有语义图标与细色条。
- 账号与资源：账号行约 64px，账号汇总约 78px，资源汇总约 74px。增加凭据状态、并发细进度、优先级/权重色块、分组/代理标记和到期提示，请求/Token 列前移。关联跳转、版本冲突保护、导入和迁移功能保持。
- 工作台：KPI 约 104–109px、资源横栏约 82px、图表 260px，吞吐并入上下文条，16 项原指标与图表生命周期保留。
- 设置：紧凑分类目录与图标、桌面双列开关、密钥/参数标签，手机专门调整控件排列。原 CLI 指纹、提示词、刷新、会话、超时等设置均保留。
- 全局：顶部栏从 67px 缩至 51px；日间侧栏 212px、夜间 60px 图标轨加上下文栏总 226px；常用控件 30px、表头 35px。新增共享语义色与密度层，所有页面及弹窗继续兼容日夜主题。新增独立 SVG favicon。

## 发布身份

| 项目 | 值 |
| --- | --- |
| UI release | `f67adb15dfb99294a16c02975f59ce5a46d0e4bfdc4a16308f188b1ea02dc1f3` |
| 前端源码 | `b9682a73f6d51a3fb6bee84af13486cf69e5055c784cba3bafadda6986e7d711` |
| 构建清单 | `7d2346a4c597b1f3d45cbef972b4775dec51d2140804322debba5023b6fa445e` |
| 首页 SHA256 | `a73eb18da3552341c9848a7be5fe19527a1117e083e3819d02288c872dd0bec5` |
| 后端 SHA256 | `a673bc08c798d95b3a3af64320008a0418d01aa6753e4da21957634e65cab1c6` |
| 静态文件 | 54 个新清单文件；90 个不冲突旧 hash 资源兼容尚未刷新的页面 |
| 前一个 UI | `67b748ab567493a99e78fffe43a316af1c56c8a833ee67b386e4a215fa60fb5a` |

实际 systemd 配置逐字节确认只修改 `--web` 路径，环境 SHA 与发布前完全一致，后端 binary/HTTP 与 WS 原生配置保持。没有数据库迁移或业务数据还原；同步 timer active，NRestarts=0，首页/live/ready 均 200。

## 验收证据

最终冻结源码和构建完成 167 前端单测、17 发布工具测试（包含 9 状态机自检），浏览器 55 账号、55 日志、13 资源、11 设置、11 外壳、8 Dashboard 检查。日志新增真实几何断言：1440 下三 Token 胶囊 Y 坐标相同、背景色互异、无需内部横滚且完整成功行不高于 76px。

发布前真实 API 25 检查通过：实际 Cookie、10 页面两主题、手机账号/日志/设置、精确候选文件 SHA。自动验证禁止业务修改与模型请求，仅允许管理读、会话操作，以及日志正常读取所需 5 分钟 TTL 查询快照。请求明细与快照次数独立保存。

上线后复验同样 **25 项全部通过**，实际公网 54 文件 SHA 与候选一致，真实管理 API 无失败、浏览器无运行异常，业务写入与网关请求均为 0。静态资源由公网取回并在本次验证内缓存已核对的字节，管理会话和 API 通过 SSH；该验证不变更生产代理、缓存或服务器配置。

报告根目录：`.local/console-ui-dense-20260924`。关键文件为 `acceptance.json`、`live-preflight.json`、`runtime-final.json`、`live-postdeploy.json`，截图在 `logs`、`accounts`、`resources`、`dashboard`、`console-browser`。正式构建不包含 design-lab 演示数据。

重跑完整最终验收：

```powershell
$env:CODEX_UI_OUTPUT = (Resolve-Path '.local/console-ui-dense-20260924').Path
python tools/console_ui_acceptance.py
```

线上精确文件与页面复验：

```powershell
python tools/console_ui_live_check.py --deployed --output .local/console-ui-dense-20260924/live-postdeploy.json
```

仅回退至本轮之前的 UI，继续使用相同后端：

```powershell
python tools/ui_release.py rollback --release .local/ui-releases/f67adb15dfb99294a16c02975f59ce5a46d0e4bfdc4a16308f188b1ea02dc1f3/ui-manifest.json
```
