# Codex Agent 前端交接 · 2026-09-17

前端已按 `REVIEW-2026-09-17.md` 和 `docs/API-CONTRACT.md` 完成修复与对接，并按后续反馈恢复统计卡片、账号字段与企业后台样式。保持 Vue 3、Element Plus、Vite、ECharts。本前端交付没有修改 Go、数据库实现或生产服务；集成验收与部署状态由集成任务单独记录，浏览器通过不代表已经部署。

## 验收入口

在项目根目录执行：

```powershell
npm --prefix frontend test
npm --prefix frontend run test:browser
npm --prefix frontend run measure
```

`test:browser` 已替换原有 `../tools/browser-test.cjs` 默认入口，依次完成生产构建、管理页面隔离浏览器测试、图表/日志专项测试、截图索引生成。脚本只启动本机服务，全部管理 API 都由 Playwright mock；不读取 `data/client-key`，不读取生产密钥，不向真实后端导入或删除账号。

- `tests/browser.mjs` 为编译后的 `frontend/dist` 启动随机端口的本机静态服务器；未被拦截的 API 请求会导致测试失败。
- `tests/charts-browser.mjs` 启动本机 Vite，默认端口 4191，用于实际 ECharts 实例和 ResizeObserver 生命周期验证。可用 `CHARTS_TEST_PORT` 更改该隔离测试端口。
- 浏览器默认使用本机 Chrome。可用 `TEST_BROWSER` 选择已安装的 Playwright 浏览器 channel。依赖通过 `npm --prefix frontend ci` 安装；不需要启动 PG、Redis 或真实管理服务。
- 单独构建：`npm --prefix frontend run build`。构建输出仅 `frontend/dist`。

## 测试结果

| 检查 | 结果 | 记录 |
| --- | --- | --- |
| 生产构建 | 通过，未提高 warning 阈值，无超过 500 kB 的 chunk 警告 | `npm run build` |
| Node 单测 | 本轮 32/32 通过，新增额度窗口保护与配置边界测试 | [本轮 unit.log](../.local/ui/validation/unit.log)；[unit-tests.txt](test-results/unit-tests.txt) 为前一轮 30 项历史记录 |
| 编译产物浏览器测试 | 19 组通过，80 张截图，无页面异常/未拦截 API | [browser/results.json](test-results/browser/results.json) |
| 图表/日志专项浏览器测试 | 本轮 10 组通过，48 张截图，无页面异常；新增恢复卡片的值与统计口径断言 | [charts-browser/results.json](test-results/charts-browser/results.json) |
| 真实隔离后端页面集成 | 8 项通过；独立 PG/Redis、合成数据、真实 Cookie/API | [本轮 real/report.json](../.local/ui/real/report.json)，与上述 mock 浏览器结果分开记录 |

本轮 mock 浏览器合计 29 组（19 + 10）。旧版记录中的 30 项单测、28 组浏览器为恢复卡片和字段前的历史结果。

重点覆盖：共享 JWT 头和共享工作区的名称碰撞、完整多行 auth.json、JSON 数组/JSONL/纯 token、错误行号和禁止降级、显式覆盖、逐项结果下载、同一行数组结果不混淆、HTML 原样文本、缓存率 0/0.8/1/无样本、旧筛选响应竞态、列保存/损坏恢复、Cookie 登录退出、旧会话检查不能覆盖新登录、取消后不弹错误、版本冲突、防双击提交、代理诊断、密钥轮换结果重试、复制失败与焦点恢复。最终契约补充的设置 `settings_version` 和 Keys `version` 均覆盖连续提交、409 回读和显式重试。

## 截图

[打开可筛选截图索引](test-results/screenshots.html)：共 128 张，支持按页面、主题、断点筛选。

- 账号、分组、代理、管理设置：每页 light/dark/ocean/mint × 320/390/768/1440，共 64 张。
- 概览、概览图表区域、日志：同样矩阵，共 48 张。
- 手机导航抽屉、创建分组弹窗：四主题 × 320/390，共 16 张。
- 自动检查页面及主要内容容器不横向溢出；表格内部允许横向滚动。弹窗不越界，关闭恢复触发按钮焦点。
- 已人工抽查窄屏账号长名称、深色弹窗、768px 海蓝代理、翡翠桌面账号、深色图表与手机日志。复核发现的固定日志操作列透出底层文本问题已修复，并添加固定列不透明断言。

截图全部来自合成数据，版本会显示 `0.2.0-mock` / `mock-charts-test`，不是生产数据或真实服务版本。

## 变更清单

