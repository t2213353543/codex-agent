# 重新推送凭据后，迟到检查与错误写回的补充修复

2026-09-27。本轮补齐两条已在本地复现的迟到写入路径，代码已同步工作区，**尚未部署**。此前线上列表按凭据版本区分历史请求的修复仍保留；本轮没有重新读取线上状态，也不把本地复现当成当前生产事故证据。

## 修复结果

管理检查缓存原来按返回顺序覆盖。实际本地管理 HTTP 复现为：旧凭据 usage 请求等待 → 重新授权保存新凭据 → 新 usage 返回200并缓存 → 旧请求返回401。旧实现把新成功挤掉，读取时只剩旧凭据的 stale 401，新额度缓存也无法复用。

`cacheManagedUpstream` 现在在存储的原子更新内，按每种检查的 CredentialVersion、CheckedAt 依次比较。旧代凭据不能覆盖新代，同代更早发起的检查不能覆盖更晚的检查；忽略旧结果不重写文档。发起旧检查的调用者仍收到自己请求的真实401，不伪造200。其他 kind、管理操作审计和业务指标保持原有语义。损坏的 `null` 缓存返回错误，避免写 nil map 时 panic。

另一条路径发生在文件存储的刷新锁释放后：网关仍按旧账号名称写 LastError，此时重新授权或改名已可能完成。本轮通过真实本地 OAuth HTTP 错误和现有缓存锁设置确定性的交错，复现普通选择、绑定会话两条路径：新凭据继承旧错误、改名后错误丢失、旧名称被其他账号复用后写错账号。

`noteAccountError` 改为携带选中账号的稳定 ID 和凭据版本，写入回调内再次比较；旧代错误不污染新凭据，同代改名仍可记录错误。重复相同错误直接终止无变化的存储修改，避免原来每次都创建临时文件、fsync、rename。这个文件路径问题不冒称线上 PostgreSQL 刷新失败写入缺少版本保护；后者已有凭据行锁和版本判定。

## 明确保留的边界

- CheckedAt 只有秒精度。同凭据、同秒的检查仍按完成顺序保存，不根据成功/失败偏好或账号管理 Version 猜测先后。
- 凭据替换本身不代表验证成功；尚无新检查时仍可展示旧结果为 stale。只有新结果已保存后，迟到的旧检查才不能挤掉它。
- 重置额度后的显式 stale 标记、隐私检查、自动任务的独立执行结果保持；管理缓存不会改变账号调度或模型输出。
- 账号名称与另一个账号 ID 存在歧义时，错误写入回调拒绝写错对象；本轮未扩大修改通用账号查找接口。

## 验证

冻结源407文件，修改3个生产文件、2个已有测试，新增4个测试文件。生产修改仅 `admin_account_upstream.go`、`gateway.go`、`gateway_sessions.go`。

| 验证 | 结果 |
|---|---|
| 原管理 HTTP／快照场景 | 旧实现6个实际叶子失败；修复后定向76 PASS、重复五轮90 PASS |
| 原错误写回场景 | 旧实现7个叶子失败，修复后重复十轮80 PASS，相邻88 PASS |
| 独立快照复核 | 13 PASS；凭据代次、时钟倒序、同秒、不同 kind、并发、reset/privacy、取消 |
| 独立错误写回复核 | 120 PASS；重推、同名重建、改名、ID歧义、重复无写及空参数 |
| Windows 完整测试 | **3142 PASS、382 SKIP、0 FAIL**，vet通过 |
| 编译 | Windows和Linux交叉编译成功；Linux产物没有运行 |

本轮同时核对代理准入快照、TLS/SOCKS桥和 Native WS 队列关闭，没有复现需要修改这些生产文件的新缺陷。新增 WS Go IPC 测试重复20轮140 PASS，相邻276 PASS；新增实际本地 TCP／标准库 TLS 场景三轮39 PASS，相邻24 PASS。代理参照14个测试事件通过，永久刷新失败记忆的本地控制40 PASS、相邻162 PASS。标准库 TLS 和 Go IPC 证据分别记录，均不冒充真实 Rust helper、生产指纹或官方 CLI 完整等价验证。

已有额度缓存测试原来调用生产写入函数强行放入“更旧”的测试状态，新排序会正确拒绝它，因此仅将 fixture 植入改为直接写管理测试存储，原缓存命中／失效／强制刷新断言全部保留。错误写回函数改签名后，一个旧测试的调用参数同步更新。初次 fixture 失败、漏改签名导致的编译失败，以及独立复核中的无效改名 fixture 失败均保留，没有当作额外生产缺陷计数。

## 可复核产物

- 源码摘要：`b6a6a70607feb1278186e166469a53b421be16536c67238886af1692e235776c`
- 归档摘要：`6979a746ec091ade5491dd192dc6e579508cb6e857c64671a2a469a51e1f5525`
- 完整测试原始日志：`14b16826e96d09ca9842aa5e2bd01da7716dac3bd7e531acec5dcd5284b7294a`
- Windows 编译：`c882227cfec5c877b0bab3bdad2ee92c7274e7c0e08c664ff38f42ae8676372c`
- Linux 交叉编译：`6e7ad29ce3c5170c6e0d79dcf878e343b837940a9a9a581e8247abb1ad973c26`

[汇总](../.local/research-20260927/transport-state/summary.json) · [修改清单](../.local/research-20260927/transport-state/combined-delta.json) · [构建与完整测试](../.local/research-20260927/transport-state/local-build/build-summary.json) · [独立快照复核](../.local/research-20260927/transport-state/review/snapshot-independent-review.json) · [独立错误写回复核](../.local/research-20260927/transport-state/review/error-note-peer/independent-review.json)

工作区中相关回归的一条复跑命令：

```powershell
go test -count=1 -run '^(TestManagedSnapshot|TestGatewayAccount(ErrorAfterRefreshAndConcurrentEdit|RepeatedErrorDoesNotRewriteFile)|TestNativeWSTransportState|TestTransportStateSOCKS)' .
```

本轮没有 SSH、模型、真实 OAuth、发布操作，未重试此前被平台拒绝的启动。407文件与工作区逐SHA一致，但 Linux 运行、race、真实 Rust helper 和 PostgreSQL／Redis 合并验证仍缺。线上最后一次已保存核对为2026-09-25T21:22:21Z的 `6ecdf93f…`，不是本轮候选。账号质量差异的根因仍未确定，本轮不声称糖果题已恢复。
