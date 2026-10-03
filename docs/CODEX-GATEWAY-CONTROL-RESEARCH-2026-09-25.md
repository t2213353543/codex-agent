# 控制字段研究：请求正文、路由提示与会话身份

本轮在 `ac25b0f0…d3d82` 基础上检查同一请求不同处理层如何解释 JSON 成员和 HTTP 会话值。确认两处真实不一致并做局部修复：路由提示可能不符合规范正文，续接会话可能因未知扩展切到另一账号，或者合并两个不同的上游会话值。

没有改变原题、model/effort 或通过标准；本轮不重复调用真实模型。下面的反例证明请求处理缺陷，不证明官方 CLI 会生成这些扩展，也不把它们自动认定为历史单轮 29 的根因。

## 路由头与规范 model/tier 不一致

官方固定 CLI 0.155.0 的 `build_routing_hint_header` 从实际 request.model 和 service_tier 构建提示。网关 HTTP 正文准备通过 JSON map 读取精确的 `model`，而提示生成器使用 Go struct 反序列化，会把未知 `Model`、`Service_Tier` 等成员也看作规范字段。

实际 HTTP handler → loopback 上游与真实 WS 两回合实验区分了两种情况：

- HTTP 正文会重新按 key 排序。普通未知 `Model` 字符串在规范 `model` 之前，最终规范值占优，这个控制原来就通过，不能宣称所有同类请求都错。
- HTTP 未指定规范 service_tier、仅有 `Service_Tier=priority` 时，旧代码却生成 `tier=priority`；未知 Model/Service_Tier 为对象或数字时，还会让合法规范正文失去路由提示。
- WS 在策略不修改正文时保留原字段顺序；后置的未知 Model/Service_Tier 可覆盖解析结果，使第一次握手提示指向另一个模型或 tier。后续第二轮仍复用同一连接，不能把握手头伪装成逐轮更新。

修复仅改 `gateway_routing_hint.go`：从最终 prepared JSON 的精确 `model`/`service_tier` 成员读取值。缺省/null tier 只生成 model；不可表示的空/错误类型仍不额外生成提示，不替上游新增校验错误；已有显式 hint（含未知扩展或不匹配值）仍保持原样。请求正文、reasoning、未知字段与 opaque 大整数均未改写。

失败前有三个顶级研究用例失败，显式 hint 保留控制通过；修复后关联 **18 个顶级、93 个含子项测试通过**，无失败/跳过。详细 HTTP/WS 反例及源码证据见[路由控制研究](quality-20260925/routing-controls.md)。

## 不一致的会话键造成跨账号续接或错误合并

`gatewaySessionKey` 在没有明确 session header 时，使用显式 prompt_cache_key 作为既有产品的亲和性 fallback。但这里的 struct 解码同样接受大小写别名，和 policy/实际上游看到的规范正文成员不同。

新双账号真实 TCP 实验先让账号 A 优先，首轮以规范 cache key 建立绑定；然后提高 B 的优先级，第二轮保持同一规范 key、previous_response_id 和 opaque history，只追加未知大小写 cache-key 字段。旧网关实际选择 B，上游的合成历史 owner 校验失败，返回 400；实际规范 cache key 和历史正文均未改变。这不是只看本地 session helper 的单元结论。

另一个真实 HTTP 实验核对 header 的线上字节：首尾 ASCII 空格/制表符由 HTTP 处理掉，与本地裁剪一致；NBSP 和全角空白却作为 header 数据到达上游，旧 `strings.TrimSpace` 仍把它们删掉，导致两个不同的上游 session 值被合并。

修复仅改 `gateway_sessions.go`：

- fallback 只读确切的 prompt_cache_key。未知大小写扩展既不分裂规范续接，也不能单独创建绑定。
- header 只裁剪 HTTP OWS 的 SP/HTAB；其他实际传输的 Unicode 字节保留在会话身份中。
- 显式 header 的既有优先级、显式 cache-key fallback、生成缓存前缀与会话身份的区分保持。自动生成的共享 prefix 仍不能把独立请求绑到同一账号。

修改前实际双账号续接与 Unicode header 共五个失败场景，另有未知扩展凭空建立亲和性的独立红→绿证据。关联 **50 个顶级测试通过、无失败**；Windows 中需要真实 native 的子项明确跳过，由最终 Linux 套件执行。详细证据见[会话身份研究](quality-20260925/session-identity.md)。

显式共享 prompt_cache_key 不一定在所有客户端中都表示一个会话，这属于现有产品 fallback 的限制，本轮不悄悄更换整体路由架构。标准 CLI 常用明确 session/thread header；本轮是把网关接受的其他兼容请求也按同一规范字段处理，避免本地和实际上游解释分叉。

## 集成与发布

两处修复已上线，当前后端 SHA256 为 `cdc07d09fbeac0d4b79ccdf27deb798eacfa95fea61da463f04623b9ad439752`。无在途请求时替换后端；账号 metadata 摘要、应用环境、前端/监听不变，没有数据库 migration。原生 TLS helper 保持 `f72e5c0f…b556`。

| 最终验证 | 结果 |
|---|---|
| Linux race + PostgreSQL/Redis + 真实 Rust helper | **873 顶级通过、0 失败、21 项明确 opt-in 跳过**，2524 个含子项终态，283.728 秒；零 DATA RACE，vet 通过 |
| 官方 CLI 工具/compact/恢复/WS 独立批次 | **7 顶级通过、0 失败、0 跳过**，13 个含子项终态，15.985 秒；实际 CLI 执行、合成上游输出 |
| Windows 全套 | **751 通过、0 失败、144 个外部环境跳过**，41.780 秒；vet、Linux 构建通过 |
| 发布后 HTTP/WS | 认证 models 200、9 个模型；WS 101 和 accept 校验通过，关闭后 in-flight=0、readiness=ready |
| 快照对应 | 全量和 CLI 的冻结 Go/SQL/module 输入与当前文件逐项一致，构建产物与运行进程摘要匹配 |

真实 Rust 双账号续接分支已在 Linux 全套执行；没有将 Windows 的 skip 算作通过。线上 WS 验收仅握手和关闭，没有模型回合。此轮无真实数学调用，不能把以上验证写成原题恢复。

证据位于 `.local/gateway-audit-20260924/go-controls-20260925-final/`、`go-cli-controls-20260925/`，以及 `.local/research-20260925/control-continuation/` 的 build-manifest、activation-before、activation-result、runtime-smoke。发布覆盖为 `zz-control-20260925.conf`，单独移除后重载/重启可回到上一已验证后端，不需回滚账号数据。

离线复现本轮边界：

```powershell
go test . -run '^TestResearch(RoutingControls|Session)' -count=1 -timeout=90s
```

本轮证明并修复的是 body/header 控制字段分叉、加密历史续接被错误换账号，以及两个上游会话值被本地错误合并。原题稳定通过和服务端账号标记/集群分配机制仍没有因此得到证明，原目标继续保留。
