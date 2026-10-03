# 旧版界面的日志页独立构建

用户已恢复 2026-09-17 19:33 的界面。本工具只升级使用记录页，保留该版导航、主题、字体、登录和其他管理页面。不能用当前前端的全量 `npm run build` 产物替代此发布包。

在项目根目录执行：

```powershell
node frontend/scripts/build-logs-ui.mjs
node frontend/tests/legacy-logs-runtime-browser.mjs
```

可维护源码为 `frontend/src/views/Logs.vue` 及其工具模块；旧运行时映射在 `frontend/scripts/legacy-logs-runtime/bridge.mjs`。输出位于 `.local/logs-improvement/dist`，完整文件清单与源码摘要记录于同目录上一级的 `build-manifest.json`。构建不会部署、修改旧发布目录或更改后端。

构建首先校验 `7ca36331d1089e9a039f0949526dcb1a65fc28b2985a0f0521620646442f207b` 的原发布清单、全部 45 个文件、依赖锁和所需导出符号。Vite 将日志页编译为一个 ES 模块，Vue、Router、API 会话对象、PageHeader 和 Element Plus 组件全部复用旧版实际导出。图标从同一依赖锁编译，仍使用旧 Vue。未知 Element 组件或第二套 Vue / Router / Element Plus 会让构建失败，不能通过忽略错误放行。

输出保留全部旧静态资源。唯一修改的旧文件是 `index.html`：在原模块入口前加入 import map，将旧日志模块 URL 映射到新模块，并增加日志页独立样式。新样式使用不同的 scoped ID；旧日志样式及路由原有的预加载清单继续保留。不会直接编辑压缩后的代码，也不会替换旧 hash 文件下的内容。

主入口 `index-D--MHP3C.js` 同时包含旧 Vue 和应用启动逻辑。不能把它修改后换成另一文件名而保留其他旧模块的导入，否则会同时运行两份应用。import map 避免了这类问题；浏览器必须支持原生 import maps。首次方案已在本机 Chrome 验证共享响应式、路由注入、API、卸载/重挂载及原设置页面。

浏览器验收仅启动 localhost 静态服务器并拦截全部管理 API；不使用真实账号或调用模型。它运行日志行为回归，并检查旧版 8 页、4 主题、1440/390 宽度；日志专项还覆盖 320 宽度。报告在 `.local/logs-improvement/browser-report.json`，截图在其 `browser` 子目录。`--shell-only` 只适合开发中先检查旧壳，报告明确记录为 `shell_only`，不能充当最终通过。

发布前应针对本次构建重新运行上述测试，验证筛选、分页、失败重试、详情、脱敏导出与手机布局。前端发布只应使用上述输出目录；同时发布已授权的网关修复时，后端另行测试、冻结并核对哈希，继续保留当前配置和数据库。
