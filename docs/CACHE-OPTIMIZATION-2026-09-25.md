# 默认会话缓存键优化

本次复现的是相同网关键、相同上游账号的不同 CLI 默认会话使用不同缓存键。修复使这类默认键在账号范围内稳定复用，同时保留每条会话原有的账号黏性及真实 session/thread 标识。

## 现场与因果边界

主任务读取的生产 24 小时统计为输入 1,530,563 tokens、缓存输入 339,072 tokens，加权比例 22.15%；615 次请求中 497 次成功、117 次错误。明细报告保存于 `.local/failure-cache-20260925/production-audit.json`。历史日志没有记录 cache key，无法据此判断默认键拆分贡献了多少缓存损失。本次本地验证使用合成 loopback 上游，没有调用模型，也没有用固定的合成 cached_tokens 声称命中率提升。

缓存是服务端行为，默认键稳定化并不保证 80%–90% 命中率。部署后应以真实上游返回的 `cached_tokens / input_tokens`，按相同模型、账号、API key 和相近工作负载评估。没有修改统计口径。

## 本地参考证据

参考仓库根目录：`C:/Users/Administrator/Desktop/项目/sub2api/current-source`。

- `backend/internal/service/openai_codex_fingerprint.go:503` 的 `shouldRewriteCodexFingerprintPromptCacheKey` 仅在非空 key 等于原始 `client_metadata.session_id` 且启用相应收敛模式时，识别默认 key；`applyCodexFingerprintPromptCacheKey` 将它改为稳定 session key。
- 同文件 `resolveCodexFingerprintIDs` 将稳定的账号 session 与不同客户端线程分别生成，因此 Sub2API 的默认缓存键可跨客户端默认会话复用。
- `backend/internal/service/openai_codex_account_identity.go` 的 `applyCodexAccountIdentityClientMetadataRaw` 按账号和 API key 隔离身份，并在原始 JSON 上局部改写。当前项目参考其隔离及局部改写原则，但没有复制身份头或 metadata 收敛。
- 本项目 `.local/codex-source-0.155.0/codex-rs/core/src/client.rs:497` 的 `prompt_cache_key` 明确区分 override、内部任务 `source:parent_thread_id` 和默认 `responses_metadata.session_id`，同文件构建 `client_metadata` 时使用同一 session 值。HTTP/SSE 与 WebSocket 共用此请求模型。

## 实现

新增 `prepareGatewayAccountPromptCache`，在 HTTP 已选定账号、尚未压缩时，以及 WebSocket 已授权账号、尚未登记及发送该轮时运行。JSON、SSE、zstd、compact、WebSocket 及原生 transport 共用同一函数。

仅当 `prompt_cache_key_enabled` 已开启、标准小写字段中的非空 key 与 `client_metadata.session_id` 精确相等时，生成 `SHA256([版本命名空间, 网关键 ID, gatewayAccountRef(account)])`。64 字符 key 满足现有 Responses/compact 长度限制。账号引用包括真实所有者及 OwnerRevision；已知所有者的常规 token 刷新不拆分缓存，未知所有者的凭据轮转会拆分。

函数先由标准库 `json.Valid` 验证整个文档，再扫描原始 JSON 的字段边界，替换目标标量；其余字节原样保留，包括未知 metadata、工具数组顺序、重复嵌套字段、大整数、历史、加密内容和 cache options。字段重复时仅使用最后值。真实会话绑定使用策略处理前解析的 session，因此缓存键合并不会合并会话或改变账号。不同网关键永远不共享此生成键；此变更不改变账号配额、并发或调度公平性。

初版 Decoder 对大 input/tool 字段会分配额外副本，已在发布前纠正。8 MiB 本地微基准中，有映射请求的分配由 50.35 MB 降至 8.40 MB（最终出站副本），缺 metadata 请求由 41.95 MB 降至 0 次分配；相应 helper 耗时由约 26/25 ms 降至约 13/13 ms。完整 JSON 校验仍有线性扫描成本。数字是本机单次微基准，不代表生产吞吐。

显式自定义 key、内部任务 key、缺失 metadata、大小写不同的扩展字段、空值及不匹配 key 保持原样。原有“缺失 key 时补齐”策略及其会话边界保持不变；没有给无身份请求创建硬绑定，没有改 TTL、breakpoints、提示词、工具或 history。

## 回归与复跑

`gateway_prompt_cache_test.go` 的真实 loopback 捕获覆盖 JSON、SSE、zstd、compact、WebSocket：每条独立会话都保持自身 header/metadata/input，两个默认会话在同一账号和 API key 下得到相同 64 字符 key。修复前 5 个场景全部失败，修复后全部通过。相同场景提供显式 opt-in 的 Rust native 分支，Windows 跳过，交由主任务 Linux 验证。

新字段扫描器另经独立 30 秒 fuzz，与 encoding/json 最终值及 Decoder 偏移结果逐字比较，359,594 次执行未见越界、panic 或错误改写。日志见 `.local/cache-optimization-20260925/review-scanner/fuzz.log`。

另外覆盖精确标量替换、显式 override、内部任务 key、metadata 缺失/类型/大小写、空白不同的 key、幂等性、开关关闭、API key 隔离、账号记录/工作区/用户/身份版本隔离，以及凭据刷新规则。

相关 policy/cache/session/WebSocket/compression/inference fidelity 套件 122 个测试项通过、5 个 native 测试项跳过；最终新增 cache 测试复跑通过。Linux/native 全套结果由主任务记录，不以 Windows skip 代替真实 helper 验证。

```powershell
go test . -run 'TestGateway(DefaultPromptCache|NativeDefaultPromptCache|AccountPromptCache)' -count=1 -timeout=90s -v
```

备份及日志目录：`.local/cache-optimization-20260925/`，包含改动前源码、`red.log`、`green-wire.log`、`green-regression.jsonl`、`green-final.jsonl` 和 `cache-optimization.patch`。部署由主任务负责，本子任务未修改运行配置或部署产物。前端当前说明仍是旧版“补缺失键/保留已有键”，本次没有修改或发布前端；准确规则以本文件及 `GATEWAY-SETTINGS.md` 为准。
