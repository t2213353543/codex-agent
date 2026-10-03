# 一次正常凭据刷新前后的原题对照

2026-09-25。本审计只读网关、固定官方 CLI 0.155.0 源码以及既有脱敏实验记录，没有刷新真实账号，没有调用模型，没有读取或输出真实 AT/RT。本文给出可执行入口和证据合同；是否执行、当前账号是否具备 RT 及最终账号选择由主任务核验。

## 1. 本轮已有证据决定下一步

最新 v7 已完成 8 个模型 POST：new2 的 direct 两次、隔离生产代码网关两次均答 21；g7 的 direct 两次、网关两次均答 29。逐格出站非认证 header 相同，input 和原有 metadata 语义保持；网关额外加了当前配置的 232-byte builtin prompt、映射 cache key，且入口/上游压缩条件有已记录差异。这说明应针对**同一个 g7 的凭据状态前后**继续缩小范围，不能把不同用户 JWT 正常差异当作错误请求证据。[v7 独立汇总](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/gateway-direct-controls-v7/review/summary.json)

v6 三组 cookie/GET 实验因 epoch 观测改变停止，仅执行 7 个 POST，其中 5 个完整答案人工为 29、2 个 429 无答案。不能写成 12 格全部完成或得出 cookie 没影响的结论。[v6 独立汇总](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cookie-controls-v6/review/summary.json)

此前 `workspace-metadata/report.json` 的四账号材料在 `2026-09-24T19:59:00Z` 已观察 client_id、scp、aud、iss、amr、plan 相同；账号/user/session/jti/时间不同。此前共有缺少 connector scopes 的来源差异不能解释当时 Q0=21、q5/q6=29。那批 q 别名不是新 g 别名，不能跨批混用。[来源核对](C:/Users/Administrator/Desktop/项目/codex-agent/docs/quality-20260925/workspace-source.md)

既有 28765 bridge 仅向 Agent 转发 AT，源服务保留 RT；历史自动恢复是源重新授权后推送新 AT，并非本文拟执行的一次网关普通 RT 刷新。v7 inventory 没保留 `has_refresh_token`，初审因此没有假设 g7 在网关中有 RT。[bridge 约定](C:/Users/Administrator/Desktop/项目/codex-agent/docs/LOOPHUB-CODEX-BRIDGE.md:45)

后续 root 已只读补齐当前必要条件，本审计读取 [v8 credential-inventory.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/credential-controls-v8/credential-inventory.json) 核验：`2026-09-25T08:20:59Z`，g7 enabled=true、Version=1、has_refresh_token=true、in_flight=0、proxy_binding_count=0；new2 enabled=true、Version=2、has_refresh_token=false、in_flight=0。两账号 user/workspace/identity/token 四个哈希均与 v7 对应登记相同。本次只读零模型、零refresh。当前 g7 具备普通管理刷新入口的必要条件；并不由此推断其 RT 有效，或 new2 与 g7 是同一注册来源。

## 2. 选择带版本前置条件的正常管理入口

推荐使用一个元素的现有批量管理动作：

```http
POST /api/accounts/actions
Content-Type: application/json
Authorization: Bearer <existing-private-admin-key>

{"action":"refresh","items":[{"id":"<current-g7-management-id>","version":123}]}
```

123 仅是示例；必须使用刚刚读取的当前管理 `Version`，不能复用本文或旧实验的数字。该请求不传 RT、AT、scope 或 client_id；服务器从权威存储读取当前凭据。这是现有正常 refresh，不是创建 OAuth session、重新登录或重新导入。

成功响应示例：

```json
{"results":[{"id":"<same-id>","ok":true,"version":124}],"succeeded":1,"failed":0}
```

HTTP 200 是批量结果容器，**不是逐项 refresh 成功**。必须核对唯一 `results[0].id`、`ok=true`、`succeeded=1`、`failed=0`，随后重新从存储读取并比较凭据 epoch。失败时 item 中有固定 code，例如 version_conflict、invalid_request、operation_failed。不能因 HTTP 200 就开始刷新后的模型请求。

源码路由为 [admin.go:1443](C:/Users/Administrator/Desktop/项目/codex-agent/admin.go:1443)，实现为 [admin_accounts_management.go:470](C:/Users/Administrator/Desktop/项目/codex-agent/admin_accounts_management.go:470)：

