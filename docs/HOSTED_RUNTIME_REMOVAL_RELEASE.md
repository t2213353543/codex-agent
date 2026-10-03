# 官方托管运行时移除：发布验收说明

本次产品方向已调整为 PowerPoint + Codex，保留可选 Codex 直连。**移除的是生产运行时实例、可选模式与公开托管入口，并非把所有历史协议实现和测试源码从仓库删除。** 保留的 App Server helper、纯函数和 fixture 用于历史兼容/回归，不构成上线时自动启动官方会话进程的授权或证明。

当前源码检查由 [audit_runtime_removal.py](../tools/audit_runtime_removal.py) 完成：

- `internal/server/main.go` 不调用 `newOfficialRuntime` 或 `newAppServerCLI`；仍初始化 BPS 和直接网络 transport。
- 管理端更新 `official_app_server` 明确返回 HTTP 400；可选项仅 PowerPoint + Codex 与 Codex 直连，二者 `hosted_runtime=false`。
- 在真实生产构造路径的 `p.Official=nil` 下，`/v1/codex/*` 返回 HTTP 404，不创建任务或官方 App Server 会话。
- 页面 HTML 与 router 标题为“高级反代系统”，模式组件没有硬编码的 App Server 选项。
- 已撤销临时未完成的官方 remote-image resolver：文件和两个调用点均移除，没有将这段未验收功能带入发布。

复跑只读源码检查：

```powershell
python tools/audit_runtime_removal.py --output .local/latest-operations/runtime-removal-source.json
```

该结果包含被检查文件 SHA-256。它证明当前源码的构造和入口条件，**不单独证明线上已经换成该二进制**。最终 release manifest 仍须关联编译 artifact、Go/前端检查、上线后 `/proc/<pid>/exe` 哈希、当前模式，以及进程树中不再启动官方会话进程的实际检查。历史配置枚举仍能解析旧 `official_app_server` 值以便迁移；管理写入口已禁止重新启用它。

## 错误文案与成功内容的边界

`internal/bps/request.go` 的四条参数错误原始构造文本仍有 PowerPoint 字样，但返回 `unsupportedParameter.Error()` 时会经过 `internal/publicerrors.Message`，对外错误已规范化。这属于错误边界处理，不需要修改所有源码字符串。实际模型 output、工具输入输出、上游身份 header 和成功 response 内容不得因包含 PowerPoint 而改写。配置页中的“PowerPoint + Codex”是当前模式名称，不应与模型成功输出混为一项验收。

## 未改价格的原证据复用

价格与费用引擎没有变化，因此复用 release `8215cf12668e4a5c5bb7b53ea0f05521dd5151289b46e9deaf093531c7edce69` 中 `gates.cost_parity` 的原始证据，而不为通过新的上游目录校验而修改价格。已逐文件核对当前三项源文件与原 acceptance manifest 完全一致：

| 文件 | SHA-256 |
| --- | --- |
| `internal/server/admin_account_window_cost.go` | `ee0a5c7a50bc75c5ac03ceeeab1f2332bfa94f8704d3e6c50be31c2e244e39e5` |
| `internal/server/account_window_prices.json` | `6426c36aca403b9f91c96837e78cf1c4e2745fcf058bd6e9a754dbe9d66ef3be` |
| `internal/server/admin_account_window_parity_test.go` | `617a5446be86d9148f59c8a9ddb44212c5ed08ef8b72e563c12a432a0a28b278` |

原 588 个费用对照 case 的报告和 Go JSONL 已按原始字节复制到 `.local/latest-operations/sub2api-cost-parity.json`、`sub2api-cost-parity-go.jsonl`。两个文件哈希分别为 `6a7200738416bd8e841e92d42635e2579a2fae6d807093ff3dd46d4d8495e9dd`、`d1ae201e894a01f4cf9a1e81ecfba6b5de886aac1da29e4137ec763efb180522`，均与原 manifest 一致。来源与当前文件相等证明保存在 `sub2api-cost-parity-reuse.json`。

这项复用证明未变的价格/引擎仍具有原冻结 Sub2API 目录下的验证证据。它不声称最新外部目录未变化，也不把本次新目录校验失败改写为成功；`new_upstream_catalog_verified=false` 已明确写入复用记录。

每分钟只读线上观察已独立启用，见 [监控说明](CODEX_AGENT_LIVE_MONITOR.md)。发布后以新进程和新时间窗口的真实 BPS 请求检查错误、usage 与缓存，不能将旧 App Server 流量的命中率当作新发布效果。
