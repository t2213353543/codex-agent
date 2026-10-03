# A 组 0.4.12 协作交接

截至2026-09-28 22:59 CST，固定模型映射已上线，A组独立核对发布和现场证据通过。持续协作目标保持 active，本文件仅记录这一版完成边界。

发布ID `7e954284ef17e1aa4cfa11851b0df3704d72e2cdb6362b808c343af7a3b2fc6c`，source `a624e5e0dba4ea20cf36711782addef70dfe8bf03d9f8e016bb470f90b03ab69`，实际PID1265766，binary `52b4fc4e59417868b6ca3fd2678c0b9d27205cc0c4475a90c441c50dc5e61818`。

## 本轮贡献与实证

- 动态候选阶段复现首选模型不支持effort就提前失败的问题，owner修复后原4反例转绿。用户后来改为固定规则，该动态结果仅保留历史，不作为线上合同。
- 静态阶段核对缓存按实际发送model分区、原请求与历史保持，配合B/C验证零目录GET、同请求固定规则、设置保存/回滚。采用主owner最终静态实现，本组没有竞争覆盖运行文件。
- 固定窗口只读审计精确命中32个完成后授权拒绝和33个token失效401；确认两次账号删除，按请求开始时间+延迟计算的完成时间与删除后授权守卫一致。token失效发生在删除前，操作者/外部原因仍未采集，不把相关性夸为完整根因。不恢复、不改绑、不重放用户历史。
- 新版最终Go/vet、12 PG、20协作race/4Redis、完整八suite live和五静态路由supplemental gate均通过；旧midflight修复前快照没有被混作最终结果。
- 封包82产物/83tar成员/21证据摘要一致。上线后五个标记合成请求全200/completed，requested/sent/record.returned/response.model符合保存的24条规则，cooldown=false且账号配置保持。

证据索引：

- [最终实网门禁](../.local/collab-a-20260928/routing-0412/release/final-current-live-review.json)
- [race核对](../.local/collab-a-20260928/routing-0412/release/final-race-review.json)
- [封包核对](../.local/collab-a-20260928/routing-0412/release/package-final-review.json)
- [上线后核对](../.local/collab-a-20260928/routing-0412/release/postdeploy-final-review.json)
- [授权时间线取证](../.local/collab-a-20260928/authorization-0411/REPORT.md)

## 下一轮保留事项

未解决范围仍包括不可用账号/外部token失效原因、service_tier（上次唯一baseline即429，字段对照未执行）、未声明上游Office工具、store/access_programs与图片预算。自然流量应按实际版本、窗口、工作负载分别观察，不能用现场五条合成请求宣布长期缓存改善或错误清零。

OAuth真实网络尝试观察另有私有候选。v4的字段保存/消费与真实PG单项通过，但独立review发现截断快照覆盖历史401的问题；v5在独立副本修复并验证，完整进展见[候选入口](../.local/collab-a-20260928/oauth-attempt-candidate/README.md)。该候选未合主树、未进入0.4.12。后续需基于新主树rebase、明确schema2与消费者合同，再做正式集成验收。
