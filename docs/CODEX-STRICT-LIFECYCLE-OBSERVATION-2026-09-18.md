# CLI 生命周期与尝试观测

本轮继续官方 Codex CLI 0.155.0 对齐，修复了真实 TLS 恢复差异，并补齐请求尝试的被动诊断。没有部署本轮代码，也没有真实模型推理。

## 实测驱动的修复

先前 helper 跨请求持有 reqwest client。在相同 loopback IP、同证书、服务端强制断连的对照中，官方 CLI 的 4 次自定义 CA 请求均完整握手，而 helper 后 3 次恢复了 TLS 会话。首轮各组使用独立临时端口，后续六组复验才固定同一实际 origin。服务端 `session_reused` 与 ClientHello PSK 扩展同时支持该差异，旧失败证据保留。

源码解释是官方 HTTP streaming 每次调用重建 API client；不是应该禁用 TLS ticket。现在原生 HTTP 默认采用 `CODEX_NATIVE_HTTP_LIFECYCLE=cli_request`，每次 HTTP exchange 新建官方客户端，同时保留 helper 进程和官方受限基础设施 cookie jar。只有显式选择 `pooled` 才跨请求复用 HTTP/TLS client，它是性能选项，不称完全复现 CLI 生命周期。Go 检查 helper ready 中的模式，缺失或不一致则拒绝，不静默接受旧二进制。WS 连接生命周期不变。

修复后六组真实实验（default native / custom CA × 官方 CLI / strict helper / pooled control）共 24 次请求、24 次 TLS 连接完成；官方 CLI 实际执行并返回工具结果。自定义 CA 下官方与 strict 都是 `[false,false,false,false]` 恢复序列，pooled control 仍是 `[false,true,true,true]`，因此并非通过禁用服务器 ticket 伪造一致。

HTTP/2 初始 SETTINGS 顺序和值一致：`(2,0), (4,2097152), (5,16384), (6,16384)`；连接 WINDOW_UPDATE 为 `5177345`，两端未观察到 priority 帧。默认 native 模式通过临时 `SSL_CERT_DIR` 完成 TLS 1.3 实握手，无 ALPN、走 HTTP/1.1，不能把缺失 H2 数据写成 H2 匹配。

这些证据仅覆盖同平台、该受控目标及所列字段，不证明生产 SNI、跨平台系统 TLS、所有服务器恢复策略、美国设备或出口地理位置。更细记录和复跑入口见 `NATIVE-HTTP2-RESUMPTION-2026-09-18.md`。

## 被动观测合同

`RequestRecord` 新增 sent/returned model、标准化 inbound/upstream path、准备/账号选择/代理选择耗时、有界 attempts、截断标记以及 observation/usage schema 来源。原 `Model`、计量与转发规则不改。返回模型只取真实 response，不用请求模型或 header 冒充。

每条日志最多 4 次尝试，按开始顺序记录。HTTP 401 -> OAuth -> HTTP retry 是三次；WS 401 -> OAuth -> 101 仅归首个准入轮，后续轮不复制握手尝试。命中刷新失败缓存、只重新读取凭据或等待锁，不产生虚假网络尝试。实际调用发送/拨号函数不等于服务器已收到或接受，缺少 headers/status 保持 nil。

新增观测字段只保存标准路由白名单（无域名/query）、状态、真实时间与有限错误分类，不保存提示词、工具内容、原始错误、账号凭据或 headers；原有 Error 字段仍按既有提取和脱敏规则保存诊断，未在本轮删除。独立阶段不是互斥分解：账号选择可能包含等待锁和 OAuth，准备阶段是在鉴权/设置之后，不能把这些时长相加当成总时延，也没有推断 auth/DNS/TLS 的缺失时长。

错误分类及截断回归覆盖：带 error 的 WS/OAuth HTTP 拒绝仍标 upstream/http_status；OAuth 200 非法结构标上游协议问题；models 304 不记成失败；有效 JSON 但结构不匹配时也可能进入原 200-byte 诊断 fallback；存储的 2000-byte 截断在原脱敏之后计算。长 bearer 被脱敏缩短的反例不误报截断。未知截断保持 nil，不把有限捕获的 WS 握手 tail 称为完整错误正文。

脱敏函数只是提取一个返回实际截断状态的 helper，旧函数返回文本保持不变；错误响应原字节、重试数量、usage 记录数及总 token 均有回归。

## 固定产物

Go 源码：`.local/source-20260918/gateway-overlay-20260918t044026-f04f7c/source`，SHA256 `4205b9633bcba32f9c14a2efc347051205cb8cb49962d1008a7d373cc9cdc237`。仍为已验管理 5a818 加明确网关/观测字段 overlay，不包含共享目录后续管理更改。

helper：`.local/native-local-build/20260918T042849Z-171e64e6/target-debug-codex-native-transport`，SHA256 `200bf8758823766918fc1799e006501c6e37401e2d3f560f11038ed3a899488f`。25 项 HTTP/UDS 与 4 项真实 WS socket 回归通过。最终 Go/helper 配对 13 个实际联调顶层测试通过，包括官方 CLI HTTP/WS、真实 shell 与 resume、SOCKS/TLS、受限 cookie、严格生命周期，以及显式 pooled 的复用对照。

同 Go 快照全库测试、vet、Windows/Linux 构建通过；Linux 真实 PG/Redis `-race` 227 顶层、485 通过事件，零失败/跳过/遗漏，110.322 秒，包含 owner 隔离、SIGTERM 轮换持久化、刷新/429 冷却、新诊断及模式握手。临时容器、网络与凭据已清理。Linux 网关 binary SHA256 为 `f3fb4607200f9f5986b5506c9bffb9c1496a109c9cbb6668cf60d8aea7a16cce`；不代表生产正在运行该产物。

## 真实入口

协作会话已发布的是较早稳定 956 网关，不含本轮 native。原授权第五专组的只读 `/v1/models` 返回 503；随后环境会话的生产诊断把该条定位为本地 account_selection 无启用账号，不是上游过载。已请求账号会话确认独立低负载窗口，未自行换组、启用失效账号或发起推理。

因此真实上游最终任务和缓存/成功率前后对照仍未完成。此前 84.22% 缓存占比属于旧候选历史任务，不移作本轮性能结论；本轮默认 strict 与显式 pooled 的取舍也不能凭合成连接数推断模型缓存收益。

重新运行配对联调：

```powershell
python tools/native_transport_compare.py --native-run 20260918T042849Z-171e64e6 --source-dir .local/source-20260918/gateway-overlay-20260918t044026-f04f7c/source --suite gateway
```
