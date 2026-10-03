# Codex 网关用户身份与刷新连续性修复（2026-09-25）

**2026-09-25 14:36 后续对照：** 新增三账号 HTTP／WS 共 12 格已完成：HTTP 四格完成、q0 两格 429；WS 六格完成。十个完整答案人工均为 29，未观察到切换 WS 恢复 21。两次 429 单列失败，不计错答。[原始 CLI 请求与传输对照证据](CODEX-QUALITY-HTTP-WS-2026-09-25.md)。下文保留前序批次与部署记录。

本轮修复已于 `2026-09-25T06:09:02Z`（UTC）完成生产验收，当前后端 SHA256 为 `bb92216f7ef2aa0f103513a4178dc2af675236c30953e423207bdc41aca277ad`。修复覆盖凭据用户字段校验、刷新后的身份换代、WS 响应链 cache key 保持及 HTTP 超限失败不留新会话绑定。同时替换了修复已接收 WS 帧被断开错误丢弃的原生 helper。发布基于并行任务已上线的 ddb 缓存版本，保留其全部其他变更。以下保留独立复现、前序候选和最终组合证据；尚未证明这些缺陷是先前答 29 的根因。

## 已证实的问题与修复

### 导入忽略官方支持的备用用户字段

CLI 0.155.0 在 `chatgpt_user_id` 缺失或为 null 时读取 `auth.user_id`。网关此前只检查主字段，导致同一工作区下，AT 的备用用户字段属于 A、ID token 的主字段属于 B 时，凭据组合仍可能通过导入并记录成 B。

`admin_account_identity.go` 现按官方优先级读取备用字段，并让它进入现有用户冲突校验。账号导入、Session 凭据解析和 OAuth code exchange 三个入口均有实际回归；显式选择工作区的既有语义保持。新增 10 个条件、三个入口共 30 个叶子场景，先观察到 15 个失败，再全部转绿。

### 刷新后旧身份掩盖新凭据归属

旧 `refreshAccountWithSettings` 只读取 AT、RT 和到期时间，忽略返回的 ID token，并继承旧 `Account.Identity`。当新 AT 已属于另一用户时，`mergeAccountTokens` 仍提交旧用户元数据；后续 owner revision 和 `gatewayAccountRef` 因而不变，HTTP／WS 的旧会话保护无法识别此次身份变化。

现在成功刷新会根据当前 AT 和返回的 ID token 重建身份：

| 响应情况 | 新行为 |
| --- | --- |
| 新令牌明确属于同一用户 | 保持 owner 连续性，更新套餐等当前声明；不继承旧的 verified 标志。 |
| 新令牌属于不同用户 | 保存已旋转的 AT／RT，并让持久化 owner revision 换代；旧会话不能使用该身份继续重放。 |
| 新 opaque AT 无可比较用户，或 AT／ID 声明冲突 | 保存已旋转令牌，清除陈旧用户身份；按未知 owner 隔离，不让旧元数据掩盖不确定性。 |
| 只有 RT 旋转，或 AT 原样返回且无新 ID token | AT 身份未变，保持原身份与连续性。 |
| 只有有效 ID token | 按官方可选字段语义独立更新身份，并与仍保留的 AT 核对。 |
| 只有无效 ID token，AT／RT 均未返回 | 没有旋转凭据需要保留；返回固定错误，原账户不变。 |
| 新 AT／RT 同时伴有无效 ID metadata | 保留旋转凭据并降为未知身份，避免丢弃已经失效旧 RT 的替代值。 |

上述身份修复的生产变化限于 `admin_account_identity.go`、`gateway_refresh_identity.go`、`proxy.go`。没有修改 TLS backend 或指纹模板。最终组合另加入下述缓存响应链与失败原子性修复。

### 合并当前生产缓存版本的两个边界修复

并行任务在北京时间 12:44:56 将 `ddb0f96d…6afad` 上线，包含账号级默认 cache key 聚合及兼容处理。完整源码摘要 `330de372…ca8e0f5` 与发布清单、源码归档逐文件匹配；与身份补丁没有重叠文件修改。[该次发布记录](FAILURE-CACHE-2026-09-25.md)。

对这份代码的独立 loopback 实验已经复现：同一 WS 响应链中切换缓存开关时，第二轮仍带原 `previous_response_id` 和空增量，却改变了 cache key；HTTP 请求在扩展 cache key 后超限返回 413，上游零次调用，但新增 session 绑定已留下。

组合补丁按最近响应 ID 的有界历史保留缓存策略和出站 key 摘要，已开始的链继续使用原策略，新完整链采用最新设置。默认 key 曾被聚合的链若变更 key 则拒绝增量，要求完整重发；启用改写但旧响应策略未知时同样不能猜测。完全禁用改写时，未知 prior 的已有透传行为保持。最近完成响应只保留 256 项，保留摘要而非原始 key；pending 响应也能查到策略。

