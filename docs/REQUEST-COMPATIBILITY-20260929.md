# 请求参数兼容与拒绝原因观测

0.4.15源码接纳`stream_options.reasoning_summary_delivery="sequential_cutoff"`，与`include_usage`可组合。准确控制保留在本地，原生摘要done、partial、item和密文仍按原顺序交付，不增加上游未经证实的字段、不生成新的摘要。未知枚举/字段和错误类型继续拒绝。

固定官方0.155 consumer在该模式下忽略partial，仅消费匹配当前reasoning item的完整done。一次同账号真实BPS观测取得99delta、1个完整418字节done，位于reasoning item.done及下一个message之前，原生和网关146事件形状完全一致。另两次实际CLI生成分别经HTTP/WebSocket，开启该feature后完成并显示reasoning项；HTTP请求明确携该字段，WS由真实配置、101及websocket记录证明路径，未单独捕获WS帧字段。事件正文未保存。该采证不证明所有未来上游响应必有摘要，也不把最终reasoning项误称独立验证了每个实时delta消费者事件。

命名空间强制工具选择：缺省namespace时只恢复唯一的同类型、同名声明；显式namespace必须严格匹配，function/custom不互换。多个命名空间同名、包括全局与命名空间同名的情形明确拒绝，指定全局工具可显式传`namespace:""`。原ToolChoice JSON、参数、工具schema和历史保持，不把未声明名字映射到相似工具。

结构化JSON Schema增加标准`definitions`容器兼容，与`$defs`一样递归验证并保留原schema和局部`$ref`。外部引用、未知约束、错误子schema仍拒绝；没有把声明为draft07的任意schema自动改写成2020-12。

参数错误在Prepare之前也按实际入口记录SSE/WS/compact。`error_param`只保存固定已知参数标识，动态未知stream_options键归为`stream_options.unknown`，不保存任意客户字段名；旧日志不回填。详情可见拒绝参数，CSV保持v4的77列，详情/JSONL含可选新字段。

结构化混合输出仍拒绝，但`structured_mixed`用三个计数和两个布尔区分多formatter、客户/其他调用、文本及拒绝内容冲突，不保存正文/工具名/参数。此诊断不授权丢弃冲突输出后伪造成功。

实际发布及同源回归以本版release evidence为准；这些更改不代表旧自然参数错误已逐条证明根因。缓存读取、创建、未知用量沿原统计合同分开。
