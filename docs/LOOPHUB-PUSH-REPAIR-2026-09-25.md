# 28765 子号推送恢复

2026-09-25 北京时间 13:48 验收：当前两个 active 母号的子号都已成功推送至 Codex Agent，源端 `pair_sub2api_status=success`、错误为空，接收器读回 `active`，目标账号已启用、工作区及用户身份匹配、只有 AT。未调用模型或刷新 OAuth，未重启 28765 或主网关。

## 真实故障

原母号绑定正常、网络正常，但接收器 `GET auth-files` 和源上传返回 **409 `destination_ownership_changed`**。源事件每约两分钟记录 `destination_reupload_started` → `sub2api_destination_error`。

对应目标 `a_441d76471862216532603a0ec1045ad9` 的标签被清空。稳定 ID、名称、workspace/user、持久映射和数据库 token fingerprint 均一致，RT 仍不存在，证明这里是可编辑管理标签丢失。现有材料未能确定清空标签的具体操作，不把它归因于上一轮网关发布。

另一个刚启用的母号 `71c653a1-c904-4951-8030-46c7d07a331b` 已有 active 子号，但 `destination_ids=[]`，没有任何推送目的地。原接收器只绑定单一 workspace，不能把其他 workspace 直接发送进去。

## 修复

`deploy/loophub_codex_bridge.py` 保留来源边界，并允许已有持久映射在标签缺失时通过只读 metadata 再核验：目标 ID/名称匹配、明确无 RT、metadata ID 与账号版本一致、workspace/user 摘要等于原检查点，全部成立才接受。上传后的既有 CAS 更新会合并恢复标签，保留其他标签。GET 不写数据；未知映射、身份替换、版本漂移、RT 冲突仍拒绝。

单个已删除或身份不匹配的文件会单独显示不可用，`needs_reauth=false`，不再使整个列表失败；临时数据库/API 故障仍返回真实失败。撤下流程同样核验所有权。

先通过管理 CAS 恢复已证实的旧标签（version 14→15），核对 AT、其他配置和历史不变。正常源调度随后补推完成，账号 version 16；没有独立导出/重放凭据。

为新母号创建独立配置、密钥、状态、私网接收器，再通过现有源管理 API 追加目的地。没有取消其他母号绑定或修改轮转配置。绑定唤醒原有调度后，自然推送成功。

| 工作区 | 入口 | systemd | 目标账号 |
| --- | --- | --- | --- |
| `8de82259-224e-4190-8950-346571251bd0` | `172.19.0.1:18881` | `codex-agent-loophub-bridge.service` | `a_441d76471862216532603a0ec1045ad9` |
| `c8d67136-d420-4572-9e84-d482c2579b06` | `172.19.0.1:18882` | `codex-agent-loophub-c8d67136.service` | `a_c4625c15b031cf53efee0d31601531fd` |

两个入口均仅绑定 Docker 私网。新配置/状态目录为 `/var/lib/codex-agent-loophub-c8d67136/`，共用版本化核验的接收源码；分组为 `g_d90349cb75cb`，初始并发 80。其他母号的目的地及全局调度配置保持。

## 验证

新增 11 项回归先复现缺标签 GET/POST/withdraw 和单个坏映射阻塞全表，再修复通过；加上原 credential-state/source-contract 共 **32 项通过**，编译检查通过。覆盖身份/版本漂移、未知映射、标签合并、CAS 冲突、持久重载、仅 AT、撤下保留历史以及逐文件隔离。

13:46:49 接收器源码已部署，SHA256 `e8dd0228a1704a00141407937570f593698d8286a8c006648a03b1f530e1b3c1`；两个接收器 HTTP 200，各 1 个 active 文件、0 unavailable。主网关 PID 不变，仍是本轮缓存修复版本 `ddb0f96d…`；28765 镜像仍为 `sha256:2cd8ff0aa7d851baa7e8688a62104be11e003bdff5263a07d4952dfa38ca3ceb`。

源端真实成功回执：旧母号 event **56514**、新母号 event **56515**，均为 `cpa_destination_synced`。13:48:08 两个子号仍 active，源 pair 状态 success，retry/pair 错误为空，映射后没有新的目的地错误。此验收证明交付，不把它写成模型推理测试。

证据目录 `.local/push-repair-20260925/`：`inventory.json`、`identity.json`、`tag-restored.json`、`new-workspace-bound.json`、`receiver-deployed.json`、`verification.json`、`ownership-red.log`、`final-tests.log`、`bridge.patch`。服务器同名检查目录 `/opt/codex-agent-integration/checks/push-repair-20260925` 保存运行回执及原接收器代码。

本地回归：

```powershell
python -m unittest discover -s tools -p 'test_loophub_bridge*.py'
```

后续新母号若需推送，须有对应工作区的私网接收配置并绑定目的地。不可直接改旧配置的 workspace 或复用旧状态，否则会混淆原映射。
