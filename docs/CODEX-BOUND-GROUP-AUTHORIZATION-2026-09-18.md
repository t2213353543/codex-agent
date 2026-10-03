# 绑定会话的当前分组授权

本轮修复已绑定 HTTP/WS 会话在账户移出允许分组后，仍可能被五秒调度缓存继续放行的问题。仅本地合成验证，不使用真实账号、不部署，不改 Rust helper。

## 修复

原路径从缓存中选出可用候选，再读取当前账户核对身份和 Enabled，但没有用当前账户 Groups 重新核对普通 key 授权。账户分组只是管理元数据，不改变 OwnerRevision，因此身份校验不能代替授权校验。两个修复前测试实际观察到撤组后的 HTTP 200/第二次上游调用，以及已连接 WS 的第二轮完成事件。

新增 `checkGatewayBoundAccountAccess`：检查调用 context、当前可识别的同一个 key、当前账户 Enabled，并对普通 key 检查其本实例当前 Groups 与重新读取的账户 Groups 是否相交。有效 admin key 保留跨分组访问，但不绕过账户停用或身份检查。

HTTP 绑定分支在账户重载后、`ensureReady` 完成后分别校验，拒绝时释放准入，不自动换账户，也不将权限变化记成 OAuth 失败。WS 每轮在账户重载后校验，并传入真实连接 context。HTTP 沿用既有选择失败 503，WS 沿用权限错误帧 403，不将撤组伪装成 owner change。

恢复分组后，同一身份和原 binding 可以继续。WS 授权拒绝后原连接会按既有行为关闭，需要以原会话重新连接；不声称原 socket 自动恢复。该修复不修改请求/响应内容、缓存 key、令牌用量、分组持久化或 OwnerRevision。

## 覆盖

- HTTP/WS 的当前账户撤组、停用、key 分组改变、未变及仍有交集对照；撤组后有其它可用账户也不迁移旧 binding。
- 恢复分组时 HTTP 原会话可继续、WS 重连后原 binding 可继续；管理员跨组、普通用户仍有交集、取消 context 等边界。
- 真实本地 OAuth 回调期间更新 key 分组：刷新得到的新 AT/RT 正常保存，原推理请求不发送、准入归零、不写入伪 OAuth 错误。
- PG/Redis 双 Proxy 场景由真实数据库修改账户分组，在保留 reader 旧调度快照时验证拒绝、恢复、管理员例外和停用；普通 key 索引仅在开始时明确同步一次，不把此测试称为跨实例 key 撤权广播。

定向复现及正控细节见 [授权回归复核](CODEX-SESSION-AUTHORIZATION-REVIEW-2026-09-18.md)。

## 基线集成

本轮切换到管理任务已验证的完整被动观测源码 `d52eda2accb3af36ea09269c7385b1875b4c50e709d19a132995dc24c76aad2d`，保留图像用量、客户端关联、阶段计时、独立 ingress 与配套查询。此前旧 bdf 基线不再冒充本候选来源。

精确冻结工具检查管理基线的源码集合/哈希、同源 race 结果、全库/vet/双平台四项本地检查及其证据哈希，再显式叠加十个网关文件，包含上轮已验证的 native proxy guard。工具八项单元测试通过。未覆盖、删除或回退其他任务的源码；本轮不重打管理前端，也不宣称新 UI 已重新部署。

- 最终 Go：`0a77f8c09cc7dd05e680097694728c963fd8dd190ac4d02e273ed52d76196618`。
- 快照：`.local/source-20260918/gateway-delta-20260918t091004-6eb373/source`。
- Helper：`6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`，未重建。

同一源码通过 27 个原生顶层测试 + 1 个 Go CLI 顶层测试（3 个子场景），共 28 项，0 跳过；六份必需场景报告存在且通过，临时容器已清理。报告 `.local/native-transport-build/gateway-compare-20260918T091019-5f515e/result.json`。

全库测试、`go vet`、Windows/Linux 构建通过。同一源码管理与网关联合 Linux PG16/Redis7 `-race`：447 项顶层测试、1077 个事件通过，0 失败/跳过/缺失，165.205 秒；选择范围为 322 网关项与 131 管理项去重合并。新增 PG/Redis 分组授权用例实际通过（0.20 秒）。报告 `.local/management-parity/local-race-20260918t091244-370d95/report.json`。Linux 构建 SHA256 为 `7716c92c884ca2fc94ce8f0872aa566ec63454d89bcd4742914df423cba720a2`。

临时容器、网络、编译镜像和测试凭据已独立确认清理；完整构建与清理记录在冻结目录旁的 `build-verification.json`。管理原有预览及数据库未改动。

## 未覆盖边界

本轮校验的是已绑定会话与 WS 后续轮次，不声称未绑定 `pickAccount` 调度缓存已经实现相同即时授权。普通 key 的 Lookup 仍是本实例索引，不声称其它进程修改 key 后立即广播撤权；admin key 有其原来的独立权威读取路径。校验是离散检查点，不是覆盖所有存储更新、刷新和网络写入的原子事务。

## 复跑

```powershell
python tools/native_transport_compare.py --native-run 20260918T054243Z-b43907cb --source-dir .local/source-20260918/gateway-delta-20260918t091004-6eb373/source --suite gateway --with-go-cli
```

以上结果不证明真实账号成功率、缓存收益或完整美国设备环境；用户取消的真实账号测试保持取消。
