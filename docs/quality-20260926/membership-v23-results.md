本轮已从真实成员列表确认：new1 与 new2 都是 `standard-user`、`prolite`；两个 owner 都是 `account-owner`、`default`。四次 GET 均 HTTP 200，每次只返回一个与预登记 stable user、workspace 和邮箱一致的成员，无冲突或重复匹配。

| 标签 | 上游成员 role | 上游 seat_type | AT plan claim |
| --- | --- | --- | --- |
| new1 | standard-user | prolite | self_serve_business_prolite |
| new2 | standard-user | prolite | self_serve_business_prolite |
| new1owner | account-owner | default | team |
| new2owner | account-owner | default | team |

这是接口返回的成员字段，不是来源数据库 `preferred_seat_type` 的推断。四行均缺失 `status`、`membership_status`、`is_deactivated`、`deactivated` 和 `is_pending`；因此没有把缺失转换为 active，也没有推断停用状态。

执行时间为 2026-09-25 17:42:53–17:42:56 UTC，即北京时间 9 月 26 日 01:42:53–01:42:56。预登记顺序为 new2、new2owner、new1、new1owner，使用各 workspace owner 的当前 AT、已保存 profile/DID 和既有路由，对每个已知邮箱仅查询一次 `/backend-api/accounts/{workspace}/users?offset=0&limit=100&query=…`。未翻页、未重试，共4次成员 GET、0模型调用、0刷新、0账号写入。

匹配同时核对顶层 `id` 的 stable user，以及 `account_user_id` 的 user 与 workspace 后缀；四份报告均为 exact match。查询邮箱 hash 还与同 AT 的 email claim 摘要一致。计划、成员结果和 claims 的 AT/user/workspace/identity 摘要均能关联到 v22 的同四个凭据主体。冻结代码在每次 GET 前后检查 source owner epoch、child assignment、profile/DID及route/config；这些门禁全部通过。没有单独留存每次原始账户快照，不冒称存在额外抓包证据。

`claims-comparison.json` 是独立的零上游请求对照。四个 AT 的 `iss`、`aud`、`client_id` 和 `scp` 类型及 canonical hash逐一相同；`scp` 数组均为 `openid,email,profile,offline_access`，顺序亦相同。单独的 `scope` 字段均缺失，不把缺失当作额外授权范围。child 的 plan claim 为 `self_serve_business_prolite`，owner 为 `team`，其精确摘要与上表值一致。这是解码后的一致性证据，没有独立验签，也不证明服务端权限、内部路由或计算分配相同。

结合此前 v22：new1 两次答29，而具有相同已观察 role/seat_type/plan标签的 new2 两次答21。因此不能把 `standard-user/prolite` 标签直接当作 new1 错答的原因；这些粗粒度标签也不足以唯一预测该题输出。owner与child之间仍存在角色、席位、凭据主体及历史等混杂，四次元数据GET不是角色或权益的因果干预。v23没有新增模型样本，也不改写v22统计。

GET发生时 source image仍为 `0f06162a…`，auth源码 `46f87d6b…`，网关 `5687aa1c…`、helper `bf920a0b…`。稍后的 source cookie修复部署不替换本轮运行基线。

独立离线复核 **164项通过**。服务器上五个原始安全产物（脚本、计划、启动标记、成员报告、claims报告）与本地bytes SHA全部匹配；执行unit终态为 `MainPID=0 / inactive / exit 0`。元数据仅保留固定枚举、typed boolean/presence和hash；复核未发现原始JWT、邮箱或Bearer字面值。原始凭据/成员响应只在内存中，报告不包含其正文。

证据位于 `.local/research-20260926/membership-controls-v23/`：`membership.json` 保存实际成员投影；`claims-comparison.json` 保存四AT安全对照；`remote-evidence.json` 保存服务器文件摘要与unit终态；`review-final.json` 保存逐项复核结论及artifact hashes。

离线复核命令：

```powershell
python .local/research-20260926/membership-controls-v23/review_results.py
```
