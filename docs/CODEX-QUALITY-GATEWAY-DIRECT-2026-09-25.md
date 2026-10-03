# 同账号镜像：直连官方与生产设置的隔离网关

**后续凭据对照的重要更新：** [v8 刷新实验](CODEX-QUALITY-CREDENTIAL-REFRESH-2026-09-25.md)中，g7 在刷新之前、仍使用本批同一 AT 时已两次答 21；成功刷新后，同一新 AT、完整请求正文和请求头摘要又分别产生 21 与 29。因此本报告的四次 29 是本批观测，不能扩展为稳定账号分类。v7／v8 捕获的会话、缓存及环境上下文内容不同，不能把跨批变化单独归因于刷新或任一字段。

2026-09-25，v7，UTC 08:03:57–08:10:04（北京时间16:03:57–16:10:04）。**8个预登记请求全部完成：new2在直连和隔离网关各2次都答21；g7在两条路径各2次都答29。该批结果随账号保持一致，没有出现经过这条Go网关代码链后把new2的正确答案变错，或绕过网关后让g7恢复正确。**

这是“**当前生产源码/设置的隔离单账号网关代码链 ↔ native helper直连官方**”对照，**不是生产监听端口的全场景质量验收**。可以说隔离网关路径确实产生了正确答案；不能说所有生产账号/负载已恢复，也不能据账号关联差异断言服务端集群、风控标签或内部算力机制。

## 事前选择与固定项

new2/g7在模型请求前按当前enabled、fresh usage allowed、不同稳定user，随后按in-flight、近期网关流量、已用quota、alias排序选择。选择时两者in-flight与最近10分钟已持久化网关请求均0；primary quota分别0%/70%。历史disabled q0不参与，也未恢复或绕过任何停用账号。

原题保持不变，模型`gpt-5.6-sol`、effort `medium`，用户验收答案21不写入模型请求。使用同一份新官方CLI首轮完整raw实体作为两路入口。顺序在执行前固定：new2-direct、g7-gateway、new2-gateway、g7-direct，再全镜像逆序。每账号每路径2次；无重试、fallback、token refresh或错误后补格。

| 固定对象 | SHA-256 |
|---|---|
| 原题input | `cd771ba38f8f75696ba703c7d3e91e5273588c91cb8c4145e3ce4b077c075c3f` |
| 官方CLI 0.155.0 | `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501` |
| 实际native helper | `9110b869e65db6bf7664e39fcfe3b2ec17fa7a0207ddbeccf28d73dbc12ccce8` |
| 入口raw body | `95a7b9ce1d4dc57e8ba1f6e1447f8325512367ae2eae0b22e2686aeafe957bf5` |
| 入口raw head | `fe18f4321093e82de9b841ab78e1c33fb4f545aca6775794992940a94d849353` |
| 隔离网关test binary | `e5bcb3e80fe709016fc7fcfd0d125988fa96bad624936e195537885ee3977319` |
| 生产源码基线 | `b2ef2700cdfe6c6acc72d0a5014c21ad76847183bbe91b4492708777ef03d302` |
| 基线上仅新增fixture后的源码 | `1bd16a08d2f103763bf5e157f18a926eb616538073487b835c15ef3ebc32b5d8` |

预检记录运行生产binary为`a47774284a6be8416b9d0ed2a20b2fb8505f9064fbb9de38258ec534ae20136a`。fixture从对应冻结源码档案构建，只新增测试入口，没有混入共享目录后续开发的生产代码。

direct每格fresh helper、cold、无目录GET；gateway每格fresh handler、metrics、session状态和helper，导入本格唯一账号、固定普通客户API key ID。仅同账号的隔离account record/owner revision跨两个gateway格保留，以稳定cache命名空间；不复用生产客户key/生产record namespace。生产settings全部保留，只把自动refresh和401recovery设false保证最多一次dispatch，本地settings Version属于隔离CAS差异。

## 8格结果

所有格均HTTP200、`response.completed`、原评分器成功解析，无人工补分或无答案格。cached与reasoning为上游terminal usage的真实嵌套字段；0保留为明确数值。

