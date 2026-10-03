# 账号模型目录与能力配置对照

本轮只读采集的 q0、q1、q4 模型目录一致。三个账号均收到 HTTP 200、HTTP/1.1、522,558 字节，每份包含 9 个模型；三个账号两两比较的模型集合、每个模型的 52 个原始字段以及字段内嵌内容的哈希均一致。套餐白名单字段均为 `self_serve_business_prolite`。这次目录采集没有调用模型推理，也未替换任何账号的模型目录。

证据为 [catalog-report.json](../../.local/quality-20260924/catalog-report.json)。脚本对完整模型记录取哈希，并按相同 `slug` 逐字段比较；没有只挑糖果题使用的模型、只比较字段名称，或忽略只在一方出现的模型。共 9 个模型为 `codex-auto-review`、`gpt-5.5`、`gpt-5.6-luna`、`gpt-5.6-sol`、`gpt-5.6-terra`、`gpt-6-astra`、`gpt-6-luna`、`gpt-6-sol`、`gpt-reserve`。

本结果只证明采集当时返回给这三个账号的目录元数据相同。目录不是每次 Responses 执行的路由证明，也不揭示后台集群、算力、采样实现、账号风险标记或未公开实验参数。因此它不证明模型实际执行相同，不证明异常账号已恢复，也不能把过去糖果题错答归因于目录差异。访问令牌中的套餐字符串只是声明事实；对比工具不验证 JWT 签名，不把套餐声明当作质量保证。

## CLI 0.155.0 源码核对

以下源码位置均相对于工作区固定快照 `.local/codex-source-0.155.0/codex-rs`，未修改此快照。它不是具有 `.git` 元数据的检出目录；这些是当前本地版本快照的实现证据，本轮不额外声称已校验上游提交签名。

| 边界 | 源码位置 | 本轮确认的行为 |
| --- | --- | --- |
| 请求 URL | `codex-api/src/endpoint/models.rs:34` | `GET models`；追加 `client_version`。`models-manager/src/lib.rs:16` 将构建版本规整为 `major.minor.patch`，本轮为 `0.155.0`。 |
| 请求认证 | `model-provider/src/models_endpoint.rs:80`、`model-provider/src/bearer_auth_provider.rs:34` | 从当前 auth/provider 构建客户端。OAuth 使用 `Authorization: Bearer` 与 `ChatGPT-Account-ID`；声明 FedRAMP 时另加 `X-OpenAI-Fedramp: true`。 |
| 普通请求头 | `login/src/auth/default_client.rs:335`、`model-provider-info/src/lib.rs:454` | 默认客户端提供 `originator`、`User-Agent` 和适用的 residency；内置 provider 提供 `version`，并可读取配置的 OpenAI organization/project。ModelsClient 调用的额外 header map 为空，没有模型执行用的会话 affinity 提示。 |
| 原生传输 | `model-provider/src/models_endpoint.rs:120` | 通过 route-aware `HttpClientFactory`/`ReqwestTransport` 执行，目录刷新外层超时 5 秒。本轮实测由父任务复用服务器 native Rust helper；Python 对比脚本不使用 requests、curl 或另一套 TLS。 |
| 目录格式 | `protocol/src/openai_models.rs:400`、`:768` | 使用 `{models: [ModelInfo, ...]}`，不是公共 `/v1/models` 的 `{data: [...]}`。支持 API key 的模型字段是 `supported_in_api`，不是 `supports_apikey`。后者若出现在原始输入中会保留差异，但工具不将其当成 CLI 同义字段。 |
| 能力字段 | 同上 `ModelInfo` | 比较 reasoning levels/default、tool mode、context/max context、input modalities、visibility、supported_in_api、truncation、verbosity、service tiers 等全部原始字段。`tool_mode` 的当前已知值为 `direct`、`code_mode`、`code_mode_only`。 |
| 指令字段 | `protocol/src/openai_models.rs:783` | CLI 可将旧 `base_instructions` 提升到缺失的 `model_messages.instructions_template`。本工具保持原始字段，不执行此归一化；两种 wire 表示可以语义相近但仍被报告为原始差异。所有指令和工具说明仅保留哈希。 |
| Cache identity | `model-provider/src/models_identity.rs:14` | 包括 provider/URL/query/auth mode，以及账号、用户、email、plan、FedRAMP 等身份摘要。有稳定账号和用户/email 的 ChatGPT 身份不以正常轮换的 AT 作为身份；显式 bearer/API key 等不满足稳定所有者条件的情况仍纳入认证 header 摘要。工具不输出或复制这些身份字段。 |
| 缓存复用 | `models-manager/src/manager.rs:32`、`:475`、`:552` | 默认 TTL 300 秒，要求版本及 identity 匹配；网络返回和异步写缓存后仍检查当前身份。新账号、套餐或 provider 不应复用其他身份目录。 |
| 目录与回退 | `models-manager/src/manager.rs:382`、`:518`、`:728` | 刷新失败会记录错误并取当前可用目录；身份不匹配时可回到内置目录。可见的 ChatGPT/OpenAI API-key 远程目录具有 authoritative 语义；其他情形可合并内置目录。缺失 slug 可以构造 fallback metadata。这些路径不能作为实时账号目录的等价证据。 |
| 认证白名单 | `login/src/token_data.rs:29`、`protocol/src/auth.rs:84` | `https://api.openai.com/auth.chatgpt_plan_type` 为已知套餐声明；`chatgpt_account_is_fedramp` 是路由声明。源码另解析 account/user/email，但报告全部排除。未知套餐仅留类型/长度/哈希，不猜测含义，也不推断账号被标记。 |

