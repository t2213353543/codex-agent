# 压缩历史解析分配优化

已部署 Go `6ecdf93f…`。北京时间2026-09-26 04:32线上HTTP模型目录200/9个模型、WS握手101验收通过；04:34再次核对实际运行进程SHA。helper仍为`bf920a0b…`，未更改TLS、身份或代理。文中性能为绑定同一最终生产源码的本机测试，不是线上延迟测试。

前一项字段保真修复使用map解析精确成员，正确保留了未知大小写扩展，但会复制每个历史项的大段opaque密文。独立微测发现1000条历史的累计分配由约7.18MB上升为11.21MB，遂单独优化。

本候选只在压缩适配器新增局部选择器：先对原始字节完整`json.Valid`校验，再使用项目既有字段跨度扫描器，定位最多两个正式成员，仅解码被使用的字段；不会为所有未知大字段建立临时map。RawMessage借用范围仅限当前调用，最终公开输出仍拥有自己的字节。共享WS解码、共享JSON扫描器、请求限额、HTTP/WS配置均保持。

独立差分曾发现首稿接受`\v{}`而旧解析器拒绝：根因是jsonObject先去掉了非JSON空白。此稿没有部署；最终改为原字节完整校验后只跳JSON规定空白，保留原拒绝行为。差分、模糊测试和显式非法空白回归共同验证。它不把既有同名重复成员last-wins政策变为严格serde拒绝。

## 近32MiB实际样本

测试输入为32700条、每条1024字节的合法JSON历史项（其中密文payload968字节），原始请求33,517,586字节，低于33,554,432上限。两版本同本机Windows/Go1.26.7、GOMAXPROCS2，各阶段`-benchtime=1x -count=3`，setup与输出检查在计时循环外。

| 两阶段中位数之和 | 当前map版 | 最终优化候选 | 降幅 |
|---|---:|---:|---:|
| 时间 |902.4072ms|735.7992ms|18.463%|
| 累计分配字节 |419,769,280|272,710,224|35.033%|
| 分配次数 |1,308,248|261,808|79.988%|

这是实际近上限执行，不是线性外推；B/op是累计分配，不是峰值RSS。两阶段中位数相加不是端到端请求延迟。prepare两组均有单样本分配波动，表格不能解释成每次保证或生产并发吞吐结论。每100/1000条另有三方比较：旧大小写不敏感版本、当前正确map版、优化候选；正确性修复不为性能回退。

[近上限原始样本与计算](../.local/research-20260926/compaction-allocation/review/near-limit-review.json) · [独立数值审核](../.local/research-20260926/compaction-allocation/review/near-limit-independent.json) · [候选清单](../.local/research-20260926/compaction-allocation/candidate-manifest.json)

优化不改变模型输入语义，也没有新模型/OAuth调用；不能据此称账号原题恢复。

完整最终392文件源码验证：Linux race3411项通过、27项明确跳过，Windows2942项通过、362项跳过，两端vet通过；13个实际native邻接场景全部通过。独立140个通过事件、130797次coverage fuzz通过。部署仅切换后端，设置、账号元数据、前端、helper、身份及EnvironmentFile列表/内容保持，临时key/group及文件已清理，无迁移。两个改动文件已核对基线后同步共享工作区。

[最终构建](../.local/research-20260926/compaction-allocation/build/build-manifest.json) · [独立正确性审查](../.local/research-20260926/compaction-allocation/review/independent-review.json) · [上线回执](../.local/research-20260926/compaction-allocation/release/activation-result.json) · [线上验收](../.local/research-20260926/compaction-allocation/release/runtime-smoke.json)
