# v25 当前出口核验与执行器审核

2026-09-25 19:02:19 UTC 的三次无授权 trace GET 均返回 200。两个账号当前 source 代理出口相同，报告为 WARP on、IPv6、US/LAX；宿主直连为 WARP off、IPv4、US/LAX，IP 摘要不同。本次检查没有模型、OAuth、刷新或配置写入。

| 检查路径 | 当前配置/调用来源 | 当前 trace |
|---|---|---|
| new1 source | 实际 `_oauth_network` 选择 `config.oauth_proxy`，SOCKS5h | WARP on，IPv6 |
| new2 source | 同上，代理原地址与解析后地址摘要均与 new1 相同 | WARP on，IPv6，与 new1 同 IP 摘要 |
| 宿主直连 | 显式关闭代理的 urllib GET | WARP off，IPv4，另一 IP 摘要 |

检查时生产 Go 为 `54615b6e…`，helper 为 `bf920a0b…`，source image 为 `5d931132…`。Gateway 两账号均没有账户代理，进程也没有代理环境变量。Source 代理域名只能在容器内解析为私有地址，但宿主到解析地址的两次无授权 TCP 连通性检查均成功；没有发送 HTTP 或 SOCKS 授权数据。模型实验适配器因此显式使用该解析地址，并记录原 URL 与解析后 URL 的独立 SHA，不依赖宿主解析容器私有域名。

实际 source 调用中，重新授权入口以已保存注册 profile 作为 fresh AuthFlow 输入；授权返回的 profile/DID 按新 AT 绑定后，由后续 TeamClient 选择。不能把这两个阶段的 profile 强行当作同一字段。此次 trace 使用当前 child selector 的 AT 绑定 profile，两个账号的 profile/DID 都匹配当前 AT；未修改浏览器 session、cookie 或注册 profile。

两账号当前 `canonical(asdict(NetworkConfig))` 摘要都为 `3c5d6284…`，与 v21 事前计划的 `oauth_route_sha256` 一致。这证明两次观察的路由配置相同，不证明历史授权请求具有相同出口 IP。Trace 也不是原 OAuth 请求重放：source 采用当前 curl 客户端及所选 profile，直连采用 urllib，不能由该 trace 对比单独推导 TLS 差异或质量因果。

v25 执行器保留 8 格反序镜像计划，单格同原始 body/非鉴权头及同一 helper，仅切换直连与当前 source 代理。12 项 actual-main 离线测试通过，包括四组 usage 均准入才发模型、私有代理 URL 与公开摘要不符时零请求、逐格 route/epoch/实际 IPC/cleanup/返回 controls 失败即停止、禁止重试与补格。实际 native SOCKS/TLS 测试属于另一个工具门禁，离线执行器测试不替代它。

证据位于 `.local/research-20260926/route-controls-v25/`：`route-inspection-plan.json`、`route-inspection.json`、`review/inspection-review.json`、`review/runner-review.json`。原 inspection 脚本冻结 SHA 为 `5db0c158e4f8d44c2ab6969d832d3b6188e6d3c14b88893df18fc56f6bd3222c`；runner 审核绑定 `5eea2763fe2dee75ab30db04b5956596d8e1988c17788b10a4ddb4f503ee9755`。公开证据只保留路径类型、枚举、布尔值与摘要，没有 token、邮箱、完整代理 URL、DID、profile 原文或 IP 原文。

离线复核：`python .local/research-20260926/route-controls-v25/review/check_route_inspection.py`。
