# Admin API 兼容生产发布 — 2026-09-27

2026-09-27T11:40:35+08:00 已完成生产切换与真实接口复测。Sub2API 的24个账号/分组/代理方法路由和CPA的8个管理方法路由已生效，原生管理台和两套兼容接口共用同一个账号库。

本次实现针对现有OpenAI OAuth账号同步客户端，不能表述为所有Sub2API SaaS管理功能或所有CPA提供商完整移植。没有配置远端目标，也没有启动后台双向远端复制；同步指客户端通过任一兼容协议对本系统账号读写，其余入口立即看到相同结果。精确支持项和明确拒绝的字段见[兼容接口文档](ADMIN-COMPATIBILITY.md)。

## 运行版本

- Release：`11566336dbf2fc13ff63fa327b3f746b7e43e18d4473d40409fd5a5fed1989ec`
- 完整源码SHA256：`34b1c6934a1cae9c4b3716c274bb417972727562c8f36615a7d6f9feee877ac1`
- 实际进程二进制SHA256：`5e74db6dc523142a9db0f1099ed941eb5e0a7f14d7ec3a32fae69c7cfdf8c473`
- PID：910578；状态active；自动重启0次。
- 前端产物保持此前已验收版本，未改动界面。

## 验证

| 范围 | 结果 |
| --- | --- |
| Go全包与vet | 3441项含子测试通过，vet通过；409项依赖环境/参数的用例明确skip |
| 管理PostgreSQL/Redis Linux race | 297/297顶层通过，0失败/跳过/遗漏，无竞态 |
| 网关BPS/Redis Linux race | 133项含子测试通过、79顶层通过，4项性能/live专用测试skip；实际Redis4/4通过 |
| 实际Codex HTTP/WS | 11项全部通过，含文件、恢复、压缩、patch、搜索、MCP、双账号、WS真实101与增量历史 |
| 未改写的Loophub客户端 | 实际import exporter与curl_cffi；11条流程通过，GET16/POST10/PUT3/DELETE3；合成上游真实inference1/privacy6 |
| 生产兼容协议 | 6组闭环全部通过；见下文 |
| 原生管理接口 | 12项通过 |
| 公网入口 | 存活/就绪/首页200；两套匿名管理401，错误信封正确 |
| 线上推理 | SSE、原生compact续聊、WebSocket预热和增量续聊全部通过 |
| 账号与配置 | 原有2账号，身份/版本/启停/关系等摘要不变；设置版本4；schema5条checksum及环境文件不变 |

第一轮PG测试有4个Sub2测试错误使用原生router，返回401。修正测试为生产webHandler后完整297项重跑通过；失败记录保留。两次冻结仅该测试文件变化，生产源码一致。真实客户端依赖外部本地源码/curl_cffi，单独在Windows执行并通过，远端selector明确排除该依赖测试，未将skip算为pass。

## 生产闭环

1. Sub2API列出真实数字ID分组和代理。
2. 用唯一合成名称创建禁用账号，CPA立即读取相同账号。
3. CPA替换合成AT/RT/ID token，Sub2API显式导出验证最新三个字段，稳定数字ID/auth_index不变。
4. Sub2API删除后，CPA和原生账号立即消失。
5. CPA multipart上传另一禁用账号，Sub2API搜索可见，再通过CPA删除。
6. CPA api-call请求本服务health，返回真实HTTP状态与body。

全部临时账号始终禁用，未进入模型调度且已经删除；原有账号元数据前后摘要完全相同。正常审计记录保留。凭据只在服务器内使用，没有传回工作站。

本次没有改动数据库schema或现有数据迁移。单实例发布沿用排空旧进程、不可变产物核验、配置切换、失败自动恢复旧版本的流程；本次未触发回滚。旧版本为`d0eb70f7629951fc8d87097b82c03e1ebcde6fda83e4dfb6991bd78df81146f4`。

## 使用与证据

根地址仍为 `http://43.225.196.170:18879`。Sub2API客户端用本系统Admin Key填入x-api-key；CPA客户端填同一个Admin Key作为Bearer或X-Management-Key。普通网关Key无管理权限。

- [完整接口/认证/差异说明](ADMIN-COMPATIBILITY.md)
- [上线汇总](../.local/admin-compat-20260927/deployment-report.json)
- [验收绑定](../.local/admin-compat-20260927/acceptance.json)
- [生产跨协议闭环](../.local/admin-compat-20260927/production-contract.json)
- [最终运行核验](../.local/admin-compat-20260927/production-final.json)
- [生产推理验证](../.local/powerpoint-release/11566336dbf2fc13ff63fa327b3f746b7e43e18d4473d40409fd5a5fed1989ec/production-smoke.jsonl)
- [最终PG/race](../.local/admin-compat-20260927/final-validation/race/report.json)
- [原生管理验证](../.local/admin-compat-20260927/after-management.json)

如需恢复上一个版本（本次未执行）：

```powershell
python tools/powerpoint_release.py rollback --release .local/powerpoint-release/11566336dbf2fc13ff63fa327b3f746b7e43e18d4473d40409fd5a5fed1989ec/package/manifest.json
```
