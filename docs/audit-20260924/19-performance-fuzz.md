# 19 — 性能、异常输入与可重跑验证

日期：2026-09-24。专项新增 `gateway_performance_audit_test.go`、`tools/verify_gateway_audit.py`、`tools/test_verify_gateway_audit.py`。生产实现由对应专项及主任务修改，本专项独立验证，不修改生产配置，不使用生产凭据或远端模型。

## 九类边界与检查结果

| 边界 | 新增验证 | 结果 |
| --- | --- | --- |
| 32 MiB 单行及扫描复杂度 | 最大合法注释行按 64 KiB 分片，末尾 CR/LF 分两次读，后续 completed 正常；多一字节拒绝 | 通过 |
| 8192 项聚合 | 索引间隔 2^40，顺序保持；第 8193 项 MaxInt64 索引拒绝且原响应不变；满容量仍可更新同 ID | 通过 |
| 数字、未知字段、UTF-8 | 9007199254740993、1e1000、中文及 emoji 保持；不经 float64 | 通过 |
| SSE 分片 | BOM、多 data 行、中文/emoji、CRLF 的所有两切分位置穷举；状态化 scanner 与原 stateless framing 的差分 fuzz | 通过 |
| native IPC 头预算 | Base64 膨胀后的最大可用 bucket 接受，下一字节拒绝；超预算 frame 不读取 payload | 通过 |
| 压缩缓存上限与并发 | 大于 1 MiB 的 encoder 不入缓存；现有 12 并发独立解码隔离回归；池最多保留两个 encoder | 通过 |
| header 归一 | 直接构造的大小写 alias、opaque 高位字节和多值保留；重复归一幂等，不改 caller slices；hop/auth 字段排除 | 通过 |
| cancel/资源收尾 | 取消压缩不借还 encoder；32 个并发 Close 只关闭一次，阻塞 Read 退出，timer callback 被收回 | 通过 |
| proxy 端口 | 1/65535、前导零、IPv6；0/65536/溢出/符号/科学计数/全角数字拒绝，归一再输入稳定 | 通过 |

实际命令：

```powershell
go test . -run 'TestGatewayPerformanceAudit|TestPolicyCompressionAuditConcurrentEncoderIsolation|TestGatewayNativeAuditCancellationClosesBodyExactlyOnce|TestGatewayAuditSSEEventProgressAcrossChunkBoundaries' -count=1 -timeout=90s
python -m unittest discover -s tools -p test_verify_gateway_audit.py -v
python tools/verify_native_provenance.py --output .local/gateway-audit-20260924/19-native-static.json
```

Go 九个新边界测试通过；runner 八个 Python 测试通过，覆盖默认/显式命令选择、继承环境隔离、embedded SQL hash、Go pass/skip/fail、没有任何测试时的假成功拒绝、源码执行中变更失败、dry-run 不执行不写报告。native 静态检查确认 261 个外部包、30 个未改官方 Rust 文件及 1 个完整记录的 WS adapter；该结果不是 TLS wire parity 证明。

## Fuzz 实测

每个 target 独立运行，`-parallel=2`，不是只跑 seed：

| Target | mutation 预算 | 本轮 executions | 结果 |
| --- | ---: | ---: | --- |
| FuzzAuditSSEFragmentedOpaqueResponse（既有） | 8 秒 | 18,564 | PASS |
| FuzzAuditOutputIdentityState（既有） | 8 秒 | 8,315 | PASS |
| FuzzGatewayAuditSSEChunkInvariant（新增） | 10 秒 | 144,283 | PASS |
| FuzzGatewayAuditNativeIPCFrames（新增） | 10 秒 | 169,186 | PASS |

新 SSE fuzz 将任意字节（含非法 UTF-8）按多段切分，验证事件进度和最终状态不依赖 chunk；收尾加入 stateful scanner 对 stateless framing 的差分验证，包含 Scanner 自己搬移/扩容 buffer。IPC fuzz 覆盖任意 kind、32-bit 声明长度、真实 payload 不足或带尾部数据，以及逐字节读取；畸形和截断帧不能产出成功正文。

状态化 splitter 合入后，针对新增差分逻辑再跑 `FuzzGatewayAuditSSEChunkInvariant` 8 秒 / 23,536 executions，并重跑既有 opaque collector fuzz 3 秒 / 26,428 executions，均 PASS。其他与此次改动无关的 target 没有重复运行。

重跑全部 mutation：`python tools/verify_gateway_audit.py --fuzz-seconds 10`。这会先进行默认验证，然后分别给四个 target 10 秒；不是四者共享 10 秒。短时间 fuzz 只覆盖本轮探索，不证明不存在其他输入缺陷。

## 与基线归档原函数的真实对照

输入来自 `.local/gateway-audit-20260924/baseline-source.zip`。解出原 `gateway_timeout.go` 和 `gateway_request_compression.go`，只重命名符号以便与现代码同模块编译；使用临时 `Proxy` 占位和原 codec 依赖，不覆盖当前源码。隔离 harness、输入 hash：`.local/gateway-audit-20260924/performance-baseline/`；原始数据：`19-baseline-benchmarks.txt`。

Windows/amd64，AMD Ryzen 7 9700X，`-benchtime=300ms -count=2`，下表两个样本取范围。

