# 账号来源的设备连续性：源码审计与本地修复

2026-09-25。已读取 sibling 的 `AGENTS.md` 与 `docs/production-stability.md`。基线为 `loophub-mother-checkout-20260920`，commit `92f39d7fd591d0a1e1934193169fe1e0e0866309`，开始修改前 `git status --short` 为空。根任务随后明确授权本地两处修复及红绿测试，覆盖仓库文档中旧会话“不跑测试”的偏好；未部署、登录、刷新凭据或调用模型，未读取 secrets/运行数据库。

**确证两处设备 ID 连续性错误，并已本地修复。** 这能证明来源程序会发送不一致的设备标识，不能证明它造成历史数学错答、账号风控或计算资源分配差异。母号画像被用于子号 Team 请求是另一个明确存在的既有设计，本轮没有改动。

| 完整路径 | 源码事实 | 当前结论 |
| --- | --- | --- |
| 注册/登录 → warmup → signin → auth 初始化 | [AuthFlow.warmup](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/auth_flow.py:2191) 从 CookieJar 读取 `oai-did`；原代码未写 `result.device_id`。signin 原代码见空结果就另建 UUID，auth 初始化又读 CookieJar | 已复现 A→B→A，见下文第一处修复 |
| 同一登录 → Codex PKCE → code exchange | [AuthFlow._exchange_codex_callback_code](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/auth_flow.py:1068) 使用同一 `self.session`、`_common_headers` 和代理；校验 state，再发送 code/verifier。授权跳转也用 `_navigation_headers`。`_rotate_impersonate_session` 遇 OAuth state、device_id 或 OpenAI/ChatGPT cookie 后禁止切换 | 没有发现正常事务中任意变换 UA/TLS 的路径。明确 invalid_state 的 bootstrap 重建会清 CookieJar/state，但保留画像；不能把这种显式重建混为静默设备随机化 |
| 已有子号重新登录 → 后续验证 GET | [oauth_repair.py](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/oauth_repair.py:128) 按账户读取已保存画像，创建独立 AuthFlow。新 token_data 含本次 device_id；[后验证](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:4145) 原代码仍读旧 child.device_id | 已复现新 OAuth 使用 B、紧接的验证使用旧 A，见第二处修复 |
| 子号加入/邀请/API/RT refresh | [TeamRotationController._child_client](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:3477) 使用 `_ensure_owner_fingerprint`，并以 child.device_id 创建 TeamClient；RT refresh 也传母号画像。上传前隐私请求的 upload_cred 同样覆盖为母号画像 | 会出现“子号保存画像 P → 母号画像 Q”的来源切换。现有 `test_task_environment_review`、`test_rotation_proxy_policy` 显式要求此行为；不是本轮新回归，不擅自修改 |
| Cookie/代理/403 轮换 | [TeamClient](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:1292) 每实例独立 CookieJar、锁及画像深拷贝；授权分支只复制同账号认证快照，不共用 Session。OAuth route 为母号专用→全局专用→当前 Team route，`{sid}` 按母号稳定派生；403 路径显式持久化新的同家族版本再新建客户端 | 未发现跨账号共用认证 CookieJar。多账号共享画像与共享 cookie 是不同事实。代理配置相同不保证动态出口 IP 不变，需要运行时证据 |

画像与传输的具体边界：[http_client.py:94](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/http_client.py:94) 给 curl_cffi Session 同时设置 impersonate、UA、Accept-Language 和 Client Hints，显式代理清 `NOPROXY`，SOCKS5 改 SOCKS5H，关闭环境代理继承；缺 curl_cffi 时有 requests 回退。`requirements.lock` 固定 `curl_cffi==0.16.3`，本地回归也确认该版本。TLS 目标、HTTP 提示与合成 VM 画像相互配套不等于存在真实浏览器硬件，仓库稳定性契约也明确说明协议 VM 的此限制。

## 第一处修复：保留 warmup 已取得的 signin 设备

离线抽取原方法，提供正常的 chatgpt.com cookie A 与无新 cookie 的 auth 响应，不导入 sibling 业务模块、不联网，得到：

1. warmup 成功，CookieJar=A，但 `result.device_id` 仍为空。
2. `get_auth_url` 生成 B，发送 `ext-oai-did=B`，当前 chatgpt.com cookie 仍为 A。
3. `auth_oauth_init` 读回 A；之后 header/Sentinel 使用 A。

修复位于 [auth_flow.py:2334](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/auth_flow.py:2334)：warmup 写回合法 ID；signin 优先使用自己目标 host 的合法 cookie，缺失时沿用已有合法 ID，否则只生成一次，并写入 chatgpt.com 的 cookie。只清理该域无效设备 cookie，不碰其他认证 cookie。auth 初始化无响应 ID 时沿用本次已发送值，不再无缘由新建。

