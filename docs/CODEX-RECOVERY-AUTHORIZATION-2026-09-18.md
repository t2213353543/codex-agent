# 401 恢复后的重试授权

本轮补齐 HTTP 401 和 WS 握手 401 恢复成功后、再次发送前的授权检查。仅本地合成验证，不使用真实账号、不部署；Rust helper 和令牌持久化实现未改变。

## 已复现问题

首次请求收到上游 401 后，网关会刷新同一账户再重试。旧实现没有在这次重试前重新检查网关 key 授权，所以 OAuth 期间 key 被独立 writer 停用、轮换或移组后，HTTP 仍发送第二个模型请求，WS 仍再次握手并返回 101。

修复前先单项复现 HTTP 200、模型请求 2 次、OAuth 1 次；随后四种 HTTP 路由和 WS 握手的撤权/存储故障场景均复现，未变权限对照本来正常。

## 修复

HTTP 的 `doAccountRequest` 和观测包装器新增必须由调用者传入的授权回调，三个生产调用点分别覆盖普通 Responses/SSE（及共用路径）、compact、models。闭包保留真实客户端 token/key ID 和实际请求 context，不从已替换成上游 token 的 Authorization 头取凭据。回调仅在 `recoverUnauthorized` 成功之后调用，在 `GetBody`、clone 和第二次发送之前执行。

`authorizeGatewayRetry` 按稳定 ID 读取当前账户，检查完整 owner ref、启用状态、当前账户分组和当前 key 存储快照。授权撤销/账户删除为本地 403，账户/key 存储不可用为固定 503，owner ref 改变保留既有 409，调用者取消保持自己的 context 错误。不重新选择账户或代理，不增加重试预算。

拒绝时关闭首次 401 的 response body，并返回 nil response，避免 HTTP/WS 外层再用原 401 覆盖本地 403/503。合法调用保持唯一一次恢复重试，原请求 body/URL/必要头和原选定代理不变。WS 在第二次上游 dial 前执行同一检查，尚无业务 turn 时不虚构业务记录。

OAuth 刷新先完成原来的 AT/RT 持久化，再进行新授权检查；检查失败不撤销新凭据，不写入伪刷新失败。HTTP 记录的最终来源为 `gateway/authorization`，attempt 列表只含实际的首次请求 401 和 OAuth 200；合法恢复才有第三个实际 request attempt。现有日志脱敏规则保持，固定错误文字也受其处理。

## 保留契约

本轮不修改 `recoverUnauthorized` 本身。除既有 owner-change 409 特判外，其它恢复函数失败仍保留原 401 状态与诊断正文，例如当前账户在 PG OAuth 期间被停用后由原恢复函数拒绝。新 helper 的 403/503 适用于恢复成功后新增检查的失败，不能笼统说所有刷新期间撤权都会改成 403。已经受理的 SSE/WS 消息错误仍不触发这条重试路径。

这是发送前离散授权检查，不是存储更新到网络写入的原子事务，不撤回首次已发送的请求，也不承诺所有并发变化下 exactly-once。再次读取用于校验身份和权限，本次合法重试仍使用当前 recovery 成功得到的凭据，不擅自追逐第三方并发再次轮换的 token。

## 覆盖

- JSON/SSE/compact/models 四路 × key 停用/轮换/改组/损坏存储/未变，共 20 个 HTTP 子场景；WS 五个对应场景。
- 低层边界验证拒绝时 `GetBody=0`、第二次发送为 0、原 401 body 单次关闭；合法对照重建一次并发送一次；成功凭据持久化。
- 实际封存 Rust helper 通过 loopback TLS 的 HTTP/2 与 WS HTTP/1.1 握手，各验证首次 401、OAuth 成功、key 撤权、本地 403、无第二次派发、Go fallback 0。
- 独立 PG writer 在真实本地 OAuth 回调期间提交账户组/Enabled/key 组/Enabled 变化，验证刷新后的存储元数据不被旧快照覆盖、AT/RT 已保存、旧 Redis binding 保留和准入释放。

## 固定来源

- Go：`bbb0ed103f7b526f1fb131071ee5aaad44fad658aaece85c0480824ccb2715c0`。
- 快照：`.local/source-20260918/gateway-delta-20260918t101341-420264/source`，基于已验 5c9f 明确叠加十个 Go 文件。
- Helper：`6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`，未重建。

同一源码的原生/CLI 联调为 28 项 native 顶层测试 + 1 项 Go CLI 顶层测试（3 子场景），共 29 项全部通过、0 跳过；六份要求的场景报告存在且通过，临时容器已清理。新增原生授权测试两个子场景合计 0.28 秒，仅为本地样本。报告 `.local/native-transport-build/gateway-compare-20260918T101358-b22839/result.json`。

全库测试、`go vet`、Windows/Linux 构建通过。同一源码管理与网关联合 Linux PG16/Redis7 `-race`：472 项顶层测试、1180 个事件通过，0 失败/跳过/缺失，175.865 秒。347 网关项与 131 管理项去除 6 个重复项。新增 PG 四种并发变更子场景均实际通过，合计 0.91 秒；报告 `.local/management-parity/local-race-20260918t101639-96789b/report.json`。Linux 二进制 SHA256 为 `5c8ca2492c77c041230b1ed94b43f4f2ef5f75ed603697dd7d07bac3e2e5fcaa`。

## 复跑

```powershell
python tools/native_transport_compare.py --native-run 20260918T054243Z-b43907cb --source-dir .local/source-20260918/gateway-delta-20260918t101341-420264/source --suite gateway --with-go-cli
```

上轮新增准入原因的前端配套要求仍保留，本轮未修改前端或旧预览。结果不代表实网缓存收益、生产成功率或完整美国设备环境已验证。