| 格次 | 账号 | 路径 | 答案 | input | cached | output | reasoning | 请求采集耗时秒 |
|---:|---|---|---:|---:|---:|---:|---:|---:|
| 01 | new2 | direct | 21 | 6881 | 0 | 1542 | 1266 | 29.287 |
| 02 | g7 | gateway | 29 | 6926 | 0 | 753 | 516 | 70.400 |
| 03 | new2 | gateway | 21 | 6926 | 0 | 1832 | 1552 | 35.707 |
| 04 | g7 | direct | 29 | 6881 | 0 | 751 | 516 | 54.127 |
| 05 | g7 | direct | 29 | 6881 | 5888 | 829 | 516 | 57.167 |
| 06 | new2 | gateway | 21 | 6926 | 6784 | 1486 | 1284 | 28.035 |
| 07 | g7 | gateway | 29 | 6926 | 5888 | 673 | 516 | 48.273 |
| 08 | new2 | direct | 21 | 6881 | 6656 | 1586 | 1328 | 31.184 |

耗时是各路径collector的请求采集时间；gateway的fixture启动、IPC launcher与关闭还在外层格生命周期内，不能把此列直接当整格成本或生产性能基准。

| 账号 | direct两次 | gateway两次 | 本轮21验收 |
|---|---|---|---|
| new2 | 21、21 | 21、21 | 4/4完整答案为21 |
| g7 | 29、29 | 29、29 | 0/4完整答案为21 |

人工逐份读取公开final：new2四份都明确利用手感选形状、9个圆形加12个五角星得21；g7四份都用最大盲取失败集合28加一得到29。人工描述另存并绑定final hash，不改自动分数、不把可见final解释冒充隐藏推理。g7四格reasoning都为516，但final hash和output token数不同；这不是相同隐藏推理、输出缓存或固定私有预算的证明。

## 网关到底改了什么：实际出口证据

gateway端用hash固定的native IPC launcher旁路观察真正已经写给helper的head/body，核对实际helper运行SHA、唯一POST、完整写入、唯一Authorization token hash、workspace hash和正常退出。collector自身仅看到loopback，不能单独声称native出口观测。

完整body字段与逐input item的安全摘要对比得到：

1. **新增`instructions`：** 来源是已启用的生产BuiltinPrompt，232字节generic helpful/precise说明、prepend模式。原CLI Lite将指令放在input、顶层没有instructions；生产prompt policy有意新增本字段。把登记的原prompt按JSON string独立编码，摘要hash精确等于实际出口`f4f986337b0363dcd7b434bca07a56216dec24ab60709ab20c166abb81f232b4`。这不是“自动补空字符串”的旧差异。
2. **改写`prompt_cache_key`：** 启用生产account cache映射，new2与g7分属不同命名空间。每账号两个gateway格record ID/owner revision摘要、映射值和全部body field摘要相同，没有重复格随机换namespace。
3. **其余原始字段语义摘要全部一致：** 原input及每个item content hash、client_metadata、model、reasoning、text和其他控制字段未变。未发现题目、用户内容或tools被改写的证据。
4. **13个非认证header值一致：** 基于`all_headers`完整列表比较，不是仅plain摘要子集。UA、originator、version、session/thread、window、beta、routing hint、turn metadata、Lite等值hash都相同。Authorization/workspace按各自账号核验，未混入其他账号。

| 出口实体 | 完整body SHA-256 |
|---|---|
| direct四格原body | `95a7b9ce1d4dc57e8ba1f6e1447f8325512367ae2eae0b22e2686aeafe957bf5` |
| new2两格gateway body | `693e22df0369c05b795f238cf9c1f3d811e0b9467721fe524550e6038b5ed487` |
| g7两格gateway body | `a03006fb4eef9e4588d221f945d02c1fba8cba19c21597a7bb90d7b22ab75324` |

还存在预先声明的传输差异：direct保留捕获的zstd；本地Python→gateway入口发送原解压实体、identity编码，实际gateway出口IPC没有request_compression字段，即none。它不是“压缩完全相同”的实验。IPC摘要位于Rust压缩/TLS之前，不能用它声称TLS wire字节相等。两路径使用同一实际helper构建和相同应用UA，生产Go转发仍会产生额外过程与设置效果。

本轮结果说明这些已观测的网关修改没有使new2在两次隔离网关调用中失去21答案，也没有使g7从29恢复21；不说明这些设置对所有问题、所有账号永远无影响。

