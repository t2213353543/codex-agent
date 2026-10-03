# 28765 母号推送到 Codex Agent

2026-09-25 已修复管理标签丢失导致的 409 推送失败，并为新母号建立独立的 18882 接收器。当前两个 active 子号均已交付、源端状态 success。以下单母号内容是首次配置记录，当前拓扑与修复边界见 [9 月 25 日推送恢复记录](LOOPHUB-PUSH-REPAIR-2026-09-25.md)。

2026-09-24 已将母号 `radios.plucks-60@icloud.com` 的推送地点设置为 Codex Agent。源服务当前只支持 `sub2api`、`cpa` 两种推送协议，因此在 Codex Agent 主机上增加一个独立 CPA 兼容接收器，未修改或重启 28765 应用，也未替换 Agent 后端。

## 当前范围与运行位置

| 项目 | 已核实值 |
| --- | --- |
| 源容器 | `loophub-multitenant-app` |
| 源镜像 | `loophub-ipv6-app:20260920-mother-checkout-92f39d7f` |
| 源内部 API | `http://172.19.0.3:8765`，以当前 Docker 网络地址为准 |
| 母号 ID | `02883395-1be1-461a-abe7-b01aeb6bd4b0` |
| Workspace | `8de82259-224e-4190-8950-346571251bd0` |
| 新目的地 ID | `12411e170dca484d9b993976164611b1` |
| 新目的地名称 | `codex-agent · radios.plucks-60@icloud.com` |
| 新目的地协议 / URL | `cpa` / `http://172.19.0.1:18881` |
| systemd | `codex-agent-loophub-bridge.service` |
| 监听地址 | Docker bridge 私网网关 `172.19.0.1:18881`；没有监听公网或 `0.0.0.0` |
| Agent 管理 API | `http://127.0.0.1:18879` |
| Agent 专属分组 | `g_d90349cb75cb` |
| 初始推送账号 | 子号 `school_96alcoves@icloud.com`，Agent ID `a_441d76471862216532603a0ec1045ad9` |
| 账号并发 | `80`；此数值是准入配置，实际成功率以独立压测报告为准 |

原母号 `destination_ids=[]`。本次仅创建该母号专属目的地并绑定该母号，没有移除其他目的地或其他母号的绑定。母号目的地推送的是它所属的 active **子号**，不是母号本人。

运行代码：`/opt/codex-agent/loophub-bridge/loophub_codex_bridge.py`。配置及状态目录：`/var/lib/codex-agent-loophub-bridge/`。配置文件 `config.json` 指向 `ingress-key`、`state.json` 及现有 `/var/lib/codex-agent/client-key`；这些密钥不可写入文档或验收输出。

## 原始源协议与调用路径

权威本地源快照为 `../loophub-mother-checkout-20260920`，对应运行镜像 revision `92f39d7fd591d0a1e1934193169fe1e0e0866309`。阅读其他 Loophub 目录时不能将其自动视为本次运行源码。

1. `webui/app.py` 的 `GET /api/team-rotation/status` 返回母号、子号及目的地。PG 模式管理接口使用 `x-admin-key`，密钥来源是容器的 `ADMIN_API_KEY`。
2. `POST /api/team-rotation/sub2api-destinations` 创建目的地。本次字段为 `platform:cpa`、名称、私网 URL、接收密钥、`enabled:true`、`timeout:120`。
3. `PATCH /api/team-rotation/owners/{owner_id}`，载荷 `{"destination_ids":["..."]}`，调用 `TEAM_ROTATION.set_owner_destinations()` 并定向唤醒该母号。
4. `webui/team_rotation.py::_sync_child_to_cpa_destination()` 调用 `webui/exporter.py::export_to_cpa()`，向目的地 `POST /v0/management/auth-files` 发送 multipart：`file=<email>.json` 和 `default_websockets=true`。
5. `build_cpa_token_json()` 的文件字段为 `type`、`email`、`expired`、`id_token`、`account_id`、`access_token`、`last_refresh`、`refresh_token`。`account_id` 来自 AT 的 `https://api.openai.com/auth.chatgpt_account_id`。
6. 上传后，源 `get_cpa_auth_file()` 请求 `GET /v0/management/auth-files?name=<email>.json`。接收器返回 `{"files":[{"name":"...","status":"active","disabled":false,"unavailable":false,"websockets":true,"default_websockets":true}]}`，源确认可用后记录 `cpa_destination_synced` 成功事件和远端映射。
7. 取消目的地绑定、移除子号时，源 `set_owner_destinations()` / `_delete_destination_remote_account()` / `delete_cpa_auth_file()` 请求 `DELETE /v0/management/auth-files?name=<email>.json`，随后清理源远端映射。更换绑定可能撤下旧地点账号，维护时应先核对已有绑定。

