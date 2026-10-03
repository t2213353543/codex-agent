# Codex Agent 功能发布记录

**2026-09-19 已被新发布取代：** 全量修改已上线为47c80b8e...，Go19c69c3a.../Linuxa673bc08...、54文件新管理UI，sealed6df Rust HTTP/WS原生均启用，WebSocket入口已打开。21项生产浏览器、健康/资源/数据核对通过，原无计费与仅被动观测约束保留。最新事实见 [全量发布](FULL-DEPLOYMENT-2026-09-19.md)，下文956/47文件为历史发布。

12:28 +08 再次现场检查通过：运行binary、健康、003 schema、47个HTTP资源、认证及数据一致性均正常，报告 `.local/postdeploy/20260918T042757.626239Z-check.json`。同步timer active、最近周期退出0，两个合格账号仍为unchanged。功能服务保持可用。总目标仍缺受控30/50/70/100实网容量与前后效果对照；该执行限制跨恢复后的多轮仍未改变，不继续重复健康/合成绿测代替完成。自然令牌轮换也未观察到，等待源实际产生新AT后再验。

后续被动错误分析见 [上线后复核](POST-RELEASE-ERRORS-2026-09-18.md)：日志来源/阶段已分开，不以少量成功请求或缺少502/429记录宣布容量目标完成。

本文件为当前发布状态；此前 `GATEWAY-RELEASE-CHECKLIST-2026-09-18.md` 的未部署/网络阻塞段落保留为历史记录。

## 已迁移、已部署

