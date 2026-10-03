# B 组：0.4.12 路由独立复核

本组只修改私有测试快照、候选补丁与报告，运行文件由主任务routing owner整合。线上已核对0.4.11，不将0.4.12未发布候选称为线上行为。

## 最新用户方向覆盖动态计划

用户随后要求先查明可用模型、硬编码并提供设置，避免每次请求尝试下一模型。主任务已接管静态单次路由，本组停止推动每请求catalog和singleflight取消候选。以下动态路径结果作为研究/历史证据保留，**不是静态方案的验收，也不是当前0.4.12必须合入的补丁**。下一步只核对新settings与请求期零catalog GET、选定model一次派发的实际边界。

## 已闭合的独立发现

| 边界 | 修复前实际证据 | 当前验证 |
| --- | --- | --- |
| auto/review仅有未来模型 | fallback成功但漏传strongestDefault，effort变空 | owner联合model+effort修复后，原断言5顶层/12子例PASS；378可用模型子集、2040effort子集均通过 |
| catalog失败虚记生成 | exact/unknown只有目录GET503、POST0，却产生一次request /responses attempt | owner四行受限guard合入；8顶层/23含子例PASS，HTTP/body逐字相同；先前真实403、refresh观察和truncated标记不丢 |
| 凭据版本更新 | 有效owner不变但credential epoch增加 | 重新GET，使用新token，不复用旧epoch缓存 |
| 同owner续聊、实际模型改变 | 目录TTL到期，旧requested alias仍在公开历史 | previous_response_id可续接；requested保持、actual model正确变化、shared cache hint按实际model隔离 |

模型“向上/向下”是项目明确偏好顺序，不是测试证明的能力排名。精确可用模型保留；限制模型跳过；未来model若无effort声明，不捏造推理档位。缺省returned model保持未知，真实不同returned不被改写。

## singleflight取消修复候选已交接

原路径由首个请求的context发目录GET；leader取消后，仍健康的follower继承context canceled并失败。新canonical请求也需目录，使此既有弱点扩大到所有冷缓存请求。

私有最小补丁 `v0412-singleflight-cancel-candidate/proposed.patch` 仅修改目录load边界，健康等待者只在共享取消/超时且自身context有效时重新竞争一次。原leader按原取消返回；不创建脱离请求的后台GET，不对403/429/503等真实提供商错误重试，不修改模型选择顺序。

三轮72PASS、0FAIL/SKIP：leader取消/超时＋16个健康等待者总计仅2GET；403/429/503共享失败均1GET；连续第二取消后不进行第三次加载；已取消等待者不加入新flight。补丁SHA `042f91f7107f004f37d239467132589c4ac8c468b4bde44f8bd06c3309aa374b`，须由owner按最小hunk整合，不能用旧整文件覆盖已完成的联合effort修复。最终合入状态待另行复验。

## 实测性能与范围

- canonical/缺席模型各100请求仅1次catalog GET；32并发冷请求共用1GET。
- 热缓存单scope基准约1.6–2.0微秒/次；1024scope约7.4–7.6微秒；16384scope约103–105微秒。测试机器Windows/Ryzen7 9700X，1000次×3轮，约23次分配/调用。
- 增长与当前每次持锁扫描过期entry相关，是本地路由函数成本，不含网络或生成。没有生产并发/端到端延迟数据，不声称用户首字相同提升。
- 当前TTL一分钟，未证明目录永远最新；失败不缓存，key/owner/credential/origin/proxy隔离保持。

## 交付索引

以下相对目录均位于 `.local/cross-session-20260928/01a0e7ed/`：

- `v0412-routing-order-review/REPORT.md`、`owner-recheck-result.json`：全组合顺序与来源。
- `v0412-routing-auth-review/REPORT.md`：授权、凭据、续聊边界。
- `v0412-catalog-attempt-candidate/REPORT.md`：已被owner整合的观测最小修复对照。
- `v0412-catalog-performance-review/`：counter与benchmark原证据。
- `v0412-singleflight-cancel-candidate/`：尚待整合的取消边界最小补丁与测试。

最新新增 `TestModelFallbackCandidate*` 不在原race族，已逐名交主owner最终改成TestBPS族或补exact选择；不能把manifest含文件当作实际race执行。真实catalog/模型生成与最终release门禁由主任务统一执行，本组没有重复模型调用。
