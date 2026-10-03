# 长会话压缩与缓存诊断

本轮没有修改生产转发逻辑或 Rust helper，实现改动仅在诊断实验、CLI 测试与验收工具。使用官方 CLI 0.155.0 和本地合成服务，不恢复真实账号测试，不部署。

## 诊断修复

可选 workload 观察器此前把所有可重读请求 body 都送入缓存诊断，导致 JSON compact/OAuth 误入 Responses SSE 解析。已复现 compact 污染同线程最近前缀、产生未完成事件，以及 OAuth 产生假诊断错误。

现在只观察 POST 的已知 `/responses` 路径，支持 `/v1`、`/backend-api/codex` 前缀。其他请求仍正常转发和计入原 HTTP 状态计数，body、query、headers 及响应字节不变。三项新增回归覆盖中间 compact、OAuth 和路径白名单。

这是实验数据归属修复，不是生产缓存算法变化。历史 12 次 Responses 及 84.22% 报告保持原样；没有证据说历史任务受这些 JSON 路径污染。详见 [诊断复核](CODEX-CACHE-DIAGNOSTIC-REVIEW-2026-09-18.md)。

## 最新 CLI 压缩

0.155 中 `remote_compaction_v2` 已是被忽略的旧开关，实际由 provider capability 决定是否使用 V2。OpenAI provider 采用普通 `/responses` 流式请求，在 input 末尾追加唯一 `compaction_trigger`，再消费一个 `compaction` 输出和 `response.completed`；不是旧 `/responses/compact` JSON 路径。

新增真实 CLI 原生 TLS/HTTP2 场景完成 5 请求、2 个实际 shell 工具回传、1 次自动压缩、2 个 CLI 进程。首次合成 input usage 为 50000，阈值 25000，触发 CLI 自身压缩逻辑；后续低 usage 避免反复压缩。不透明结果在后续 3 个请求及 resume 中原样保留，cache key、instructions、session/thread ID 保持稳定，Go fallback 为 0。

18 项断言全部通过。记录中只有实际 trigger 请求 `NativeCompaction=true`，其余 4 条为 false；合成 usage 总计 input=50400/output=5，未丢失或制造用量。opaque 是测试字符串，不是可验证的真实密文；该实验不证明解密能力、真实上下文压缩质量或缓存命中收益。详见 [V2 源码与执行记录](CODEX-CLI-COMPACTION-V2-2026-09-18.md)。

旧 `TestRealCodexCLI` 同时补上 V2 识别并保留 legacy fixture 分支，移除无效配置。本轮实际运行其 HTTP、WS 回退、自动压缩与 resume 三个子场景，均使用 0.155；压缩场景观察到 legacy=0、V2=1。保留旧分支不等于本轮重新跑过旧版 CLI。

## 固定验证

- Go 源码：`863040e0e9b18ab1ad0007ee2c5396804abcf4e6c868c30fa6620f30d5897e69`。
- 快照：`.local/source-20260918/gateway-overlay-20260918t074709-4b8a17/source`，无计费管理基线仍为 bdfbf92，不包含管理随后新增的 UI/查询包。
- Linux 生产二进制：`1ea1e92f9bd65c3fcbeb4b2a36981df0814c2834268aab9becd065279682cd5a`，与上轮一致，证明本轮未改变生产代码构建结果。
- Helper：`6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`，未重建。
- 本地联调：23 项原生顶层测试 + 1 项 Go CLI 顶层测试（3 个子场景），共 24 项通过、0 跳过。不能把这 24 项都称为原生传输测试。
- 原始结果：`.local/native-transport-build/gateway-compare-20260918T074720-c718f3/result.json`；压缩详情在同目录 `gateway-compact-cli.json`。五份要求的场景报告均存在且通过，容器已清理。

同一快照全库测试、`go vet`、Windows/Linux 构建通过。本地 Linux PG/Redis `-race` 309 项顶层测试、759 个事件全部通过，0 失败/跳过/缺失，169.647 秒，临时资源已清理。报告 `.local/source-20260918/local-linux-race-20260918t074946-a53d96/report.json`。验收脚本的 5 项 Python 单测也通过。

## 复跑

```powershell
python tools/native_transport_compare.py --native-run 20260918T054243Z-b43907cb --source-dir .local/source-20260918/gateway-overlay-20260918t074709-4b8a17/source --suite gateway --with-go-cli
```

此命令只使用本机隔离容器与合成凭据。尚未据此证明生产成功率、真实缓存率、美国出口或完整设备指纹。
