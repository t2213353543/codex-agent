# 管理设置恢复与可配置功能（2026-09-18）

用户要求：上版回退 UI 导致设置入口丢失，补齐全部功能开关并增加自定义设置。本轮保持恢复后的旧整体 UI，仅替换 Settings 页面；不得整包发布当前已被否定的新整体设计。基线以当前线上及最新日志页替换产物为准。

root 负责网关实际接入、页面构建桥/兼容打包及验收。settings_contract 负责 store.go Settings、admin.go settings handlers 和设置持久化/API测试。settings_frontend 负责 Settings.vue 和独立设置子组件/设置浏览器测试。settings_audit 只读核对线上/旧产物差异。

保留全部原设置：admin key 查看/复制/轮换、admin_api、WebSocket、CLI 身份开关及 linux/macos、内置提示词及内容、粘性会话及 TTL、缓存键补齐、系统信息。

新增字段契约：

| JSON 字段 | 类型 / 默认值 | 行为 |
| --- | --- | --- |
| http_stream_enabled | bool / true | 允许 HTTP Responses SSE；关闭返回 403，不影响原生 WS |
| http_json_enabled | bool / true | 允许 HTTP Responses 非流式 JSON；关闭返回 403 |
| compact_enabled | bool / true | 允许 Responses compact；关闭返回 403 |
| models_enabled | bool / true | 允许模型列表；关闭返回 403 |
| compatibility_normalization_enabled | bool / true | 开启现有参数/消息兼容转换；关闭不做该转换 |
| automatic_token_refresh_enabled | bool / true | 临期令牌自动刷新；关闭不刷新，未过期照常使用，过期明确拒绝；手动管理刷新仍可用 |
| upstream_401_recovery_enabled | bool / true | 上游明确401时同账号单次刷新恢复；关闭原样返回401 |
| builtin_prompt_mode | string / prepend | prepend 前置 / append 后置 / if_empty 仅未提供指令时补充 |
| upstream_stream_idle_timeout_seconds | int / 300 | 10..3600；HTTP上游流读取空闲超时，非整体截止时间 |
| upstream_response_header_timeout_seconds | int / 90 | 5..600；HTTP Responses/models 等待响应头 |
| upstream_compact_timeout_seconds | int / 1200 | 30..3600；compact 响应头与整体超时 |

新增 bool 持久化使用可判断缺省的字段，旧数据保持原行为；显式 false 必须可保存。保持 settings_version 乐观锁、部分更新与409处理，GET返回全部字段。不改账号或实际业务设置，测试以临时存储与合成上游为主。

2026-09-18 管理对齐 root 新持续目标：用户已确认“对齐现有 Codex 账号可用的全部功能”，范围 Accounts 与 Logs 全功能对照 Sub2API。独占 Accounts.vue、Logs.vue 及新增组件/utils/api；management_backend负责accounts.go/accounts_pg.go/admin.go账号handlers与注册管理route（不编辑settings函数），root负责新admin_usage_*.go，不改任何网关runtime。当前会话发现源码连接池在途导致短暂编译失败，已确认归另一授权源码会话；不会覆盖。新矩阵 docs/ACCOUNT-PARITY-MATRIX.md、USAGE-PARITY-MATRIX.md、MANAGEMENT-PARITY-BACKEND.md。UI继续旧壳，局部构建需同时保留settings会话新Settings模块，发布前必须联合冻结、不可一方以旧快照覆盖另外两页。root将新建build-management-parity.mjs，暂不改现有build-logs-ui.mjs/bridge。尚未部署，勿把在途新增API依赖当可发布稳定版本。

2026-09-18 设置恢复本轮完成并准备冻结：Settings.vue 已恢复旧白卡单栏，20字段（13开关+4数字+2选择+提示词）实际可保存，11新增功能已接入网关。Go全库245顶层/651含子测试PASS、39环境SKIP，vet PASS；前端63Node单测PASS，旧壳20字段行为与64页面/128截图浏览器验证PASS，最终手机留白微调在复验。局部构建 .local/settings-restore/release-dist，仅49文件，保留原46个静态资源及Logs；frontend/scripts/build-settings-ui.mjs/legacy-settings-runtime 独立工具可重现。我们未部署，不持有发布锁；管理对齐/集成owner联合发布请从此overlay继承Settings映射（新JS/CSS）并配套当前Settings后端契约，不能用旧47文件快照覆盖此页。报告 docs/SETTINGS-RESTORE-2026-09-18.md。本会话不再主动修改共享Settings代码；源码owner仍可继续已授权网关改动，但务必保留settings各开关与timeout接入。

最终冻结确认：手机白卡留白修复后的全部64页面/128截图与20字段行为复验已PASS；root已再次检查源码/49产物哈希，0差异，目视桌面/手机合格。settings-frontend.zip SHA256=803cc922530c1e65b1981585f80e7e3f05610b1e43c33f255ca935a7b7859de5；frontend清单digest=89ebf9ef4a807088965a7981f63433e7559313a60676165f6fba8ce70fac9047。该包只Settings局部修改且包含原Logs overlay。所有本轮代码/测试/构建工具已冻结，未部署。后续联合发布交管理/源码/集成owner依照共同冻结快照验收。
