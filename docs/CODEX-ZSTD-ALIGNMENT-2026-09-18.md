# 原生请求 Zstd 对齐

本轮完成请求压缩链路的源码对齐与本地验收。遵守用户取消真实账号测试的指示，所有 CLI 认证文件均为临时合成数据，执行在 `network none` 容器和 loopback 上游内；未读取桌面认证、未调用真实模型、未部署。

## 实现

网关现在保留入站 `POST /responses` 的 Zstd 选择：先解压并执行现有请求策略/准备，再压缩真正发出的 JSON，不直接转发修改前的压缩包。未压缩请求不主动压缩，compact/models/WS 不继承 Responses 的 HTTP 压缩选择。

普通 Go 传输使用已有 zstd 库的 level 3，一次准备好 wire bytes 供 401 重试复用；这保证协议和解码语义，不声称压缩帧与官方 Rust 逐字相同。原生 helper 使用官方 `EncodedJsonBody::encode` 和 `Request::prepare_body_for_send`，通过 RawValue 保持准备后 JSON 内部 key 顺序、重复 key、数值和转义，实际编码采用官方 level 3。外围空白、无效 JSON、重复 Content-Encoding 等歧义明确拒绝，不静默重写。

helper ready 必须声明 `request_zstd_v1`。旧 helper 缺能力时返回网关 503 并记录本地能力错误，不能回退未压缩或误记上游传输故障。OAuth 刷新保留既有独立生命周期与观测 context，但显式清除仅属于推理请求的压缩标记。

界限：入站 wire 与解码正文仍各限 32 MiB；新增准备后 JSON 的 32 MiB 检查，防止字符串 input 包装或策略修改使正文增长后绕过限制。Go 编码输出限 33 MiB，以容纳不可压内容的 frame 开销；这些 Go 边界超限在派发前返回 413。Rust helper 另有 33 MiB 压缩输出防御性边界，该内部错误当前按传输失败处理，不声明它也返回 413。Zstd 的 skippable frame 路径会把底层 MaxBytesError 变成 UnexpectedEOF，因此额外保留实际线缆超限标记，避免把超限误报为坏包 400。

## 官方 CLI 证据

官方 0.155.0 源码选择条件是压缩 feature 开启、认证 uses_codex_backend、provider is_openai。四组实际 CLI 隔离实验均通过，共 8 个 Responses 请求、4 次真实工具回传：

| 合成客户端状态 | 实际请求编码 |
| --- | --- |
| ChatGPT + provider 名 OpenAI + feature 开 | zstd |
| ChatGPT + OpenAI + feature 关 | 无编码 |
| API key + OpenAI + feature 开 | 无编码 |
| ChatGPT + 非 OpenAI 名 + feature 开 | 无编码 |

固定直接实验：`.local/native-compression/20260918T054235-af38b4/`。这证明本地 CLI 分支，不证明远程账号认证。其它 uses_codex_backend 类型的源码 gate 说明见 `NATIVE-REQUEST-COMPRESSION-2026-09-18.md`，未将四个样本说成所有 auth 类型都实测。

最终整链路 `官方 CLI 合成 ChatGPT -> Go -> Rust UDS -> loopback` 已通过：入站/出站两次均 Zstd，工具结果回传、整份 JSON 语义、两条 Responses usage 和准入释放保持，Go fallback trap 为 0。该 fixture 只为比较 JSON 关闭可选 prompt/compatibility；另有开启策略的准备正文与 401 重试回归。JSON key 顺序和压缩帧在网关准备后可变化，不要求入站与出站压缩字节相同；同一准备正文的重试 wire bytes 已验证相同。

报告：`.local/native-transport-build/gateway-compare-20260918T055515-eb2d44/`。首轮统计误把 CLI 的独立模型列表 GET 当成模型轮，调整测试只统计 Responses 后重跑通过；不是删除实际推理请求或修改计量来凑通过。此前捕获的 OAuth 压缩标记泄漏和线缆超限状态误分类也均有修复回归。

## 固定产物

最终 Go 候选：`.local/source-20260918/gateway-overlay-20260918t055355-d97964/source`，SHA256 `a6d5994719dca2ff1f9fcb1037ec5d858e47f1e7329ac82c64466f5dd5f027d0`。全库测试、vet、Windows/Linux 构建通过，release 配对 18 个本地联调顶层全部通过，TLS 对照也重新通过。

实际 Linux PG/Redis `-race` 对应前一个快照 `591414e365496c6df37f168fff9aec1bd2f69de34d6d6b62d94d18169a2aa696`：272 顶层、590 通过事件，零失败/跳过/遗漏，129.173 秒。最终候选仅修改一处未进入该竞态选择集的 CLI 集成测试统计，所有生产文件、被选测试文件和 272 个测试名逐项一致，见候选目录 `race-equivalence.json`；并不声称对最终 hash 另跑过整套 race。被修改的 CLI 测试已在最终候选单独实际执行通过。

最终 helper release SHA256 `6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`，14,872,608 字节。26 项 HTTP/UDS、4 项 WS、3 项官方压缩测试通过；静态 PIE/无动态加载器及空根文件系统启动验证通过。包 `.local/native-releases/0.155.0-linux-x86_64-6df426c0a8eb.tar.gz`，SHA256 `b994f94d784c064502ff3d883ad004c390abae0676bf31c362612acbd46e1015`。

本轮只是压缩协议、限额和本地行为证据。正文缩小不代表模型缓存率提升，合成响应的 token 数也不是实网成本或质量结果。此前 owner revision 的新旧 writer 不可混写约束仍适用；当前生产服务与本候选独立，未开启新能力。

复跑本地整链路：

```powershell
python tools/native_transport_compare.py --native-run 20260918T054243Z-b43907cb --source-dir .local/source-20260918/gateway-overlay-20260918t055355-d97964/source --suite gateway
```
