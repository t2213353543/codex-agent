# 持续根因调查与修复（2026-09-30）

目标仍在继续：修复真实原因并持续观察，不通过隐藏/重分类/降低请求语义声称错误消失。用户明确排除限流处理和新增等待，本轮保持此约束。

## 证据与已发布修复

基线是两节点实际SHA 814ea54328a9aa411820f78ffdd7417017b6f4636fce0d6a9ecfb34f15bcc60b，源码使用精确隔离release-source，没有夹带主目录其他任务。

1. 工具schema误判。相同工具定义含properties.file_id或encrypted_content：放顶层tools成功；放协议支持的input.additional_tools.tools或tool_search_output.tools被递归扫描误当账号专属历史，明文旧会话错误503。改为仅在这两个顶层协议项的合法工具定义树里识别schema语义；其它真实文件、加密内容、未知/畸形定义继续保留owner保护。改前两项失败，改后三种位置均200且只调用上游一次。
2. 已完成的托管搜索被误当待执行工具。生产structured_mixed记录显示formatter1、client_call0、other_call1/2/4，复现6项全部失败。现在仅已声明/协议支持、status=completed且ID有效的web_search/server tool_search可与最终formatter共存；保留完整搜索轨迹、密文及usage，待执行/客户端工具、未知工具、文本冲突和schema失败仍拒绝。请求writer中原本禁止所有search+formatter的矛盾指令同步修正。
3. 失效绑定被失败请求无限续期。抽样5,151个Redis绑定，5,029个指向56个已不存在的管理账号；失败的GETEX/claim每次又延7天。现在只读查找/检查不续期，只有通过授权+准入的准确owner及活跃租约CAS续期；不复活已过期或覆盖已换owner的绑定。真实Redis验证失败请求TTL不续、合法会话续期、跨实例共享均通过。
4. 虚报服务档位。Sub2API按模型名字合成priority/ultrafast，Codex客户端按service_tiers决定发参；真实BPS单变量证据是省略tier完成、显式priority上游422。BPS目录现明确service_tiers=[]、additional_speed_tiers=[]，防止Sub2的missing-field merger再次补造能力。目标单分组将读取实际上游目录，保留5个模型映射；不静默删除已请求的档位。

发布release：df1004b60f60730fc036fbbfc778b516533cfba71e669126395b77c769699e4d。
运行binary：096aac21025f09da0a09606d91ee3fc6cea559bdc2a78467e84d8775f89556cf。

验证：674项相关Go测试（含子测试）通过；20个外部fixture/live门控跳过。go vet通过。冻结候选在同机Linux隔离PG/Redis的4个明确测试全部通过且无源码漂移。另有原生工具目录边界10项验证，模型目录及Sub2实际merge函数对照验证。

生产真实单次请求完成两次web_search_call并返回严格JSON，response.completed、schema验证通过。12项健康/页面/管理API检查通过。旧HTTP/WS保持自然排空，不能将切换后的所有日志误认成新代流量。

20:33:02 CST，目标Sub2API单分组已用updated_at和旧配置CAS改为读取对应上游的真实模型目录。5个模型映射逐项保持，其他组字段hash不变，未改其他分组；只按原缓存协议失效该组325条认证缓存。实际客户端目录确认所有service_tiers和additional_speed_tiers均为显式空数组，efforts来自上游，默认medium；旧5.6-sol的low和max/ultra均是先前按模型名生成，并非该组显式策略。没有发起模型生成，也没有重试429。证据.local/error-repair-20260930/catalog-pin-applied-123309.json。

## 持续观察

codex-agent-error-monitor.timer每60秒读取共享request_logs最新20,000个主键行，并按重叠ID去重统计新观测完成记录，避免用请求开始时间漏掉长请求。完全失去重叠时明确报告gap_detected。只保留固定协议枚举、状态、错误MD5和计数，不存正文、完整错误字符串或凭据。滚动最多60份、每份最多40个非429分组与20,000个ID，不再制造无界监控文件。SQL只读、10秒statement_timeout。文件位于/var/lib/codex-agent-observation。

复查命令：

    python tools/error_monitor.py status

部署监控脚本和timer可复跑：python tools/error_monitor.py install。

## 尚未证明解决的部分

- 老绑定中的缺失账号与当前账号都没有同时匹配workspace/user的身份；不能恢复不同账号的加密历史。不是简单缓存延迟。保留真实拒绝，不批量清除所有会话或丢弃加密上下文。
- service_tier旧客户端缓存/显式配置需要观察目录刷新后的流量；目录修复不代表既存客户端立刻停止发送旧参数。
- store错误同一文案同时覆盖true和非法类型，不能都猜成store:true。APIKey上游没有证据表明Sub2自动添加了true。
- b067错误已精确匹配未声明function/update_plan；官方PowerPoint工具集含planner，但尚未证明可以关闭隐含工具或请求writer丢了声明。没有伪造工具结果或执行未声明动作。
- 部分连接重置、invalid_encrypted_content和其他上游错误需要后续逐条复现与边界证据，不通过重发已产生输出的生成请求掩盖。

关键证据目录.local/root-cause-20260930，以及.local/redis-history-storage-test/20260930T121827.932426Z/report.json。目标保持active，不宣称全部错误已解决。

后续模型协议、有限工具诊断及账号轮换根因修复见 `docs/MODEL_PROTOCOL_AND_ROTATION_ROOT_CAUSE_20260930.md`。两节点当前新连接已使用binary `1dc3f79dc87219c98ab2231be08364ca3e55c3e969dc925b114a23d81688b168`；Loophub配套候选尚待现有注册任务排空与旧Codex WebSocket代退出，不能只凭新连接健康宣称整个自动轮换已修复。
