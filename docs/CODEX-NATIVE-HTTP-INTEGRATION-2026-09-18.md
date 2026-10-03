# 原生 HTTP 接入

本轮把此前可运行的官方 Rust HTTP helper 接入 Go 网关请求生命周期。代码默认关闭，尚未部署或使用生产账号推理；真实官方 CLI 已通过合成上游完成工具调用和回传，不等同真实模型推理。

## 运行范围

启动配置使用 `CODEX_NATIVE_HTTP_HELPER` 指定 Linux helper 绝对路径，同时要求 `CODEX_NATIVE_HTTP_SHA256`。缺一、路径或哈希不匹配时启动失败，不静默降级。Windows 构建仍可用默认 Go 模式，但原生 HTTP 构造明确拒绝 Windows。启动日志明确 HTTP 使用 Rust，WebSocket 仍使用 Go。

HTTP Responses（SSE/JSON）、models、compact、OAuth 复用现有转发与恢复策略，底层改为 Rust HTTP；没有改变工具 schema、历史、账号选择、缓存键或已接受模型轮的重放规则。WS message IPC 尚未实现；不能用 Rust 的已升级消息流伪造 Go 101 body，否则会重复握手、掩码和压缩处理。源码依据与可执行向量见 `native-transport/ws-plan/`，14 项测试验证的是契约，不是 WS 运行时。

当前原生 HTTP 只支持 direct 和显式 HTTP 代理。配置 SOCKS5 时明确失败，不直连、不回退 Go；已有生产账号固定 SOCKS5 出口，因此本轮没有开启生产原生模式。后续需要补齐这条真实路由，不把“可调用原生 HTTP”缩成最终完成目标。

## 生命周期

- 每 helper 使用私有 0700 目录内的 Unix socket，socket 权限 0600，并校验同 UID；随机 IPC 令牌和代理凭据只经 stdin/IPC，不写 argv 或诊断。
- 池按代理配置指纹、版本、目标 origin 和响应头超时区分，最多 16 个进程，每个最多 32 个活动事务。启动中和退出中的占位也算在上限内，不把等待退出的进程从容量中提前抹去。
- 启动等待、二进制 hash、退出等待都在全局锁外，独立请求可取消。proxy snapshot 倒序到达不退役更新版本；没有稳定 ID 的独立代理不互相退役。
- Body.Close、Context 取消、正常 END 或协议错误均关闭所属 IPC 连接并只释放一次。正常响应结束前的截断不是正常 EOF；保留重复响应头、原始头值和正文，禁止协议错误变成成功。
- 代理版本在选择和实际 RoundTrip 再次校验。旧版本的新请求失败关闭，活动流继续；关闭服务会等待 helper 回收。Go 和 Rust 两端清理都检查 socket 身份，替换路径不被误删。
- 请求体上限与网关一致为 32 MiB。持续 SSE 不加总期限，上游 idle 仍由现有 Go 流包装计时；等待下游不算上游 idle。compact 保留总期限。池容量/不支持代理等本地故障归网关 503，而不是误记上游 502。

## 已验证证据

最终 Go 快照为 `7c5d417bcc8d62a1faac86c28a04a2671d5c44238b5eeabee95d60840e2bdd53`，目录 `.local/source-20260918/gateway-overlay-20260918t031826-7744cf/`。同源全库测试、vet、Windows/Linux 构建通过；Linux `-race` 176 个顶层测试、404 个通过事件，零失败/跳过/遗漏，90.122 秒，真实隔离 PostgreSQL/Redis 均运行并已清理。Linux gateway binary SHA256 为 `da0213696f705631bd3a66afb719aafa6e98dbee5995c8fcd56e2be839b8d6e0`。这不是随后变化的共享管理树。

