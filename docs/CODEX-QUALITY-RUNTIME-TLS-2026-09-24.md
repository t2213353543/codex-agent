# 质量调查：实际 TLS 后端与账号模型目录

2026-09-24 第三轮续查。**修复了一处已经生效的运行时 TLS 指纹偏差；未证明异常账号的错答已恢复，也未证实隐藏集群的分档机制。** 此文补充[已上线的账号隔离修复](CODEX-QUALITY-ISOLATION-DEPLOYED-2026-09-24.md)。

## 运行时偏差及实际修复

读取现网进程 `/proc/<pid>/environ` 发现 `SSL_CERT_FILE=/etc/pki/tls/certs/ca-bundle.crt`。旧 `deploy/full_release_remote.py` 会在缺少配置时自动加入这个值。官方固定版本 `custom_ca.rs` 只要选中了 `CODEX_CA_CERTIFICATE` 或 `SSL_CERT_FILE`，就调用 `use_rustls_tls()`，即使文件只是操作系统默认 CA bundle。

因此，“native Rust helper 已启用”不等于“HTTP 正在使用 CLI 默认平台 TLS”。此前现网 HTTP 实际运行的是自定义 CA 选择出来的 rustls；WS 原本就使用官方 ExplicitCodexTls，两者不可混称。

在与网关相同 UID 下启动相同 helper，向真实上游发送**不带凭据**的 GET，得到以下结果：

| 配置 | 真实结果 | 含义 |
|---|---|---|
| 不设置三个 CA 覆盖变量 | 验证 TLS 后收到 HTTP 401，HTTP/1.1，正常 END/进程退出 | 当前主机默认系统信任可用，无须强制 custom CA |
| 仅 `SSL_CERT_DIR=/etc/pki/tls/certs` | 同样 401、HTTP/1.1 | 显式目录能保持默认 HTTP backend |
| 旧部署的 `SSL_CERT_FILE` | 401、HTTP/2 | 与源码的 rustls 分支和 ALPN 行为一致 |

HTTP 401 在这里是预期的未认证响应，只证明连接与正常证书验证通过，不能证明账号或模型推理。HTTP 版本是观测，TLS backend 结论同时依赖固定源码选择逻辑与实际进程环境。

已经在静默窗口移除现网这条部署默认 CA 文件覆盖，并保留其他服务配置；`CODEX_CA_CERTIFICATE` 和 `SSL_CERT_DIR` 没有另行设置。网关重启后进程中三个覆盖变量均不存在，readiness 正常。没有禁用证书或主机名验证，没有替换系统证书。系统 bundle 实际目标为 `/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem`，模式 `0444`。

通用部署函数已经修复：

- 不再自动注入任何 custom CA 环境变量。
- 用户显式设置保持原值，不按文件名猜测并删除；非空 `CODEX_CA_CERTIFICATE` 优先于 `SSL_CERT_FILE`。
- 校验生效 CA 文件与显式 `SSL_CERT_DIR`，覆盖目录权限、原始符号链接父路径、中间链接和最终路径。
- 原有空值保留；文档明确 HTTP custom CA 选择把空值当未设置，但 rustls-native-certs 对空环境路径有独立语义，不能笼统等同“变量不存在”。

新的发布准备会复制修正后的脚本；旧的不可变发布包仍是历史产物，不应原地更改 hash 或当作新包重发。Linux 部署回归 **37/37 通过，零跳过**；Windows 31 通过、6 个 POSIX 权限测试明确跳过。

### 变更后的实际验收

- 现网 `gpt-5.6-sol / medium` 原糖果题：HTTP 200、完成、答案 **21**，76.692 秒。
- 现网 WS：握手 **101**，同连接两轮均成功。
- 网关二进制仍为 `84ca77af5c1d9a49d293180f4ccf38396df2e8ed3a1108d73df0c868315985f0`，helper 仍为 `f72e5c0ff8798c02f430f7aa36f74009dad40fade6111dad722390891239b556`。此轮更正了运行环境，没有重新替换这两个二进制。

正常账号改动前也常答 21，因此该条验收只证明正常请求继续工作，不能用于宣称质量因果改善。

## 三个账号的实时模型目录

选择之前已记录的 q0、q1、q4，复核当前同一身份后，用真实原生 helper 默认 TLS 分别 GET `/backend-api/codex/models?client_version=0.155.0`。每个账号使用独立 helper 进程；凭据与原始目录仅在服务器内存处理，保存匿名化标量和字段摘要，没有推理调用。

三个请求均为 HTTP 200 / HTTP/1.1，原始目录都是 522,558 字节。九个模型按相同 slug 比较，52 个字段的完整值摘要全部相同，没有某账号独有或缺失的模型；三者 AT 的计划声明均为 `self_serve_business_prolite`。

这排除了该次可见模型目录/能力配置的差异，但**目录不是实际推理执行配置，不能据此排除账号隐藏状态，也不能从相同 plan 宣称同算力集群**。比较器保留 missing/null/empty、模型重复、HTTP 错误、旧版本缓存/回退等区别，18 项离线测试通过；完整上游指令文本没有写入报告。

## 异常账号的官方 HTTP 副对照

此前 q1 在完整默认 CLI 路径遇 WS 403。本次复核它仍是原 user/workspace 与相同 AT 后，用完整官方 CLI 0.155.0 平台包运行一次 `--websocket=false` 副对照；这是带正常 OpenAI 认证的自定义 HTTP provider，不能描述成默认内置 WS/fallback。

题面、model 和 medium 档位保持，未提供 21。该运行无启动告警、无工具事件、模型目录载入正常，但回合返回明确限流错误，1.105 秒退出，没有最终答案。HTTP 副对照不自动重试；相应错误不计为数学错答或恢复成功。临时 AT 副本与 CLI auth/home 已清理，没有修改 28765 推送地点或触发 OAuth。

当前已复现的事实依然是：多个账号可以在网关外答 29，隔离后的网关也可能答 29；服务端仍有独立的 403/429/模型容量失败。**不能通过这一轮 TLS 环境修复宣布 q1/q4 恢复“满血”。** 可验证的源码/部署问题继续修复，隐藏服务端机制没有被猜测性 header、改题面或筛选正确结果替代。

## 证据与复验

- 详细源码/权限边界：[Q08](quality-20260924/08-runtime-tls.md)。
- 目录/身份/缓存规则：[Q09](quality-20260924/09-account-catalog.md)。
- `.local/quality-20260924/tls-runtime-probe.json`、`runtime-tls-correction.json`、`tls-final-runtime.json`。
- `.local/quality-20260924/catalog-report.json`：脱敏后的三账号完整目录比较。
- `.local/quality-20260924/tls-candy/`、`tls-ws/`、`clean-http-q1/`：实际请求结果。
- `.local/quality-20260924/runtime-tls-deploy-tests-linux.log`：37 项 Linux 测试。

离线复验不发送模型请求：

```powershell
python -m unittest discover -s tools -p test_full_release_remote.py
python -m unittest discover -s tools -p test_quality_catalog_compare.py
```
