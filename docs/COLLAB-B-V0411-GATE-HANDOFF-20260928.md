# B 组：0.4.11 最终门禁交接

本组持续协作目标仍开启。0.4.11 已由主会话实际部署，本组已独立核对激活和三条postdeploy回执：PID1068422，binary SHA `d674881063ba0aae5ce92b226334b31b4d8402328ac0931faca8806ea52744d4` 与封存包一致；实际冷却开关false。strict=true JSON、strict=false SSE、json_object JSON均200/completed、用量与记录匹配、内部工具隐藏，设置与账号配置保持。回执 `.local/cross-session-20260928/01a0e7ed/v0411-final-evidence-review/deployment-audit.json`。

监控脚本更新属于C独立运维证据；全模型fallback属于下一版私有候选，不能与本次上线混称。以下保留本组门禁修复与失败重跑的完整过程。

## 最终实跑的环境修正（21:52后）

首次实际race `20260928T134245.975065Z` 已失败，20项新增选择全部执行，其中19PASS、Redis关闭保期限一项FAIL，原4项BPS Redis通过，无race warning。原始错误为Redis NOPERM；新增用例的通用fixture默认 `codex-agent:test:`，与隔离账号获准的 `codex-agent:integration:` 前缀不一致。

主任务已仅修改runner的私有env文件，同时设置 `TEST_REDIS_PREFIX=codex-agent:integration:bps-test:`。fixture继续自行加随机后缀，不修改ACL/业务数据库/测试语义。当前runner SHA **`72810acefaff1f71e168d2588d7965c2f82232dc12cadc3d705dc6175a6b7c92`**，覆盖下文历史20项交接SHA。本组AST离线验证实际env-file/docker装配：移除prefix的反例失败、当前配置通过，零网络，证据 `v0411-race-prefix-fix/owner-prefix-check.json`。

实际补跑 `20260928T135230.139791Z` 已终态 **PASS/exit0，20项协作全PASS、无race warning、无源码漂移**，Redis关闭保期限项真实通过。671项backend mapping保持不变；最后完整live原批次 `20260928T134245.967471Z` 已exit0，8suite与A补充门禁全部要求通过；最终PG11PASS与Go/vet继续有效，不重发模型。原失败报告保留，不改写。

独立审计逐一比对report和原始go-test.jsonl，20项及子项均无SKIP/FAIL，原4项BPS Redis也PASS。补跑report SHA `ca9b2d8b5bc80367eec918dfc60c5e2f36011a9ac7879b0c740ef99a3021139e`；11项PG report SHA `e3d09f0d2071d269ff1c88bb805fa380ab83ed5319b360844471c090c260652a`。详见本组 `v0411-final-evidence-review/retry-latest.json`。原full-source中旧runner引用与后续监控工具改动仍需主owner更新绑定；这与后端源保持一致是不同层次，不能静默跳过工具差异。

## 已落地且冻结的改动

主会话21:34授权本组独占 `tools/bps_race.py` 与新 `tools/test_bps_race_selection.py`。原server race仅选TestBPS及少量official族，漏掉本轮异常落账、限流冷却、局部选择诊断与writer错误回归。

最终选择集合为19项纯本地协作测试；`--with-redis`再纳入关闭冷却不删除共享deadline的1项，共20项。包括用户最新默认关闭要求的当前请求仍可failover、下一请求不因旧冷却被阻止的回归。只扩展测试选择，不改变冷却默认值、重放或账号策略。

每个精确声明在冻结源码中必须存在，每个受要求测试结果必须为PASS。missing/SKIP/FAIL均使报告FAIL。`--redis-only`仍只检查原四项，默认无Redis时不会把环境跳过算作新增覆盖。official synthetic中误纳入的 `TestOfficialUsageRepair...PG...` 已排除，真实PG另由主任务11项PG runner验证。

最终SHA（21:40:45 CST）：

| 文件 | SHA256 |
| --- | --- |
| `tools/bps_race.py` | `1d5ad51fef102bc52ab1a8fb84ee852475fce31c88a7ecefe1d8450f44a20df5` |
| `tools/test_bps_race_selection.py` | `f5475f4b2485cffe21177864ba93926592fa3aec237645555388ae948ca0619b` |

7项离线测试全部通过，root独立复跑也通过。它们实际验证选择集合、缺源码拒绝、报告缺项/跳过/失败拒绝和不同运行模式，禁止网络调用。B没有提前启动真实race。

最短复跑：

```powershell
python -B -m unittest discover -s tools -p test_bps_race_selection.py
```

主任务最终race沿用原命令即可自动纳入新覆盖：`python tools/bps_race.py --with-redis --include-official`。历史12项/19项版本SHA不再用于最终freeze。完整回执 `.local/cross-session-20260928/01a0e7ed/v0411-gate-review/handoff.json`。

## 同轮独立集成补证

- typed错误分支5项PASS：真实存储错误仅返回固定安全文案；JSON/SSE/WS无底层凭据/诊断泄露；WS授权撤销仅一次failed，同lane恢复后可正常完成。报告 `typed-structured-error-review/REPORT.md`。
- history控制16个四轮跨HTTP/WS场景PASS：text省略继承，显式null/空对象/仅verbosity/type:text按顶层替换清除，换schema/json_object正确，分支回旧response恢复旧format。公开与保存历史无内部formatter/孤儿调用。报告 `structured-history-controls-review/REPORT.md`。
- CLI结算确定性机制PASS：真实loopback客户端读到completed时，handler若阻塞在上游Body.Close，终态计数和metrics可以仍为0；释放后均追平。该机制不追认历史真实失败。建议live等待后仍显式检查settled和记录数等于上游请求数，不能仅用measured>=2。报告 `structured-cli-settlement-review/REPORT.md`。

以上目录位于 `.local/cross-session-20260928/01a0e7ed/`，每份报告绑定自己的快照和范围。其它owner并行变化分别记录，不能把这些局部PASS拼成一份从未实际运行的整树验收。

## 最终发布仍需的证据

由主任务在相同最终冻结源码上运行完整Go/vet、race/Redis、真实PG、前端封存验证和真实BPS全套，其中structured六步、官方CLI带schema的真实客户工具SSE/WS均须PASS。A组补充live verifier应按 `--require-complete` 严格检查单目录/单binary，不拼旧快照；本组不重复模型请求。

主work的PG wrapper现已改为 `raise SystemExit(check.main())`，本轮11项实际PG PASS已由原始report/test.jsonl复核。此前吞失败码的反例保留用于溯源，不能只靠外层exit0判断数据库覆盖。
