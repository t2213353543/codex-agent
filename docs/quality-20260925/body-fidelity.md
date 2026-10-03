# 完整请求正文的字节与内容差分

日期：2026-09-25。新增离线/真实 TCP loopback 测试，没有 SSH、账号请求、模型调用或生产代码修改。

## 结论与边界

在本次完整 CLI 形状的合成请求中，Go gateway 实际上游正文从 **2644 字节变成 2750 字节**，但完整 JSON token tree 相同，包括所有未知字段、不透明历史、精确数字和嵌套重复成员。大小变化可全部解释为移除排版空白及 JSON 字符串转义；没有证据表明该变化降低推理设置或丢失历史。

这个结果不能直接替代历史真实账号 CLI 的 `36243 → 36636` 字节差分。真实请求必须对其实际入站与实际出站独立运行内容比较；下文补充了根任务随后执行的官方 CLI→回环捕获，其请求与历史实时样本不同，不能倒推旧的 393 字节增量。不能看到长度不同就宣称故障，也不能仅因控制通过就宣称所有真实请求完全一致。

## 比较方法

旧的 `map[string]any` 比较即使启用 `UseNumber` 也会覆盖同名 JSON 成员，无法证明 RawMessage 子树完整保留。新增 `researchBodyCanonical` 使用流式 JSON token 树：

- `UseNumber` 保留数字原始词法，不经过 float64；`9007199254740993`、`1e1000`、`-0`、`1.2300e+009` 都分别保留。
- JSON 对象保存完整成员列表，按名称稳定排序；不同 key 的顺序不计作内容差异，同名成员的次数和相对次序保留。
- 数组严格保留顺序；字符串比较解码后的有效 Unicode 内容，因此合法 `\/`、`\u003c` 与字面字符等价。
- 节点类型显式编码，区别 null、布尔、数字、字符串、数组和对象。比较覆盖完整对象，包括意外增加的字段，不只逐项比较预选字段。

比较器故障注入控制确认：大整数相差 1、巨大指数变化、负零变化、数字词法重写、嵌套重复成员消失或换序、数组换序都会改变摘要；对象排版/不同 key 顺序/合法字符串转义不会。这里对数字词法的要求比数学值相等更严格，不宣称这是通用 JSON 数学规范化。测试只对合法 Unicode 输入给出结论，不将未配对 surrogate 的容错解码作为保真证明。

没有修改重复 JSON 成员的生产处理策略。本测试刻意把重复成员放在 opaque 子树里，检查它们作为 RawMessage 经过网关时是否仍存在，而不是为重复协议控制成员选择新的含义。

## 实际回环链路

`TestResearchBodyRichCLICompleteWireTreePreserved` 通过真实 TCP 入站调用完整 `ServeHTTP`，再通过真实 TCP 上游捕获已接收正文。测试四种组合：入站 identity/zstd、CompatibilityNormalization 关闭/开启。Linux 下提供固定 Rust helper 时，同样四组切换为 native transport；不能把 Go 的通过算作 native 通过。

请求包含 developer/user/assistant message、phase、reasoning/compaction encrypted content、function/custom call 与 output、encrypted_function_args、namespace、完整 tools schema、reasoning effort/context/summary、tier、text schema、cache options、previous_response_id、stream options、future 扩展。字符串包含中文、emoji、有效 surrogate-pair escape、`<>&`、斜线、反斜线、引号、换行和 U+2028/U+2029。嵌套重复成员和非 float64 可精确表达数字分布在 history、schema、cache 和 future 子树内。

该主 fixture 明确设置 stream=true、store=false、instructions 和客户端 cache key，关闭 builtin prompt。兼容规则在它上面没有预期变换，因此要求完整 token tree 一字不差。实际结果：

| 边界 | 字节数 |
| --- | ---: |
| 入站解压正文 | 2644 |
| 仅去除 JSON 排版空白 | 2564 |
| 进一步执行 HTML/U+2028/U+2029 转义 | 2750 |
| 实际上游解压后正文 | 2750 |

四个 Go 组合具有相同结果：

```text
ingress_raw_sha256 = 000d374cee720e1c569022b73f64c816642e612c0b965d99a064ec97f2052373
upstream_raw_sha256 = 881e20e1772fb55b9032fdbe718ebbe132ebb2bae194d58a3a1b0f11fa1424ce
full_tree_sha256 = 0165184995a3bf6f10af5a58ce6fa671644341d8f08dab5e96aaf1147cc54411
exact_tree_equal = true
```

raw hash 不同，tree hash 相同；106 字节净增长来自移除 80 字节空白后增加 186 字节转义。上游正文的对象 key 重排也会改变 raw hash，但不改变内容树。

## 明确适配规则的对照

`TestResearchBodyKnownAdaptersChangeOnlyDeclaredFields` 独立定义六组预期正文，实际调用 ServeHTTP 和 TCP 上游，不使用生产变换函数生成 expected：