- 地址：[Codex Agent](http://43.225.196.170:18879)。2026-09-18 12:09 +08 公网 `/`、`/health/live`、`/health/ready` 均 HTTP 200；根页面字节与发布 manifest 一致。
- 发布时间：2026-09-18 12:00:54 +08。Release `35624e0cafd9f60cae88f61d772464de2f0b891638c6a950f5f0a17668ebee70`。
- Linux binary SHA256：`a66c7be3de344221d6433a6bc7140523d95a6a068fd2312757c80f2dc50c073d`。来源为固定956生产源码，不是共享工作区后续 native 或 commercial 功能。
- owner 源码 SHA256：`956fc90be91a922ac1593b283102bc5149674dac337564e6023479ec8e60a6f9`；相同文件映射的 v2 摘要为 `a1d958c3a3deab35278baa65c4c30553b117c4d9ecdd76263b80994637655abe`。
- `003_management_cleanup.sql` 已实际执行，000–002 原 checksum 不变。003 SHA256：`31ca0e1e58bb846c9691bd505718a1c13ef3aba03f9abf157034317e85cde56e`。
- 前端完全保留原 47 文件，映射摘要 `d87032e071e7c72343ccb178857d4e56a0200ea7af87aa3b262083e73d15d1c6`。未发布被撤回的新壳，也未混入后续 54 文件管理功能包。

本次发布包含有界客户端缓存/连接复用、稳定账号 ID 准入、刷新失败合并、账号与模型限流冷却、自定义超时池及被动观测；没有通过换模型或隐式重放请求掩盖错误。原生 Rust HTTP/WS 模式未启用。

## 现场验证

| 项目 | 实际结果 |
| --- | --- |
| 连接恢复 | 使用已有 Clash 香港 S01，SSH 握手后恢复用户原节点；没有启动此前被拒的独立代理进程 |
| 生产 preflight | 独立 PG/Redis 地址、受限角色、default CRUD、密钥与运行进程连续性、旧 binary/unit/UI hash 全通过 |
| 停写切换 | 先暂停本项目同步，排空旧服务，执行真实迁移，再启动候选；窗口 0.860 秒，无新旧进程同时运行 |
| 切换前后数据 | 29 账号/29 凭据、35 分组关系、28 代理关系、636 日志/636 用量事件，累计 7,339,169 tokens；身份与凭据摘要完全相等 |
| 实际重启 | PID 3745056 → 3751665，正常重启；严格对账身份、配置、关系、凭据版本、日志/累计统计全部保留 |
| 管理接口 | 无认证/错密钥拒绝，正确 Bearer/cookie 登录、查询、注销通过，HttpOnly + SameSite Strict；数据库与概览统计一致 |
| 生产数据库写入 | 禁用合成账号9项实际通过：创建、metadata CAS、同身份合成AT替换、旧version409且PG不变、API删除及PG消失。测试账号已清理，没有启用/刷新/推理，凭据版本1→1→2 |
| 静态资源 | 47 文件逐个磁盘与 HTTP SHA 校验通过 |
| 生产网页 | 17 项通过：账号/概览/日志/设置、桌面1440/手机390、四主题、会话恢复/退出、无脚本异常或资源错误 |
| 浏览器路径 | 真实生产服务经短期 SSH 转发验证；另做公网无认证健康/首页验证，不冒充公网已认证页面全测 |
| 同步服务 | timer 已恢复 active，初次周期退出0；观察3个正常周期，当前2个符合资格账号均 unchanged，无错误、转发 RT=0 |

同步来源持续变化，29 个保留账号不等于当前有29个可用Pro。资格失效账号由同步禁用，保留历史；现网旧的手工账号未被本次替换。观察窗口没有发生自然 AT 变化，**自然轮换未观察到**；跨语言和真实 PG 上的凭据 CAS 已有合成验收，不据此补填源系统轮换结果。

12:17 +08 的被动聚合读取显示，切换后已有125条 `status=ok/HTTP200`、2条401、1条400和1条HTTP200但流内失败。这些请求并非本次工具产生，流量条件未控制，不能计算为四档结果、不能据此认定上线降低429或模型能力变化。未导出内容或真实账号字段，证据为 `passive-after-deploy.json`。

## 测试范围

固定956的 Linux PG18/Redis8 网关159项、管理73项通过；剩余PG17中的旧shutdown夹具初始化问题已由源码owner正式修复。独立核验仅测试文件变化，生产输入完全不变，正式Linux-race1项与旧16项观察到的通过组成17项覆盖，未改写旧失败报告。

真实隔离回退6项、候选+旧前端实际API浏览器13项通过。1000禁用合成账号/50000日志下，600次串行管理请求全部成功：列表P95约31ms，日志P95约42ms，缓存概览P95约27ms，未缓存范围概览P95约89ms。这里只是本机管理负载，不是上游容量。

统一本地验收命令：

```powershell
python tools/gateway_acceptance.py --release-config docs/GATEWAY-ACCEPTANCE-INPUTS-2026-09-18.json
```

此入口仍如实返回高并发容量/显式选跑等未完成的总目标门禁。功能发布凭证由 `tools/gateway_release_acceptance.py` 独立校验产生，仅接受明确的功能范围，不能让真实压力未执行变成通过。

## 回退

旧应用与前端保留在 `/opt/codex-agent/releases/4d4c4dfd91de306b6d1f79bfba232df49c85d3a1dfdda06bbf199538a7322e38`，旧binary `a9bf68d1899ec1d58381c788b84cc63123c88cb8196d1acb0b4faafa1b65831d`。私有PG备份、旧unit及配置留在新release的 `.upgrade-private`，权限受限。

在本项目目录中执行：

```powershell
$env:CODEX_AGENT_SSH_PROXY='http://127.0.0.1:7897'
$env:CODEX_AGENT_CLASH_REGION='hk'
python tools/workbench_bridge.py stop
python tools/upgrade.py rollback --release .local/upgrades/35624e0cafd9f60cae88f61d772464de2f0b891638c6a950f5f0a17668ebee70/upgrade-manifest.json
```

回退先停止候选，再恢复准确旧 unit/binary，保留最新 PG 和新增003表；不会用旧dump覆盖新令牌/日志。旧版缺少稳定ID重授权接口，因此回退后同步服务应保持停止，直至兼容接口重新就绪。当前没有执行这次生产回退，隔离库已验证旧binary可运行在003后的schema。

## 证据与限制

发布证据根目录：`.local/upgrades/35624e0cafd9f60cae88f61d772464de2f0b891638c6a950f5f0a17668ebee70/`，含 manifest、preflight、activate、切换前后数据摘要、重启及严格对账、47资源/认证/数据检查、公网健康、同步观察。生产浏览器证据在 `.local/production-browser/20260918T040536.461993Z/`；未保存认证页面截图或真实账号内容。

- **功能测试通过、003已迁移、稳定后端已部署**是三个分别有证据的状态。
- **30/50/70/100真实上游并发仍未执行**，此前启动动作受平台限制；没有伪造成功率/P50/P95，也没有宣称本次发布已经降低生产上游错误率。
- 自然AT轮换在当前观察窗口未发生，不宣称持续轮换现场验收已完成。
- 地址沿用既有HTTP，session `Secure=false` 与该地址匹配；本次未配置HTTPS、未变更管理密钥或加密密钥。
- 未修改 Sub2API、accounts、28765；18876仅经只读管理接口提供合格账号，未改支付/注册流程。
