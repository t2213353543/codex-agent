# 不透明输出的账号归属

0.4.24 已于2026-09-29 08:21（北京时间）切入生产，08:22:16的后置验收通过。当前release `b3adc5ac…`、PID3588453、binary `0cdd4ed7…`，对成功生成的不透明输出保存归属索引，用于后续完整历史请求的账号选择。下文保留原失败、修订验证和升级覆盖边界。

原本会话绑定与成功历史分开保存：历史按 API key 和 response ID 读取，带有完整 Owner；会话绑定有独立有效期。客户端提供 `previous_response_id` 时，网关能读取旧 Owner。只带完整历史、不带 previous ID 时，绑定过期或推导会话所用的工具、instructions、prompt-cache key 变化，可能导致重新选账号。

作者已在真实 gateway 加本机合成上游复现五种错误路线：账号 A 成功返回的不透明内容原样保留、成功历史仍可读取，但下一请求被派到 B。合成上游只验证归属，不实现或破解真实提供商加密；这证明网关路由存在缺口，不足以认定所有自然 `invalid_encrypted_content` 都来自该缺口。有效绑定、稳定内容、明确会话下工具变化及过期后使用 previous ID 的四个对照仍留在 A。

已发布实现的约束如下：

| 边界 | 行为 |
| --- | --- |
| 写入来源 | 只登记已成功完成、通过协议及工具校验、完成授权和账号提交的实际输出 |
| 索引身份 | API key、推理模式与解码后的 JSON `encrypted_content` 字符串摘要；不解密、不作 base64 转换 |
| 有效类型 | `reasoning` 与 `compaction`，不把普通文本、工具参数或客户端任意输入当成新产物 |
| 读取时点 | 展开 previous history 后、账号选择前，只查询本次输入涉及的摘要，不扫描全部历史 |
| 命中归属 | 恢复完整 owner 约束，再走原账号、权限、并发和绑定检查，不直接授予账号使用权 |
| 冲突 | 多个已知 owner 不一致、与 previous owner 不一致或同摘要重复登记到另一 owner时拒绝；不能任取一个 |
| 保存一致性 | 先检查全批冲突和存储类型，再写成功历史及索引；冲突不应留下批次前半部分 |
| compact | 使用明确传入的实际 `result.Output` 建索引，因为保存历史时其内容会移入 `Request.input`、保存的 `Output` 为空 |

索引生命周期与当前 unbounded BPS 成功历史一致；bounded 路径不新增永久索引。key 和模式隔离不能由相同密文字节绕过。原有 opaque 禁止跨账号重放、owner身份变更拒绝、客户取消和已交付输出规则保持。仅 token 轮换且完整 owner 不变可以继续；换用户、删除重建或身份丢失不能借索引复活旧 owner。

作者本地候选的9个顶层、45个含子测试通过，原五种错误路线均改为仍使用 A，opaque 内容保持；另有 key/mode、JSON转义等价、批量末项冲突无部分写、输入不登记及 compact 对照。独立真实Redis存储验证4个顶层、5个子项全部通过，证明同一测试进程内新的Proxy与独立client可读取归属、存储类型错误和末项冲突不会留下部分写；它不等于跨OS进程或完整PG gateway验证。首轮冻结 `47fc5c83…` 的 Go 5024项及vet通过，但首轮live失败，未用于发布。

第一轮13组真实请求中，新增 `OpaqueOwnerRecovery` 的compact首步成功，续接第二步HTTP502，整体退出1。原失败摘要没有记录第二步实际账号、派发次数或错误阶段，因此不能从该502判断真实密文损坏、错发B或具体提供商拒绝。原失败证据保留，不被后续通过结果覆盖。

随后仅修订测试的序列化比较基准并增加安全诊断，运行代码未变。新的单项真实验证完成1次compact与1次续接，两步HTTP200、无替代账号调用、标记恢复及密文保持；这不倒推旧502的确切原因。最终测试又新增上游compact输出到公开输出的count/type/id/cipher断言，冻结为 `05135160…`，完整Go 5025项通过、vet通过且无漂移。最终整组结果来自这份源码的独立运行，没有用前一单项结果替代。

