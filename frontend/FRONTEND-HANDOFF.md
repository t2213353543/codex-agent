# PowerPoint + Codex Gateway 前端交接

2026-09-27。Vue 3、Element Plus、Vite、ECharts 与账号、API Key、分组、代理、日志管理保持现有技术体系。当前推理产品使用 PowerPoint BPS，Codex 为客户端与工具执行环境；没有“完美兼容”或固定质量承诺。

## 当前入口

- 登录页、页面标题、侧栏统一为 PowerPoint + Codex Gateway。
- 接入指南默认 `gpt-6-astra` / `xhigh`，provider `name = "OpenAI"`、`wire_api = "responses"`、`supports_websockets = true`。模型和档位以账号目录为准；provider 标识可自定义但必须与配置节一致。
- 设置、运行诊断、账号运行状态展示后端 `inference` 的实际值，缺失显示“未提供”。部署变量放在高级配置中，只读展示。
- 会话账号绑定固定开启，TTL 可配置；缓存按 Key/账号强制隔离；旧 CLI 指纹和缓存隔离关闭入口已移除。
- 客户端参数规范化、令牌刷新、HTTP/WS/JSON/compact/models 与超时设置保留。PowerPoint 必需协议转换不由可选兼容开关关闭。
- 账号用量明确是 ChatGPT 订阅额度；模型目录和显式生成/压缩测试使用 PowerPoint。读取目录不代表推理已通过。

## 后端字段合同

`/api/settings`、`/api/diagnostics` 返回 `inference`；`/api/accounts/:id/runtime` 在 `effective_policy.inference` 返回同一对象：

```json
{"backend":"powerpoint_bps","profile":"powerpoint","base_url":"https://bps.openai.com/basispoints/api","tools_version":"tools-powerpoint-core-2026-08-10-2b886486","web_search":"enabled","codex_fallback":false}
```

`PowerPointService.vue` 统一渲染。`bpsRuntime.js` 只显示服务返回的信息，不生成默认生效值，不显示 URL 凭据、查询参数或 fragment。既有 Cookie 会话、实体版本与设置版本冲突处理不变。

## 构建与验证

从项目根目录运行：

```powershell
npm --prefix frontend ci
npm --prefix frontend test
npm --prefix frontend run build
node frontend/tests/settings-browser.mjs
```

本轮 175 项 Node 单测、生产构建通过；设置及接入浏览器验收 12 项通过，覆盖 16 个实际设置字段、错误/409、草稿、密钥轮换、实际 PowerPoint 信息、CLI 配置以及 320/768/1440px 布局。记录为 [settings/results.json](test-results/settings/results.json)，对应截图位于同目录。浏览器测试全部使用本机合成 API，没有连接生产账号。

`npm --prefix frontend run test:management` 执行更广的管理专项；旧指纹测试已从集合移除，相应禁用入口由当前 settings 测试覆盖。本轮未把历史广泛验收的结果重新计入当前通过数。真实 Go/BPS/CLI 验收使用 [tools/bps_validate.py](../tools/bps_validate.py)，其结果与浏览器 mock 分开记录。

产物为 `frontend/dist`。部署必须把相匹配的 Go 后端与前端一起发布，源码构建和本机浏览器通过不代表已部署。旧版本交接原文见[2026-09-17 历史交接](../docs/history/2026-09/FRONTEND-HANDOFF-2026-09-17.md)，旧路径、界面和测试数量仅用于追溯。
