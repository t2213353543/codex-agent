# Sub2API / CPA 账号管理与同步接口

本系统在原生 `/api` 外提供两套兼容入口，操作同一个账号库。现有同步客户端可把服务器地址改为本系统，使用本系统的管理员密钥，直接导入、更新、读取、停用和删除 OpenAI OAuth/Codex 账号；两套协议及原生控制台立即看到同一份状态，不需要独立 CPA 桥接进程。

这是账号同步协议兼容，不是把 Sub2API 的用户、订阅、计费或 CPA 的其他模型提供商移植到本系统；也不会自动连接远端 Sub2API/CPA 实例开展后台双向复制。没有相应执行能力的字段返回错误，避免请求显示成功而运行配置未生效。

## 连接方式

服务器根地址：`http://43.225.196.170:18879`，客户端配置的根地址不带管理路径。

| 客户端 | 管理路径 | 管理员认证 |
| --- | --- | --- |
| Sub2API 同步客户端 | `/api/v1/admin` | `x-api-key: <本系统 Admin Key>` |
| CPA / CPA Manager | `/v0/management` | `Authorization: Bearer <本系统 Admin Key>` 或 `X-Management-Key` |
| 原生接口 | `/api` | 原有 Bearer / 浏览器 session |

兼容入口接受本系统 Admin Key 的 Bearer 形式，不实现 Sub2API 用户密码登录/JWT 签发。Bearer 与另一认证头同时出现时必须相同；重复/空/冲突认证头会拒绝。网关普通 API Key 和浏览器 cookie 不取得兼容管理权限。所有响应使用 `Cache-Control: no-store`。

## 已实现的 32 个方法/路径

Sub2API 统一成功响应为 `{"code":0,"message":"success","data":...}`；列表分页返回 `items,total,page,page_size,pages`，`page_size` 最大 1000。

| 方法 | Sub2API 路径（省略 `/api/v1/admin`） | 实际行为 |
| --- | --- | --- |
| GET / POST | `/accounts` | 分页/搜索/筛选；创建 OpenAI OAuth 账号 |
| GET / PUT / DELETE | `/accounts/{id}` | 稳定数字 ID；读取、更新凭据和配置、删除 |
| GET / POST | `/accounts/data` | 明文凭据显式导出；portable data 账号/代理导入及逐项结果 |
| POST | `/accounts/{id}/schedulable` | 暂停/恢复实际账号调度 |
| POST | `/accounts/{id}/clear-error` | 清除当前错误和刷新失败缓存 |
| POST | `/accounts/{id}/refresh` | 调用已有 OAuth 刷新流程，持久化实际返回的令牌 |
| POST | `/accounts/{id}/test` | 账号专属真实 SSE 测试，结束事件 `test_complete.success` |
| POST | `/accounts/{id}/set-privacy` | 实际上游隐私设置确认后更新观察记录 |
| GET / POST | `/groups`、`/proxies` | 资源列表与创建 |
| GET | `/groups/all`、`/proxies/all` | 可供同步器使用的数字 ID 资源数组 |
| GET / PUT / DELETE | `/groups/{id}`、`/proxies/{id}` | 原生资源读取、更新及关联删除语义 |

CPA 返回本身的 `{files:[]}`、`{status:"ok"}` 或 `{error:"..."}` 格式。

| 方法 | CPA 路径（省略 `/v0/management`） | 实际行为 |
| --- | --- | --- |
| GET | `/auth-files` | 账号元数据列表；name/auth_index 组合筛选 |
| POST | `/auth-files` | multipart 单/多文件，或 raw JSON + `?name=x.json`；部分成功207 |
| DELETE | `/auth-files` | name / names / all 选择；真实删除，不伪装为停用 |
| GET | `/auth-files/download` | 显式下载当前凭据 JSON |
| PATCH | `/auth-files/status` | 真实 disabled 状态，可同时核对 auth_index |
| PATCH | `/auth-files/fields` | 支持的管理字段、凭据和普通扩展元数据更新 |
| GET | `/auth-files/models` | 使用指定账号读取实际模型目录 |
| POST | `/api-call` | `auth_index`/`authIndex`、`ensureFreshToken`、`$TOKEN$`替换；返回真实 `status_code,header,body` |

## 一致性与差异

- 原生账号 ID 保留；Sub2API ID 由原生 ID 确定性投影为正安全整数，CPA `auth_index` 为稳定摘要。改名、换令牌及重启不改变身份映射；检测到歧义会拒绝。
- CPA 文件名映射独立于账号展示名。管理写入、凭据、文件映射、兼容元数据及租期在生产 PostgreSQL 中原子提交；版本冲突、写入失败不会只更新其中一部分。
- 两边下载始终使用当前 AT/RT/account ID/过期时间，原始 ID token 在两种协议间同步保存，不生成伪造 JWT。普通列表使用白名单字段与令牌脱敏；只有显式导出返回完整凭据。扩展凭据元数据在 PG 使用现有加密信封保存。
- Sub2API 优先级小值先，原生/CPA 大值先，兼容层按相反符号投影。跨不同实现复制时，应理解其排序方向和字段范围；不能把未经转换的数字当成同一优先顺序。
- Sub2API 外层 `expires_at` 是账号租期，内部凭据 `expires_at` 是 Token 到期时间。租期写入已有调度机制，到期实际停用，不把两者混用。
- `proxy_id:0` 清除 Sub2API 代理绑定；缺省/null 保持原值。已有 Loophub 某版本用 null 表示清空，与其目标官方实现不一致，本系统不把该客户端差异隐式改成另一种协议。
- CPA `proxy_url` 必须匹配本系统已配置并启用的代理；直接 URL 不会静默创建副资源。Sub2API portable data 支持代理导入和 `proxy_key` 映射。普通代理列表不回显密码。
- Sub2API `status` / `schedulable` 最终映射原生实际 enabled 状态；CPA或原生改动后读取不会继续报告与实际可调度状态相反的旧值。
- CPA websockets 只能接受与全局实际设置一致的值；不支持单账号开关覆盖。生产全局 WebSocket 已启用，可接收同步客户端 `default_websockets=true`。
- 非 OpenAI OAuth、模型映射、独立模型白名单、自定义网关请求头、SaaS计费/订阅、HTTPS代理等未实现能力明确拒绝。OAuth登录路由及其他未注册管理路径返回404，不伪报通过。
- 文件兼容模式沿用原生文件持久化和资源删除限制；不同资源文件之间不具有 PG 外键事务语义。生产使用 PostgreSQL，关系存在性由实际外键保证。

## 验证与来源

协议对照来自本地 Sub2API、CPA Pro/Manager 源码和实际 Loophub 同步客户端。测试直接加载未改写的 exporter，使用其真实 `curl_cffi` HTTP/multipart 实现访问隔离服务，覆盖11条创建、更新、凭据跨协议读取、暂停恢复、真实假上游SSE/隐私及删除流程。

新增回归还覆盖双认证头冲突、未知字段/重复JSON、稳定身份、乐观锁、故障注入回滚、凭据密文、租期、关系约束、多文件207、真实api-call状态和取消边界。最终执行结果、发布身份与生产验证见[同日发布记录](ADMIN-COMPATIBILITY-RELEASE-2026-09-27.md)。

```powershell
go test ./internal/server -run 'Test(AdminCompat|ManagedCompat|Sub2Compat|CPACompat)' -count=1
```

配置独立 `TEST_DATABASE_URL` 后执行对应 `TestPG...`；真实客户端测试需要本地 Loophub exporter 和 curl_cffi，缺失时明确跳过。