源会发送 `Authorization: Bearer <key>` 和 `X-Management-Key: <key>`。接收器兼容这两种请求头，仅允许专属接收密钥；所有上传都核对 AT workspace、用户身份、邮箱、到期时间及文件名。邮箱不使用初始化固定名单，因此同一 workspace 后续轮转的新子号可以自动推送。专属密钥只配置给本母号目的地。

源网页的“测试目的地”接口先执行公网 URL 校验，可能拒绝这个明确配置的私网接收地址。实际验收使用容器网络上的鉴权 GET、真实 multipart 推送和源成功回执，不以网页测试按钮代替实际推送结果。

## Agent 写入与撤下行为

- 初始导入使用 `POST /api/accounts`，`replace:false`，固定 workspace/email 派生名称及专属 tag。初始账号先禁用，随后设置专属分组、并发 `80` 并启用。
- **仅转发 AT**、workspace ID 和真实到期时间。源接收文件中的 RT、id_token 不转给 Agent；源服务继续独占 RT 刷新职责，避免两个系统竞争轮换。
- 更新 AT 使用稳定 Agent ID 的 `POST /api/accounts/{id}/reauthorize`，提交最新 `version`、`allow_identity_change:false`。凭据未变化时不重新授权。
- 修改账号前确认专属 tag、固定名称和 `has_refresh_token=false`，拒绝接管其他来源账号。初始化导入后的短暂崩溃可按固定名称找回自有账号，并以 CAS 重新授权。
- 接收 DELETE 后对**本接收器所属账号执行禁用**，保留请求历史，状态中标记 `withdrawn`；CPA 列表不再显示该文件。后续同身份重新推送可恢复启用。
- `state.json` 只保存映射、摘要、到期时间和处理状态，不保存 AT、RT 或接收密钥。状态原子写入并 fsync。
- `proxy_ids` 为接收器配置项。本次部署值为 `[]`，即新账号使用 Agent 当前直连路径；该配置与源 OAuth 代理分别管理。

`GET /health` 仅证明适配器进程接受请求。鉴权 `GET /v0/management/auth-files` 进一步读取 Agent 实际账号元数据；模型调用成功仍须另行确认，不能将成功入库当作上游推理通过。

## 维护与复核

服务器内只读状态命令：

```sh
systemctl status codex-agent-loophub-bridge.service --no-pager
```

源绑定脚本可幂等重跑；它先检查鉴权接收接口、母号身份及 workspace，再创建或复用准确目的地、追加绑定并复读验证，不修改其他母号：

```sh
python3 /opt/codex-agent/loophub-bridge/loophub_codex_configure.py --config /var/lib/codex-agent-loophub-bridge/config.json
```

本地文件为 `deploy/loophub_codex_bridge.py`、`deploy/loophub_codex_configure.py`。重启接收器不应删除 `state.json` 或重建接收密钥；否则会丢失撤下映射或导致源鉴权失败。母号迁移到其他 workspace 时应建立新的专属配置与映射，不能直接覆盖当前 `workspace_id`。

临时停止接收器使用 `systemctl stop codex-agent-loophub-bridge.service`。这会停止接收新推送，**不会自动禁用已导入账号**；源取消目的地绑定则走正常 DELETE 撤下流程。

验证记录保存在本地 `.local/mother-benchmark/source-push.json`，其中包含脱敏源绑定、运行服务状态、代码 SHA256、实际 Agent 账号及一分钟只读观察结果。已完成 Python 编译检查和 12 项合成契约验证：真实 HTTP 鉴权、CPA multipart、并发/代理/分组写入、仅 AT 转发、幂等、CAS、重启、撤下保留历史、再次上传、workspace 拒绝、状态无凭据、导入崩溃恢复。真实模型性能由同目录其他基准报告记录。

## 已观察到的自动凭据恢复

2026-09-24 06:36:50 UTC，源自身的 OpenAI 用量探测遇到 OAuth HTTP 401，同时基准请求记录上游 `token_revoked`。源主动监控随即启动原有重新授权流程，于 06:37:04 获得新 AT，06:37:12 通过本适配器完成推送并恢复源调度。没有另外执行 RT 刷新或人工改写凭据。

06:39:03 UTC 核验源保存 AT 的 SHA256 与适配器 `token_sha256` 完全一致；母号和子号仍 active，原高级席位仍 assigned，Agent 账号 version 为 12、enabled=true、last_error 为空。该现场证明真实 AT 更新已沿既有源恢复流程同步；上游撤销原因尚无证据，不能据此归因为代理差异、适配器或某一种请求。脱敏时间线及摘要见 `.local/mother-benchmark/token-recovery.json`。恢复后推理成功率须读取独立模型测试结果。
