# A 组结构化输出校验器实测

2026-09-28 20:45 CST。实验位于 `.local/collab-a-20260928/structured-validator/` 独立 Go module，主项目 `go.mod/go.sum` 和冻结源码均未修改。没有读取账号、调用模型或部署。

## 已完成的可运行边界

锁定 `github.com/santhosh-tekuri/jsonschema/v6 v6.0.3`，依赖校验记录保存在实验自己的 `go.sum`。选择固定 Draft 2020-12，开启 format assertion，覆盖默认 loader 并拒绝外部资源。schema 和 output 均使用 `json.Number` 解码，重复键（含转义同名）、尾随文档、非法 UTF-8、孤立 UTF-16 surrogate 在校验前拒绝；原始 output 字节仍由调用者保留。

38 个顶层/子测试完成事件全部 PASS，0 fail、0 skip，源码前后摘要一致。覆盖：

- required、enum、additionalProperties、嵌套数组、min/max、精确十进制 multipleOf。
- `9007199254740993` 与相邻整数不会因 float64 舍入混为同值。
- `$defs` / 本地 `$ref` / 递归引用、nullable anyOf、email format、regex。
- 非法 schema、缺失本地引用、http/file 外部引用和未知 dialect 编译失败；外部 loader 仅记拒绝，不执行 I/O。
- Go regex 不支持的 lookahead 明确报错，不忽略约束后返回通过。
- 合法代理对/emoji/转义引号保留，孤立 surrogate 不被 `encoding/json` 静默替成 U+FFFD。

运行结果：[report.json](../.local/collab-a-20260928/structured-validator/report.json)，原始事件：[test.jsonl](../.local/collab-a-20260928/structured-validator/test.jsonl)。一条复跑：

```powershell
python .local/collab-a-20260928/structured-validator/verify.py
```

## 不能据此宣称的能力

库验证 JSON Schema，不证明 BPS 接受相同 schema，也不证明全部 OpenAI structured-output 特性已支持。普通 JSON Schema 忽略未注册 keyword/format；本实验显式测试了这个事实。产品入口仍需对支持的 dialect、keyword、format、regex 和 schema 形状明确校验，不能以库 compile 成功当成上游 admission 成功。

官方契约区分 JSON mode 与 schema strict，根 schema 为 object、严格对象的 required/additionalProperties 等限制需要单独处理。refusal 与 incomplete 不能被包装为成功 JSON。[OpenAI Structured Outputs 文档](https://developers.openai.com/api/docs/guides/structured-outputs)

当前原型的 `Compile` 返回通用校验器；为了确认数字/数组/anyOf 行为，测试允许非 object 根。这些是校验器单元能力，不能直接接入公开 `text.format` 而绕过其根对象约束。

21:01 交叉审查补充：B 组对主候选同一版本库的极端指数与超大 cardinality 已复现 panic/约束绕回，见共享协作入口的 B 组 runtime review。这些极端输入不在本实验38项范围内；本实验的直接库封装不构成可上线的资源/数值边界。正式入口须先拒绝不能精确表达或不能安全编译的约束、避免库panic传播，不能仅复用这里正常范围的大整数正负例便声称所有数值安全。B 组持有最小红测及 owner 修复，本组不重复修改同一实现。

真实 BPS 的既有证据仍仅为一个 strict function enum 正例与非法 schema 反例；直接 `text.format` / `response_format` 被拒绝。下一步在独立 projector 原型接入此校验器，验证公开输出、真实 usage、提交历史与失败终态的一致性；真实工具混合、续聊/密文/compact 尚需后续独立协议验证。

## 固定版本实现依据

固定版本 `Compiler.DefaultDraft`、`AssertFormat`、`UseLoader`、`AddResource` API 已逐项核对，避免误用 master 分支旧版本 API。[v6.0.3 compiler 源码](https://github.com/santhosh-tekuri/jsonschema/blob/v6.0.3/compiler.go)

v6.0.3 的默认 JSON decoder 保留数字但不拒绝重复键或孤立 surrogate；本实验在调用库前增加严格词法检查。[v6.0.3 loader 源码](https://github.com/santhosh-tekuri/jsonschema/blob/v6.0.3/loader.go)

## 单机缓冲成本

独立 20 次迭代 benchmark（Windows amd64 / Ryzen 7 9700X，预编译 schema、输入为 object 单个长 answer）：

| answer 长度 | 校验耗时/次 | 累计分配/次 |
| --- | --- | --- |
| 1 KiB | 6.0 μs | 5,600 B |
| 256 KiB | 1.06 ms | 1,573,092 B |
| 1 MiB | 4.72 ms | 6,290,929 B |

证据 [benchmark.txt](../.local/collab-a-20260928/structured-validator/benchmark.txt)。这里约 6 倍的累计分配不等于同一时刻的峰值内存；当前 decoder 建树路径仍有明显拷贝成本。不能把单机短基准称为生产吞吐。接入前应在完整投影、多并发、取消与大输出场景评估；仅用“先收完全文再验证”会增加客户端首字等待，必须单独观测，不能沿用内部函数参数首字时间。