## 并发与当前来源记录

后验READ ONLY数据库聚合在8个实验时间窗内找到的同账号生产网关已完成请求重叠数全部为0；改善了v6 q0存在生产并发的明显混杂。查询按请求start及latency推结束，±1秒窗口，只看查询时已持久化记录；direct和隔离fixture请求本身都不进入生产日志。外部渠道、未完成/未持久化以及查询窗口之外的活动仍未观测，因此不能说账号绝对没有并发。

当前来源只读查验：new2找到对应登记记录且当前AT精确匹配，保存profile为macOS、`environment_source=synthetic_protocol`、version3，其profile hash留存。顶层device字段与profile内部device hash不同，但没有证据表明它们在同一次历史请求线上同时发送；不能把两个保存字段视作已证实的wire身份冲突。new2在本批仍四次21，因此不能把这份合成profile直接当作必然错答或环境完美的证明。

g7在此次查询的当前来源返回明确HTTP404；这只表示此来源当前未找到，不能推断所有历史来源都不存在或账号来自什么环境。本轮没有更改设备profile、Cookie、UA、token或来源记录，也不从这组元数据给账号贴内部风控标签。

## 独立核验与清理

结果审核脚本对全8格最终通过：`passed=true / findings=[] / full_predeclared_design_completed=true`。核对计划镜像顺序、原题/capture/工具pins、16份前后usage均allowed、身份四hash前后不变、单次模型dispatch、gateway实际IPC request/head/body/credentialhash、生产settings逐字段、fixture/launcher/helper哈希、终态usage、finalhash与冻结评分器重算。

每个gateway格有两个helper生命周期记录：一个仅启动校验、没有IPC exchange/上游请求的disposable helper，另一个执行唯一模型POST。没有把启动校验helper误计成第二模型请求。跨文件helper exit按instance ID匹配，文件系统枚举顺序不作为协议顺序。

UTC08:11:37服务器清理证明：unit `MainPID=0 / ExecMainStatus=0 / inactive / dead`，两个隔离account state、临时credential与raw捕获文件均已删除。服务器execution及32份公开证据文件（8 report、8 control、4 upstream evidence、4 server report、8 IPC JSONL）的原始字节SHA与本地独立重算全部匹配；每个helper正常回收，无强杀、无残留inflight。

启动前Python工具Linux177项通过、0跳过；fixture离线7项通过、live入口明确skip。独立自有14项故障测试通过，曾发现并修复SSE未知错误标识泄露、完整settings管理字段混入、prelauncher异常遗留凭据、direct边界错误不停批及IPC额外GET/重复认证未拒绝。详情见[设计与独立审核](C:/Users/Administrator/Desktop/项目/codex-agent/docs/quality-20260925/gateway-direct-control-review.md)。

## 结论范围与证据

最有价值的已验证边界是：**同一正确样本账号new2在direct与保留生产prompt/cache/compat设置的隔离Go网关两条路径均保持21；同一错误样本账号g7在两路均保持29。** 这支持本批条件下的账号关联差异，反对把Go转发路径当作该两账号差异的充分原因；不定位账号内部状态的来源，不证明私有服务端cluster或“降智标签”。

每条件仅两次、账号事先择选、固定session/cache可能保留服务端状态。不可把这8格与v5/v6或自然CLI补充合并成随机总体正确率；也不可把隔离单账号fixture等同于生产多账号路由、共享状态、负载或真实客户请求。

- [计划](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/gateway-direct-controls-v7/plan.json)与[执行收据](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/gateway-direct-controls-v7/execution.json)
- [独立逐格结果、实际字段差分及final](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/gateway-direct-controls-v7/review/results-final.json)
- [人工final判据](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/gateway-direct-controls-v7/review/human-annotations.json)
- [并发只读观察](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/gateway-direct-controls-v7/concurrent-traffic.json)
- [当前来源查验](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/gateway-direct-controls-v7/source-provenance.json)
- [退出与32份证据哈希核验](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/gateway-direct-controls-v7/cleanup-verified.json)

```powershell
python .local/research-20260925/gateway-direct-controls-v7/review/review_results.py --require-complete --output .local/research-20260925/gateway-direct-controls-v7/review/results-final.json
```

该命令只读本地证据，不联网、不发模型请求、不修改原报告。
