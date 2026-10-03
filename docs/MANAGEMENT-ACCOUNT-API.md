# Codex账号管理API实际契约

> 当前修订：项目不包含计费。下方历史成本/倍率相关接口与选项不再启用，账号和资源/租期功能保留。旧 bundle 成本段兼容忽略，显式计费导入/导出选项返回 400，详 [无计费契约](MANAGEMENT-NO-BILLING-SCOPE.md)。

更新于2026-09-18；对应现有Codex账号，不扩其他供应商。管理API沿用现有管理员Cookie/CSRF或Bearer认证，所有响应no-store。路径中的账号标识要求稳定ID，不用名称重新解析写入目标。此文仅记录已落源码，PG通过状态以隔离验收日志为准，不把未配置环境的skip计为PASS。

## 列表、选择和写入

`sort_by=last_used_at` 支持最近业务请求全结果排序；升降序均将无匹配记录者置后，同值按账号 ID 同方向排序。PG 依据持久 usage_events，文件依据留存且带稳定 ID 的明细；不使用旧按日累计或名称猜测。列表、选择和筛选导出复用该规则。

`GET /api/accounts/manage`支持完整筛选：search（名称/稳定ID/备注/标签不区分大小写子串）、group_id、proxy_id、tag（精确）、status=ok/error/unverified/disabled/expired、enabled、has_refresh_token、ungrouped、no_proxy、expires_before（不含上界，未知expiry不匹配）、expires_after（含下界）。日期使用Unix秒整数，布尔true/false；拒绝未知/重复query参数。

sort_by=created_at/updated_at/name/priority/weight/concurrency/quota/expires_at，sort_order=asc/desc，默认created_at desc，ID同方向稳定决胜。page默认1，page_size默认50最大500。返回`{accounts,total,total_kind:"exact",page,page_size,has_more}`；PG只读repeatable-read获取总数和页，文件模式扫描真实全量。ETag覆盖响应所有字段，包含实时并发/用量；If-None-Match命中返回304空body，仍no-store，前端只在当前内存保存响应。

`POST /api/accounts/selection`接`{filters:{...筛选排序，不含page/page_size}}`，返回`{items:[{id,version}],total,limit:10000,filter_hash,selected_at}`。超过10,000明确413，不截断。选择固定当时版本，执行时逐项CAS，并非持有全量数据库锁。

`POST /api/accounts/actions`接`{action:update/delete/refresh,items:[{id,version}],patch?}`。update/delete最多100，refresh最多10；数组groups/proxies/tags完整替换，省略不改，空数组清空。patch不带version；扩全部既有数值字段。返回`{results:[{id,ok,code?,error?,version?}],succeeded,failed}`。错误脱敏，CAS失败不会调用OAuth或将账号标失效；开始的RT轮换保留原凭据持久化保护。

`POST /api/accounts/{id}/reauthorize`接`{version,credentials,allow_identity_change}`，保留ID、Name、管理元数据、关系、调度数值、配额窗口和创建时间。原account_id或已知user_id变化/缺失时409 identity_change_requires_confirmation；新refresh_token/client_id缺失不沿用旧身份值。成功`{ok:true,id,version}`，凭据状态unverified。`POST /api/accounts/reauthorize-batch`接`{items:[{id,version,credentials,allow_identity_change?}]}`最多10条，返回逐项ok/code/error/version及succeeded/failed，绝不按名称匹配。

## 导入、导出

`POST /api/accounts/export`是metadata导出，`POST /api/accounts/export/credentials`是独立显式凭据下载。body为`{filters}`或`{items:[{id,version}]}`互斥。最多10,000条、JSON最大30MiB，超限413。items任一版本过期则整个导出409；JSON attachment、no-store，凭据导出PG审计。metadata不含access/refresh/id_token/client_id等认证字段。

凭据文件为`{type:"codex-agent-accounts",version:1,exported_at,accounts:[{name,notes,tags,enabled,concurrency,priority,weight,groups,proxies,quota,window_days,window_start,created_at,credentials:{access_token,refresh_token,account_id,client_id,expires_at}}]}`；metadata类型为codex-agent-account-metadata。

`POST /api/accounts/import-bundle`接`{data:上述凭据文件,dry_run,replace,group_mapping?,proxy_mapping?}`。新账号恢复管理值、生成新ID；外部关系ID必须映射到真实本地关系，未知关系逐项失败。dry_run不落数据。响应`{imported,added,duplicate,replaced,failed,dry_run,results:[{index,name,id?,status,code?,error?,warnings?}]}`。metadata文件不能恢复凭据；普通auth.json/Sub2API格式继续原导入路由。

