# 联合候选 956 核验

本次只检查 owner 的不可变 overlay、对应归档/二进制与实际日志；未从共享 Go 工作区冻结，未访问远端、代理或真实推理，未迁移部署生产。

## 候选身份

- 管理基线：`5a818844f613ad35602ac466c80d8949a2e7f7a180e96ae21c4cbd1573ce6d22`。
- 新联合源码（owner compact JSON）：`956fc90be91a922ac1593b283102bc5149674dac337564e6023479ec8e60a6f9`。
- 同一逐文件 map 的 v2 默认 JSON 摘要：`a1d958c3a3deab35278baa65c4c30553b117c4d9ecdd76263b80994637655abe`。摘要不同来自 JSON 分隔符，不是两套源码。
- 新 Linux 二进制：`a66c7be3de344221d6433a6bc7140523d95a6a068fd2312757c80f2dc50c073d`。
- 同源归档：`e5bae543dc17c33f9f8cb9b7dbf2ea88a6accd789f4e524f526813722bc790cd`。
- 适配报告：`.local/joint-candidate/20260918T024841.878837Z/joint-candidate.json`。

归档所有成员、owner `source/` 逐文件内容、两个目标二进制大小及哈希、基线归档、overlay map 和实际差异均已核对。适配目录包含独立 `local-source/`、`source-manifest.json`、`source.tar.gz` 和 Linux 二进制；不修改 owner 目录。

## 文件归属

新增 3 个文件：`gateway_observation_hooks_test.go`、`gateway_observation_transport.go`、`source_native_helper_test.go`。

修改 5 个文件：`gateway.go`、`gateway_client_pool.go`、`gateway_observation.go`、`gateway_timeout_pool_test.go`、`gateway_websocket.go`。

其余 166 个基线文件完全保留，包含管理、账号、统计、设置与四个迁移文件。实际目标 map 等于基线 map 加申明的 gateway overlay，没有未申明变更或删除。

旧 5a 候选的 custom-direct compact 显式 timeout 缺陷**不套用到新二进制**：新候选实际包含 `gateway_client_pool.go` 修复 SHA `234a6bebee0550a68d053f1efa0da145805fb47fff210769fe5f46d54e42e6ba`；默认/custom RoundTripper、compact 总期限与 disabled/deleted variants 三项回归均在新快照 Linux race 中执行通过。旧 binary 没有被修改，其历史失败仍有效。

## 测试证据

| 范围 | 实际结果 | 原始证据 |
| --- | --- | --- |
| 新快照网关 Linux PG/Redis race | 159 顶层 / 379 PASS 事件，0 fail/skip/missing | `.local/source-20260918/local-linux-race-20260918t023552-c45439/report.json` 与绑定 `race.log` |
| 新快照管理 Linux PG/Redis race | 73 顶层全部 PASS，0 fail/skip；是 root 独立重跑，不是沿用基线 | `.local/source-20260918/local-linux-race-20260918t024608-5548d0/report.json` 与绑定 `race.log` |
| 本次独立冻结副本全库 Go | exit 0；344 顶层 PASS、66 SKIP；916 PASS 事件、71 SKIP 事件 | 适配目录 `local-verification.json`、`local-test.log` |
| 本次独立 vet | exit 0 | 适配目录 `local-vet.log` 与 stderr |
| adapter 单元测试 | 4 项 PASS | `test_joint_candidate.py` |

两份 Linux race 的 source map、选定名称与日志终态集合一致；PG 非 superuser、Redis ACL、依赖 ready 由报告记录，Linux/CGO 工具链及通过/无 DATA RACE 由原始日志核对。9 项 `TestGatewayObservationHooks*` 全部出现并通过，包括真实请求/响应字节保持的 `HTTPProvenanceAndBytes`，不是只检查函数名存在。

全库本地命令使用 OS 必需变量和明确的 Go 缓存路径白名单，移除继承的 TEST/CODEX/PG/Redis/凭据/代理开关，GOPROXY=off。首轮 GOENV=off 未带本机自定义 module cache，导致离线依赖不可见；该次失败记录保留在 `.local/joint-candidate/20260918T024751.033907Z/`，没有计为通过。补入实际本地 cache 路径后第二轮执行上述完整结果。跳过项仍不能作为验收通过。

## Adapter 合同

`codex-agent-joint-candidate-v1` 保留 `go_source_sha256`、`linux_binary`、`source_archive`、`evidence` 形状，增加 `base_go_sha256`、`overlay_delta`、`known_defect_status` 与 `evidence_roles`。消费者应按明确角色取证，不能按 basename 在多份 report/source-manifest 中任选。

角色：`source_manifest`、`gateway_race`、`management_race`、`local_verification`、`overlay_manifest`、`base_candidate`；每项含路径和 SHA256。管理 54 项前端产物只标为基线交付引用，`included_in_release=false`；发布计划仍保留已捕获现网 47 项资源。

重跑：

```powershell
python integration/gateway_environment/joint_candidate.py --management-race .local/source-20260918/local-linux-race-20260918t024608-5548d0/report.json
```

结果是**已核验联合候选**，不是发布 acceptance：`publish_eligible=false`、`migrated=false`、`deployed=false`。实际生产发布须继续由 root 汇总旧二进制回退、新后端加保留 UI 的兼容验证及现场预检，不因源码核验通过自动发布。