1. 每项必须唯一非空 id、positive version，refresh 最多 10 项；本实验只提交一项。
2. `GetMetadata()` 核对 ID 和版本；`Get()` 加载真实凭据后再核对版本及 RT 非空。
3. `Store.Refresh()` 锁/权威读取后，callback 再核对版本，只有正确版本才执行 OAuth。
4. 并发若已更换 AT，Store 会返回已提交的新快照而不执行 callback；该管理动作检查 `called=false` 并返回 version_conflict，避免误称本次触发了一次 refresh。

旧 `POST /api/accounts/refresh` 接受 `{id}` 或 `{name}`，只有旧 AT 对照、没有调用者显式 version 前置条件，因此不建议用于此次需要准确归因的受控干预。[admin.go:582](C:/Users/Administrator/Desktop/项目/codex-agent/admin.go:582)

管理 refresh 本身不以账号 enabled 状态作为完整前置 gate，也不证明没有在途请求；root 要在调用前固定 g7 enabled、inflight、epoch 和 source 推送状态。禁用/换号不是这次计划的替代动作。

## 3. 请求与官方 CLI 对齐到什么程度

| 边界 | 网关当前普通 refresh | 固定 CLI 0.155.0 |
|---|---|---|
| URL | 正常启动 `https://auth.openai.com/oauth/token` | 同官方 token endpoint；环境可显式覆盖 |
| Method/body | POST application/json，仅 client_id/grant_type/refresh_token | `RefreshRequest` 相同三个字段 |
| grant_type | refresh_token | refresh_token |
| scope | 不发送 override | 正常 refresh 不重发 scope |
| client_id | 优先账号持久化 ClientID；空时使用官方 Codex app ID | 使用官方 app ID，或明确配置的 client override |
| 默认应用身份 | 已配置的真实 runtime UA/originator；不加模型 provider Version | default auth client UA/originator；没有模型 provider Version |
| 模型专属 headers | 不带 Authorization bearer、ChatGPT-Account-Id、session、routing/Lite 或模型 Version | token grant 本身不使用模型 provider header |
| 请求压缩 | 明确脱离模型 zstd request flag | JSON auth client；不按 Responses 的 request compression 路径 |
| 出口/代理 | 账号的已选路由与指纹设置 | AuthRouteConfig 的 route-aware 策略 |
| response tokens | 独立处理可选 AT、RT、ID token；新凭据先保存 | `RefreshResponse` 三项均为 Option |

源位置：[proxy.go:453](C:/Users/Administrator/Desktop/项目/codex-agent/proxy.go:453)、[gateway_oauth_headers.go:12](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_oauth_headers.go:12)、[官方 manager.rs:1598](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/login/src/auth/manager.rs:1598)、[manager.rs:1710](C:/Users/Administrator/Desktop/项目/codex-agent/.local/codex-source-0.155.0/codex-rs/login/src/auth/manager.rs:1710)。

`client_id` 是令牌发行应用，不应为了让两个账号字段相同而改成另一个应用。账号 ClientID、AT 中 client_id、azp 应分别记录来源和一致性；它们不总是同一个字段，不能拿未知 azp 覆盖正确的持久化 ClientID。

普通 refresh 不等于授权码 exchange：后者的官方 raw auth client 没有同样默认 UA/originator。也不等于 `chatgptAuthTokens` 的外部宿主管理更新；外部模式由宿主回传新的 AT，不掌握 RT。本文选择普通管理 refresh 才会实际使用存储里的 RT。

官方 HTTP 的重定向、失败分类/错误正文、未知 expiry 与网关重试冷却行为仍不完全相同。网关近期修复了应用 header 与身份连续性，不能把这张表扩展为完整 OAuth/TLS 行为逐字节一致。

## 4. 成功旋转后必须从持久存储取新 epoch

普通管理入口经过 `p.refreshAccountWithFingerprint()` → `refreshAccountWithCooldown()` → `refreshAccountWithSettings()`，在 OAuth 开始前尊重取消；开始后有独立 45 秒 `WithoutCancel` 预算。文件 Store 持锁完成 exchange 和原子写入；PG 使用每账号本地/advisory lock，网络阶段不持数据库事务，开始后使用独立总预算落库，然后读回权威记录。

不能因为管理 HTTP 连接中断、超时或响应丢失就再次发送同一次 refresh。RT 可能已经轮换；应先等待已确认执行的操作完成，再只读权威记录/audit 判断是否已提交。当前 API 没有 request_id 幂等键，重复点击是另一轮真实 refresh，不是安全重放。

