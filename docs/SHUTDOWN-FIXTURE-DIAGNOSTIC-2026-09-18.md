# 退出刷新测试夹具诊断

范围仅不可变联合源码 `956fc90be91a922ac1593b283102bc5149674dac337564e6023479ec8e60a6f9` 的 `accounts_shutdown_test.go` 独立 Go overlay。未修改共享 Go、owner 冻结目录或正式测试；没有生产请求、远端访问或部署。

## 实际失败阶段

root 原始剩余 PG race 的 17 项中，16 PASS，只有 `TestPGShutdownPersistsRefreshAfterClientDisconnectAndSIGTERM` 在第 165 行 `gateway returned before refresh: <nil>` 失败。`<nil>` 是 HTTP 客户端 transport 没有报错，不代表业务请求成功。

本次同一隔离 Linux PG18/Redis8 race 中，只运行三个诊断 wrapper，原业务断言保持不变：

| 夹具差异 | 实际首 HTTP | 结果 |
| --- | --- | --- |
| 原夹具 | 401，固定分类 `invalid_api_key` | OAuth `started` 前失败，未进入客户端断开或 SIGTERM 阶段 |
| 仅持久化管理密钥 | 403，记录分类 `other` | 同样在 OAuth 前失败 |
| 持久化管理密钥并使用默认设置 | 请求进入刷新，随后测试主动断开客户端 | 完整断开、SIGTERM、drain、重新读取新 AT/RT、usage event/log/累计计数全部 PASS，1.50 秒 |

第二项 403 的具体 message 没有输出，不能把未记录原文写成实测。源码解释为 `Settings{AdminAPI:true}` 将 HTTPJSONEnabled 等 bool 设成 false，`gateway.go:567` 的 HTTP response mode 门禁在账号刷新前返回 403；第三项改用默认设置后完整通过与这一解释一致。

## 确认的夹具问题

- 原父测试 `accounts_shutdown_test.go:78` 仅保存 `Settings{AdminAPI:true}`，未初始化 PG 的 `admin_key` 文档。
- 子进程把管理密钥赋给 `Proxy.AdminKey`，但 PG 模式的 `gateway.go:84` 调用 `currentAdminKey`；`sessions.go:124` 读取 PG 文档，不回退信任内存字段。原 401 是该认证边界的真实结果。
- 当前默认设置除了 AdminAPI，还开启 HTTP 模式与自动凭据刷新等开关；只初始化 key 不足以让本 fixture 进入要验证的刷新生命周期。

建议由源码/测试 owner 修改正式夹具，最小初始化逻辑为：

```go
if _, err := s.DB.initializeAdminKey(context.Background(), shutdownTestAdminKey); err != nil {
    t.Fatal(err)
}
settings := defaultSettings()
settings.AdminAPI = true
if err := NewPGSettingsStore(s.DB).Set(settings); err != nil {
    t.Fatal(err)
}
```

正式修复后应重新冻结并运行本测试；不能把诊断 overlay 的 PASS 当成原 956 测试已修复或原 17 项整体通过。现有证据不支持“发生令牌丢失”的结论，原失败尚未到达相关阶段。

## 证据与重跑

```text
.local/source-20260918/local-linux-race-20260918t025639-f5dad9/report.json
.local/source-20260918/local-linux-race-20260918t025639-f5dad9/race.log
.local/source-20260918/local-linux-race-20260918t025639-f5dad9/diagnostic-summary.json
race.log SHA256: b12faf815e6b3a843b7a322375ca0a738c5718ed335ec8227cf4ff532b0a7cda
```

整次诊断 report 故意保持 `fail`，因为原夹具及只修 key 的两个对照确实失败；第三个完整初始化对照通过。无跳过、无 DATA RACE、未 OOM；临时容器、网络与凭据已清理，原 956 源文件哈希未变。日志只打印 status 和固定分类，没有请求/响应头、凭据或原始 body。

重跑诊断（本机 Docker，仅合成依赖）：

```powershell
python integration/gateway_environment/shutdown_fixture_diagnostic.py
```

只重新生成摘要、不执行测试：

```powershell
python integration/gateway_environment/shutdown_fixture_diagnostic.py --summarize .local/source-20260918/local-linux-race-20260918t025639-f5dad9
```
