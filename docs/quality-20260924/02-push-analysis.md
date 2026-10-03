# 28765 推送、身份与凭据边界核对

日期：2026-09-24。范围：canonical `../loophub-mother-checkout-20260920` 的只读源码核对、现有 CPA 桥接与 benchmark 核对、临时隔离接收器实现。本文未执行任何生产操作；运行态账号选择与实际糖果题由主任务执行。没有修改 28765 源码、绑定、账号、代理池、OAuth 或席位。

开始前已读 canonical 的 `AGENTS.md` 和 `docs/production-stability.md`。源码指出实际运行位于主机 `/root/loophub-28765`、Compose project `loophub-ipv6`；必须以 `docker inspect` 的当前容器和镜像为准，不可启动旧主机的轮转实例。

## 结论与证据边界

1. owner 的推送目的地接收的是该 owner 下 active **子号**，并不把 owner 自身变成模型账号。按多个 owner 数量计“多账号评测”前，必须逐条核对真正使用的子号身份、workspace 和请求日志中的实际账号。
2. 既有 `mother_benchmark_report.py` 报告的样本仅含一个实际 Agent 账号。其 23 个已完成糖果题均为 21；报告还明确保留了 401 `token_revoked` 的独立窗口以及 80 并发的 gateway capacity 拒绝。它们不支持“其他账号均正常”，也不支持“401 或容量拒绝等于模型答案下降”。
3. 401/token revoked 是凭据不可用证据，workspace 错配是身份/权限错误证据。它们可以造成请求失败、重试换号、错误样本归属。是否存在账号被分配到不同算力集群，仍需上游可观测证据；单个错误答案及代理/TLS差异不能单独证明这种分配。
4. 审查开始时旧桥 `list_files()` 仅核对 enabled、initialized、存储的 exp 和 group：未到期但已撤销 AT 可继续向源报 active，掩盖凭据修复信号。同轮主任务已安排另一个专项修复这一旧桥；本文不覆盖其最终实现或测试结论。
5. 源 `_credential_hash()` 优先哈希 RT；AT-only 接收器使用 AT。如果源更换 AT 而 RT 未变，源 destination sync hash 可保持相同，远端又报 active 时可能跳过上传。这是代码层面的可触发条件，**尚非本次真实 stale AT 事件证据**。应在请求前后比对源 AT SHA256、接收器 receipt SHA256 和 JWT exp/iat，同时记录 source OAuth 更新时间。

## 调用与身份映射

| 边界 | canonical 实现 / 本项目实现 | 必须核对 |
|---|---|---|
| 管理鉴权 | `webui/app.py`, `x-admin-key`; 密钥来自运行容器 `ADMIN_API_KEY` | 仅在服务器内存读取，不输出 `docker inspect` 的完整 Env |
| owner → workspace | `GET /api/team-rotation/status`, `owners[].id/workspace_id` | 显式 owner_id + workspace；不依赖展示名称或默认邮箱 |
| owner → active child | 同状态接口 `children`, `owner_id/status/user_id/assigned_at` | active_children 与实际 active 行相符；同一次分配代次 |
| child → AT | `GET /api/registered/{quoted_email}` 或容器内 `db.get_registered(email)` | 此 API 返回凭据，调用与响应解析只在服务器内存完成；不得打印完整结果 |
| Team AT 身份 | `team_rotation.py::workspace_oauth_identity`, `_verify_workspace_oauth` | AT 的 `chatgpt_account_id` 匹配目标 workspace；Team plan；实时 usage 验证 |
| CPA 导出 | `_sync_child_to_cpa_destination` → `exporter.export_to_cpa` | multipart `file=<email>.json`, `default_websockets=true` |
| 导出 account_id | `exporter.build_cpa_token_json` | 来自 AT claim；不是任意重贴 workspace 字段 |
| 源远端映射 | `team_rotation_sub2api_accounts` | 键为 workspace、child_email、destination_id、clone_index；CPA 只有 clone 1，remote_account_id 是 filename |
| 旧桥 → Agent | `deploy/loophub_codex_bridge.py` | 只传 AT/account_id/exp；拒绝 RT 所有权冲突；按 workspace/user 哈希保持身份 |
| 真正使用的账号 | gateway request log `account_id`, `attempts` | 用 `X-Client-Request-ID` 对齐每条测试，检查所有 attempts，不能只看最终账号 |

