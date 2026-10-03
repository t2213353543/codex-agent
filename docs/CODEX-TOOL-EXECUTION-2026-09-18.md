# 工具调用与文件任务验证

本轮使用官方 CLI 0.155.0、已封存 Rust helper 和本地脚本化 Responses 服务。实际执行 CLI 工具，不使用真实账号、不调用模型上游、不部署。Rust 生产实现和 release 二进制未变。

## 修复

非流式 JSON 聚合有一处可复现重复工具风险：同一非空 item ID 先占隐式或显式 index 0，后又声明 index 2，聚合器曾保留两个槽位，可能将同一个自定义工具调用返回两次。

`response_output.go` 现在在修改集合前拒绝此身份/索引冲突。JSON 调用返回固定 502 错误，不携带工具 ID 或内容，不追加上游重试。同 ID、同索引更新仍可用；不同 ID 的乱序完成仍按索引组装。SSE 流式请求依旧原字节透传，本轮不对流式工具事件去重或改写。

新增测试先实际出现 3 项失败，修复后 5 项新增及既有聚合回归通过。事件是明确构造的异常兼容输入，不声称官方服务正常响应必然出现这种索引冲突。详见 [聚合器复核](CODEX-CUSTOM-TOOL-OUTPUT-REVIEW-2026-09-18.md)。

## 真实 CLI 任务

两个真实 CLI 进程在全新 HOME/CODEX_HOME、临时工作目录内，经过 Go 网关、Rust UDS helper 和本地 TLS 1.3 / HTTP/2 上游，完成：

1. 从 CLI 实际提供的工具定义中找到 `apply_patch`，创建待修正文件。
2. 用实际 `exec_command` 校验，收到预期失败输出和退出码 7。
3. 确认失败已回传且文件仍为旧内容后，再发 `apply_patch` 修正。
4. 用 shell 校验最终字节及 SHA256。
5. 新进程 resume 同一会话，再次从磁盘读取并校验，没有重建或改写文件。

实际结果：7 次 Responses 请求，2 次 custom `apply_patch`、3 次 function `exec_command`，5 个工具结果逐一按 call ID 校验；shell 退出码为 `[7,0,0]`。最终文件 41 字节，SHA256 `de812bc619265d3eb6422bf3270009fa14262822545f4b139458d2127be561aa`。

21 个任务断言通过，Go 上游回退次数为 0。输入前缀、同进程 instructions/tools、同线程跨 resume 的 `prompt_cache_key` 保持稳定。这里证明发送结构稳定，不是实际缓存命中率提高；上游 usage 和所有“模型回复”均为合成数据。

该任务是连续工具执行与确定性失败修正，不是模型自主定位错误、并行工具调度或操作系统级 exactly-once 证明。报告不保存工具原文、文件内容、私有路径或凭据。

## 验收脚本

`native_transport_compare.py` 根据冻结源码中存在的场景要求对应报告；本候选四份 HTTP、WS、压缩及文件任务报告全部存在且 `passed` 必须为布尔 true。测试退出非零、OOM、跳过、报告缺失或失败均不能整体通过。较早未包含编辑/压缩夹具的快照不被要求提供相应新报告。

冻结源码校验现在同时核对完整文件集合和重新计算的 Go 哈希，并在编译前后、测试后复核，拒绝 manifest 外新增 Go 文件、缺失文件及伪造哈希。5 项 Python 回归通过。

## 固定证据

- Go SHA256：`5a9eddb2d5b1ec882cfd83e62bd58576ed6de33a1fc6f7571a6631d37222ff4b`。
- 快照：`.local/source-20260918/gateway-overlay-20260918t072922-6e441b/source`。
- 管理基线：已验证无计费 `bdfbf92...`，不包含当前管理后续 UI/查询变更，不是部署包。
- Helper SHA256：`6df426c0a8eb703929f888055192e09cec0d2f7eea9391f2ae891e1ca6213740`。
- 原生联调：`.local/native-transport-build/gateway-compare-20260918T072935-9c540a/result.json`，22 个顶层测试全部通过，0 跳过，临时容器已清理。
- 文件任务：同目录 `gateway-edit-cli.json`，21 个断言通过。

同一冻结源码全库测试、`go vet`、Windows/Linux amd64 构建通过。本地 Linux PG/Redis `-race` 306 个顶层测试、746 个事件全部通过，0 失败/跳过/缺失，159.781 秒，资源已清理。新增自定义工具 5 项、原有响应聚合 11 项均包含在内。原始报告：`.local/source-20260918/local-linux-race-20260918t073123-e4da73/report.json`。

## 复跑

```powershell
python tools/native_transport_compare.py --native-run 20260918T054243Z-b43907cb --source-dir .local/source-20260918/gateway-overlay-20260918t072922-6e441b/source --suite gateway
```

未验证真实账号、生产效果、美国出口或完整设备指纹；用户取消的真实账号测试保持取消。