相同access_token且相同/空新RT为duplicate；同AT但新非空RT不同，则保存新RT及明确传入的client_id，保留本地所有管理字段，结果replaced。冲突account_id拒绝；dry-run不轮换。既有同名不同token仅在replace明确开启时替换凭据。

凭据导出可加`include_resources:true`，只包含选中账号关联的分组与代理。代理用户名/密码须另加`include_proxy_credentials:true,confirm_proxy_credentials:true`，未确认不输出秘密。bundle.resources=`{version:1,groups:[{id,name,description}],proxies:[{id,name,type,host,port,enabled,has_credentials,credentials_included,username?,password?}]}`。

恢复分两阶段：`POST /api/accounts/import-resources`接`{resources,dry_run,group_mapping?,proxy_mapping?,confirm_proxy_credentials?}`；返回`{dry_run,results:[{kind,source_id,id?,status:create/created/mapped/failed,code?}],group_mapping,proxy_mapping,succeeded,failed}`。dry-run无写；资源同名冲突须明确映射，绝不覆盖；有认证但备份缺秘密返回credentials_missing，写含密码需确认。资源逐项创建并保留enabled；产生的mapping交原import-bundle恢复账号关系。两阶段如实显示部分完成，不宣称跨store原子；不探测代理地址或推理。

## 身份元数据和隐私筛选

导入支持OAuth tokens旁的官方agent_identity JWT字符串或Record，读取workspace/user/runtime/email/plan元数据并与OAuth user/workspace核对；私钥不保存、不返回。纯agent_identity私钥认证缺少任务签名流程，明确拒绝，不把私钥作为Bearer。稳定identity.stable_key是workspace+user的摘要；同工作区不同用户保持不同身份，定向重授权跨用户需确认。JWT未验证信息明确verified=false。

`GET /api/accounts/{id}/metadata`返回`{id,version,identity:{workspace_id?,user_id?,email?,plan_type?,agent_runtime_id?,stable_key?,source,collected_at,verified:false},privacy:{status:disabled/unknown,source,collected_at,stale},upstream:{kind:{source,collected_at,stale,ok}}}`。GET不发上游请求；旧账号即时解析JWT来源的collected_at=0表示未保存采集时间。

账号列表、全结果selection及按filters导出统一支持`privacy=disabled|unknown`。仅成功、5分钟内且凭据版本相符的禁训练快照视作disabled；没有查询能力时unknown，不能伪称enabled。新凭据使原隐私证据失效。

## OAuth会话

| API | 请求与结果 |
|---|---|
| POST /api/account-oauth/start | `{proxy_id?,target_id?,version?}` -> `{session_id,phase,auth_url,redirect_uri,expires_at}`；PKCE S256、随机state、localhost1455固定回调、30分钟TTL |
| POST /api/account-oauth/exchange | `{session_id,code,state}`，code可完整精确回调URL；管理员会话绑定、一次性claim、真实官方交换，凭据私有暂存到ready |
| POST /api/account-oauth/import | method=refresh_token/mobile_refresh_token/session/pat；RT字段refresh_token/client_id，session字段credentials，PAT字段access_token；同样可指定proxy_id/target_id/version |
| GET /api/account-oauth/{session_id} | 只读session_id/phase/method/expires_at/target_id/has_refresh_token/expires_at_token/identity_change/target_changed；不返回凭据 |
| DELETE /api/account-oauth/{session_id} | 取消并清除未保存凭据、state/verifier；交换进行中409 |
| POST /api/account-oauth/apply | `{session_id,name?,defaults?,allow_identity_change?}` -> `{ok:true,id,version}`；固定target CAS，幂等重读已保存结果 |

mobile_refresh_token必须提供原client_id，不猜移动客户端ID；session接导出的OAuth JSON，支持accessToken/refreshToken等常见字段，不能将浏览器session cookie伪装成OAuth token。PAT要求at-并执行官方whoami，拒不支持FedRAMP环境。配置代理为空且target存在时使用账号绑定代理；有绑定但不可用不回退直连。开启OAuth不会顺带改隐私设置或发模型测试。

OAuth apply新建还可传notes（≤8192字节）、tags（≤50项各≤100字节）、enabled；target重授权提交这些管理字段会拒绝，避免凭据更新意外覆盖原管理配置。

持久私有state在PG使用既有加密文档，文件模式目录0700/文件0600（不宣称文件内容加密）。上游错误正文和凭据不回显。

## 上游快照、测试和控制

