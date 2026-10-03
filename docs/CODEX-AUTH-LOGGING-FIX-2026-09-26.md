# 授权选择与回调日志脱敏修复

已部署源端镜像 `20260926-auth-log-redaction-v1`，SHA `0f06162ae4da3c5e62062b962d5497dac3d29f4dc84ea9a15f9310ed75705525`。线上健康、PostgreSQL 正常，原调度开关恢复，208 个实际打包文件哈希验证通过。

原 `AuthFlow._choose_account_select` 会直接向 stdout 打印上游 response body、Location 和异常文本，另有 debug 日志包含 session ID、回调 URL。这些内容可能包含授权 code、state、token 或 cookie，也会干扰将 stdout 当作结构化回执的调用方。

现在四个选择/重定向/重新授权/回调方法仅记录固定路由、方法、状态码、是否有重定向以及异常类别。进展通过原 logger → QueueLogHandler → SSE 通道保留；授权请求、回调值返回及候选 fallback 顺序不变。其它 AuthFlow 方法不变；这不代表整个项目所有日志或显式启用的原始 trace 均已审计。

真实方法合成测试先复现12个失败点，修复后9项通过，覆盖成功200/201/302/303、无 continue_url 的 cookie 交接、500/JSON异常/网络异常与真实 SSE handler。生产镜像中7个相关测试模块共86项：75项通过、11项旧 PostgreSQL 用例因无数据库连接明确跳过。未触发真实授权或模型请求。

候选从当时线上 `74c42b0e…` 的208文件清单构建，仅改 `auth_flow.py` 并新增一个测试，其余207文件原样保留。正式维护 checkout 已按原文件哈希校验后同步，未覆盖其它工作。

[源文件与测试清单](../.local/research-20260926/source-auth-logging/candidate-manifest.json) · [实际镜像测试](../.local/research-20260926/source-auth-logging/image-results.json) · [上线验收](../.local/research-20260926/source-auth-logging/live-after.json)

只读复核：`python .local/research-20260926/source-auth-logging/verify_live.py`。