“合法”定义为长度 1–128 的 ASCII 字母数字/`.`/`_`/`-`，可不变地用于 URL、header、cookie；没有凭空宣称上游只接受某个 UUID 版本。域隔离继续保留：如果 auth.openai.com 真正返回新的合法 cookie B，它仍优先于 ChatGPT 域 A，原 ChatGPT cookie 不被改写。源码原有 cookie helper 已明确允许两域分别设置设备 ID，不能为追求表面相等而覆盖服务端合法状态。

官方 CLI 0.155.0 源码不实现 Web signin 的 `ext-oai-did`，它把浏览器登录委托给浏览器。因此本结论来自来源程序本身的 URL/cookie/header 矛盾，不伪称是官方 CLI 的设备协议规范。

## 第二处修复：新 OAuth 结果的设备用于其后验证

[team_rotation.py:4145](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/webui/team_rotation.py:4145) 仍先核目标 workspace/plan，然后复用已有身份核对检查 email/user。新 `token_data.device_id` 合法且可证明同 user 或同一 AT 时，用它发送紧接的验证 GET。新设备类型/内容无效、身份明确变化或无法确认新旧身份关系则在创建 TeamClient 前拒绝；不继承母号 device_id。

旧 token_data 没有 device_id 时保留既有 child.device_id 行为。此处不写 child、不变更用户画像，不改变母号画像选择或 Team/OAuth 路由。若旧凭据完全不可识别，新 OAuth 又携带新设备，新增校验会明确拒绝这次后验证；这是避免跨身份继承新设备的可见限制，不能假装所有历史异常凭据都能自动恢复。

## 验证与可复核产物

新增 [test_auth_device_continuity.py](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/tests/test_auth_device_continuity.py)。修改前 8 个顶级测试产生 9 个失败断言；最终新增 10 个顶级测试，加已有 Session/cookie、Team 画像、OAuth route、代理策略共 **60 项通过，0 失败、0 跳过**。覆盖 requests 与真正 curl_cffi CookieJar 的域隔离；网络请求全部 Mock。三份修改文件均做语法编译。

复跑（在 sibling 根目录，使用已安装锁定依赖的 Python）：

```text
python -m unittest tests.test_auth_device_continuity tests.test_auth_session_retry tests.test_task_environment_review tests.test_oauth_route_integration tests.test_rotation_proxy_policy -v
```

证据目录 [source-environment](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/source-environment) 包含 `offline-repro.json`、`device-red.log`、`device-green.log`、`device-production.patch`、`device-freeze.json`。原始 baseline 方法位置及 SHA 在 offline repro 中，最终三个文件 SHA 和运行命令在 freeze 中；工作区仅两份生产 Python 和一份新增测试有改动，保留可审 diff，未提交或上线。

## 实际来源环境最少还需什么

需要从真正运行的 28765 容器核对以下脱敏数据，不能拿这个 checkout 的 README、组合数或画像 label 代替：

1. 运行 image digest、三份相关源码 SHA、Python/curl_cffi/libcurl TLS 版本、requests 回退是否实际启用；只记录 CA/proxy 环境变量存在性与配置摘要。
2. 同一账号一个登录事务的阶段序列：warmup、signin、auth init、code exchange、后验证。每步记录账号/user 摘要、事务 ID、UA/Client-Hints 摘要、impersonate target、profile 总摘要；分别标明 auth cookie ID、ChatGPT cookie ID、query ID、header ID 的哈希，不能把画像中的硬件 catalogue `device_id` 混为 `oai-did`。
3. 每阶段选定代理 URL 的带盐摘要、代理配置版本、探测时间、出口 IP 的带盐摘要及 country/ASN（确有观测才填）。现有 `_on_proxy_identity` 是可选回调；固定代理 URL 与当前 trace 不能证明全过程无出口漂移。
4. 仅记录 CookieJar 实例/所属账号标识和 cookie 的 name/domain/path/secure/expiry，值只哈希。重授权前后记录 AT hash/iat、持久 device_id hash、fresh result device_id hash，以及是否遇到 explicit invalid_state/403 轮换事件。

不要为此直接打开旧 `AUTH_TRACE_DUMP`：原 [auth_flow.py](C:/Users/Administrator/Desktop/项目/loophub-mother-checkout-20260920/auth_flow.py:621) 的 dump 会保存完整 Set-Cookie、响应体及 verifier，超出这里需要的脱敏边界。这个审计未启用日志开关，也未复制任何真实 Cookie/token。