HTTP 在选择账号及写入绑定前，按实际 JSON 字符串跨度校验确定的 key 扩展长度。超限本地拒绝不会新增绑定，也不会为清理失败请求而误删此前合法绑定。初版相关回归与追加边界各自保留记录；最终组合会重新验证全部代码。[缓存审计和复现](quality-20260925/shared-worktree-cache-review.md)。

## 验证与构建来源

### 组合回归额外发现原生 WS 已收完整帧丢失

旧 helper `f72e5c0f…9b556` 中，`tokio::try_join!(pump, output)` 在 pump 读到紧随完整消息的断开错误时会取消 output，导致已入队的 ACK 或完整消息没有交给 Go。组合 Linux 全套的真实原生 WS 用例抓到一次 EOF；固定组合源码再跑十次得到 5 失败，旧生产 ddb 源码同条件十次也有 1 失败，排除了只由本次缓存策略补丁引入的解释。

新增不依赖网络调度的 Rust 回归在同一次 poll 依次提供完整消息和错误，旧逻辑确定性失败。修复让上游错误按同一 FIFO 排在之前的 ACK／MESSAGE 后，形成 ERROR／END；输出写失败仍及时停止生产者，客户端取消仍立即停止，不把错误处理改成无界等待。TLS 库、依赖锁、HTTP backend、WS 配置及证书策略均不改变。旧 helper 的源码归档和运行工件已核对；实际生产 Rust 差异只在 WS relay。

新 helper 的最终完整验证已经通过：40 项 Rust、26 项 HTTP loopback、8 项 HTTP/TLS 边界，零失败；原 EOF 用例保持立即关闭行为连续 20 次通过，最终 Linux 全套也通过。首次三个新增网络测试因 fixture 缺少官方 fork 的扩展协商而在握手处失败，已只修正测试服务端配置，生产 relay 保持。旧失败日志与实际最终通过日志分别保留。[独立来源与审阅](../.local/research-20260925/identity-cache-integration/native-helper-review/provenance.json)、[决定性复现及修复](../.local/research-20260925/identity-cache-integration/native-cache-eof/README.md)。

身份单独快照摘要为 `08119c97968953d52acaf46ff0c233a52f4068a0dd7bafa96244afcb149d7076`，以 `020fa045…2065e1` 为基底。它构建成功但未激活。最新组合快照为 `8825314cf3ad227865ee876f22c4ed27076ff1e181b545bbfa5081fa011f9b84`，以实际当前 `ddb0f96d…6afad` 为基底，保留全部既有变更，再合入七个生产修复文件。合并清单明确记录三个源摘要及无重叠核验：[组合来源](../.local/research-20260925/identity-cache-integration/merge-manifest.json)。

新增刷新测试覆盖真实文件 Store、PostgreSQL Store、落盘再读取、身份换代、旧 HTTP／WS 授权边界、仅 ID／RT 响应及错误不污染旧值。为保留既有“同用户刷新”测试含义，29 个旧测试文件的 46 处成功 OAuth mock 明确补入同一合成用户的 ID token；原 AT 字符串和原断言保留，未知 owner 场景未改。[逐项夹具说明](../.local/research-20260925/native-auth-provider-v4/refresh-fixtures/README.md)。

Windows 最终全套 **803 通过、0 失败、148 条件跳过**，2688 个含子测试终态，vet 通过。Linux 最终 race／PG／Redis／native 全套 **927 通过、0 失败、23 opt-in 跳过**，2822 个含子测试终态，339.404 秒，无 DATA RACE，vet 通过。新增 PostgreSQL 刷新身份场景实际通过；跳过不计为通过。此前仅备用字段补丁的 Linux 922 通过属于独立前序快照，不能替代最终刷新版本的全套。[最终构建清单](../.local/research-20260925/identity-refresh-validation/build-manifest.json)。

另有独立 overlay 复核：**18 个合成叶子场景通过**，实际执行 HTTP 401 重试和 WS header/body 两种路由的握手，检查旋转 RT 持久化、不同用户不发送旧历史、同用户正常重放、旧 session 不迁移和日志脱敏。把身份重建替换为旧的陈旧身份行为后，六个场景如期失败，确认测试能捕获原类缺陷。[通过记录](../.local/research-20260925/refresh-identity-review/wire-green.log)与[mutation 对照](../.local/research-20260925/refresh-identity-review/wire-stale-owner-mutation-red.log)。

## 最终组合验证与上线

- Windows 全套：**822 通过、0 失败、148 跳过**，2764 个含子测试终态，vet 通过。
- Linux race／PG／Redis／native 全套：**946 通过、0 失败、23 opt-in 跳过**，2898 个含子测试终态，无 DATA RACE，vet 通过。15 个新增 PostgreSQL 刷新身份场景实际执行。
- 完整官方 CLI 0.155.0 对合成上游：**7 项、13 个含子项全部通过，零跳过**，覆盖 HTTP／WS、工具和连续交互；没有真实模型调用。
- 生产实测：HTTP models 200/9 项、WS 101/正常关闭、usage 200 往返、health ready、in_flight=0。带备用用户字段的异用户凭据通过 dry-run import 返回逐项拒绝，未写入任何账号。
- 原生 helper 更新为 `9110b869e65db6bf7664e39fcfe3b2ec17fa7a0207ddbeccf28d73dbc12ccce8`，修复 WS 已收帧在断开时丢失；TLS 依赖与证书／代理环境不变。Rust、HTTP loopback 及 HTTP 边界完整验证均通过，计数与原始日志见 native-ws-eof 证据目录。
- 八账号部署前后元数据相同，真实 runtime identity、设置和前端保持；没有数据库迁移，没有主动刷新真实 OAuth 凭据。

