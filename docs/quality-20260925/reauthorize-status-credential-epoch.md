# 重新授权后旧 401 仍显示异常：凭据版本修复

2026-09-25。已复现并修正当前状态串用旧凭据请求结果的缺陷。现场 new1 的源、bridge、网关 AT 一致，新凭据已落库，最新非推理 usage 查询200；网关列表仍为`status=error`。页面实际标签为“请求或凭据异常”，`credential_status=unverified`显示“尚未验证”，并没有字面“凭据失效”。[现场摘要](../../.local/research-20260925/reauth-status-fix/live-evidence-summary.json)

原状态链以稳定account ID查历史最近请求，但没有记录该请求使用的凭据版本。真实reauthorize已经正确递增PG credential_version、清LastError、重置CredentialStatus/LastCheckedAt，旧management usage401也正确标记stale；历史业务401仍被账号总状态读取，因而新凭据继续显示异常。单纯删除旧记录不足以修复：正在执行的旧凭据请求可能随后完成，再次污染状态。

合成红测通过真实`POST /api/accounts/{id}/reauthorize`替换AT，随后核对：新credential_status为unverified、LastError为空、LastCheckedAt=0、旧usage snapshot stale=true，但account detail status仍error。[原始红证据](../../.local/research-20260925/reauthorize-status-review/status-red.log)

修复采用独立`CredentialVersion`合同：

- RequestRecord新增`account_credential_version`，由实际HTTP发送/401重试以及WS握手凭据打标，不在请求结束时从当前账号或名字推测；该发送边界由主任务实现和测试。
- PG metadata-only读取现有credential_version，不解密凭据。文件store新增独立`credential_versions`映射，首次迁移从1开始；实际AT/RT/client/workspace/identity改变或显式credential replacement推进，改名/组/notes等元数据不推进。
- 文件状态扫描先按当前正数credential version筛选，再取最新记录。历史无epoch记录只保留审计，不假装已验证当前凭据。
- 新migration004创建`account_credential_request_state`，每account一行、外键删除级联。只接受权威当前正数epoch的请求；UPSERT只允许更高epoch，或同epoch且(time,id)更新，防旧快照等待row lock后回退。
- 旧`account_request_state`、request logs和usage历史保持原写入规则，继续提供LastFor/统计审计；当前状态读取新表。详情、筛选、排序使用相同current epoch规则。
- 文件management snapshot/usage cache/额度操作也统一读取真实CredentialVersion，不再拿metadata Version代替。凭据替换重置旧维护状态；普通元数据更新保持有效观察。

迁移只新增结构，不删除或改写历史数据，不回填缺少凭据版本的旧请求。旧二进制可忽略新表；迁移后部分没有新epoch请求的账号显示未验证，是没有当前凭据证据的正确状态，不自动标为正常。

新增合成测试覆盖：新凭据先unverified；新凭据200后旧epoch迟到401不覆盖当前ok；改名保持epoch与状态；筛选/排序与详情一致；历史LastFor仍呈现真实旧请求；删除账号只清current状态；文件epoch重启持久、legacy迁移、实际credential字段推进和metadata不推进。Windows管理相关84个顶层、含子项196项全部通过，`go vet ./...`通过。随后补齐file兼容列表`/api/accounts?status=ok&limit=1`的两页实际验证；它与detail/manage共用当前epoch状态，并在两次读取间发生credential变化时不复用旧状态。追加7个顶层、含子项20项通过，涵盖两处旧snapshot测试从metadata Version纠正为真实CredentialVersion，未放宽门禁。PG包装测试已交主任务在隔离候选完整执行，不把本机跳过当通过。

[候选manifest及精确文件摘要](../../.local/research-20260925/reauthorize-status-review/candidate-manifest.json)；[仅本子任务相对a477源码差异](../../.local/research-20260925/reauthorize-status-review/credential-status.patch)。正式候选由主任务在a477基础加永久grant修复、当前epoch修复、实际dispatch打标和bridge修复组合验证；共享目录中其他缓存策略修改未纳入本交付。

本修复针对状态归属。bridge另发现的目的端实际token核验缺口单独修复，但不将其当作本次现场“源没有推送”的证据；现场已确证推送成功。本文不把状态显示修复解释为模型推理质量变化，也不宣称尚未完成的部署已经上线。