AT、RT、邮箱和完整 JWT payload 不进入本报告。可公开关联使用 alias、identity SHA256、workspace SHA256、AT SHA256、iat/exp、请求 ID。

## 路由与指纹不是同一个配置层

- `_proxy_for_owner()`：母号 `_resolved_team_proxy` / `proxy`，再全局 Team proxy，再代理 API；代理 API 结果按 owner 缓存约 5 分钟，避免一轮内随机漂移。
- `_oauth_network()`：母号 OAuth 专用代理优先，其次全局 OAuth 代理，再当前 Team 路由；`{sid}` 从 owner 身份稳定派生。
- `_team_workspace_oauth_for_usage()`：获取每 child 的数据库 OAuth lease，凭据持久化前核对 owner + assigned_at；成功后复用既有 Team AT。
- `oauth_repair.reauthorize_registered_account()`：复用注册持久指纹，并校验邮箱、user 和目标 workspace；不是导入桥为了实验而执行的动作。
- `_verify_workspace_oauth()`：在线 usage GET 使用 Team 路由及 owner 画像，与 OAuth 登录路由可不同。
- CPA management 推送走管理端网络。推送 JSON 不携带浏览器画像或上游代理；旧桥只有显式 `proxy_ids` 才写 Agent proxy 绑定。网关实际推理 TLS、WS、HTTP headers、出口由 Agent 配置决定，不自动继承 28765 的 OAuth 画像与出口。
- 浏览器登录画像和 Codex CLI 推理画像有不同职责，差异本身不证明错误；应核验网关自己的 CLI 请求、TLS 和代理配置是否一致，再做同凭据、同 payload、同出口的对照。

## 最小影响测试路径

实现：`tools/quality_push_receiver.py`。

采用一个新的私网 CPA receiver + 一个临时 destination。每个 allowlist owner 必须恰有一个 active child，OAuth 状态已 success、AT 剩余至少 15 分钟。只给所选 owner **追加**此 destination，保留原目的地；不改其他 owner，不把测试目的地设为独占，不触发注册或购买。

严格固定 `owner_id + workspace_id + child identity hash + email hash + assigned_at`。若自然轮转换了新 child，接收器拒收新身份；不会把后来换号的答案混入原样本。

配置示例使用占位 ID，不包含邮箱或凭据：

```json
{
  "schema": 1,
  "run_id": "quality-20260924-run01",
  "state_dir": "/var/lib/codex-agent-quality/run01",
  "listen_host": "172.19.0.1",
  "listen_port": 18882,
  "source_container": "loophub-multitenant-app",
  "owners": [
    {"owner_id": "OWNER_ID_1", "workspace_id": "WORKSPACE_ID_1", "alias": "q1"},
    {"owner_id": "OWNER_ID_2", "workspace_id": "WORKSPACE_ID_2", "alias": "q2"}
  ]
}
```

`source_network` 可选；默认只选择与 receiver IP 同网段的那个实际容器网络，若不唯一拒绝运行。配置不是凭据文件；实际 ingress key 在 prepare 时在服务器生成。state 根与子目录 0700，所有写入使用原子替换、文件 0600 和 fsync；Linux lifecycle/credential flock 分别防止并发配置和恢复时的 late-upload race。

`allowed_peer_ips` 可选，默认 `[]`，最多 4 个确切 unicast IP，不接受 CIDR、主机名、unspecified、multicast 或重复项。用于主机已有 NAT 将本机健康探测 SNAT 为特定公网 IP 的实测场景；不自动推断主机公网IP，不扩大监听范围，仍强制专属key。非空列表纳入 prepare 的 scope，之后更改必须建新 run/state；缺省与空列表兼容既有未包含此字段的 control。该配置不允许覆盖已准备run的身份或网络范围。

服务端生命周期：