| 热路径 | 基线 ns/op | 当前 ns/op | 基线 B/op | 当前 B/op | allocs/op 变化 |
| --- | ---: | ---: | ---: | ---: | --- |
| 每次 Read 新 timer → 复用，普通 body | 143.2–143.6 | 42.53–42.62 | 248 | 0 | 3 → 0 |
| Read 加 SSE event 进度判断 | 143.2–143.6 | 55.63–56.44 | 248 | 0 | 3 → 0 |
| zstd 4 KiB 重复文本 | 93,246–97,617 | 1,040–1,114 | 1,631,541–1,631,544 | 4,112–4,116 | 17 → 2 |
| zstd 4 KiB 随机字节 | 73,756–74,230 | 2,703–2,832 | 1,636,917–1,636,918 | 9,488 | 18 → 3 |
| zstd 128 KiB 重复文本 | 85,950–88,459 | 17,540–17,602 | 1,758,518–1,758,520 | 131,088 | 17 → 2 |
| zstd 128 KiB 随机字节 | 82,508–85,674 | 31,435–33,287 | 1,930,548 | 303,120 | 18 → 3 |

压缩对照包含不可压缩随机字节，避免只用高度重复文本夸大收益。缓存池只保存两个且只服务 <= 1 MiB 正文；这里测的是 warm encoder 热路径，不能推导冷启动、大于阈值正文或并发溢出时同样收益。`B/op` 是分配量，不是 retained heap/RSS。

重跑该隔离基准：

```powershell
go -C .local/gateway-audit-20260924/performance-baseline test . -run '^$' -bench BenchmarkBaseline -benchmem -benchtime=300ms -count=2
```

## 新确认的 SSE O(n²) 扫描问题

主任务 Linux race 验证在 32 MiB SSE 分片长行上耗时数分钟，定位到旧 `splitSSELine` 每次扫描全部累积前缀。主任务新增 `sseLineSplitter.scanned`，collector 保存该状态；尾部 CR 只重看一个字节，派发行后清零。32 MiB 每 64 KiB 一次读取时，旧方式约重复扫描 8 GiB 前缀；新方式只扫描未看过的部分。

本专项逐行复审状态重置、尾部 CR、buffer shift、EOF、空 token，并新增最大合法长度/超限和分片 fuzz。旧 wrapper 保留无状态语义，直接与新方法做相同 Scanner 配置对照。`-benchtime=200ms -count=1`：

| 单行长度 | stateless | stateful | 当前吞吐 |
| --- | ---: | ---: | ---: |
| 1 MiB | 2.907 ms | 0.482 ms | 2,176 MB/s |
| 4 MiB | 42.692 ms | 1.849 ms | 2,268 MB/s |
| 32 MiB | 2,481.992 ms | 15.854 ms | 2,116 MB/s |

32 MiB 这一夹具约快 156 倍。Scanner 自己的 buffer 倍增仍使本项总分配约 67 MB；此次优化降低重复扫描 CPU，不宣称取消了大行缓冲。测试不设易受机器负载影响的时间断言。原始结果为 `.local/gateway-audit-20260924/19-scan-benchmarks.txt`。

```powershell
go test . -run '^$' -bench '^BenchmarkGatewayPerformanceAuditFragmentedLine$' -benchmem -benchtime=200ms -count=1
```

其余当前热路径：WS message budget 38.14 ns/op，0 B/op/0 alloc；SSE 常量状态扫描 4 KiB/64 KiB 约 664/671 MB/s，0 B/op/0 alloc；8192 个稀疏工具项观察加最终 JSON 聚合约 23.689 ms、13.20 MB/op。最后一项仍有必要的 JSON 解析、身份索引与排序成本，没有将整体聚合宣称为零分配。

## 一条命令的最终本地复验

```powershell
python tools/verify_gateway_audit.py
```

默认执行 `go test ./... -json -count=1 -timeout=20m`、`go vet ./...`、独立 native source provenance。Go/Cargo 依赖均离线，缺工具链或缓存会如实失败，不自动 pip/下载/部署。继承的 `CODEX_*`、`GATEWAY_*`、`TEST_*`、live API/DB/Redis/proxy 入口清除，显式 binary 参数再注入本地受控测试。它不读取 production/account 配置，不连接 live model 或生产服务。

可选：`--native-bin PATH`、`--cli-bin PATH`、`--race`、`--rust`、`--fuzz-seconds N`；native/CLI 仅启用已有 loopback fixtures，Linux 限定测试在其他平台仍跳过。`--rust` 精确运行 helper 的 `cargo test --locked --offline --release --bin codex-native-transport`，不误跑去掉 dev dependencies 的 vendored crate 测试。可先用 `--dry-run` 审阅命令而不执行。

报告 stdout 是 JSON，同时写入 `.local/gateway-audit-20260924/verification-TIMESTAMP.json` 和独立日志目录。包含 checks/tests 各自 pass/skip/fail 数、测试名、命令、耗时、输入文件 SHA256 清单及执行前后汇总 hash。hash 涵盖 Go 源/测试、go.mod/sum、embedded SQL、native 生产源/测试/vendor/tools/lock/provenance 和 runner。

任何源码执行中变化都使 `source_stability` 失败。缺省 native runtime、CLI runtime、HTTP/WSS TLS parity、Rust、race、fuzz mutation 均有显式 skip；Go 自身的 PG/Redis/实际 native 等 skip 也保留。存在 skip 时状态为 `passed_with_skips`、`all_checks_executed=false`，不能把 source provenance 或全跳过的 native 分类称为真实运行通过。

本专项完成精准回归、四个 mutation fuzz、基线与当前 benchmark、runner 帮助/plan/异常回归；最终冻结源码后的完整默认 runner 与 Linux 全量/race 由主任务统一执行，避免在并行修改期间重复全套并产生混合源码证据。

主任务最终工具复核补充：optional runtime只按leaf测试归类，Go父测试在所有子测试skip时的pass不能冒充执行；未显式指定原生/CLI二进制时分类必为skip。追加反例测试后runner工具9项通过，完整默认命令已实跑并记录源码稳定。
