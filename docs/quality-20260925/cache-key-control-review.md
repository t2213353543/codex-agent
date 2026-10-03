# v9 prompt_cache_key 干预：独立控制审核

2026-09-25。本任务只审计划、源码与离线转换/runner，不调用模型，不刷新凭据，不调整生产缓存设置。实验问题限定为：同一完整原题请求、同一当前账号 epoch 下，保留默认 key 与每格显式新 UUID key 两种条件的可见响应有何不同。**新 key 不等于关闭缓存，也不保证 cached_tokens=0。**

## 历史反例先于本轮结果

已独立读取原始 native report：

| 同一 v7 批次 | 答案 | cached input tokens | reasoning tokens | 含义 |
|---|---:|---:|---:|---|
| [01-d-new2](../../.local/research-20260925/gateway-direct-controls-v7/01-d-new2/report.json) | 21 | 0 | 1266 | 缓存未命中也会答对 |
| [05-d-g7](../../.local/research-20260925/gateway-direct-controls-v7/05-d-g7/report.json) | 29 | 5888 | 516 | 缓存命中也会答错 |

两个反例来自同批 direct 原生 helper 请求，均 HTTP 200 正常完成。更早 v3 `01-q0` 也是 cached=0、答案21；v3 `04-g3` 是 cached=5888、答案29。因此不能把 cache hit 作为答对的充分条件，也不能把 cache miss 作为答错的充分条件。[v7 报告](../CODEX-QUALITY-GATEWAY-DIRECT-2026-09-25.md)

v8 刷新后的 g7 在同新 AT、相同已观察 body/header 下分别答 21（cached6656/reasoning2388）和29（cached0/reasoning425）。这提出值得受控检验的相关现象，没有消除采样、时间、连接、后端状态或缓存状态的混杂。v8 刷新前 g7 已两次21，不把其正确归因刷新，也不因刷新后一次29称账号被降级。[v8 证据](../CODEX-QUALITY-CREDENTIAL-REFRESH-2026-09-25.md)

## 8 格设计与可测边界

账号为执行前仍 enabled 且 usage allowed 的 new2/g7；g7 必须绑定 v8 正常刷新后的新 AT 摘要 `79dd4bf6…283ab`，不得沿用旧 `19107bed…88116`。new2 仍按当前完整 epoch 重读核验，历史快照不代替实时状态。disabled、移除、身份/AT 未登记改变立即停批，不换账号补齐。

两个条件各每账号两次，共8格，先四格交叉再完全镜像。建议顺序为：`fixed-new2, fresh-g7, fresh-new2, fixed-g7, fixed-g7, fresh-new2, fresh-g7, fixed-new2`；执行以最终事前 plan 为准，不能看到答案后换序。每格 fresh native helper 直连官方、cold、单 POST、零自动 retry/refresh/fallback，不走 Go 推理链；usage GET 的 helper 与模型 helper 分开。

一份新官方 CLI 0.155.0 首轮请求由零模型 capture 获取并冻结。原题、全部 input/指令/工具、model/medium、service tier、session/thread/window/device 元数据、非鉴权 header、request compression 都固定。fixed 条件保留原始 body bytes 和原 cache key；fresh 条件只替换**顶层 `prompt_cache_key` 的原 JSON value span**为该格新 UUID 字符串，前后其他每个字节不变，包括数字词法、Unicode 转义和空白。

四个 fresh key 在任一模型答案产生前生成并登记，逐格 UUID 唯一、与原 key 不同，也不能等于保留的 session/thread 身份值。plan 可保存 key 摘要及派生 body hash，具体 UUID 为非凭据、可在私有执行计划中保存；生成时刻和 pre-dispatch hash 必须使“预登记”可审。不能在失败后另生成 key 再请求。两次 fixed 的 key 在同账号保持，两次 fresh 故意不同，这是干预本身。

本轮测得的是显式 cache key 选择下的响应差异，不是纯 cache hit 因果试验。服务端是否命中由 response usage 观察，不能根据 key 条件补造命中；改变 key 也可能影响内部路由，因此即使结果变化也不能指定某个缓存节点或计算集群。各格 key 属共享输入的一部分，不能把8格当独立随机样本估计长期正确率。

## 官方源码与生产映射的范围

固定 CLI [core/src/client.rs](../../.local/codex-source-0.155.0/codex-rs/core/src/client.rs:497) 的 `prompt_cache_key()` 先接受内部 override，其后才是内部代理 parent key 或默认 `responses_metadata.session_id`。`with_session_context()` 为 `pub(crate)`，实际 session 初始化还会调用 guardian review-session 的 override 生成器。这支持协议中 key 与 session 可分离，**不证明普通用户有一个公开 CLI 配置开关生成本轮每格 UUID**。本轮属于已说明的协议字段干预，不冒称完全自然 CLI 行为。

