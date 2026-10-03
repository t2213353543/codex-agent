# B 组：0.4.12 静态映射复核

依据用户最新要求，本候选以管理设置 `bps_model_routes` 一次决定最终target，请求期不查目录、不逐模型试错。此前动态catalog/singleflight报告仅保留历史，本组不再推动其补丁合入。

## 实际部署与独立核对（22:58 CST）

0.4.12 已部署，release `7e954284ef17e1aa4cfa11851b0df3704d72e2cdb6362b808c343af7a3b2fc6c`，PID1265766，binary SHA `52b4fc4e59417868b6ca3fd2678c0b9d27205cc0c4475a90c441c50dc5e61818` 与已审包一致。实际24条规则逐行核对，5条现场路由全部200/completed，requested/sent/returned和xhigh档位匹配配置，usage已知；冷却false，设置/账号配置保持。

修订675项后端在Go4397PASS/456环境SKIP、race20协作+4Redis+midflight、PG12、真实8suite中同源。包82产物/83tar成员/21证据/55前端逐SHA一致。本组只读回执 `v0412-static-release-audit/deployment-audit.json`，没有重复生产模型请求。这证明固定映射交付，不代表所有其他兼容错误已消除。

## 已验证通过

- 静态六模型的ID、全部effort、默认档位与主任务一次管理发现 `static-model-discovery.json` 逐项完全一致。该比对不再调用管理或模型接口。
- 设置非法target、重复source、缺通配兜底、空串、非字符串和旧CAS均拒绝，不污染已保存规则。
- Windows真实共享锁阻止atomic rename时API返回500，旧内存/磁盘/路由不变；解锁后相同版本重试成功，临时文件清理。
- 新规则保存立即生效，target是最终值而非下一规则source；旧请求快照保持旧规则；导出state实写后新SettingsStore重读规则逐字一致。
- 显式API空串拒绝；null作为未提供；旧内部零值Set兼容默认；明确落盘空串拒绝，彼此没有混用。
- 正常JSON、WebSocket、compact两轮派发目录GET=0，原请求模型与实际发送/返回模型分别记录，公开历史和owner不变。
- 未知source使用通配兜底；target400/404只一次POST、不试另一个模型。

设置套件4顶层/7子项PASS、无跳过；672文件冻结快照无漂移。正常派发证据与仍失败项分别保留，不将后者抹掉。

## 错误路径缺口已闭合

`gateway_bps.go` 收到422或403仍调用旧 `bpsWriteCapabilityError`，在生成错误路径额外GET `/responses/access`。独立合成复现：422时POST1/GET1；首账号403、第二账号成功时两POST均相同固定model，但GET1。

主owner已从生成错误路径删除该hook。B新建681文件私有快照，复用原请求与断言（只去掉已撤销fixture接口的3处参数），3顶层/7子项全部PASS：422只1POST/0GET；403换账号仍固定同模型、2POST/0GET，JSON/WS/compact历史不变。原红与一次fixture编译适配日志单独保留，新结果在 `v0412-static-dispatch-review/after-tests.txt`。B没有修改owner实现，此次静态零GET缺口已闭合，不再作为发布阻断。

## 在途更新的单请求一致性也已修复

独立复现发现第一账号派发后修改映射，第二账号重试会改用新target。主owner已固定当前逻辑请求的rule字符串：原序列 `[sol,astra,astra]` 变成 `[sol,sol,astra]`，新独立请求仍读取新配置；只固定路由表，其他Settings/授权仍实时读取。

B对owner新实现独立复验 **7顶层/16含子例全部PASS、682文件无漂移**：403/429、SSE、WS、compact保持原target；在首POST后禁用API key或关闭HTTPStream/HTTPJSON仍阻止第二POST。报告 `v0412-static-midflight-candidate/owner-recheck-result.json`。B的私有pointer候选已被owner的字符串+initialized实现取代，不应重复应用。

主任务22:42重新冻结并重新执行门禁。旧22:34 race虽执行PASS但明确source_changed=true，原Go/PG/live证据不能证明新增midflight修复；旧报告完整保留，新门禁状态以对应新批次为准。

## 证据与边界

本组证据根 `.local/cross-session-20260928/01a0e7ed/`：

| 目录 | 内容 |
| --- | --- |
| `v0412-static-catalog-review/` | 记录目录与硬编码能力的独立一致性测试 |
| `v0412-static-settings-review/` | 设置CAS、写失败、热更新、导出重读与空值契约 |
| `v0412-static-dispatch-review/` | JSON/WS/compact与错误路径真实loopback请求计数 |
| `monitor-install-review/` | 独立核对监控最终ead2…脚本已安装，定时器成功推进并绑定0.4.11 runtime |

本组静态检查全部无真实模型调用、无生产配置改动。测试不证明固定目标对所有账号永远可用；配置是显式路由决策，实际上游拒绝必须保真返回。主任务随后已完成上述统一验收与0.4.12实际发布，私有研究与最终发布证据分别保存。