| 范围 | 主要变化 |
| --- | --- |
| `src/api.js`、`src/router/index.js`、`src/views/Login.vue` | POST/GET/DELETE session；HttpOnly Cookie 会话；移除长期 admin key 存储并清理遗留键；取消请求、认证代际隔离、统一错误反馈；保留 HTTP status/code 供 409 处理 |
| `src/App.vue`、`src/components/SideNavigation.vue`、`src/theme.js`、`src/style.css` | 移动导航抽屉、简化顶栏、主题语义色/间距/视口约束、可见键盘焦点；刷新重建页面组件，不再访问中转路由；版本来自后端，移除通知红点；页签与偏好校验/版本 |
| `src/views/Accounts.vue`、`src/utils/importAccounts.js`、`src/utils/accountFields.js` | 恢复 12 个默认列、21 个可选列和完整详情；恢复并发/优先级/权重/额度/窗口编辑及导入默认配置；严格格式解析与行号、服务端导入预览、逐项明确替换与脱敏下载；ID+version；服务端筛选/分页；常用筛选；批量启停/标签/绑定解绑代理；到期提示与真实刷新等待状态 |
| `src/views/Dashboard.vue` | 对照旧生产前端资产恢复全部 16 项原有指标；补充分组、启用 KEY、输入/输出/缓存 Token、今日平均耗时和保留期统计；Element Plus 中性卡片分组；0.8 显示 80%；无样本独立显示；明确今日时区、所选范围、近 5 分钟吞吐、持久事件累计和保留期；图表生命周期、空态与日志跳转 |
| `src/views/Logs.vue`、`src/utils/logExport.js` | 游标分页、`total=-1` 不伪装总数；URL 筛选/页码和常用筛选；文本详情、稳定账号 ID 跳转；当前页白名单脱敏 JSONL |
| `src/views/Groups.vue`、`Proxies.vue`、`Settings.vue`、`Keys.vue` | 提交防重、取消与错误反馈；分组/代理 version 冲突处理；代理密码不回显，空值保持、显式 clear_password；诊断阶段与 HTTP/IP 严格成功判断；独立密钥轮换和 request_id 重试。Keys 仅安全和交互修复，未增加网关功能 |
| `src/components/CopyTextDialog.vue`、`src/utils/management.js` | 复制失败提供自动选中的只读文本；toast 不展示完整密钥；关闭清空临时回退内容并恢复焦点；管理页筛选在页面内存保留 |
| `src/utils/latestRequest.js`、`format.js`、`preferences.js`、`echarts.js` | 请求顺序/取消、统一空值与比例格式、偏好版本校验、按需图表注册 |
| `src/main.js`、`vite.config.js`、`package*.json` | Element Plus 组件及 CSS 按需引入，图标显式按名称导入；ECharts 仅 Line/Bar/Pie/Scatter 与实际使用组件；页面继续懒加载，分离共享 canvas renderer |
| `tests/*`、`scripts/*`、`artifacts/*`、`test-results/*` | 隔离 mock 验收、体积基线与对比、截图索引和运行结果 |

### 本轮恢复清单与行为边界

- 对照 `.local/ui-reference/legacy/Dashboard-CDrxqoOv.js` 恢复账号总数、启用账号、API KEY 数、区间/今日请求、区间/今日/累计 Token、缓存率、平均耗时、RPM/TPM、峰值 RPM/TPM、成功/失败请求。输入、输出、缓存 Token 从同一区间趋势求和；没有后端全局异常账号数量字段，不把停用数量或当前页数量假作全局异常数。
- 账号默认列为名称、状态、并发、优先级、权重、分组、用量窗口、请求用量、平均耗时、代理、令牌到期、操作；另可显示输入/输出/缓存、凭据状态、备注、标签、最近验证、创建/更新时间、账号 ID/版本、最近错误，共 21 列。选择框是额外的表格控制列，不算数据列。
- 旧六列默认偏好自动升级为十二列，其余自定义列选择保留。详情展示配置、额度已用/百分比/窗口起点、全部用量分项、凭据状态及刷新令牌是否存在；秘密值仍不返回或展示。
- 编辑恢复名称、并发、优先级、权重、额度、窗口及显式重置；导入恢复并发、优先级、权重、额度、窗口、分组及代理默认配置。修改默认配置后必须重新预览，保留逐项替换确认。
- 编辑备注等元数据时不发送未改变的 `quota` / `window_days`，避免后端按现有契约重置计量窗口。仅额度或窗口实际改变、或明确勾选重置时提交相应字段，并显示重算提示。始终提交已读取的实体版本，409 回读后等待重新确认。

