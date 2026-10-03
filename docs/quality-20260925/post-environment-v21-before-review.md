# v21 before 基线独立核验

before 四次固定原题 native HTTP 请求均完成，独立核验140项通过、0 findings。顺序为new2、new1、new1、new2，结果new2两个21，new1两个29；第一格自动null保留，以独立原文注释确认21。

| 格 | 账号 | 自动答案 | 人工注释 | input | cached | reasoning | output |
|---|---|---:|---|---:|---:|---:|---:|
| 01 | new2 | null | 21 | 6881 | 0 | 2239 | 2484 |
| 02 | new1 | 29 | — | 6881 | 5888 | 516 | 783 |
| 03 | new1 | 29 | — | 6881 | 5888 | 516 | 688 |
| 04 | new2 | 21 | — | 6881 | 6656 | 1346 | 1622 |

第一格原文首句“最少21”、方案9圆12星、末尾方框21一致；中间如果/或是配对条件和20颗反例。注释绑定 final SHA `c08df088e346385f44f8d7495510c2260d192b00e2b96670fd50402f62e2859e` 与原 report SHA `6e5f91097d6f8fd35967a44ea749ac86a4f8b09eb29f2c903cd9800b5ebfbb11`。没有修改原评分器或报告。

这阶段是“源环境修复已经部署，仍使用阶段开始时已有AT”的基线。new2绑定当前token SHA `889c9b1d…`，并非v20此前的`84ba24ba…`；new1绑定`0e28971d…`。四次实际正文哈希、应用头、认证账号、模型medium控制、独立helper及前后usage/epoch均通过。不能把它写成未修代码与已修代码比较，更不能用这些旧凭据结果判定修复后重新发行的凭据质量。

完整before回执已实际执行冻结 `run_phase_remote.py` 的 `validate_before_barrier`，也通过源干预脚本调用同一冻结函数的合同。只抽取/执行纯校验函数或替换无I/O inventory导入，不读取管理密钥、不发网络请求。原文和模型错误策略均不按答案是否正确改变；本次四格恰好全部完成。

首次本地汇总的唯一SHA问题是采集器使用文本读取将 `source-runtime-pins.json` 的CRLF改为LF。对象内容相同，但原始字节哈希不同，因此初次审查保留失败项；随后根任务以SFTP原始bytes重新取得实际远端文件，恢复冻结SHA `9fd495b3…`，最终核验通过。没有放宽字节哈希门禁或把问题归为生产源码变化。

before unit已退出，MainPID=0、inactive、退出码0。原始body/head仍按预登记保留在私有目录供after复用，不能宣称本批全部raw已清理。源干预及after结果尚不属于这份before结论；必须确认每账号仅一次真实新授权、DID/profile绑定新AT、目的端版本绑定回读及新的usage门禁后，才能报告“新凭据的after”。

补充只读流量证据：`before/concurrent-traffic.json` SHA `c75d55873af0be2256b077ebf28e7ea7fe6e20d9f642dc89d89fe0ee84ba9e60` 中，四格时间窗的已完成、持久化生产请求重叠均为0。它不覆盖未完成请求或外部账号使用，不能据此宣称账号完全没有其他活动。

证据在 `.local/research-20260925/post-environment-controls-v21/review/before-final.json`、`actual-before-barrier.json`、`source-before-barrier.json`、`human-annotations.json`。本次独立复核为零网络、零重新授权、零模型调用。

只读复核：`python .local/research-20260925/post-environment-controls-v21/review/review_phase_results.py before`
