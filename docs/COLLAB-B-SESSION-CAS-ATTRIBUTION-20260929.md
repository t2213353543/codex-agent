# B组会话绑定提交错误归因复核

2026-09-29 02:00 +08：独立旧版反例与主会话修复后对照完成。当前线上仍以实际部署回执为准，本报告证明受测代码路径，不构成下一版已上线结论。

02:26 +08更新：typed归因及压缩失败计量已随0.4.17正式发布，release49c77636…/PID2058745/binary4cf31b8c…，18:24:32 UTC现场验收通过。异常分支由同源Go/race/PG17支持，现场普通smoke未故意造CAS失败；完整[发布复核](COLLAB-B-V0417-RELEASE-REVIEW-20260929.md)。旧六条的具体绑定竞争原因及<4秒/20秒分界仍按本文限制解释。

## 六条生产记录支持的事实

主会话唯一只读投影保存于`.local/session-reliability-release-20260929/prior-cas-records.json`。固定旧0.4.15窗口与id上界恰6条，B只分析本地结果，没有重复SQL。

六条全部为SSE/error、记录HTTP200、source/phase=client/response_write。每条schema2账本只有一次真实generation，实际headers200、没有refresh且未截断；发送/返回模型相同，无终态或已知用量。ToolPolicy为auto、目录0且非结构化。首个输出观测时间2845–3912ms，之后5–6ms收尾。

该首字观测不证明字节已交付客户。一次真实generation也不排除之前发生本地账号选择失败；没有owner/session关联，不能指定真实修改绑定者或把六条强归同一并发竞争。未知用量保持未知。

已进一步用部署源码确认latency_ms为包含此前尝试的完整请求耗时，六条最大3918ms，且本请求lease在选路后才创建、首tick固定20秒。因此这六条不可能由其自身20秒“相同替代账号续租误取消”触发。主会话另有21秒真实反例支撑续租修复，但不能将它与这六条短请求串成已证根因；其他请求先前改变绑定仍未被排除。证据在`session-cas-next-evidence/lease-timing-boundary.json`。

## 已部署源码的错误传递路径

已固定0.4.15源码的9文件对照：内部`officialFailoverWriter`在首个真实输出交付前commit失败，错误返回`gatewayResponseWriter`，后者无差别先标client/response_write。随后BPS写入原CAS错误并保持首次source，SSE异常退出，finishAborted仅补空值并落账。

因此attempt中的client来源也是record回填，不能作为第二份客户端断线证据。当前六条最终标签不匹配ReadCancellation的canceled/timeout、结构化response_validation或正常outer统一409分支；但这些旧元数据仍不足以知道原绑定为何变化。

## 最小独立红绿

相同两个fixture分别在正式0.4.16冻结源码与主会话实际修复后的独立副本执行：

| 场景 | 已部署代码 | 主会话修复后 |
| --- | --- | --- |
| 内部CAS commit返回错误 | target.Write=0、visible=0，却标client/response_write；未交付TTFT/FirstOutputKind保留，断言失败 | 同样零target写/零可见字节，原cause通过Error/Unwrap保持；改gateway/session_binding，TTFT/FirstOutputKind清空，断言通过 |
| 真正target.Write失败 | target.Write=1，client/response_write，通过 | 原分类/行为保持，通过 |

这里测试直接使用实际两层writer和commit屏障，没有发真实模型请求，没有修改主会话实现。初次读取时当前源码已含修复，那个初green日志单独保留；没有把它倒称成旧版反例。真正原红来自正式PG173634.831282封存源码，四关键文件SHA与0.4.16发布清单一致。

该独立小边界不重复主会话的20秒真实lease并发测试。它证明内部错误不会再被误标为客户端断线，且不会泄露未获绑定的输出；不证明所有旧CAS失效原因已经解决，也不支持放松不同owner保护。

0.4.17已于02:03 +08冻结并启动统一门禁；本组新增10项合成测试全部被正式race选中，PG wrapper包含`TestPGBPSSessionCommitObservationRoundTrip`。普通上线smoke沿用两生成/一本地负例/三导出，未制造真实CAS冲突或TTL到期，不能代替这些正式异常链路用例。

## 证据入口

- `session-cas-next-evidence/owner-projection-analysis.json`：6条本地复算与查询来源SHA。
- `session-cas-next-evidence/final-record-path/SOURCE-REVIEW.md`：已部署源码行号与必要条件。
- `session-cas-attribution-review/deployed-before-tests.txt`：旧版1红1绿。
- `session-cas-attribution-review/owner-after-tests.txt`：同fixture两项绿。
- `session-cas-attribution-review/deployed-source-comparison.json`：基线与发布清单一致。

以上路径根为`.local/cross-session-20260928/01a0e7ed/`。受测原输出、未知状态与失败证据保持，不通过改写旧数据来使报告变绿。