`mergeAccountTokens()` 保留账号 ID、Name、Enabled、Groups、Proxies、并发/权重/预算等管理属性；缺失 RT/client_id/account_id 回填当前值。新 AT/RT 持久化会提升 credential version 与 metadata version，清除旧 credential error。版本准确变化以实际读回为准，不硬编码一定等于 old+1，因为管理并发可产生额外更新。

已部署的 [gateway_refresh_identity.go](C:/Users/Administrator/Desktop/项目/codex-agent/gateway_refresh_identity.go:16) 重新从新 AT/ID token 构造未验签的身份元数据，不让陈旧 `Identity.UserID` 掩盖变化：

- 已知同 user 和显式选定 workspace：保持 owner revision，可更新 plan/email，不继承旧 verified 标记。
- 不同 user、opaque 新 AT 没新身份证据、AT/ID 矛盾：仍保存已旋转凭据，但换 owner revision/降 unknown；旧 HTTP/WS 续接失败。
- 只有 RT 旋转，或相同 AT 且无新 ID：保持原身份。
- 有效 ID-only：按可选字段更新；仅无效 ID 且无 AT/RT 时返回固定错误，原值不变。

这些规则已有文件/PG和真实 loopback HTTP/WS回归，不是通过本轮真实账号试探才决定行为。[已上线身份修复记录](C:/Users/Administrator/Desktop/项目/codex-agent/docs/CODEX-GATEWAY-IDENTITY-REFRESH-2026-09-25.md)

失败冷却可能在新管理动作内部阻止实际 OAuth 网络发送；因此 `version_conflict`、缓存失败和一次真实 grant 失败必须区分。成功项和存储 token hash 改变是最低证明，但若需要声称“本轮恰一次 OAuth POST”，还要对应实际 refresh attempt/audit/传输证据，不能仅凭管理请求次数推算。

## 5. claim 记录合同：同用户前后，不从字段猜风险标记

JWT 在本任务中只解码 payload 做一致性对照，没有验证签名、issuer key、audience 或 token 有效性。报告统一标 `claims_source=unverified_local_jwt`。AT、RT、真实 ID token 的完整字节均不落到公开结果；分别保存 SHA-256、字段存在性/类型/长度及采集时刻。

| 字段 | 应保留的对照 | 解释与隐私边界 |
|---|---|---|
| JWT header alg/typ/kid | alg/typ仅固定允许值可明文；kid仅 hash；未知键名/值hash | 签名参数或key轮换，不是性能档位；不输出JWT三个段。 |
| iss | 值类型/规范化hash；精确官方issuer可显示 allowlisted 标记 | 刷新前后应同授权服务；不同issuer需调查来源，不能直接称降智。 |
| aud | 保留原数组/字符串类型和整体hash，另做字符串集合hash | 数组顺序变化不能误报权限变化；仅固定官方aud可标记。 |
| client_id、azp | 各自 presence/type/hash，与存储ClientID和官方常量的比较布尔 | 不同应用可能合法；不能混同两字段或改写以模拟另一client。 |
| scope、scp | 两者分别存在性、原值hash；严格空格分词/字符串数组后再给规范化去重集合hash和数量 | 必须区分 missing/null/空集合；只列固定已知 scope 名称，未知 scope只hash。不能凭token scope判定隐藏compute。 |
| iat、nbf、exp | 有效整数可保存时间戳与TTL/delta | 刷新后通常变化，但不保证每次都发新AT；检查秒/毫秒单位，未知值不擅自换算。 |
| auth_time、pwd_auth_time | presence/type/hash，已知数字可保存受控时间差和单位标记 | 可能代表最初认证而不是refresh；不要求每次与iat同时变化。 |
| amr、acr | 顶层与authnamespace分别记录；原集合hash及已知认证方法分类 | MFA/password来源事实，不是被标记证明；未知字符串仅hash。 |
| jti、session_id、sid、nonce | 仅 hash/存在性/长度 | token/session唯一性正常变化，绝不明文输出或当成风险字段。 |
| sub | 仅hash，同类AT前后可比较 | AT与ID token的sub语义可能不同，不拿跨token sub不等就断言异用户。 |
| auth.chatgpt_user_id / auth.user_id | 两字段分别hash，再按官方优先级给resolved_user_sha256 | 同一用户对照必须一致；fallback不漏校验；不同用户原本当然不同，不能解释质量。 |
| auth.chatgpt_account_id | 仅hash，与显式AccountID单独比较 | 显式选择workspace是独立字段；不要把默认claim不同直接判越权/降智。 |
| auth.chatgpt_account_user_id、poid、org/project相关ID | 仅hash，不和user_id混用 | membership、workspace user、organization ID可能各有语义。 |
| auth.chatgpt_plan_type | 固定已知plan可明文，其余hash；不从plan补席位/配额 | 相同plan不证明同权益，计划变化也不直接证明quality恢复。 |
| auth.chatgpt_compute_residency、chatgpt_account_is_fedramp | 原字段存在性/type/hash，已知bool可显示 | 路由/地区声明，不是高低算力标记；不能自行加header绕过。 |
| profile.email/name、email、关联组织列表 | 仅hash/类型/数量 | 不公开账户个人信息；email一致不替代stable user校验。 |
| 未知oauthapp/delegation/device字段 | 键名和值都hash，保留树结构计数、presence/type | 只有实际出现才记录；不补造名为“risk/降智”的解释字段。 |