`GET /api/accounts/{id}/upstream`只读`{id,snapshots,persistent:true}`，绝不访问上游。快照字段id/version/credential_version/kind/model?/ok/http_code/checked_at/latency_ms/source/stale/error?/data。凭据变化或超过5分钟为stale；未知用量字段null，不混为本地Quota。

`POST /api/accounts/{id}/upstream`接`{version,kind:models/usage/stream/compact/reset_credits,model?,prompt?,confirm_inference?}`。stream/compact需exact model与confirm_inference，prompt最大4096字节。模型目录仅为catalog存在，不冒充推理验证。测试必须收到response.completed，HTTP200 failed/incomplete仍失败；compact必须有真实compaction，只返回存在性，不泄漏encrypted_content。保留无自动别名、刷新、重试的行为。

`POST /api/accounts/{id}/test-stream`接`{version,model,prompt?,confirm_inference:true}`，返回真正text/event-stream：event phase/data `{phase:"connecting"|"streaming"}`；event delta/data `{text}`；event result/data完整快照。delta在上游结束前flush。客户端取消传递上游，取消的测试不缓存成功；旧JSON路径保留。测试独立于网关业务日志/计量和账号调度状态。

`POST /api/accounts/upstream/batch`接`{items:[{id,version}],kind:models/usage,force?:true}`，最多10项，始终显式实时读取；每项8秒，总90秒。返回`{results:[{id,ok,code?,snapshot?}],succeeded,failed}`；不支持推理kind，不会因页面轮询自动请求上游。

`POST /api/accounts/{id}/privacy`接`{version,disable_training:true,confirm:true}`，按参考PATCH禁训练，返回上游ack。没有伪造“已读取当前隐私”的状态。

reset_credits支持参考的snake/camel字段、直接数组及credits/rate_limit_reset_credits/items/data列表信封；上游数量缺失但有完整列表时按已知可用券计算并标count_source=credit_list，缺源数据不伪造0。消费前服务端重新查询，必须status=available、reset_type=codex_rate_limits、已知且未来expires_at，并再次校验账号版本。`POST /api/accounts/{id}/reset-credit`接`{version,credit_id,request_id,confirm:true}`。同request_id持久幂等，不同credit冲突；返回上游code=reset/nothing_to_reset/no_credit/already_redeemed以及windows_reset。未知结果及5xx标outcome_unknown且不自动重试；不重置本地Quota或配额窗口。root另行实现显式开启的管理自动重置策略，通过同一fresh验证/幂等方法调用。

## 定时测试

`GET/POST /api/accounts/{id}/schedules`返回`{plans}`/新plan。创建body`{kind,model?,cron,timezone?,enabled,max_results?,confirm_inference?}`；每账号20计划，kind=models/usage/stream/compact，启用推理计划需显式confirm_inference。默认UTC，支持IANA；五字段cron支持列表/范围/步长、月份/星期缩写、DOM/DOW标准关系、DST。默认保留20结果，最大100；5年内无触发点拒绝。

`PUT /api/account-schedules/{id}`同完整配置+version；`DELETE ...?version=`；`GET .../results?limit=1..100`返回`{results,running}`。plan含id/account_id/version/kind/model/cron/timezone/enabled/max_results/next_run_at/last_run_at/running。结果含id/scheduled_at/started_at/finished_at/status/snapshot?，status=running/ok/failed/skipped/unknown。

root独立10秒worker调用处理器，最多4并行，先持久claim和90秒lease，再请求。多实例不能重复同回合；重启不补发错过的全部时段。过期运行标unknown，不重放；禁用/删除不新派发，已发送请求可能完成；账号禁用或删除为skipped。配置写入与审计同事务，worker checkpoint不生成审计噪音。

## 独立账号租期

`GET /api/accounts/{id}/lease`返回`{id,account_version,lease:null|{version,expires_at,auto_disable,restore_on_extend,disabled_by_lease,disabled_version,disabled_at,restore_skipped_reason?}}`。`PUT`接`{version:0或当前租期版本,account_version,expires_at:Unix秒或0不限,auto_disable,restore_on_extend}`。

租期独立于token expires_at。只在显式auto_disable后到期关闭现有enabled；延期恢复须证明由租期停用且账号版本仍等于停用版本。后续手工修改时保留关闭并返回account_changed。PG启停、加密lease来源、审计同事务；文件模式lease写入accountState.managed_account_leases，同一个文件原子替换。故障注入覆盖来源保存失败时停用与恢复双向回滚。

## CRS

`POST /api/accounts/sync/crs/preview`接`{base_url,username,password,proxy_id?,allow_insecure_http?}`。默认HTTPS，HTTP要显式true；拒URL内凭据/query/fragment及重定向。参考Sub2API调用POST /web/auth/login与GET /admin/sync/export-accounts?include_secrets=true，只导入openaiOAuthAccounts。最多10,000行，导出16MiB、持久预览8MiB，15分钟TTL。

