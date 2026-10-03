# B组0.4.20发布复核

2026-09-29 04:13 +08：0.4.20已部署，20:12:25 UTC现场验收及B独立核对通过。

fullsource：`5b760dc5fa226616c8bc2c5a2678b30539b807552c1c68f0a7ddef0f0dc652ef`，1314完整源索引/735执行源。

release：`6207cd8871eb5bbacd9857aa6e53709c3a261a8460ff6d3e136329694d5536f9`；线上PID2479613；binary：`f71fc1a2f90d857b705bb20b47e416ebd76ad50bb39e65a18a353109e5e5c4ee`。

## 最终修改与原证据

Prepare继续把显式additional_tools及tool_search_output纳入有效工具目录，但只用显式additional_tools判断是否已经有客户声明。晚到的重复发现结果不再撤掉最前面的当前工具声明；客户自带声明ID、顺序、工具定义/冲突校验、required/forced限制和原历史保持。

B两轮真实本机gateway用例证明正常previous_response_id续接可触发原问题，并验证候选前缀从115字节恢复53753字节。原Output discovery保存/下轮拼接、原tools继承及客户声明保持；原测试缺server output.status的旧证据保留，补齐status/显式null call_id的当前版本已采用为TestBPSDiscoveryPrefixFromStoredServerResult。采用版本仅改test名、失败文案和gofmt，原candidate运行逻辑与main8ea9a509…一致。

真实模型动机实验先两次各1请求在基线失败，独立诊断明确client search声明缺parameters；纠正实验后四请求通过，candidate第二轮read82284/create209/input82807=99.3684109%。旧失败不覆盖，实际累计6由root执行的模型请求，不称预算4就是累计4。

该四请求实验编译.19，仅在transport插回首轮声明；两arm有独立缓存key/marker及不同真实输出，不代本版源码门禁。正式新增TestBPSLiveDiscoveryCachePrefix已在本版源通过：直接使用candidate Prepare、GetBody只读、生产api_key域/完整owner，两次真实调用；首turn input82595/read0/create82280，第二turn input82849/read82280/create255=99.3132084%。原prefix/真实clientcall与本地registry配对/答案/上游响应和记录计量全部相符，实际无请求重写。此数值不借旧AB读数、仍为受控单会话场景，不是自然全量收益或跨租户隔离实测。

## 当前正式证据

完整Go/vet原终态通过：4809含子PASS/470环境SKIP/0FAIL，1626topPASS；新四核心和一个真实history回归均实际PASS。B独立核history的DISCOVERY_PREFIX_EVIDENCE与完整server形状的私有候选观察逐字段相同。

PG200048.729925原19top/32含子全部唯一RUN/PASS、无skip/fail；执行源735与race/current/freeze一致。沿既有数据库合同复跑，不假称本版新增数据库字段或专门discovery PG schema。

race200048.757771原报告PASS/exit0/no race warning/no source drift；B逐raw确认1185唯一RUN/终态=1167含子PASS/18合法SKIP，438topPASS/18SKIP，5required/20协作/4Redis/95official全部逐项PASS。18skip相较.19仅多新DiscoveryCachePrefix的live环境条件，实际provider验证单列。新正常history原证据与Go一致，735冻结/归档/current相同。live200048.756989原exit0、11top实际全PASS，testbinary996e3e75…与735源绑定。包内新discovery两row按本版fixture SHA f5c9eb30…逐项验证，不以静态选择代实际执行。C另持十一组live逐项子用例审计。

前端237单测/3浏览器/55制品和monitor30原同SHA证据来自.19，没有无意义重建。A已核它们与新冻结源一致。本版包核对将保留新source要求，不能用原AB旧源代新live，也不沿用.19“前端重新构建”的描述。

## 验收边界

包内原validate、89制品/28证据/90归档名称与SHA集合已通过；734 backend scope加既有embedded JSON为735执行源，包内PG/race/live映射精确一致，full/current/freeze无漂移。前端与monitor为明确同SHA复用，不套用.19重新构建描述。

manifest/activate/postdeploy同version/PID/binary。SSE5333ms和JSON3505ms均completed/双HTTP200、实际2request/0refresh；固定请求/发送/返回模型与max→xhigh、输入输出/缓存已知0与创建量、transport与ToolPolicy合同正确。top_p=1本地4ms400精确param且零派发；普通bound/mixed字段省略，三个精确CSVv4导出通过。settings27、api_key缓存、冷却false、全部账号配置和六迁移保持。

B只读取保存回执，0额外生成/SQL。本轮前端未改按包内同SHA复用，没有额外现场GET回执就不宣称重新测过静态资源。普通smoke不触发工具发现路径，其行为依同源正式门禁；受控缓存读数不等于自然全量收益，后续自然窗口须扣除实际已入账的验收流量。

私有证据目录：`.local/cross-session-20260928/01a0e7ed/v0420-release-audit/`；正常历史、WS和原失败边界在`discovery-prefix-boundary/`。持续协作目标保持active。

只读复跑已保存的上线回执：

```powershell
python -B .local/cross-session-20260928/01a0e7ed/v0420-release-audit/check_deployment.py .local/reliability-release/6207cd8871eb5bbacd9857aa6e53709c3a261a8460ff6d3e136329694d5536f9
```