当前生产 [gateway_prompt_cache.go](../../gateway_prompt_cache.go:17) 在设置启用、具有客户 key 和账号时，只对已解码字符串 `prompt_cache_key == client_metadata.session_id` 的默认 key 做稳定账号/客户 key namespace 映射；显式不同 override 透传。namespace 纳入 gatewayAccountRef 与 owner revision，同已知 owner 的正常刷新保持，身份迁移分隔。

[gateway_policy.go](../../gateway_policy.go:70) 另会在缺失/null/空 key 时生成默认值，不能与显式 override 混同；[gateway_sessions.go](../../gateway_sessions.go:24) 先使用 session/thread 等头做账号绑定，cache key 是后备。v9 的请求不经过这些 Go 映射，生产设置完全保持，因此结果不能叫生产网关缓存策略质量验收，也不能用它倒推出修改现网 key 映射一定有益。

## 转换与 runner 审核合同

- 输入完整 JSON 只解析用于校验和定位，拒绝任意深度 duplicate key、非法 JSON、非有限数、超界大小和错误类型；escaped 顶层字段名按解码后判同名。嵌套同名 key 不是替换目标。
- fresh 仅替换一个根 value span；固定条件字节全同，fresh 其他原 bytes 全同；任何附带 session/input/head/metadata 修改都是致命边界失败，不发送模型。
- capture SHA、工具/CLI/helper SHA、每格派生 body SHA、完整非鉴权头、实际 IPC body/auth/workspace 各层分别核对。派生头的 body长度/hash字段若有变更，仅属于本地捕获描述；真正出站 header 名称、顺序和值保持。
- 原 key 和四个新 key 的数值类型/UUID格式/唯一性、8格镜像和每账号条件次数，在事前 plan 和 dispatch 前重复校验。按计划绑定 g7 新 epoch；两个不同 alias 必须代表不同用户，不能仅用不同 workspace 判用户不同。
- 每格前后核账号 enabled/identity/AT 与 usage，真实 HTTP失败无答案不计错答。明确 allowed=false 跳过并保留，任何身份/哈希/投影/cleanup/persistence异常停后续，不能以“continue普通上游失败”掩盖边界错误。
- 写计划先于 dispatch；执行回执要区分计划格、adapter调用、实际 native POST，最多8。helper退出、socket删除、私密capture清理与最终 server/local byte hash 独立确认。

## 离线验收与本组合终态

转换器独立 [check_projection.py](../../.local/research-20260925/cache-key-controls-v9/review/check_projection.py) 7项通过，覆盖escaped字段名/value、Unicode前缀、嵌套同名、精确大数、duplicate、错误pin与重复头。runner完整main AST使用真实control core与真实projection，仅管理/usage/native I/O合成，[check_runner_graph.py](../../.local/research-20260925/cache-key-controls-v9/review/check_runner_graph.py) 最终15项通过。初稿4红为附带修改input/header未拒、native实际body/auth不符未停后续；已补独立span拼接、计划head摘要、实际发送证据逐项核验后转绿。

另验证全组任一账号事前不allowed或预检超过180秒时零模型、零模型plan；g7旧epoch、usage期间变更/disabled、持久化或cleanup失败都阻断。零实际dispatch回执保留`observed_request_boundary_matched=null`，不虚称验证过发送。对应runner SHA `84a68441d0d889aaaae407a14998320bb9c11de5146affb099fae8b0b0475668`，projection SHA `1cdbb121f059e36584d6de70e25aafb1d8f55a499cab5e49249adbdb8127cb31`；通过范围是离线合同，不是模型结果。

**原new2/g7组合最终未开始模型实验。** 新库存g7的v8刷新后同epoch AT得到usage401；随后一次独立只读GET确认固定错误码`token_revoked`，没有刷新、登录或模型调用。其前后identity/AT摘要相同，不能称本地过期或凭据被本轮偷偷替换，更不能反推撤销原因。原v9已归档`not_started_group_auth_precondition_failed`，模型POST=0、refresh=0，raw capture删除，capture unit已退出。[归档回执](../../.local/research-20260925/cache-key-controls-v9/experiment.json)、[认证诊断](../../.local/research-20260925/cache-key-controls-v9/auth-diagnostic.json)

若另选账号，必须在新的目录和计划中使用新的前置库存、capture、epoch及usage证据。不能把原v9零调用计入缓存效果，也不能以自动刷新g7后继续原计划混入第二种干预。