1. caller stream=false/store=true 转成上游 stream=true/store=false。
2. 缺省 instructions 添加空字符串。
3. 显式 null instructions 保持 null。
4. input 字符串转成单个 user/input_text 项，文本保持原义。
5. 明确启用 builtin prepend 时，只在 instructions 前面加入指定文本。
6. 明确启用 builtin append 时，只在 instructions 后面加入指定文本。

每组都携带重复成员、精确数字和 Unicode 的 opaque 对照子树；比较完整最终树，确认没有白名单外的增加或丢失。builtin prompt 是用户设置明确授权的正文变换，不能把它算作“完全无变换”的请求，也不能用忽略 instructions 的比较掩盖它。

## 验证

新增文件 `gateway_research_body_fidelity_test.go`。最终 Windows 针对性验证：**17 个顶层测试通过，含子项 74 pass、1 个 native 分支明确 skip、0 失败，0.237 秒**；包括本次三组测试、既有 Q01、policy、opaque compression 相关控制。日志 `.local/research-20260925/body-fidelity-final.jsonl`；逐场景大小和哈希也在 `.local/research-20260925/body-fidelity-green.log`。

复跑：

```powershell
go test . -run '^TestResearchBody' -count=1 -timeout=90s -v
```

Linux 设置 `CODEX_NATIVE_HTTP_TEST_BIN` 为实际 Rust helper 后，同一命令启用 native 回环矩阵。根任务负责这个环境验证；本报告不预填未执行结果。

冻结测试 SHA256：`9ca5abc22fdbeb51be539e6ea3935216dfb5fd25eef4032eafec948133ddd524`。

## 补充：真实官方 CLI 与 Rust helper 的双轮压缩链路

已直接复核根任务生成的 `.local/gateway-audit-20260924/go-cli-compression-fidelity-20260925/compression-report.json`。该批次在 `--network none` 环境执行官方 npm CLI 0.155.0 → Go gateway → UDS Rust helper → HTTP loopback，使用合成 ChatGPT auth，不读取桌面 auth，不访问真实账号或模型。CLI 成功执行本地合成工具并把实际工具结果放入下一轮请求，最后退出 0；两轮 input/output、usage、admission 释放检查均通过。

| 轮次 | 入站解压正文 | 上游解压正文 | 增量 | 语义变化字段 | 序列化变化字段 |
| --- | ---: | ---: | ---: | --- | --- |
| 首轮 | 36806 | 37106 | +300 | 无 | input、instructions、tools |
| 工具结果续接 | 37787 | 38087 | +300 | 无 | input、instructions、tools |

每轮均核对 **13 个实际顶层字段**，缺失/新增字段列表为空。入站有 60 个未转义的 HTML 字符、0 个对应 escape，出站有 0 个原字符、60 个 escape；每个字符从 1 字节变成 6 字节，`60 × 5 = 300` 恰好解释全部增量。

报告中的完整语义摘要分别为：

```text
首轮入站 = 出站：fd91c78e652e25621067994f3ec0566824ee17dd0b89b53dc5bdc8e2224b4df3
续接入站 = 出站：9e12482616812227c60f7f7eb9ecac24c289cc52f6a196eb0c3ce1a8931154c5
```

这份报告采用根任务既有 semantic JSON 比较器；它与上文本任务保留重复成员及数字词法的 token-tree 摘要不是同一种摘要格式，不能相互直接比较。两者形成不同层面的证据：真实 CLI 双轮常规正文无语义变化，以及专门构造的复杂 opaque/重复成员/精确数字正文在独立严格比较中保持完整。

运行时 CLI SHA256 为 `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`，helper 为 `f72e5c0ff8798c02f430f7aa36f74009dad40fade6111dad722390891239b556`。两个外部 hop 都是 loopback HTTP，因此这不是新增 TLS、外部代理或真实账号模型质量证据；也不证明历史实时请求的 393 字节差分来源。

随后根任务将严格 token-tree 比较接入实际 CLI 压缩链并重新冻结执行，最终报告为 `.local/gateway-audit-20260924/go-cli-compression-parse-fidelity-20260925/compression-report.json`。已直接核对 `passed=true`、network none、两轮 `content_tree_preserved=true`、13 个字段无新增/丢失/语义变化，以及工具结果回传成功。新批次正文大小为 **36809→37109**、**37790→38090**，每轮依旧只有 +300 的序列化差异。

这次真正采用上文严格比较器的完整内容树摘要是：

```text
首轮入站 = 出站：e084bf8b0262537819c3709aff6d3031716cce96bf29d1564d5b88b3854f411f
续接入站 = 出站：a04e861b3b70de013c623cf8c031949914ba457d20c35a56ffc8025543dfd696
```

新旧两份报告对应不同冻结批次，不将它们的原始大小或摘要混用。最终 tree 证据加强了真实官方 CLI 常规双轮请求的正文保真结论；仍不能倒推历史真实账号样本的 +393 来源。
