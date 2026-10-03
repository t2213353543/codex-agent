# 同容器 Linux observer 前后基准

日期：2026-09-25。结果仅覆盖 SSE/WS 的本地 observer 回调链，包括 response 身份、cooldown、metadata 和 terminal 观察；不包含网络、TLS、数据库、真实账号或模型执行，也不是整网关吞吐量。

## 环境与冻结证据

使用现有隔离 integration 主机，通过既有直接 SSH helper 执行。唯一新增的远端路径是 `/opt/codex-agent-integration/checks/audit-20260924/bench-parse-fidelity`，没有读取生产配置、没有挂载账号凭据、没有部署。

同一个容器 `3bd71ab29c90db9386628fbb4c63982a17c072d31bf117d4a979b0a7c07caf08` 顺序完成双方预编译、before 五轮、after 五轮。Docker 实测配置：network=`none`、CPU quota=2、内存=4 GiB、GOMAXPROCS=2、CGO_ENABLED=0。Go 为 `go1.26.8 linux/amd64`，CPU 名称 Intel Xeon Gold 6230 @ 2.10GHz；镜像固定为 `sha256:eb1d21756fc64fbd95138b52362d6952f31898a666d57b5bdc02eb22bdf4f556`。

before 的三个源文件取自 `.local/quality-20260925/parse-performance/`，逐项校验与 `auth-catalog-continuation/build-manifest.json` 完全一致。其余生产文件均与当前 after 相同；没有把其他功能修复混入基准差异。

| 文件 | before SHA256 | after SHA256 |
| --- | --- | --- |
| gateway_cooldown.go | cc3bf88b64cf2a8b192969b5be17a49a6af964876e780283dd62008f092c636a | eb84c2cfc3b49bdaf34052612fe7aa787bdbddc7798e59f07f702b2c0a2c6fe7 |
| gateway_websocket.go | 52cef82c6f67d4fea95304017124b9a30cb31e833c3a66ac8c7fc751d53eea12 | 1681184d0dc7984f57456d95448c69e6f816eeeb572d76646cf6169d908484e8 |
| gateway_ws_event.go | 90999add47c65041d70f2b246ba1b08d3a6738070ff0edce1ef4327b0635391b | e57e8be2b84ac2bb6d321fcb42a9ccfb4fa3697962a973328e4ee30b8c595a4d |

benchmark 原文件 `gateway_research_parse_performance_test.go` SHA256 为 `354ee14ce842249710b5292c0777f2b6c2e9e002da7489d3c2403687755a908a`。隔离快照仅抽出 Benchmark 函数和必需 imports，避免新测试函数引用 before 不存在的 helper；before 唯一调整是把 `observeFrame` 改回旧生产链的 `observeCooldown` 后 `observe`，setup、数据、循环和断言均相同。工作区 Go 文件没有被改写。

## 五次样本中位数

| 场景 | before ns/op | after ns/op | 耗时下降 | before B/op | after B/op | 分配字节下降 | before/after allocs/op |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| SSE token delta | 19704 | 15313 | 22.3% | 5362 | 4377 | 18.4% | 123 / 97 |
| WS token delta | 26772 | 14145 | 47.2% | 7612 | 4585 | 39.8% | 176 / 96 |
| WS 1 MiB tool done | 39916771 | 13272639 | 66.7% | 5455276 | 2167179 | 60.3% | 173 / 97 |

每个版本、每个场景均有五个有效样本；容器退出码 0、未 OOM。before 包运行 5.238 秒，after 5.346 秒；该总时长受 benchmark 迭代次数影响，不应拿它作为单次事件性能比较。表格使用每次 operation 指标的中位数。

顺序固定为 before→after，没有做随机顺序或统计显著性推断。共享主机调度和 CPU quota 可带来噪声，因此不以这些微基准宣称线上模型更快；减少解析与内存分配的效果只在指定 callback 链内成立。

## 原始材料与复跑

全部材料保存在 `.local/research-20260925/parse-benchmark/`：

- `before.txt`、`after.txt`：完整原始基准输出。
- `comparison.json`：原始数值、每组中位数与降幅。
- `source-manifest.json`、`sources.tar.gz`：双方完整源文件 SHA256 和冻结快照。
- `run.json`、`container-state.json`：确切 Docker 参数、镜像、network none、资源上限和退出状态。
- `before-compile.log`、`after-compile.log`、`go-version.txt`：预编译与工具链证据。
- `run_benchmark.py`：可审阅的冻结、上传、启动、状态查询和收集脚本。启动拒绝覆盖已有远端路径，避免误抹历史结果。

在分别解开的冻结 before/after 目录内，同一基准命令为：

```sh
go test -run '^$' -bench '^BenchmarkResearchParseProductionObservers$' -benchmem -benchtime=300ms -count=5 .
```

`source-manifest.json` 文件 SHA256：`657f21974c0f0a9a96667220564d5ff799838dffec0809fffcf340f498b1ddb4`；`comparison.json` 文件 SHA256：`1ed15982df38ea01058e410ebab6a61e1b566910ab94468e0b43f543aaa76caa`。
