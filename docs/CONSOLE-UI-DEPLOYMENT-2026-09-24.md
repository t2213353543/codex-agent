# 留白 / 夜航：全局 UI 重构发布

> 本文为 14:05 首轮发布记录。用户随后要求提高信息密度并丰富语义装饰，14:31 第二轮已上线；现行版本见 [精细布局发布记录](CONSOLE-UI-DENSE-2026-09-24.md)。首轮上线后公网文件验证成功，但完整浏览器复验因静态资源传输超时未完成，不将其记为通过；第二轮有独立验收结果。

2026-09-24 14:05:10（Asia/Shanghai）上线到 [Codex Agent](http://43.225.196.170:18879)。用户在本地三套可交互预览中选定留白作为日间、夜航作为夜间，并明确要求重构项目 UI、上线。

## 实际行为

- 留白为浅色侧栏与绿色重点色；夜航为石墨深色、紫色重点色与双层导航。右上角切换并保存外观，登录页同样支持切换。新浏览器默认夜航，已保存 light/dark 偏好保留；旧 ocean/mint 转为留白。
- 完整替换应用外壳、登录、工作台和全局组件样式。账号页面重排主操作、真实本页概览、列表与偏好；日志分为查询、分析、记录；设置新增 8 类定位目录。
- 保留现有账号授权、导入、租期、计划、迁移备份、日志观测、筛选、快照、导出和清理等业务。分组、KEY、代理、接入指南、诊断、操作记录采用统一组件样式。
- 移除旧多页签条；导航切页复位滚动位置。保留全局搜索、Ctrl/Cmd+K、侧栏折叠、手机导航、密度偏好、Cookie 会话和退出确认。
- 正式构建使用 `frontend/src`，不包含 design-lab 示例数据或 localStorage 演示资源。后端 API、Go/SQL 和原生传输代码均未变更。

## 发布身份

| 项目 | 值 |
| --- | --- |
| UI release | `67b748ab567493a99e78fffe43a316af1c56c8a833ee67b386e4a215fa60fb5a` |
| 前端源码指纹 | `81bb8a8ed134d663c9fe4d1cffde54a30d2b80f56148e6b9543e70062a72d715` |
| 构建清单指纹 | `794aee9ddce10584ebf842c0238ab54ccf2513c1c7de5dbb9be66b51e2b2f45a` |
| 首页 SHA256 | `36cca744ac0b912db7f2c9be271eef4c1c1ce742109d6e976d47a10426f0a419` |
| 后端 release | `47c80b8ef9b99492df4187bf5ff235719c9531eac998d621b7e0f36261a34cd9` |
| 运行后端 SHA256 | `a673bc08c798d95b3a3af64320008a0418d01aa6753e4da21957634e65cab1c6` |
| 静态文件 | 53 个精确候选文件，另保留 49 个不冲突旧 hash 资源供未刷新的浏览器使用 |
| 服务目录 | `/opt/codex-agent/ui-releases/<UI release>/frontend/dist` |

发布逐字节验证 systemd 只改变 `ExecStart` 的 `--web` 路径，运行二进制仍为 a673，环境文件未由本次发布工具改写。HTTP/WS 原生模式仍启用，同步 timer 保持 active。没有执行数据库迁移、数据回退、业务清理或模型推理。

## 验证

最终冻结候选完成以下检查，报告均以源码、构建与 artifact SHA 绑定：

- 167 项前端单测；生产构建无超大 chunk 警告。
- 账号 55 项、运营日志 54 项、资源管理 13 项、设置 11 项、外壳 11 项、Dashboard 8 项浏览器回归。
- 留白/夜航下的桌面与 320/390/768 像素检查；登录、弹窗、移动导航、焦点恢复、主题持久化、图表生命周期和业务状态验证。
- 发布前 25 项真实后端页面检查，实际 Cookie 登录、10 页面 × 两主题、手机账号/日志/设置以及候选 53 文件 SHA 校验。
- 17 项发布工具测试，包含 9 项状态机自检：只变更前端路径、幂等、失败自动恢复、后续部署保护、凭据与产物绑定等。
- 公网全部 53 个候选文件字节一致；首页、`/health/live`、`/health/ready` 返回 200。

真实页面检查允许会话创建/退出、管理 GET、账号最近使用/用量查询 POST，以及正常打开日志所需的 5 分钟 TTL 查询快照。没有更改账号、网关设置或原始请求记录，没有网关/上游请求。快照创建次数单独记录，不声称完全没有存储写入。真实数据验收未生成含业务内容的截图，公开登录截图不含凭据。

机器报告与截图位于 `.local/console-ui-20260924`。发布前 `acceptance.json`、`live-preflight.json`，线上 `runtime-final.json`、`public-verification.json`、`live-postdeploy.json`；冻结包位于 `.local/ui-releases/<UI release>`。

## 重跑与回退

完整重新验收（会重新构建，并进行受限真实管理页面检查）：

```powershell
python tools/console_ui_acceptance.py
```

精确当前候选上线后验证：

```powershell
python tools/console_ui_live_check.py --deployed --output .local/console-ui-20260924/live-postdeploy.json
```

仅回退本次 UI，后端继续使用兼容的 a673：

```powershell
python tools/ui_release.py rollback --release .local/ui-releases/67b748ab567493a99e78fffe43a316af1c56c8a833ee67b386e4a215fa60fb5a/ui-manifest.json
```

回退材料在服务器该 UI 的 `.ui-state/<release>/previous.service`。不能为回退 UI 而启动旧 956 后端或恢复旧业务数据库。
