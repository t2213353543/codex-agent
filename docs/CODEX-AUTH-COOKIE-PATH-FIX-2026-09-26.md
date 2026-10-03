# 登录设备与 Cookie 路径一致性修复

已部署 `20260926-auth-cookie-path-v1`，镜像 SHA `5d931132e1d19ae46a14a5fef885b5bce27e3a0ca104f0cbafb89960e9a49bd5`。服务和 PostgreSQL 健康，209个实际打包文件哈希一致，调度开关恢复。

原 cookie 查找只比较域名，并把名称转成小写；同名 cookie 存在多个路径时优先选长路径，却没有判断该路径是否适用于当前请求。真实 AuthFlow＋curl_cffi 的本地 TLS 请求证明：`/unrelated` 或相似但不匹配的 `/api/auth/sign` cookie 会被放进登录 `ext-oai-did`，实际发送的 Cookie 却只有根路径的另一个值。不同名称 `OAI-DID` 也可能被误当成 `oai-did`。

现在按 cookie 名的大小写精确匹配。登录 DID 查询使用 `/api/auth/signin/openai`，四处实际 session 读取使用 `/api/auth/session`；路径须完全相同，或满足目录段前缀规则。多个适用 cookie 按真实路径优先级选择，同路径保留 jar 顺序。旧诊断调用未传 path 时保持原 host 选择逻辑，没有改写已有设备值或 CookieJar。

候选只有 `auth_flow.py` 一个生产文件及一个新测试文件。11项新增实际本地 TLS/CONNECT 测试和105项相关测试全部通过。原代码有6个明确行为失败，另1个新API调用因旧签名不支持而报错，单独记录。最终生产镜像额外跑8个模块共97项：86项通过、11项依赖数据库的旧用例明确跳过，未把跳过计为通过。整个修复没有真实授权、模型调用或账号数据改写。

检查还覆盖了 profile 初始化、认证建立后的 TLS recipe 切换保护、明确重新开始认证时的 session 状态及 Sentinel adapter。没有因本次修复更换 UA/TLS 指纹或 Sentinel 算法；这些仍是合成协议环境，不是真实浏览器。没有证据证明历史错答账号实际触发过本次 cookie 冲突，也没有宣称质量恢复。

[实际请求复现及边界](../.local/research-20260926/auth-environment-continuity/review.md) · [冻结清单](../.local/research-20260926/auth-environment-continuity/candidate-manifest.json) · [生产镜像测试](../.local/research-20260926/auth-environment-continuity/image-results.json) · [上线验收](../.local/research-20260926/auth-environment-continuity/live-after.json)

只读复核：`python .local/research-20260926/auth-environment-continuity/verify_live.py`。
