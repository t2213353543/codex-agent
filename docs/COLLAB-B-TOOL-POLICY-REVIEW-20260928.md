# B 组工具策略观测协作记录

2026-09-28，0.4.13上线后的下一轮。主会话持有运行代码集成和真实请求；B提供独立性能及记录保真证据，不部署、不另发模型请求。线上0.4.13与私有/待发布候选分开。

## 性能结论

原观测helper每次item捕获都会遍历有效工具目录。主会话新增lazy helper只在正常化前借用type/name/namespace三个标量，实际失败时才解析目录与身份；Prepare和whole-response校验前仍各保留一次完整快照。

独立同轮对照（Windows11 / Go1.26.7 / Ryzen9700X，单核150ms×3中位，**每次操作16个回调**，每8工具1namespace）：

|工具数与场景|原正常化路径ns|逐item完整观测ns|lazy观测ns|逐item完整观测B|lazy观测B|
|---|---:|---:|---:|---:|---:|
|128，全部正常|739.2|54865|973|22592|224|
|128，末项拒绝|2234|58488|7633|22753|1800|
|1600，全部正常|713.2|712116|1036|212544|224|
|1600，末项拒绝|12382|756774|81582|212718|13673|

正常路径lazy与baseline均224B/24alloc。末项失败仍需一次目录扫描，不能称所有路径O(1)。这不包含Prepare/whole-response开销，不用于承诺线上响应时间。三臂使用同一输入、源码、共用benchmark分派、相同观测初始值及错误sink；独立适配审查未发现不公平边界。

5个直接parity子例重复3次全部PASS，包括forced choice恢复namespace后仍失败时的错误、修改后item和观测字段一致。源码及候选SHA无漂移。数据：`.local/cross-session-20260928/01a0e7ed/tool-policy-perf-review/lazy-comparison/report.json`；原始日志同目录benchmark.log。

## 请求变化及缓存边界

B已运行24个合成Prepare对照，仅最后developer item变化，之前history/tools及其他body字段相同，compact全文不变；工具排序只保证新增清单确定，不使完整请求对原tools顺序不敏感。短名1600工具额外wire115620B/约束UTF8文本96418B，字节不是token，固定cache key不是命中证明。C随后更完整的Unicode/结构化矩阵及单次构造候选归C维护，本组不重复拓展。

证据：`tool-policy-request-impact/run-20260928T154714.288545Z/report.json`，说明和一条复跑见其上级README.md。

## 记录生命周期与真实效果

当前源码每次Prepare成功后创建独立观察pointer，只在原3个工具拒绝点写入，第一拒绝记录不会被后续cleanup覆盖。内存recent/导出存在浅拷贝，已读生产消费者只读且无发布后写路径，尚无已证共享污染。

B两个私有overlay测试通过：直接helper构造record后，typed JSON/脱敏导出保留已知0和false，未观测旧记录省略；后续请求拒绝不修改先前成功历史，文件关闭/重开保持字段。此为真实文件/导出路径测试，不代gateway/PG/failover验收，后者由主会话/A持有。证据在 `tool-policy-record-review/`。

新进程首份自然监控实为上线2成功+1本地拒绝三个smoke，无法据此证明自然工具错误消失。主会话第一A/B尝试只有基线HTTP400，0完整pair，不能推断精确清单的生产效果。B不重启或重复发请求，后续实验和发布以主会话实际回执为准。
