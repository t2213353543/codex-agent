# 单命令热更新

入口用于已有稳定入口控制器的环境。准备好的不可变发布包仍须绑定当前发布证据；本脚本不构造验收结论、不执行数据库迁移，也不调用旧的停服后启动流程。

```powershell
python tools/hot_release.py --release .local/reliability-release/<release-id>/package/manifest.json
```

正常共享并发版本之间，执行以下流程：

1. 校验远端同 release-id 的 manifest 哈希、完整文件清单和全部文件哈希。
2. 仅当目标包不存在时，调用已有 `tools/reliability_release.py stage` 上传完整包。精确相同的包直接复用；部分包或哈希不一致时停止，绝不覆盖现有目录。
3. 调用该发布包内的 `deploy/zero_downtime_release.py` 启动候选实例。控制器核实旧实例身份、相同存储、原数据库迁移清单、候选健康及共享账号并发。
4. 在同一个控制器锁中核对本次指定的 active/candidate 身份，再切换稳定入口。旧世代保持运行，既有 HTTP/WS 不被中断。

每次控制操作都读取 `active-runtime.json`，不会把 `codex-agent.service` 或某个PID硬编码成当前世代。包装器调用服务端控制器函数时持有其原有 `zero-downtime.lock`，并在同锁内核对期望的 active/candidate，以免在状态读取后误激活另一个任务准备的包。

## 分步操作

只上传并启动候选，确认就绪后退出：

```powershell
python tools/hot_release.py --release <manifest.json> --stage-only
```

只激活已经上传、已就绪且身份匹配的候选；不会补上传或覆盖其它候选：

```powershell
python tools/hot_release.py --release <manifest.json> --activate-only
```

只读查看当前 registry/入口状态，不自动 reconcile：

```powershell
python tools/hot_release.py --release <manifest.json> --status
```

候选默认选择 18890–18909 中未被 active/draining 世代或监听占用的私有端口，也可指定 `--port 18892`。服务端继续拒绝公共端口、受保护端口和已占用端口。

## 首次旧版迁移

旧 `process_local` 世代不认识新 Redis 租约，不能与新版本并行接收推理而声称仍共同遵守账号并发。

脚本只在新实例再次确认健康后 hold 新流量，等待旧请求自然排空。默认观察窗口10秒，可用 `--legacy-wait-seconds 3` 缩短，范围0–60秒（默认仍为10秒）。此窗口只约束排空观察，不强制中断在途请求或远端系统调用。

- 自然排空：控制器切换到新共享并发实例，保留旧实例。
- 观察窗口结束仍未排空：自动 `resume-legacy` 恢复新流量到旧实例，保留已就绪候选；返回等待状态，退出码2。
- 不设置旧请求超时，不kill连接、不停止旧服务、不在本次命令内反复hold/解除hold。
- 排空窗口在服务端执行，SSH界面关闭不等于发送kill命令。主机/控制进程发生异常时仍需按实际status处理，不能假定已经恢复。

脚本不会自动 retire legacy。长连接可能使首次迁移一直无法自然完成，应检查实际连接状态并另行处理，不能把“观察超时”改成“强制排空”。

## 丢失回执与重跑

上传、候选启动或切换出现不确定结果时，先读取实际状态：

- 上传失去回执：重新验证远端包，不重复覆盖上传。
- 候选启动失去回执：检查 registry 中是否已登记同一候选，不直接重复启动。
- 切换失去回执：先 status；若存在 `pending.switching`，调用控制器的 exact-intent reconcile，再读状态。若新目标已确切提交则报告已激活；若仍是旧目标则退出，不在本次命令内再切一次。
- 已证明旧 legacy 目标仍被hold且 pending 已恢复为非switching阶段时，才可执行受身份守卫的 resume；不能盲目将流量切回旧实例。

任何错误都不会触发旧 stop-first rollback。入口耐久性未确认、registry与入口目标不一致、候选变更、共享存储或schema不同都会停止并保留世代，等待核查。

入口首次启动或重启后，`last_update_durable` 可能是 `null`。包装器只在服务端控制器锁内读取 `INGRESS_STATE`，确认磁盘 JSON 与当前入口的 revision、generation、address、held **精确一致**后，附加 `verified_persisted_target` 证据供只读状态和 stage 使用；原字段仍为 `null`。显式 `false` 不适用此兼容，即使磁盘相同也拒绝。

共享版本实际 activate 前，若仍为该已核对的初始 `null`，包装器用精确旧 revision 执行一次“相同目标、相同 held、revision+1”的 CAS，要求真实返回 `last_update_durable=true` 才继续激活。丢回复只读取并核对这次确切的新 revision，不盲重试；目标/held 被其它控制操作改变、磁盘不匹配或耐久确认失败时不激活。首次 legacy 的真实 hold 操作本身会完成持久化，不额外切换流量。

同一release已激活时，重跑返回 `already_active`，不会重新stage/switch。

## 旧共享世代回收

默认保留draining世代。可显式添加 `--retire-drained`，对每个旧**共享并发**世代仅尝试一次控制器retire。控制器必须核实无连接、无活跃handler/刷新/待持久化任务才允许停止；不满足就保留，不等待或强杀。legacy永远跳过自动retire。

## 输出与退出码

标准输出只给固定JSON摘要，包含状态、release-id、实际PID/binary SHA、是否hold和保留世代数量。不输出进程环境、管理key、token、上游凭据或原始SSH stderr。

| 退出码 | 含义 |
|---|---|
| 0 | `staged`、`activated`、`already_active` 或只读 `status` |
| 2 | 旧请求未自然排空已恢复流量，或 reconcile 后本次没有切换 |
| 1 | 包、控制器、身份或耐久状态有错误/不确定，需先核对状态 |

## 本地纯模拟验证

```powershell
python tools/test_hot_release.py
```

测试使用临时文件与假控制器，覆盖精确包复用、partial/mismatched包拒绝、stage/activate分步、CAS期望身份、默认共享切流无hold、首次legacy10秒内恢复、丢回复reconcile、不盲回滚、非就绪retire保留和敏感异常不输出。不连接SSH、不修改生产、不发模型请求。
