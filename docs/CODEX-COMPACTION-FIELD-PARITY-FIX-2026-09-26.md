# 压缩接口字段保真修复

已部署 Go `58580fcf…`。北京时间2026-09-26 03:59:52线上验收通过：模型目录200、9个模型，WS握手101并关闭释放连接；04:01再次核对实际进程SHA与产物一致。helper保持`bf920a0b…`。

兼容 `/responses/compact` 接口将旧JSON请求适配为订阅后端的流式Responses压缩。旧代码在四处使用Go结构体解析，大小写不敏感；官方类型标签按精确名称读取。遇到原始字段`type`与不透明扩展`Type`同时出现时，扩展值可能改变类别判断，删除携带`encrypted_content`的历史条目，错误生成两个压缩触发器，或将合法数组/对象扩展报为400。请求`Input`、消息`Role`、摘要`Encrypted_Content`也会污染历史选择或造成502。

候选仅将该适配器四处已知字段读取改为现有精确成员解析。未知扩展及加密密文保持RawMessage，真正的`compaction_trigger`仍只保留一个最终控制；小写必填字段缺失或类型错误仍失败，不能用大写别名补齐。正文内推理参数、工具输出、精确数字、并行工具配置和其它扩展字段保持原值。

真实TCP测试保存旧源码红例与修复后绿例。普通HTTP和WS两轮对照、加密reasoning/tool output、保留user/developer上下文、摘要仅在output_item.done中出现、原生压缩及gzip拒绝边界均分别验证。独立复核5个顶层、7个叶子通过，无跳过；其本机Go transport结果不冒称真实native已测。

同名重复JSON成员仍保持现有map-last-wins兼容策略；本轮没有把它改成官方serde的严格重复拒绝。旧JSON压缩适配缺少CLI本地harness元数据，选择保留消息和token预算仍有已记录差异。这是修复明确的数据丢失入口，不是整个压缩协议完全等价证明。

[候选清单](../.local/research-20260926/request-field-parity/candidate-manifest.json) · [真实红例](../.local/research-20260926/request-field-parity/field-parity-red.log) · [独立审核](../.local/research-20260926/request-field-parity/review/independent-review.json)

本轮糖果题是新会话，未调用压缩接口；不能把这项历史丢失缺陷当作其错答原因。

最终391文件冻结源完整验证：Linux race3298项通过、27项跳过，Windows2829项通过、362项跳过，两端vet通过；13个新增native场景全部执行通过。发布仅换Go后端，设置、账号元数据、helper、前端、身份与环境文件顺序/哈希保持，临时key/group及文件已清理，无数据库迁移。生产文件和新增测试已核对旧SHA后同步工作区。

独立性能测量：1000条×1KiB历史，两阶段中位数合计25.12→28.27ms，累计分配7.18→11.21MB。它不是峰值内存；近32MiB仅有线性估计。本修复新增map复制成本已列为下一项独立优化，不用未测候选覆盖发布证据。

[完整构建](../.local/research-20260926/request-field-parity/build/build-manifest.json) · [发布回执](../.local/research-20260926/request-field-parity/release/activation-result.json) · [线上验收](../.local/research-20260926/request-field-parity/release/runtime-smoke.json) · [性能对照](../.local/research-20260926/request-field-parity/review/compact-performance-review.json)
