# 网关传输后续验证

本轮继续此前的源码与真实 CLI 工作，没有部署生产，也没有新增真实模型压力。完成三项实际改进：自定义超时复用连接池、第二批观测指标接入真实 HTTP/WS 入口，以及官方 Rust HTTP helper 的构建和同平台 TLS 对照。

## 网关修改

自定义响应头超时现在是有界池的键，不再每轮克隆 transport 后关闭。默认 direct client、注入的自定义 RoundTripper、代理版本校验及 compact 默认 20 分钟保持；自定义 direct compact 的总超时漏项也已修复。两批各 4 个合成请求，旧克隆方式新建 8 条 TCP，池化方式只建 4 条，第二批新增 0。8 个新顶层测试重复 10 次通过，覆盖同时不同超时、活动 SSE、128 项容量、代理删除/禁用/ABA 轮换。

第二批 metadata 不再只存在 parser：HTTP 实际尝试返回处记录响应头耗时，成功 401 恢复包含恢复过程；最终尝试无头时保持未知，失败 OAuth 后仍返回原 401 时不把之后的刷新延迟算进头时间。WS 只在首个准入轮记录真实握手耗时。请求与响应的模型/tier/effort 来源分开，错误阶段只标实际经过的分支。NativeCompaction 仅在 Responses 端点及相应原生输入语义下判断。9 个 hooks 测试重复 5 次通过，验证转发字节、重试次数和计量记录数不变。前置鉴权及未准入拒绝仍不新建 usage 记录，故不是所有拒绝都已有持久 phase。

HTTP/1 连接复用的受控实验已明确：完整 EOF 的 6 请求只建 1 条连接；即时终态但尚未消费 chunked EOF 的样本无法复用。强行读到延迟 EOF 的控制组增加约 100 ms。官方 CLI 的 SSE reader 也在 Completed 后退出；本轮没有为了复用而增加等待、后台无限 drain 或重放。此实验解释一种机制，不能替代此前真实双层网关链路的完整根因取证。

## 精确快照

管理会话仍持续修改共享代码，本轮没有回退它的文件。采用其已验收管理基线 `5a818844f613ad35602ac466c80d8949a2e7f7a180e96ae21c4cbd1573ce6d22`，叠加明确归属的当前网关源码，生成独立、可复核的验证归档。

- Go SHA256：`956fc90be91a922ac1593b283102bc5149674dac337564e6023479ec8e60a6f9`。
- 源码：`.local/source-20260918/gateway-overlay-20260918t023414-8e3565/source`；同目录包含基线与逐文件 overlay manifest。
- 同快照全库测试及 vet 通过；Windows/Linux amd64 构建通过。
- Linux `-race`：159 顶层、379 通过事件，零失败/跳过/遗漏，77.909 秒。真实运行独立 PostgreSQL/Redis，验证非 superuser 与 Redis ACL 边界，无宿主端口；临时容器、网络、凭据均清理。
- Linux 产物：同目录上级 `codex-agent-linux-amd64`，SHA256 `a66c7be3de344221d6433a6bc7140523d95a6a068fd2312757c80f2dc50c073d`。

此证据只覆盖明确的管理基线加网关 overlay，不包含管理会话之后的新增功能，也没有包含新前端。不能把共享目录随后的任意构建当作这个已验版本。真实 CLI 的 12 次成功和 84.22% 缓存占比仍是上一候选证据，本轮没有重写其版本归属或声称缓存增益因果。

## 原生 Rust

`native-transport/` 复用官方 0.154.0 底层 HTTP 库，不依赖整个 CLI；27 个 vendored Rust 源文件与来源逐字相同。实际 Cargo 解析得到 250 个包；所有 registry/git 依赖的 version/source/checksum 均与官方锁一致，3 个本地包按显式清单核验，最终以 `--locked` 编译 Linux musl 二进制。原始字节、重复头、错误状态、取消、背压、连接复用、显式代理、超时与 TLS 证书校验等 16 项 loopback 测试全部通过。

直接与官方 Linux CLI 0.154.0 比较时，首次发现 helper 缺少 rustls `prefer-post-quantum` 特性：自定义 CA 路径的组顺序和 ClientHello 长度不同。核对已锁定 crate 的条件编译源码后补齐该特性；第二轮默认 native ClientHello 字段匹配，自定义 CA ClientHello、TLS/HTTP 协商也匹配。默认信任实验只捕获首个 hello，不是成功验证完整默认 TLS 会话；扩展顺序随机部分不作为固定指纹。

最终 helper SHA256：`95dc45ab20b063a2b5367f12efbd0f7184b572cf783929c42f03ee3b14b1868d`。构建来源：`.local/native-local-build/20260918T024358Z-f23140f1/`；最终对照：`.local/native-transport-build/compare-20260918T024519-b551e0/`。首轮错误证书 fixture 与首次 TLS 特性差异的失败证据均保留，没有覆盖。

helper 尚未接入 Go 生产流量，只是可运行的受控原型。下一步仍需受服务身份保护的 IPC、Go Body/Context 生命周期接入、代理版本池管理、HTTP/2 SETTINGS 与会话恢复对照，再处理官方分支 WebSocket。Linux native/OpenSSL 不等同 Windows Schannel；修改 locale 或请求头不能证明美国设备或模型上游出口地区。完整目标仍在进行，未标记完成。

## 构建环境

此前远程构建环境已准备好，但本轮重新核实时 SSH 通道不可用，因此没有宣称远程新验证成功。本地 Docker 原先停止，启动时遇到残留 socket 错误；仅对两个已核对的运行时 socket 目录作了带时间标识的备份并重启，未 factory reset、未改数据卷、未改宿主包或生产服务。Rust/Go/C 工具安装只发生在本轮隔离容器。构建 cache 和退出容器保留用于复核与增量构建，不含真实账号资料。

复跑网关竞态：

```powershell
python tools/gateway_local_race.py --run --source-dir .local/source-20260918/gateway-overlay-20260918t023414-8e3565/source --expected-go-hash 956fc90be91a922ac1593b283102bc5149674dac337564e6023479ec8e60a6f9
```

本轮汇总 JSON 保存证据路径与 SHA256。更细的字段语义见 `CODEX-GATEWAY-OBSERVATION-HOOKS-2026-09-18.md`，H1 实验见 `GATEWAY-H1-TERMINAL-DIAGNOSTIC-2026-09-18.json`，原生协议见 `native-transport/README.md`。