最终 `001000` 运行的13组真实请求已全部通过，新增opaque用例本轮明确通过首跳密文比对、保持原账号的续接和标记恢复；不是沿用中间7a0单项结果。最终23项PG和完整参数race也通过。实际registry/front、已验证发布包和后置验收身份一致：两次新实例生成成功、top_p本地负例400且无上游派发、三次CSV对账通过，计费开关true、设置版本27与账号配置保持。94次health在同一连接跨两个generation均成功，旧PID在观察期间不变；本轮没有追加模型SSE/WS切流探针，不将前次 .23 SSE实证移称本次执行。

升级覆盖有明确边界。旧 .23 成功历史不会自动获得新摘要索引；实现不扫描旧历史，也不把客户端传入的旧内容登记为当前账号产物。旧 previous ID 若仍可读取，原 Owner 约束继续有效；旧完整历史若没有可读 previous ID 或有效绑定，且索引未命中，仍按现有未知状态规则处理。失败或取消前已公开的部分输出也不能因为“客户见过”就称已建立成功历史归属。因此不能宣称全部历史请求或已删除账号的 opaque 续接都可恢复。

旧历史回填已有固定范围的生产只读试跑：日志ID `(79750,79950]` 共200行，发现95条候选历史和118次缺失proof观察；观察次数不是去重索引数。随后仅补索引的工具通过隔离PG/Redis验证，保持来源历史、绑定及PG记录不变，检查来源字节、归属冲突、存储类型、幂等性和现有索引TTL。

首次生产补写重新盘点时，候选因当前owner记录变化降为41条、43次缺失观察，清单摘要不再匹配原核定值。工具在第二遍写入开始前停止，确认索引写入0、不确定写入0。原失败回执保留。

主会话随后按同一200行范围的新清单 `fd7c56f3…` 建立独立操作，于08:46:49（北京时间）执行：41条核定历史全部完成，新增43条去重索引、确认写入43条、不确定写入0；应用PID与binary保持。之后只读回查的候选清单摘要相同，43条索引均匹配原owner，缺失0、冲突0。摘要覆盖符合条件历史的密文、归属及proof，不代表所有跳过记录完全未变。原95条只读盘点、首次写前拒绝和这次实际补齐必须分开记录。这只完成固定批次的索引维护，没有重放客户请求验证续接，也不涵盖全部旧历史、已删除owner或compact-only历史。

有限账号清单也不能替代逐请求归属证明：已观察的10037个thread key中，2100仅匹配当前启用的管理ID，7937未在当次账号表找到；扫描未完成且非原子，没有核完整 owner 的用户、工作区、revision及当前key授权。这不是全集比例，也不能直接归因为删除或替换操作。

证据入口：

- [原路由复现核对](../.local/collab-a-20260928/post-auth-remaining-review/opaque-original-reproduction-review.json)
- [历史索引合同](../.local/collab-a-20260928/post-auth-remaining-review/opaque-history-index-contract.md)
- [候选源码审查](../.local/collab-a-20260928/post-auth-remaining-review/opaque-candidate-source-review.md)
- [本地候选日志核对](../.local/collab-a-20260928/post-auth-remaining-review/opaque-candidate-local-review/review.json)
- [真实Redis存储证据](../.local/collab-a-20260928/post-auth-remaining-review/opaque-redis-evidence-review.md)
- [升级覆盖范围](../.local/collab-a-20260928/post-auth-remaining-review/opaque-upgrade-coverage.md)
- [实际生产身份与后置验收](../.local/collab-a-20260928/opaque-release-review/actual-deployment-b3adc5ac/review.json)
- [有限账号清单口径](../.local/collab-a-20260928/post-auth-remaining-review/inventory-review.md)
- [固定批次实际补齐与回查](../.local/collab-a-20260928/post-auth-remaining-review/backfill-production-apply-final/REPORT.md)