为了避免“相同JSON数字先转float”改变摘要，原 payload 比较需保留精确数字词法并拒绝重复 key/非有限数。scope/aud集合hash属于额外语义投影，不覆盖原始结构摘要；缺字段不能用默认空值抹平。

## 6. 最小可审阅执行顺序

root 已在看到 v8 模型结果之前选定具体设计：前四格 `[new2, g7, g7, new2]`，只刷新 g7 一次并验证成功持久化及用户/workspace连续，后四格仍为 `[new2, g7, g7, new2]`。预算 **8 POST + 1 次管理 refresh**，固定同一份完整 raw CLI body；new2 是不刷新对照，AT/RT、scope、client、device 不做人工改写。管理 refresh 只有在当前 CAS 和必要条件继续成立时执行。

g7 的 stored ClientID 如为空，应在凭据收据中记录 `client_id_source=official_default`、实际有效client的hash与官方常量匹配布尔；如显式非空，则记录 `client_id_source=stored` 和其hash，并与AT client_id/azp分别比较，不输出未知rawclient字符串。RT前后只保存指纹及是否存在，不保留令牌字节；OwnerRevision也只给hash/是否一致。成功OAuth响应不保证每次必定轮换RT；“已保存返回RT”和“RT hash发生变化”是两个不同事实，应按实际分别记录。

1. 只读当前 g7 的管理 ID、enabled、has_refresh_token、Version/CredentialVersion/OwnerRevision摘要、代理关联、stored ClientID摘要、当前 AT/RT hash、resolved user/workspace；同当前权限/额度状态绑定。若不持有 RT，记录这一事实，不把 AT-only导入改成RT导入。
2. 冻结原题、完整请求body和非认证头、模型与medium、出口、fresh-helper模式、helper/网关SHA。body仍原题，判据仍21；不把期望答案写进prompt。
3. 本轮使用上面明确登记的四个新 before 调用以获得同期 new2 对照，不将 v7 历史两次29冒称为本轮新 before。旧body若已删，重新获取完整CLI capture并明确新的session/cache/context输入，不能以哈希恢复正文；该同一新body随后固定用于全部八格。
4. 只发一次带当前version的普通 refresh action，保存safe请求/响应、开始结束时间、实际OAuth次数证据。等待完成后从权威存储读取新AT/RT hash与版本，核resolved user/workspace/client来源；同用户是继续比较的前提。
5. 若身份变化、owner unknown、失败、401/429、source又并发推送或额度耗尽，停止推理对照并保留原结果；不要补发直到答21。
6. 若新epoch有效且身份连续，在同样freshhelper/固定request条件执行预登记的四个 after 调用，new2/g7各两次，与before重复数一致。每次前后仍核当前epoch，usage不等于特定模型授权证明。记录实际response model/effort/tier/token usage/id/hash和原最终答案。
7. 对照分别呈现“凭据grant结果”“持久化结果”“真实usage/模型结果”。管理刷新成功不能写成模型质量恢复；29→21也只能先说明这一时间序列关联，不能排除随机性、时间/后端状态变化或认定某个claim是根因。

执行root应保持源推送和生产在途状态的事实记录。本文没有要求新增审批，也没有替root擅自修改源任务或停服务；已授权范围内由root根据当前实况完成具体步骤。
