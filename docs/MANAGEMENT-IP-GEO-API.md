# 使用记录 IP 归属地手动查询

更新：2026-09-18。能力用于管理界面手动查询已显示 IP 的大致归属地，不读取账号凭据，不执行推理，也不改变网关采集、转发或日志。

## 来源与主动触发

已核对 [GeoJS 官方 Geo 文档](https://www.geojs.io/docs/v1/endpoints/geo/)：固定 HTTPS Geo 接口接受 `ip` 查询参数，多 IP 返回数组，字段包含国家、省州、城市、所属组织及时区。组织字段优先使用官方当前的 `organization_name`，兼容旧 `organization`；官方返回 Unknown 时界面显示未提供。[官方 Country 文档](https://www.geojs.io/docs/v1/endpoints/country/) 给出了逗号分隔批量参数。本站每批 50 个是管理接口限制，不冒称第三方官方上限。

单条组件 `LogIpGeo` 在用户点击“通过 GeoJS 查询归属地”后才请求。批量组件 `LogIpGeoBatch` 只使用当前已显示页面的 IP，先预览数量与排除统计，点击第二个明确的 GeoJS 查询按钮后才开始。页面加载、组件挂载、输入变化、刷新列表或查看详情都不会自动查询；IP 或当前页变化会取消旧请求、丢弃旧结果。

唯一向 GeoJS 发送的用户数据是本次选定、校验通过的公网 IP 列表；不会转发管理员 Cookie、账户令牌、账号名、密钥、请求正文、错误文本或其他日志字段。来源服务由后端固定，用户不能提供 URL。

## 管理接口

`POST /api/logs/ip-geo` 要求现有管理员认证，继续 `Cache-Control: no-store`。请求为：

```json
{"ips":["8.8.8.8","1.1.1.1"],"force":false}
```

示例地址仅说明格式，测试没有真实查询这些 IP。每次原始输入 1–50 个；整个批次先通过 `net/netip` 验证，拒绝主机名、URL、端口、区域标识、括号和非标准 IPv4，随后将 IPv4-mapped IPv6 还原为 IPv4，按规范 IP 去重。任何格式无效项使整个请求 400，且不产生部分外发。

私有、环回、链路本地、CGNAT、测试文档、基准测试、组播、保留及协议特用地址不会外发，返回 `status: private`。IPv6 仅允许原生全球单播 `2000::/3`，继续排除协议特用、文档和 6to4 段。该判断是保守的数据外发策略，不代表允许的每个 IP 当前真实可路由。段依据 [IANA IPv4 特用地址登记](https://www.iana.org/assignments/iana-ipv4-special-registry/) 与 [IPv6 特用地址登记](https://www.iana.org/assignments/iana-ipv6-special-registry/)，不使用参考前端的宽松私网正则。

返回 envelope 为 `{items, source:"GeoJS", cache_ttl_seconds:86400}`。`items` 按规范化后的首次输入顺序返回：

| 字段 | 含义 |
| --- | --- |
| `ip` | 本项规范 IP。 |
| `status` | `success`、`private` 或 `error`。 |
| `country`、`country_code`、`region`、`city`、`organization`、`timezone` | 第三方真实提供且通过长度/字符校验的结果；缺失字段不编造。 |
| `source` | 确实调用或使用其历史缓存时为 `GeoJS`；非公网项不伪造来源。 |
| `fetched_at`、`expires_at` | 成功取得响应的 Unix 秒及缓存到期时刻；失败/私网项不提供假查询时刻。 |
| `cached` | 是否直接采用未过期的服务器内存成功缓存。 |
| `error_code`、`error` | 固定脱敏错误。含 `non_public`、`not_found`、`invalid_response`、`response_too_large`、`upstream_unavailable`。 |

HTTP 200 表示批量处理完成，必须逐项读取 status；部分或全部 GeoJS 失败不能解释为成功归属地。无结果、错误 IP、重复响应、坏 JSON、越界值和不可用服务均有明确失败项，不编成“未知”成功。不缓存失败，也不在刷新失败后冒充成功使用旧结果。

## 资源边界与展示

外部请求固定 `https://get.geojs.io/v1/ip/geo.json`，拒绝跟随重定向，等待与读取共 10 秒上限，最多 256 KiB 响应，进程内最多 4 个在途请求。解析后必须严格对应本次请求 IP；字段长度上限为国家 100、地区/城市 200、组织 300、时区 100 字节，控制字符拒绝。

只有成功结果进入内存缓存，最多 1024 条，24 小时过期，重启丢弃，无磁盘存储和新增迁移。命中缓存保留原 fetched_at，不改成当前时间；显式 `force: true` 发起新查询。超过上限移除最旧记录。

单条界面支持展开详情、来源链接、查询时刻、缓存标识及明确重试。批量最多接收当前页 500 条原始记录，点击前固定去重后的公网目标，随后每 50 个顺序执行，不因翻页吸收新目标。取消后保留已完成批次事实；未完成项不标成功。归属地只表示网络地址的大致位置，不代表用户真实实时或精确位置。

## 验证

`admin_usage_geo_test.go` 采用假 RoundTripper 与内存数据，覆盖公网/特用段、映射 IPv6、整批预检、认证、无敏感头转发、缓存时间/容量/force、并发上限、错 IP/重复/缺失/超长/重定向/网络错误/取消。`frontend/tests/geo-lookup.test.js` 覆盖同类本地分类、显式同源 POST、结果对应、50 个分块、固定目标及取消。Go 专项 5 顶层 / 16 含子测试、Node 专项 7 组均通过，证据在 `.local/management-parity/ip-geo/`。

测试没有真实查询用户 IP；旧壳中的单条/批量行为由 Logs 页面 owner 的合成浏览器验收验证，是否发布以主任务最终冻结记录为准。
