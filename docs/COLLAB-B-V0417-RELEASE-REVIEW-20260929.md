# B组0.4.17发布复核

2026-09-29 02:26 +08：主会话已部署0.4.17，18:24:32 UTC现场验收及B独立回执核对通过。

- release：`49c77636fd630acd9e2266bc58bf572953af2f6ed8273184d8249e41071b6590`
- PID：2058745
- binary：`4cf31b8c027ab7e226bf94a69577586ab4432aa615419033b127bdece21f9ad3`
- 最终fullsource：`0636f98e359a4a3d27c8737bed61cab8fb37aee98175417f85b208aef560c384`

## 交付的行为

内部会话绑定提交错误使用typed原因，归为gateway/session_binding，清掉没有实际交付的首字时间与输出类型；真正客户端写入失败保持client/response_write。原错误内容及Unwrap保持，拒绝输出屏障不放松。

并发请求已提交同一个精确replacement时，本请求续租可继续；不同winner、存储失败、取消及原授权/重放守卫保持。此修复有21秒实际lease反例，**不能用来解释旧六条不足4秒的请求**；两种问题证据分开。

已收到有效原生compact响应、随后本地OnComplete失败时保留真实usage/model，但仍error、没有公开输出或成功历史。普通成功计量位置不变，非typed撤权失败也不被误改成功。

附件fileID缓存保绝对TTL与目标即时过期判断，将全表清理降为每分钟机会执行。普通命中成本显著降低；触发清理的访问仍在锁内O(N)，没有容量上限、后台worker或命中续期，不以本地微基准预测模型TTFT。

## 正式验收与历史分界

修订Go/vet通过：4727含子PASS/462环境SKIP/0FAIL；本版12新top/25含子全部通过，lease实际21.02秒。PG180823.053397的17项全PASS，包括提交失败HTTP200但逻辑error、TTFT与usage未知及成功统计/CSV保真的SessionCommitRoundTrip。

最终race181047.133509单轮PASS：新12、协作20、Redis4、official95逐项唯一通过，无DATA RACE/OOM/源码漂移；server298selected全部终态，308.8秒/600秒预算。723项执行源与PG/最终freeze/current一致。

C逐项核最终live181223.895919的9top+20必需子项全部PASS，summary SSE/WS这次均有及时done与可见reasoning；测试源没有因首轮失败放宽。首轮旧source5fcd的无摘要SSE失败及旧race source_changed记录均保留，未用于证明新源。

前端235单元/3browser和monitor25server证据以逐SHA相同方式复用。不可变包88制品/27证据/89归档文件全SHA与集合匹配，包内validate通过。Windows本机只验证内容，主会话Linux preflight/activate另有实际回执。

## 现场回执

manifest、activate及postdeploy同version/PID/binary。SSE gpt-5.5→gpt-5.6-sol、JSON codex-auto→gpt-6-astra均completed，max→xhigh，schema2、request_type/stream、ToolPolicy五字段以及response到record计量一致；两次实际request、没有refresh。

top_p=1本地4ms400，wire/record参数精确top_p、零派发。三条普通请求bound/mixed省略，3exact-event CSVv4导出通过。settings27、api_key缓存域、cooldown=false、账号配置和六项迁移保持。

B仅读已保存执行回执，未重复生成或导出。现场没有故意制造绑定冲突、压缩提交失败或TTL过期；异常分支依据正式synthetic/race/PG验收，不把普通smoke称作全部自然错误解决。

证据根目录：`.local/cross-session-20260928/01a0e7ed/v0417-release-audit/`。重点：`deployment-audit.json`、`package-49c77636-audit.json`与`gate-coverage/revised-gates/completed-{pg-go,race}.json`。

只读复跑：

```powershell
python -B .local/cross-session-20260928/01a0e7ed/v0417-release-audit/check_deployment.py .local/reliability-release/49c77636fd630acd9e2266bc58bf572953af2f6ed8273184d8249e41071b6590
```
