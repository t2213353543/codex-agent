# 被动日志采集联合验收

本轮响应用户明确授权：“可以增加被动的数据采集，用于日志观测。主动观测我将会自行解决。”新增采集只发生于已有请求处理路径，不新增主动探测、自动推理、上游测试、重放或定时检查，不引入计费。

## 完成的链路

- 图像输入/输出Token：读取上游明确整数及其来源；nil与0分开，不从图片内容推算，不存图片URL/base64，不额外累加到原业务总Token。聚合整数溢出与浏览器精度超限均明确显示，未知不伪零。
- 客户端关联：只读取唯一且合规的已有`X-Client-Request-ID`，HTTP按请求、WebSocket按连接标识来源；不生成、不转发额外头、不回写响应，独立于上游RequestID/ResponseID。查询、快照、统计、脱敏导出与清理支持独立精确匹配。
- 授权与响应阶段：观测现有授权调用和实际响应处理边界。WebSocket按轮独立，未开始接收时未知；不会把首轮结果复制到后续轮，不移动既有记录、释放和重试顺序。各阶段重叠说明保留。
- 准入前拒绝：独立有界队列及加密管理文档，新增只读管理列表。401、方法/端点拒绝、设置失败及关闭WS等固定早期拒绝不进业务日志、usage_events、请求成功率或Token；存储慢/失败和队列满不阻塞拒绝路径，覆盖缺口可见。
- 前端接入以上真实字段、样本覆盖、来源与范围；准入窗口只GET管理API，详情没有探测或重放按钮。CSVv3现71列，仅业务观测字段，准入事件分开查询。

精确字段、时序和边界见 [被动观测契约](MANAGEMENT-PASSIVE-OBSERVATION-API.md)、[图像用量](MANAGEMENT-PASSIVE-IMAGE-2026-09-18.md)、[准入事件](MANAGEMENT-INGRESS-EVENTS-API.md)。

## 原行为验证

`TestGatewayPassiveHTTPWireAndTiming`、图像HTTP测试覆盖SSE、非流式收集和compact；WS测试覆盖多轮和来源隔离。断言实际上游请求次数、响应字节、原Token与准入释放保持原行为，不仅验证helper值。

`TestGatewayPassiveIngressDoesNotChangeWireOrUsage` 对比采集关闭/开启的5类早期拒绝，HTTP码、正文、响应头逐项相等，上游调用0，业务记录0；原始路径/query/body/Cookie/Authorization均不进入观测。

`TestManagedPassiveRuntimeToQuery` 和PG版本实际执行本地合成上游成功请求与一条本地无效Key拒绝，验证真实采集→持久化→详情/统计/导出，以及独立准入查询；业务表只增加成功请求一条。所有这些请求仅本机合成服务，不访问真实上游。

## 固定版本

- Go：`d52eda2accb3af36ea09269c7385b1875b4c50e709d19a132995dc24c76aad2d`，254文件。
- 前端manifest：`15aee9e0b8008d0a32e44b9c0e029504124a2f63d9a09bad5574ce22d2e98718`。
- 前端资源集合：`22bc3735626dd8ce5dcd8c79e3c05143fdf895cc8897505f5fe6f1ef8b7b5c38`，54文件；相对既有本地恢复基线的48个框架资源不变。
- 账号入口 `accounts-managed-DbyyGTOP.js`，日志入口 `logs-managed-BWnE9MOu.js`。前端目录 `.local/management-parity/passive-observations-20260918`。

## 验证结果

| 范围 | 结果 |
| --- | --- |
| 独立管理/新增被动采集Linux PG16/Redis7 race | 131顶层、373事件通过，0失败/跳过/缺失；源码未变、临时资源清理完成 |
| 相同Go网关既有回归+管理联合race | 440顶层、1053事件通过，0失败/跳过/缺失；含取消、会话、重试、授权、关停与新增观测 |
| 完整Go测试 | 498顶层通过、121环境依赖跳过、0失败；含子测试1292通过、137跳过 |
| vet、Windows/Linux构建 | 全部通过 |
| 前端单元 | 161通过，0失败/跳过 |
| 浏览器回归 | Accounts55、Logs54通过，页面异常0 |
| 视觉 | 账号48张、日志226张，覆盖四主题/320/390/1440；准入丢弃与未确认计数、事件详情底部另补截图 |

管理及完整构建证据 `.local/management-parity/local-race-20260918t083846-425102`；联合网关race为 `.local/management-parity/local-race-20260918t084201-027055`。联合选择器沿用既有网关fixture范围，不运行需要独立真实native程序/CLI账户配置的测试；这些环境依赖不被声称已验。固定包索引见 [机器报告](MANAGEMENT-PASSIVE-OBSERVATION-VERIFICATION-2026-09-18.json)。

可在同一冻结源码上复验联合回归：

```powershell
python tools/passive_observation_race.py --expected-go-hash d52eda2accb3af36ea09269c7385b1875b4c50e709d19a132995dc24c76aad2d --source-dir .local/management-parity/local-race-20260918t083846-425102/source
```

使用已有本机Docker镜像、隔离临时数据库和内部网络，无镜像下载、真实账号、部署或公网推理。

## 边界与状态

本机真实预览：`http://127.0.0.1:52636/#/login`，固定相同Go/前端，用独立PG/Redis保留3条明确合成业务记录和1条实际本机无效Key请求产生的准入事件。真实Cookie/API已验证关联ID筛选、图像/阶段0与nil、71列CSV、采集器configured/running=true；准入拒绝后业务仍为3条/12Token/0账号，没有上游请求。证据 `.local/management-parity/passive-preview/api-verification.json`、`synthetic-passive-api.json`，与合成上游E2E证据分开，不把注入示例称为真实图片推理。root独立检查ready200。

真实浏览器另完成14项检查、28张桌面/手机截图，页面、API、静态资源错误均为0，没有额外外网请求或非预期写入。准入错误手机页已目视确认。结果存于同预览目录的`browser-verification.json`，是固定源码包外的独立补充证据。所有先前本地预览与数据保留。

本轮按用户授权修改了网关的被动观测钩子及服务生命周期，不能写成“没有改动网关文件”；路由、重试、请求/响应内容和业务计量的保持由上述回归证明。共享快照另继承其他任务在native transport/CLI测试的变更，管理报告不代替其独立发布认证。

未执行生产部署，未改变已有本地预览或数据。旧记录不回填，没有真实来源的图像尺寸/张数、DNS/TLS等明细仍保持未知；新关联不会自动新增返回头。全参考运行时恢复、每账号协议配置、Spark/Agent签名等主动或行为改变能力未在本次授权中实现，不因此宣称整个历史总目标完全完成。