激活前发现并行发布的版本变化时，旧候选在修改服务前退出。最终组合以当前 `ddb0f96d…6afad` 为严格基底，激活前再次核对运行 SHA；没有回退别人已发布的兼容或缓存功能。[构建清单](../.local/research-20260925/identity-cache-integration/build-manifest.json)、[激活记录](../.local/research-20260925/identity-cache-integration/activation-result.json)、[生产验收](../.local/research-20260925/identity-cache-integration/runtime-smoke.json)。

切换阶段还实际触发过一次环境校验回退：旧 systemd `EnvironmentFile` 优先级覆盖了新的 helper `Environment` 设置。旧后端与 helper 已自动恢复并核验；最终改为追加仅包含 helper 路径及 SHA pin 的专用 EnvironmentFile，再成功切换。生产冒烟已纳入激活的自动回退范围；失败回执保留为 `environmentfile-priority-activation-failed.json`，没有将这次部署工具失败隐藏或算作成功。

缓存修复的兼容边界：启用默认 key 改写时，无法识别或已从有界历史淘汰的 prior 链需客户端重发完整输入。此次没有证明所有第三方客户端会自动重发；官方 CLI 的已测正常连续交互通过。禁用改写的未知 prior 保持透传。

## 八账号鉴权与目录的进一步证据

先前“官方 CLI 直连”使用 `name=OpenAI`、`requires_openai_auth=true` 的自定义 HTTP provider，仍是 ChatGPT 登录态。源码与 [OpenAI 鉴权文档](https://learn.chatgpt.com/docs/auth)均不支持将它误称为 API key 分支；但它关闭 WS／重试且手工装载 auth 容器，不能等同默认完整登录生命周期。

本轮对 q0、g1–g7 分别执行一次原生 GET 模型目录：八次 HTTP 200、每份九模型，28 对完整记录逐字段一致；返回体也都是 522612 字节、相同摘要。sol 的指令模板、推理选项、tier、tool mode 和 Lite 字段均相同。当前目录差异没有得到支持；这些 GET 不能还原早先推理时刻的目录，也不能观察隐藏集群。[目录复核](../.local/research-20260925/native-auth-provider-v4/catalog-review.json)。

随后在独立断网命名空间内，经官方 app-server 的 `initialize → account/login/start(chatgptAuthTokens) → account/read` 读取八账号本地认证状态。全部识别为 ChatGPT、`self_serve_business_prolite`，user/token/workspace 与前一轮登记相符，前后没有变化。没有 thread／turn、模型或网络 OAuth 调用；令牌仅走内存与 stdin。

初版协议读取器误拒绝官方启动通知；失败收据保留，修正依据是固定源码的完整通知顺序。最终八格均记录 `completed_with_known_zero_turn_warning`：主机未安装系统 bubblewrap，CLI 提示可使用内置副本。这一固定提示仅在零 turn 认证读取中被接受，`model_reference_clean=false` 始终保留，不能把此结果称为模型运行环境或服务端接受性证明。远程控制通知均为 disabled，进程、IO 线程与临时数据清理已核验。[认证复核](../.local/research-20260925/native-auth-provider-v4/auth-review.json)。

完整原生认证工具的最终 Linux 离线协议测试 24 项通过；旧 HTTP 参考工具另已补上显式 ID token 与 AT 同工作区不同用户的拒绝校验。先前 24 次答题均使用单 AT 输入，没有证据表明触发了该工具漏洞。实现及逐项源码依据见[鉴权路径审计](quality-20260925/native-auth-provider-audit.md)。

## 当前结论与复跑

八账号先前的答题结果仍为人工统计 3 次 21、21 次 29，没有覆盖或重新解释原始评分。当前八账号的登记身份与令牌一致，未在真实账号上观察到本次修复针对的刷新换用户事件；不能把合成复现当成已经证明这些账号的错误来源或恢复。

冻结快照的身份回归可从仓库根目录复跑：

```powershell
go -C .local/research-20260925/identity-cache-integration/verified-build-source test -count=1 -run 'TestGatewayRefreshIdentity|TestResearchIdentityFallback|TestCacheReview' .
```

该本地命令不代替 PostgreSQL、Linux race 或真实账号验收。身份单独证据位于 `.local/research-20260925/identity-refresh-validation/`，最终组合证据位于 `.local/research-20260925/identity-cache-integration/`，原始模型结果仍在 `.local/research-20260925/live-account-controls-v3/`。