```sh
python3 /opt/codex-agent-quality/quality_push_receiver.py prepare --config /var/lib/codex-agent-quality/config.json
# 在独立服务进程中执行 serve，并确认它仍存活。
python3 /opt/codex-agent-quality/quality_push_receiver.py serve --config /var/lib/codex-agent-quality/config.json
python3 /opt/codex-agent-quality/quality_push_receiver.py configure --config /var/lib/codex-agent-quality/config.json
python3 /opt/codex-agent-quality/quality_push_receiver.py snapshot --config /var/lib/codex-agent-quality/config.json
python3 /opt/codex-agent-quality/quality_push_receiver.py restore --config /var/lib/codex-agent-quality/config.json
```

`serve` 是前台常驻命令，不能与后面的 configure 串在同一前台进程等它退出；主任务使用独立 process/service 启动它。工具本次未远端执行。

Prepare 只读 source status/registered，保存 allowlist、原绑定、AT 哈希、OAuth 更新时间，不保存源 AT/RT。Configure 检查接收器鉴权 GET 可用，再创建专属 destination 并逐 owner append+复读。目的地创建响应不明时通过已落盘 phase + 精确 name/url/platform 恢复，不盲目创建第二个。

接收器只接受 RFC1918 IPv4 的显式监听地址，来源默认允许 RFC1918 与本机 loopback，另可显式配置上述 exact peer IP，并始终验证专属 key；不接受公网/0.0.0.0/localhost监听、不跟随管理API重定向、不读取环境代理。loopback 来源用于主机的鉴权健康探测，监听地址本身仍须私网。GET 不回 token；普通无 filename 的 GET 仅做就绪探测。带 filename 的 GET 返回 CPA schema，并在本地过期或存储完整性不匹配时返回 error。CPA `active` 仅表示本地已接收且未过期，**不是上游实时认证/推理质量保证**；接收器没有推理代理功能，也不会为了 GET 状态调用模型。

每个 alias 的 AT 文件为 `state_dir/credentials/<alias>.json`，字段 `access_token`、`account_id`、`expires_at`，另有 alias/identity SHA256。RT/id_token 一律丢弃。每次有效上传都会落入最新 AT；同身份的更旧 iat（或相同 iat 但更早 exp）倒退上传会被拒绝，更新 iat 且 TTL 缩短的正常替换允许。`snapshot` 输出 `accounts[].credential_file`、alias、hash、iat/exp、upload_count、接收时间，供 Go live harness 在服务器本地加载；snapshot 不输出邮箱/AT/RT。

Restore 首先核对 destination 所有权，拒绝删除被未选 owner 绑定的本次 destination；逐 owner 只减本次 ID，保留原绑定与并发新增绑定；复读确认后删除自有 destination。源正常 DELETE 流程撤下接收记录，最后设置 restored 并删除遗留 AT，保留无凭据回执和 control 便于核验。目的地 API 没有版本 CAS，configure/restore 都做事前检查与事后复读；无法将跨多个 owner 的远端操作包装成原子事务。失败时保留 control 和 phase，修复可见冲突后重跑同一 restore，不复制旧列表覆盖并发变化。

## Server-only 只读查询与比较

`Source(config)` 在服务器上执行一次 `docker inspect`，仅在内存取 ADMIN_API_KEY 和匹配的 Docker network IP。调用 `Source.call('/api/team-rotation/status')` 后应只输出 allowlisted owner 的匿名摘要，**不能直接 print(status)**：owner.proxy 可能包含代理用户名密码，events 可能包含邮箱。

单 child credential 的具体只读调用：

```python
row = source.call('/api/registered/' + urllib.parse.quote(child['email'], safe=''))['data']
identity = jwt_identity(row['access_token'])
receipt = read_private(state_root / 'receipts' / (alias + '.json'))
safe = {
    'alias': alias,
    'source_at_sha256': sha(row['access_token']),
    'received_at_sha256': receipt.get('token_sha256'),
    'same_at': sha(row['access_token']) == receipt.get('token_sha256'),
    'workspace_matches': identity['workspace_id'] == selected['workspace_id'],
    'user_matches': identity['user_id'] == child['user_id'],
    'expires_at': identity['expires_at'],
    'source_oauth_updated_at': child.get('oauth_reauth_updated_at'),
}
print(json.dumps(safe))
```