网关自身 `gateway.go:899` 的 `/models` 路径会选中账号后设置对应 AT/Account-ID，并使用指纹 profile 的 `client_version`；未开启指纹时沿用客户端 query 或默认版本。成功 body 的上限是 8 MiB。该路由的读取限制不等于目录内容已通过 CLI 全部反序列化验证。本轮服务器只读 GET 同样限制 8 MiB，原始 body 仅在内存中解析，对外只保留脱敏报告。

## 工具与复验

[quality_catalog_compare.py](../../tools/quality_catalog_compare.py) 是离线工具，入口 `compare_accounts(records)` 可直接接收服务器 helper 返回的原始 JSON 对象。`parse_json(raw_bytes)` 拒绝重复 JSON key、NaN/Infinity 和超过上限的输入。输入示例中的 `catalog` 可替换为 `catalog_file`，相对路径按 manifest 所在目录解析：

```json
{
  "schema": "codex-quality-catalog-input-v1",
  "accounts": [{
    "alias": "q0",
    "catalog_file": "private-q0-models.json",
    "provenance": {
      "source": "network",
      "client_version": "0.155.0",
      "http_status": 200
    },
    "auth_plan": {
      "claim_source": "access_token",
      "chatgpt_plan_type": "self_serve_business_prolite"
    }
  }]
}
```

为避免保存原始目录，服务器采集应在内存中构造相同 records 对象，调用 `compare_accounts` 后只写返回值。本轮实测使用这条内存路径，未保存目录中的系统指令、工具指令或身份信息。输入 `http_status` 可以省略，但报告会明确标记未观测；实时采集必须填写实际状态。本工具的必需字段和外层 schema 校验不是完整 Rust serde 仿真。

比较规则包括：

- 仅明确 `source=network`、`client_version=0.155.0`、未出现错误状态的目录参与账号间网络比较。cache/bundled/fallback/unknown、非 200、错误 envelope、重复 slug 或错误目录结构均排除，保留原因。
- `missing`、`null`、空字符串、空列表和空对象分别记录。已知安全布尔、整数、枚举可显示值；说明文本、指令和未知字段值仅显示类型、长度与 SHA-256。未知字段名称也用哈希代替，避免将意外身份键名写入报告。
- 每份目录保留所有模型；未知格式的 slug 使用哈希键，同样参与集合比较和逐字段差异。顺序变化不被误判为模型能力变化，字段内部数组顺序仍保留在哈希中。
- 认证摘要只输出来源、已知套餐和 FedRAMP 布尔。不会输出 access token、refresh token、个人 ID、email、完整 auth 对象或未知账号 flags。

离线比较命令：

```powershell
python tools/quality_catalog_compare.py private-input.json --output catalog-report.json
```

本轮 [test_quality_catalog_compare.py](../../tools/test_quality_catalog_compare.py) 的 18 项测试通过，覆盖模型集合不一致、完整同模型差异、空/缺失/null、错误目录与版本、duplicate JSON key/slug、429 和 error envelope 排除、缓存/回退排除、未知值与私有文本不泄漏、匿名别名约束及文件入口。复验：

```powershell
python -m unittest discover -s tools -p test_quality_catalog_compare.py -v
```