返回`{preview_id,expires_at,source,accounts:[{source_id,name,enabled,priority,has_refresh_token,has_proxy,target_id?,target_version?,status:ready/invalid,error?}],total,scope:"openai_oauth_only"}`。预览不写账号或代理，不回传秘密。

`POST /api/accounts/sync/crs`接`{preview_id,selected_account_ids:[source_id],sync_proxies,allow_identity_change}`，1至100项。单次claim；完全相同请求重读结果，选项变化冲突。响应`{results:[{source_id,id?,version?,status:added/updated/duplicate/failed,code?}],succeeded,failed}`。同源sourceID持久绑定本地ID，更新要求预览版本一致；已有账号只更新凭据，保留管理字段。新账号用源启用/优先级/说明；sync_proxies只为新账号复制/复用源代理，已有代理不改。

同步不隐式刷新OAuth、不推理、不删除源账号、不导入其他平台。中断不盲重放running预览，可重新预览，凭借源绑定、凭据幂等和CAS恢复；未知半完成不宣称完整回滚。

## 验证与文件边界

源码：admin_accounts_management.go、admin_account_oauth.go、admin_account_upstream.go、admin_account_controls.go、admin_account_schedules.go、admin_account_leases.go、admin_account_test_stream.go、admin_account_crs.go、admin_account_upstream_batch.go、admin_account_identity.go、admin_account_bundle_resources.go；admin.go仅管理路由；accounts.go新增管理identity和file租期容器，accounts_import.go/accounts_pg.go扩管理导入RT保留/身份密文及刷新前置冲突。测试为同名前缀_test及admin_account_management_refresh_test.go、admin_account_controls_validation_test.go。

已通过文件隔离专项与全库go test ./...（11.623s）、go vet ./...，日志.local/management-parity/backend-third-go-test.log与backend-third-go-vet.log。PG入口TestPGManagedAccountsContract、TestPGManagedOAuthFlows、TestPGManagedUpstreamChecks、TestPGManagedAccountControls、TestPGManagedSchedulesAndLeases、TestPGManagedAccountTestStream、TestPGManagedCRSSync、TestPGManagedAccountETagBatchAndRotation；具体结果由root记录。

测试覆盖8并发单claim/重启/unknown、租期来源恢复、真实SSE提前delta/abort、CRS无写预览/秘密隔离/绑定CAS、ETag304及RT轮换dry-run/幂等/冲突。全部是httptest合成上游，没有真实账号或模型调用。账号今日批量、本地usage/成本/趋势、清理任务由root模块负责；网关转发文件未被本组修改。

## 冻结版本发布兼容性复核

复核对象为冻结Go归档manifest `8878b6da648605146d9e6c3929b132e2fd46cdc88209c18ed0ad157f537cd60f`。schema 003仅新增`management_cleanup_targets`及到期索引，不修改account_credentials、accounts或settings字段，不改旧迁移checksum。新服务启动会检查全部嵌入迁移，因此必须由部署owner在切换前显式执行迁移并核对checksum；启动不会自动迁移。本轮只读复核未执行任何生产迁移。

身份摘要是accountSecret密文JSON中可选的identity字段，沿用原加密密钥/KeyID和凭据密文列，不需要重新导入或批量重加密旧账号。缺少identity的旧密文仍能解码，管理页可从现有JWT即时提供未验证摘要；未持久采集时间为0，不伪造历史collected_at。保留原数据库加密密钥是读取旧凭据及新management文档的必要条件。

PG租期记录复用加密settings命名空间，启停账号与来源记录/审计在同一事务内提交；文件模式则在原accounts.json新增可选managed_account_leases，旧文件无字段可直接读取。隔离PG注入租期文档写失败已覆盖停用、恢复双向回滚；Identity PG（60.10秒）和资源恢复PG（51.46秒）已通过，见management-pg-identity-tests.log及management-pg-bundle-resources-tests.log。

回退旧程序时，旧二进制不会执行新的租期/定时/自动重置管理worker；旧代码若重新保存凭据或文件账号文档，可能丢弃其不认识的新identity/managed_account_leases字段。因此数据库完整备份应包含settings、account_credentials及相同加密密钥；不能用仅账号/资源bundle代替全部管理状态备份。现有账号bundle和旧migration CLI不迁移计划、租期、OAuth暂存及CRS关联等独立management状态，跨存储搬迁须单独处理这些管理文档；本次PG原地升级不涉及该路径。