对 existing Agent account，服务器读取 `/var/lib/codex-agent/client-key` 后通过 `GET /api/accounts/{id}`、`GET /api/accounts/{id}/runtime?model=...`、`GET /api/accounts/{id}/upstream` 获取 metadata、有效策略与已有上游快照；读取 `/var/lib/codex-agent-loophub-bridge/state.json` 仅挑选 hash，不下载原文件。请求真相从独立测试 request ID 与 gateway `attempts` 关联。model/effort、service_tier、response.model、reasoning_tokens、终态/流是否完整、timeout、真实出口和加载二进制哈希分别记录。

数据库深查优先容器内只读 `SELECT`，显式只读 transaction，或调用已知只读接口；不要为取得 token 导入 `webui.app`/运行 `init_db`，它们可能带启动或迁移动作。`db.get_registered` 本身不刷新OAuth。不要调用 export API 不明入口、repair/reconcile、fingerprint regenerate 来“读取状态”。

## 必须分开的至少 12 个故障边界

1. **owner 与实际 child 混淆**：多 owner 并不等于多实际推理身份；按 child user/AT 和 attempt account_id 核验。
2. **workspace 错配**：个人/free token 被手动冠上 Team account_id；源与 receiver 都检查 AT 原 claim，失败记为身份错误，不记糖果错误。
3. **AT 已撤销但尚未到期**：JWT exp 校验不足；需要真实 401/已有凭据版本匹配的上游状态，不能仅看 bridge active。
4. **源 AT 与接收端 AT 不同**：RT hash 优先的去重可能漏掉 AT-only 更新；比对 SHA256 与上传时间，不用 source success 文案代替。
5. **RT 重复消费**：旧 bridge/新 receiver均不持有 RT，测试不刷新；原有其他接收端若仍刷新同一RT，属于独立的潜在撤销来源，未经事件证明不能归因。
6. **OAuth 与 Team/proxy 与推理出口不同**：分别记录稳定 route 的摘要；不输出带密码 URL，不把 OAuth proxy 当作模型请求实际代理。
7. **自然轮转中途换号**：绑定新目的地会 wake owner；既有监控仍可能 OAuth/轮转，工具不承诺阻止它。用 assigned_at、oauth_updated_at、AT sha 的前后差异划分实验窗口，身份改变即作废/重建allowlist。
8. **错误被最终成功隐藏**：gateway 可能多 attempts、换 account；必须按全部 attempts 归因。
9. **部分或错误 SSE 重构**：旧基准已遇到 completed.output 空但 output_item.done 完整；保留 raw terminal、独立 done events、最终输出，协议错误不记有效答案。
10. **本地下游容量拒绝**：旧 80 并发 503 capacity 未到上游；质量对照从小并发配对开始，不把容量拒绝或 timeout当作答案下降。
11. **归因采样范围错误**：`mother_benchmark_remote.py` 的 monitor/log query 固定旧 setup.group_id/key_id；从 extra 覆写 `--key-file`/base-url 虽能改变请求目标，但监控和日志仍是旧 scope。多账号新实验不能直接沿用它的覆盖结论，应独立记录 exact request IDs/credentials。
12. **回滚误撤其他目的地**：`set_owner_destinations()` 会对移除的目的地执行远端 DELETE，再清映射；必须 append 并在恢复时只去掉本次ID，不把 owner.destination_ids 设置成只含测试目的地，也不全量回写陈旧snapshot。

## 本机验证

```powershell
python tools/test_quality_push_receiver.py
```

17 项合成用例通过：真实 localhost HTTP 鉴权与 multipart；RFC1918/loopback管理来源允许且公网来源默认拒绝，监听地址仍不允许loopback/public；额外exact IP仅放行该地址且纳入scope，默认空与既有control兼容，CIDR/multicast/unspecified/无效/超量/重复IP拒绝；AT-only；同身份AT更新与回退拒绝，更新签发但TTL缩短的AT正常接收；workspace/user/email/未知child拒绝；无效JWT/过期/未来iat拒绝；状态完整性；撤下再上传；source child代次改变；绑定并发冲突；未知创建响应恢复与幂等；未选owner保护；append/restore保留原地点及并发新增地点并删除AT；公开监听地址与scope变更拒绝。Python 编译检查通过。

这些测试验证接收和恢复契约，不代替主任务的真实推送、原绑定恢复、源OAuth未意外改变或糖果题对照结果。
