# B组0.4.15发布复核

2026-09-29 01:20（Asia/Shanghai）：主会话已部署0.4.15并完成现场验收，B独立交叉核对通过。

- release：`5d75f656986291a8648372032fa5c58f629719e95663a599fc79918f6bb3cce7`
- PID：1801914
- binary：`aacfb7aac3fc48522fee2648fb2bdcb0ae6a97c21bce532974b61986d204440d`
- 封包fullsource：`9a7b1d89c067f814a1fb5321745c11272d5f128b99f33bd287a2d921fe832857`

## 发布证据

最终修订runtime冻结bbb37299…与包内710项执行源码一致；封包fullsource的变化仅增加验收组合工具。Go/vet通过，4632含子PASS/460环境SKIP；PG15全部实际PASS，新增CompatibilityRoundTrip有独立原始记录。前端227单元、标准3browser和额外实际组件/全app证据由主会话及A/C核对。

首轮真实live的WScompact观测失败保留。修复受信websocket分发体的识别后，新完整9组live165500.712359单轮PASS，WScompact标志正确，摘要SSE/WS用例通过。该成功仅属于修订源码。

修订race165500.712722遇服务端包300秒总时限，原报告仍FAIL/exit1；没有将其改写为PASS。主会话同710源精确补170803.182221的39缺项+4 Redis，43项实际全通过；组合为272选中主测试中256真实PASS/16明确skip。协作20与Redis4完整、原中断hosted大JSON测试补过，无DATA RACE/OOM或源码漂移。B独立逐项与源实体检查，并实际调用正式组合验证器；39项逐个删项/改skip的纯反例均拒绝。

不可变包97制品/37证据/归档98文件全SHA与集合一致，原超时和补轮报告的SHA与包内副本相同。Windows本地只核内容，不替代主会话Linux preflight/activate。

## 现场验收

17:19:12 UTC回执与activate/manifest同PID/binary：两条带合法sequential_cutoff控制的SSE和JSON请求均200/completed，模型静态映射及max→xhigh、response到record的真实input/output/read/create一致。实际2generation request、0refresh；RequestType与stream准确，ToolPolicy整数0/false及无错误元数据保真。

非法摘要投递enum在本地4ms返回400，wire参数与record.ErrorParam均精确为stream_options.reasoning_summary_delivery，零派发、ToolPolicy未出现、mixed字段未出现。三条marked event的CSVv4/77列导出通过。

settings27、api_key cache、cooldown=false、全部账号配置和既有六迁移保持。B没有额外执行生成、导出、SQL或部署；只读已保存回执并绑定实际脚本1e460834…。

## 后续边界

短OK现场请求只证明参数准入与记录，不证明每个摘要done都实时显示；正式CLI与WS压缩依据各自真实门禁。原自然图片上传401/429、bound503及structured混用仍需后续观测，不能以本版smoke推断全站零错误或缓存改善。

B的bound观测已重基至最终0.4.15、作者20主项76含子PASS、独立9场景前后完全相同，仍为私有候选，未随本版部署。它可用于后续区分旧绑定不匹配的原因，不追溯补造旧六条日志的缺失字段。

证据根目录：`.local/cross-session-20260928/01a0e7ed/v0415-release-audit/`，重点为`deployment-audit.json`、`package-5d75f656-audit.json`、`race-timeout-recovery-final-audit.json`与`race-completion-review/completed.json`。

只读复跑（零请求）：

```powershell
python -B .local/cross-session-20260928/01a0e7ed/v0415-release-audit/check_deployment.py .local/reliability-release/5d75f656986291a8648372032fa5c58f629719e95663a599fc79918f6bb3cce7
```
