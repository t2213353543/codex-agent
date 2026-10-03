# B组0.4.21发布复核

2026-09-29 05:07 +08：最终新源Go/vet、20PG、race、11live及不可变包独立核对通过，但发布目录当前只有stage.json，尚无preflight/activate/postdeploy。主会话正在准备不中断已有连接的切流方案，不能据之前“发布进行”计划宣布.21已上线；生产最新已验证仍为.20。

最终fullsource：`65fb8efde6f14f05459ccd3922a947ccb550ed34a69dbb37f1e717e06cb79a83`；1321完整源/742执行源。首轮旧source814ec2b…保留。工作目录`.local/compact-failure-release-20260929/`。

release：`da72d726d12674b032b48bf9360d8b673da4895d1a570bf264910ee2f5540048`；待部署binary：`ad63239e880d79a2f57ca41c28ee2304d8772512c732d42a92988d56cd6bc20b`。

## 本版实际范围

等待响应头或native compact Body.Read期间的真实caller取消，按已有cause规则记aborted/client/canceled并停止输出，不再返回伪造502。只凭ctx后来变取消不覆盖真实上游失败。初取消fixture错误命中headers阶段的记录保留，修正为Body.Read信号后的结果与独立headers两路径分别证明。

共享bps.CompactResponseMetadata只提取合法顶层id/model/usage计量视图；native失败先保完整响应中的已测量用量，再显式保持record.Status=error，拒绝时不保存可续历史。嵌套response不能覆盖root身份或用量。桥接成功需至少一个真实非空compaction状态，未知status或completed+非nullerror在任何done公开前拒绝，避免先吐出压缩状态再报错。

合法legacy省略status仍保持成功。显式status=null或空字符串原native偶然成功的形状现被拒绝，是有意校验收紧；不是声称全部历史非标准形状保持兼容。薄计量不构成成功、重试或历史提交资格。

## B消费者证据

B仅将作者已保存before/after三条有限投影（failed_state/completed_with_error/invalid_json）送文件存储、CSVv4及managed统计，实际1top2subPASS。完整失败保input/output/read/write与known指针，仍3error/0success；invalidJSON未知CSV留空。原汇总器误把test2json的长Output分片当完整行失败，原raw保持，离线重组确认测试PASS，不重跑也不捏造缺失的process/source尾回执。

该消费者fixture显式补NativeCompaction=true，原投影未含该字段，不能当实际producer标志观测或其筛选验证。默认统计不依赖该标志，三行计量结论不受影响。共享parser最终投影三行非耗时字段与原保存输入完全同，仅一个attempt耗时0→1ms；这证明原消费者字段结论仍适用，不冒称旧测试跑过新parser或22种完整形状。

管理Go/SQL源审确认responses/compact仍是推理且cache eligible（除warmup），error/aborted不因status本身排除真实用量；unknown不补零，known read0仍是样本。管理Errors含aborted，monitor另分列，不能把compact误说成control或声称取消从全部非成功明细消失。

## 正式新增PG范围

TestPGBPSCompactFailureUsageRoundTrip使用真实gateway三次本机上游失败的完整record，经prepareMetricRecord补存储身份后写PG，非人工构造计量。三行input1000/output11皆UsageKnown=true，cache分别750/250、0/0、未知；fresh JSONB/detail/list、JSONL普通/脱敏、普通CSV77列、status/endpoint筛选及managedstats覆盖。

预期3error/0success/0aborted，input3000/output33，已知read样本2、unknown1、750/2000=37.5%，write样本2/合250。该项不含aborted PG、completed-with-error PG、全usage未知或OverviewCacheUsage，需与其他测试范围分开。

首轮PG203336.799145实际statusfail/exit1/no drift，20top中19PASS，唯一新compact用例在175行失败。B随后确认这是脱敏JSONL分支仍用原ResponseID调用assertJSON，而现有产品脱敏会按固定算法替换该ID。源审先前没有识别这个断言错误，不能据“源审无阻断”反推PG应通过。主会话修正测试期望并增加原ID不可泄露断言，保留产品脱敏和用量断言；原失败不改写，不跳过PG。B精确比对1321源仅此test一hunk变化，runtime零变化，新fixture3c63a4f3…。

修订PG204449.389135现statuspass/exit0/no drift，20项及新TestPGBPSCompactFailureUsageRoundTrip实际通过；旧line175修正后后续CSV也按原严格断言实际完成。B门禁子逐raw收口另存revised-gates，不把原失败报告改绿。

## 正式门禁与后续

首轮live203336.815261真实summary SSE未产生summary_done/visible，10top通过但SequentialSummaryCLI失败且WS子未执行，原结果保留，不能归因于compact修复。主会话因此取消拟议的test-only复用，最终仍重新跑整轮；中间204334遗漏括号导致编译退出、0provider，原编译失败证据另保。三个失败范围不混用，不放宽摘要判据或验收规则。

最终Go/vet通过4874含子PASS/471环境SKIP/0FAIL，新11top/65含子实际全PASS。PG204449.389135为20top/33含子唯一RUN/PASS。race204516.241625原statuspass/exit0/no drift/no DATA RACE，B逐raw核1250唯一RUN/终态=1232含子PASS/18明确SKIP、449topPASS/18SKIP，11required含65层级/20协作/4Redis/95official全部逐项PASS。live204449.405879原exit0、11top+20必需子项唯一PASS，testbinary5418166b…与742源对应；本次摘要SSE/WS均收到done1/visible，原失败仍保留，不推“每次上游一定生成摘要”。

前端237/55制品/7证据、monitor30与价格按SHA复用。包内原validate、90制品/29证据/91归档文件集合及SHA完全一致；741backend scope加既有embedded JSON为742执行源，包内PG/race/live映射精确同final/current。旧四红绿log封为动机证据不代最终门禁，首失败PG测试不放行，未添加test-only例外。

以上包与current一致是20:54:35审查时点；后续owner已开始其他运行源码改动，不能把新功能算入这份不可变包或用旧65fb门禁覆盖当前工作树。新切流准备的准入/连接/维护/验收身份边界按独立方案核，不更改本包原证据。

现场仍为主会话唯一2生成/1本地top_p拒绝/3精确CSV，B只读保存回执。普通smoke不触发compact取消/失败或无效终态，不能用它代上述专项。整体持续协作目标保持active。

另已出现真实.20自然业务窗口：主保存20:46:48进程数据扣3条smoke后759请求/747ok/12error，已知输入读取率84.0792%。其中6CAS与4bound失败由主唯一元数据查询继续分析；本版compact修复不冒覆盖这些剩余会话问题，不能拿.20窗口当.21效果。