## 包体对比

以下为恢复卡片和字段之前的包体优化基线，不作为本轮重构后的精确包体结论。统计范围：`frontend/dist/assets` 下所有 JS/CSS 文件相加，不只比较入口文件；gzip 使用同一脚本逐文件压缩后相加。单位 kB = 1000 字节。页面懒加载保留。

| 资源 | 优化前原始 | 优化后原始 | 优化前 gzip | 优化后 gzip |
| --- | ---: | ---: | ---: | ---: |
| JS 合计 | 2,399.3 kB | 1,360.8 kB | 791.6 kB | 470.2 kB |
| CSS 合计 | 382.5 kB | 252.4 kB | 52.9 kB | 39.5 kB |

JS 原始减少约 43.3%，gzip 减少约 40.6%；CSS 原始减少约 34.0%，gzip 减少约 25.4%。最大 JS chunk 约 424.5 kB。实际减量来自按需组件、图标和图表模块；renderer 拆分只调整加载边界。没有修改 `chunkSizeWarningLimit`。

精确字节明细：[优化前](artifacts/bundle-before.json)、[优化后](artifacts/bundle-after.json)。`npm run measure` 可重算当前构建。

## 已对接的后端契约与集成注意

- Session：`POST /api/session` 登录、`GET /api/session` 恢复、`DELETE /api/session` 注销。普通管理请求不携带长期 Bearer key。前端需与支持本版契约的后端一起交付，旧后端没有 session 时不会降级保存 admin key。
- 账号：列表用 `limit/cursor/search/status/group_id/proxy_id`；编辑/删除/刷新/详情及关联日志优先稳定 ID；修改传 `version`。`account_id` URL 查询可直达详情。
- 导入：`POST /api/accounts/import/preview` 先预览，`dry_run:true` 才接受；提交 `POST /api/accounts`。每个被选中的条目单独提交，只有该条明确选择替换才用 `replace:true`。`results.index` 与错误行号按后端确认均为 1-based；源数组同一行多条结果仍按条目对应。
- 批量：`POST /api/accounts/batch` 逐项 ID/version/patch，不把部分失败报成全部成功。单次页面最多选择 100 项。
- 代理：密码留空省略字段，清除用 `clear_password:true`；读取仅使用 has_password，兼容剔除旧响应中的 password。
- 密钥：GET settings 使用 `admin_key_masked`；轮换调用 `POST /api/settings/rotate-key`，失败后在当前页面会话中复用同一 request_id；只有轮换返回的新 key 可复制，不写 localStorage/sessionStorage。
- 设置和 KEY 乐观锁：POST settings 的 admin_api 更新传 `settings_version`，成功接收新版本；GET/PUT keys 传实体 `version`。409 后读取最新值，等待用户再次确认操作，不自动覆盖。程序版本显示仍仅使用 settings 的字符串 `version`。
- 日志：按 `cursor` 翻页，`total_kind=not_computed` 显示当前页数量/更多。概览采用 `cache_rate_has_samples`、`timezone`、`retention_days`、`totals_scope`。

本轮 mock 证明前端契约与交互；真实隔离后端页面测试另有 8 项记录。后端持久化事务、真实导入去重、迁移和生产部署仍由后端/集成任务单独验收，未使用 mock 或页面通过代替这些服务端验收。

## 仍需后端支持

| 能力 | 当前真实行为 | 待确认契约 |
| --- | --- | --- |
| 账号跨页排序 | 服务端筛选/分页已接入；可选排序明确只作用于当前页，排序保存在 URL | 排序字段、方向及与 cursor 的一致性规则 |
| 独立手动验证 | 展示持久化最近验证/失败原因/credential_status；没有伪造验证按钮 | 验证触发、结果与并发规则 |
| 后台刷新进度 | 现有同步 refresh 显示“等待服务器完成”，成功/失败如实反馈；不生成假进度百分比 | 任务 ID、状态查询或事件流 |
| 日志独立详情/全量脱敏导出 | 详情为当前返回记录快照；导出仅当前页，严格去掉账号/密钥/代理/IP/模型和原始错误 | 单条详情与全量脱敏导出的查询/任务契约 |
| 代理诊断历史 | 显示真实本次检测阶段、时间、HTTP、IP 与错误 | 历史持久化与分页查询 |

上述能力未猜测新端点，没有假数据占位功能。本轮恢复已有管理契约中的并发、优先级、权重和额度窗口配置，未修改调度、计费、配额执行或路由实现。