首个 Rust UDS 二进制来源 `.local/native-local-build/20260918T030140Z-831da301/`，SHA256 `19db53c3b78bca7a78b0f5a0b1c1b7243c680418edcccaa0afbf984924945a2c`。它的 23 项 loopback 测试曾全部通过，但后续新增满容量取消/补位测试发现真实问题：初始 32 条通过，立即补充的 32 条仅 30 条返回响应头，2 条连接被重置。该二进制因此不作为最终原生验收产物。失败证据在 `.local/native-refill-probe/20260918T032201Z-3b44427b/report.json`，需要修正 Rust 接受连接与获取槽位的顺序，再重编译和重验。250 个解析包中 registry/git 依赖与官方锁逐项一致，本地包显式核验，27 个官方 Rust 源文件保持原样。

该问题已修复并重验：Rust 在可取消的 select 分支内先获取槽位再 accept，满容量的新连接在有限 socket backlog 等待，不立即 reset，不增加等待任务、不重放。最终 helper 来自 `.local/native-local-build/20260918T032505Z-1cbad96e/`，SHA256 `253d88c2cd4bfa8c655ff7dfce6c2ed7b70dc7dc28567516269af9dc581b670f`；24 项 loopback 全通过、零跳过。独立相同 1 CPU / 1 GiB 条件下再跑 11 批，每批 32 条全部返回响应头，合计 352/352，无重置；报告 `.local/native-refill-probe/20260918T032703Z-e86be60b/report.json`。这是合成取消/补位验证，不是生产账号并发测试。

构建时发现了实际缓存缺陷：确定性归档时间戳使 Cargo 将已变化的 main.rs 误判为 Fresh，曾返回旧 TCP-only 二进制。runner 已改为更新本地源码时间并 `cargo clean -p codex-native-transport`，再 `cargo build --locked`；收集器拒绝 main 变化但产物 hash 不变。失败 run 保留，最终 UDS build 日志和新 hash 均确认真正重编译。

官方 Linux CLI 0.154.0 通过链路 `CLI -> Go native HTTP -> Rust UDS -> loopback TLS` 完成两次请求及工具回传；fixture 特意把 Go 上游 Transport 替换为必失败实现，所有成功必须经过 Rust。17 个断言全部为 true，包括两个请求/两条用量记录、原生应用头和请求 schema、TLS1.3/h2、工具结果入历史、准入释放及无 Go fallback。合成用量为 input 50/output 2，不用于缓存率或质量结论。

同一 suite 还运行了 Responses SSE/JSON、models、compact 的显式 401 恢复，证明同账号原请求仅恢复一次、RT 轮换已保存；取消不重放、HTTP 代理三次请求复用一个连接、禁用代理后不再发请求均通过。最终运行路径与 Go hash 见同名 JSON，不使用此前候选通过替代新版本验证。

UDS helper 的默认 native ClientHello 与 custom-CA rustls ClientHello/协商对照复验通过。默认仅捕获首个 hello，custom CA 才完成 TLS/HTTP；不是生产 SNI、会话恢复、HTTP/2 SETTINGS 或美国设备证明。此前 `prefer-post-quantum` 特性缺失的失败证据仍保留。

最终 Go `7c5d417...` 与最终 helper `253d88...` 配对复验全部通过：真实 CLI/网关 suite 在 `.local/native-transport-build/gateway-compare-20260918T032642-e11095/`，TLS 对照在 `.local/native-transport-build/tls-compare-20260918T032652-f69d53/`。这两个结果均来自修复后的同一二进制，不借用旧 helper 的通过结果。

## 下一边界

原生 WebSocket、SOCKS5 路由、HTTP/2 SETTINGS/恢复握手、真实上游端到端任务及部署仍未完成。本轮只在可复核的隔离环境执行，未覆盖或开启生产服务。管理会话持续修改共享文件，因此验证使用已验管理 `5a818844...` 加明确网关 overlay，源码/产物均固定哈希；不要用共享目录后续构建冒充该候选。

复跑真实 CLI 合成链路：

```powershell
python tools/native_transport_compare.py --native-run 20260918T032505Z-1cbad96e --source-dir .local/source-20260918/gateway-overlay-20260918t031826-7744cf/source --suite gateway
```
