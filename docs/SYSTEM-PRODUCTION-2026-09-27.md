# 系统优化生产发布 — 2026-09-27

已于 2026-09-27T11:00:41+08:00 完成生产切换及复测。本次发布包括管理系统性能优化、同工作区已完成的网关优化与控制台调整。服务版本仍为 `0.3.0`，精确版本以不可变 release ID 和运行二进制哈希识别。

- Release ID：`d0eb70f7629951fc8d87097b82c03e1ebcde6fda83e4dfb6991bd78df81146f4`
- 完整源码 SHA256：`309f7553f34e806030c4415aa5a014c7e7401b2c050e148ec7a49373ab6be73b`（925 文件）
- 运行二进制 SHA256：`f3d632e73b843248ac21d3d8ce61e37e060f669def6188f9dd31b18f53a24ce3`
- 运行 PID：`763839`；发布状态 `active`；发布后自动重启次数 0。
- 公网控制台：[登录页](http://43.225.196.170:18879/#/login)。

## 发布前验证

冻结当前代码后执行完整 Go 测试与 vet：3359 项含子测试通过；400 项依赖显式外部配置/性能参数的测试跳过，不计为已执行。实际 Codex HTTP 8 项、WebSocket 3 项另行全部通过，覆盖文件、恢复、压缩、apply_patch、搜索、MCP并行、双账号和真实101连接。

同版 BPS Linux race 133 项含子测试通过，79 顶层通过，4 项性能/live测试跳过；Redis4项全部通过。管理 PostgreSQL/Redis Linux race 272/272通过，无失败/跳过/遗漏。临时验证容器与私有环境文件均已清理。

前端隔离重建与先前通过185单测和浏览器验收的产物逐字节一致。打包的56个公开文件匹配，内部Vite manifest不属于线上公开文件。发布器22项事务、健康与回滚测试通过。

## 生产验证

| 范围 | 结果 |
| --- | --- |
| 存活/就绪/公网首页 | HTTP 200；首页SHA与新包一致 |
| 管理接口 | 12项通过：账号、分组、代理、Key、日志、概览、设置、诊断、审计、coverage、批量用量、运维 |
| 前端资源 | 公网匿名Chrome登录页正常；JS/CSS、字体、favicon均200并匹配新包；无未捕获JS错误、无请求失败 |
| 匿名会话 | 预期401；浏览器未提交登录或写接口 |
| 模型目录 | 6个模型，包含gpt-6-astra |
| HTTP SSE | 收到真实delta及成功终态，验证文本匹配 |
| 原生compact → 续聊 | 加密compaction产生并成功恢复随机标记 |
| WebSocket | 预热、previous_response_id和stream_id增量续聊通过 |
| 数据与配置 | 2账号、6分组、1代理、6Key；稳定对象身份/版本/配置摘要一致；设置版本4 |
| 数据库/环境 | 5条schema migration checksum不变；production.env摘要不变 |
| 运行状态 | active，PID和二进制匹配，自动重启0次 |

发布采用现有单实例排空与替换流程，不宣称零停机。切换前锁定原PID、unit/drop-ins、环境、schema、产物；失败会恢复原服务产物，不恢复或覆盖用户数据库。本次切换成功，没有触发回滚。

最终只读核验导入发布器时生成了Python字节码缓存，已删除本次核验产生的确切文件并改为 `python3 -B`；随后60个包文件和无额外文件检查通过。此核验过程未改变业务程序或数据。

性能报告中的数字仍是隔离合成测试，不能当成上线后真实生产延迟提升测量；本次线上请求用于功能和版本确认，没有对业务施加压力测试。

## 证据与回滚入口

- [发布汇总](../.local/system-production-20260927/deployment-report.json)
- [完整验收绑定](../.local/system-production-20260927/acceptance.json)
- [最终运行核验](../.local/system-production-20260927/production-final.json)
- [切换结果](../.local/powerpoint-release/d0eb70f7629951fc8d87097b82c03e1ebcde6fda83e4dfb6991bd78df81146f4/activate.json)、[发布状态](../.local/powerpoint-release/d0eb70f7629951fc8d87097b82c03e1ebcde6fda83e4dfb6991bd78df81146f4/status.json)
- [线上推理检查](../.local/powerpoint-release/d0eb70f7629951fc8d87097b82c03e1ebcde6fda83e4dfb6991bd78df81146f4/production-smoke.jsonl)
- [管理接口复测](../.local/system-production-20260927/after-management.json)
- [公网浏览器报告](../.local/system-production-20260927/public-browser/report.json)、[截图](../.local/system-production-20260927/public-browser/login-desktop.png)
- [性能实测报告](SYSTEM-PERFORMANCE-2026-09-27.md)

仅在需要恢复旧版本时运行（本次未执行）：

```powershell
python tools/powerpoint_release.py rollback --release .local/powerpoint-release/d0eb70f7629951fc8d87097b82c03e1ebcde6fda83e4dfb6991bd78df81146f4/package/manifest.json
```

回滚目标为本次preflight锁定的旧发布 `26f88f12bbb9ef7627e6bfe4e3f06653b21c3206503c28ce0fd600b13ef21edc`。如后续其他部署已改变配置，发布器会拒绝覆盖，需先检查最新运行状态。
