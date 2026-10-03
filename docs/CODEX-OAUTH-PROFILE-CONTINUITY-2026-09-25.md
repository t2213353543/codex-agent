# 28765 子号 OAuth profile 连续性修复

2026-09-25 已部署 `20260925-oauth-profile-continuity-v3`，镜像 SHA256 `74c42b0e6adf1a10955b5fc575dd101c8a7f5dbada701db626575be1578bc418`。14:44:54 UTC 只读验收确认服务和 PostgreSQL 健康、调度开关与切换前一致。本版包含此前已上线的 [OAuth 设备绑定修复](CODEX-OAUTH-DEVICE-BINDING-2026-09-25.md)。

## 可复现的问题

子号重新授权会恢复自己的保存画像；`AuthResult` 返回授权流程实际使用的 profile。原 Team 后续代码却在 OAuth 验证、子号 AT/RT 请求、部分补推前隐私设置中强制选择母号 profile。独立运行真实方法和完整 `TeamClient` 构造器证明：子号 AT/DID 被配上母号 UA、Client Hints 与 curl TLS 模拟配置。

这证明本地环境选择跨操作不连续，未证明服务端因此给账号设置标记或降低模型计算量。该项目的画像标注为 `synthetic_protocol`，保持其连续性不等于把协议模拟变成真实浏览器。

## 上线行为

身份核验后的新 OAuth profile 优先用于本次请求；后续读取优先使用与实际 AT 匹配的独立 OAuth profile，再使用该子号已有保存画像。只有完全缺少子号画像时，才允许只读使用已有母号画像作为明确告警的兼容回退；不会调用画像生成或额外地理查询。母号自己的请求保持自己的配置，既有网络路由继续沿用。

新 OAuth profile 保存在 `extra.oauth_fingerprint_profile` 和 `oauth_profile_token_sha256`，与 token 原子写入，不覆盖浏览器 session/profile/device/cookie。不同 AT 未提供 profile 时不推断继承；保存 token 的投影不把历史兼容配置冒称本次 OAuth 观察。

非法 profile 明确报错，不以其它画像继续请求。已验证工作区、邮箱和用户身份的新凭据先保存，再进行画像及在线验证；验证失败不标记成功、不继续推送。损坏的可选 metadata 不导致已经签发的新 RT 被丢弃。子号刷新还增加写入前的用户身份一致性核验，避免相同邮箱、不同 user_id 的返回覆盖旧凭据。

## 验证边界

测试覆盖新鲜与持久化 profile、单飞等待、两条补推入口、RT 与 browser session 两种恢复路径、无效画像、凭据身份变化、分配取消、数据库写入拒绝、损坏 metadata、行锁等待和原子回滚。真实 `fingerprint_http_headers` 和完整 `TeamClient` 校验使用的 UA、语言、Client Hints 与 TLS recipe；现有 28 种画像模板通过兼容检查。

第一份 profile 镜像仅构建，因 `_child_client` 仍会在非法新画像后回退旧配置而拒绝上线。拒绝记录保留；修正版本使用新的不可变发布编号，不能将首次构建成功当作发布成功。

最终冻结的三个生产文件通过作者 17 项、两组独立 21 项和 20 项测试，以及真实隔离 PostgreSQL 7 项验证。所有原始红测、修正记录和被替代候选均保留。

第二份镜像的旧测试出现 4 个错误叶子，原因是两份夹具没有提供画像，且一处明确断言应使用母号画像。只为这两份测试提供固定子号画像、将该旧断言改为正确子号来源，并增加不调用母号画像生成的断言；原设备、身份隔离和代理路由检查保留。独立 AST 审核确认其它测试方法未变。

最终镜像从当前线上 208 文件清单构建，替换 3 个生产源文件和 2 个测试文件，其余 203 文件保持原字节。实际镜像完整导入成功，6 个旧测试模块共 77 项：66 项通过、11 项需要 PostgreSQL 的旧测试跳过，新增数据库逻辑另由上述 7 项真实 PostgreSQL 验证。

当前两个账号的保存画像仅在内存中传入无外网的最终镜像，以真实 `TeamClient.request` 向本地 HTTP 接收器发送合成凭据请求：两者的选中 profile、实际 UA/Client Hints/语言与 TLS 模拟 recipe 均保持一致，实际 curl recipe 配置受支持。只读聚合查询亦确认当前 2 个 active/pending 子号均有合法自身画像，无两侧都缺失的记录。这里验证的是实际请求构造和模拟配置，不冒称完成真实上游 TLS 或重新授权。

生产兼容边界：若未来导入的子号既无自身画像，又无可复用的已保存母号画像，会明确返回 `CHILD_OAUTH_PROFILE_MISSING`，不再临时生成画像。此行为是有意变更，不能称对缺配置账号毫无影响。

切换前相关 worker 全部空闲；上线后 207 个实际打包文件哈希一致（`.gitignore` 不入镜像），旧调度开关恢复。正式维护 checkout 已同步这 5 个审查文件，写入前逐个核对旧版本哈希，未覆盖其它未提交工作。验收 SSH 曾一次在握手阶段失败，未执行任何远程操作；随后只读验收成功。

回执：[线上验收](../.local/research-20260925/account-capabilities-v19/source-environment/profile-live-after.json)、[最终镜像测试](../.local/research-20260925/account-capabilities-v19/source-environment/review/profile-next/image-results.json)、[两账号实际请求构造](../.local/research-20260925/account-capabilities-v19/source-environment/review/profile-next/selected-profile-results.json)、[精确源文件审核](../.local/research-20260925/account-capabilities-v19/source-environment/review/profile-next/final-review.json)。

现有 CPA/Sub2API 账号导出和网关桥接不携带该 profile，本修复作用于源端 OpenAI 请求。本次发布验收没有主动重授权或调用模型，两个账号 AT 当时与只读快照一致。随后独立执行的 [v21 新凭据实验](CODEX-QUALITY-AFTER-ENVIRONMENT-REAUTH-2026-09-26.md) 已完成：两账号均换发 AT，new1 原题仍两次答29，未观察到恢复；new2 过程回执缺失已按观察性设计披露。

只读复核命令：`python .local/research-20260925/account-capabilities-v19/source-environment/verify_profile_live.py`。
