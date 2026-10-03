# 08 · 部署环境导致的 TLS 后端差异

2026-09-24。已确认一处源码一致但运行行为不一致的部署缺陷：旧 `deploy/full_release_remote.py` 在未配置时自动写入 `SSL_CERT_FILE=/etc/pki/tls/certs/ca-bundle.crt`。官方 Codex 0.155.0 把任何被选中的非空 `CODEX_CA_CERTIFICATE` / `SSL_CERT_FILE` 当作 custom CA，并强制 HTTP 使用 rustls。系统 CA 文件也触发这个分支。因此，旧部署自动改变了默认 Linux HTTP TLS 后端及实测 HTTP 协议选择。

这项缺陷已修复在部署源码中；主任务另执行了有来源核验的生产环境迁移。这里记录传输证据，不把它称为账号标记解除、模型集群恢复或糖果题质量恢复。

## 来源与旧部署动机

- 旧快照 `.local/full-release-20260919/package/deploy/full_release_remote.py:42` 的 `setdefault` 是注入来源；注释称目的是使用系统 CA bundle。它随后只校验文件存在、最终目标及 DynamicUser 可读性。
- `.local/full-release-20260919/production-before.json` 只记录 `system_ca_exists: true`；2026-09-19 发布报告也只证明系统 CA 路径与读取权限通过，没有“不设置覆盖变量则系统根验证失败”的实验。现有证据不能支持这个覆盖变量当时必需。
- 本次源文件 `.local/codex-source-0.155.0/codex-rs/http-client/src/custom_ca.rs` 和 vendored 对应文件 SHA-256 都是 `c796e150be2d0f93f4ac6620eac4324cfc5168f3eae1d80146a4690acd6fc776`，关键差异不是修改这份官方代码，而是进程环境改变了它选择的分支。
- 官方 `custom_ca.rs:387-410` 按非空 `CODEX_CA_CERTIFICATE`、非空 `SSL_CERT_FILE` 的顺序选择；`:307` 对选中的 bundle 执行 `use_rustls_tls()`；没有选中时保留 reqwest builder 的默认 TLS 后端。
- 官方 `websocket-client/src/lib.rs:64-77` 使用 `ExplicitCodexTls`，由 `custom_ca.rs:245-286` 构建 rustls native roots 加 custom roots。去掉 HTTP custom CA 覆盖不会把 WebSocket 改成 OpenSSL。

## 同生产主机、同 DynamicUser 的实测

主任务提供的 `.local/quality-20260924/tls-runtime-probe.json` 与其 `tls_runtime_probe.py` 已逐项复核。探针使用实际 helper、与网关相同 UID/GID、空附加组；不发送账号凭据。它通过 helper 请求真实 `https://chatgpt.com/backend-api/codex/models?client_version=0.155.0`。401 是未认证后的应用响应，说明已完成正常证书校验和 TLS/HTTP 传输，不能解释成模型质量测试。

| 环境 | 同 UID | 实际响应 | HTTP 版本 | helper 退出 |
| --- | --- | --- | --- | --- |
| 三个 CA 环境变量均未设置 | 是 | 401，正常读到 END | HTTP/1.1 | 0，无 stderr |
| 仅 `SSL_CERT_DIR=/etc/pki/tls/certs` | 是 | 401，正常读到 END | HTTP/1.1 | 0，无 stderr |
| 旧部署 `SSL_CERT_FILE` 系统 bundle | 是 | 401，正常读到 END | HTTP/2 | 0，无 stderr |

实测 helper SHA-256 为 `f72e5c0ff8798c02f430f7aa36f74009dad40fade6111dad722390891239b556`；对应网关 SHA-256 为 `84ca77af5c1d9a49d293180f4ccf38396df2e8ed3a1108d73df0c868315985f0`。主任务在服务器核验系统 bundle 入口 `/etc/pki/tls/certs/ca-bundle.crt` 实际解析为 `/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem`，文件权限 `0444`。没有关闭证书或域名验证。当前主机默认系统根已能在生产 UID 下完成 HTTPS，因而本次迁移不必改成另一个覆盖变量。需要显式根目录的部署仍可配置 `SSL_CERT_DIR`。

`.local/quality-20260924/runtime-tls-correction.json` 记录主任务只移除已核验为自动注入的覆盖，环境摘要从 `6e99a727a34b267686eb9118e8fc52ab061c4d8a5710f3e3b96aa83fd0761f28` 变成 `661c289d520b2ecbbd01826ecb425cb9b343379b4fd9b3dd49520be08fd573cf`，`certificate_verification_disabled` 为 false。这个迁移与通用部署函数分开；通用函数不会看到熟悉的系统路径就猜测并删除用户设置。

## 部署修复

`native_environment` 现在不再注入 `CODEX_CA_CERTIFICATE`、`SSL_CERT_FILE` 或 `SSL_CERT_DIR`，重复部署也不会恢复旧默认注入。已有值逐字保留，包含空值和用户显式指定的系统 bundle。

新 release 的 `tools/full_release.py:81` 会复制当前部署脚本并将其纳入产物 manifest。历史不可变 release 内的旧脚本保留原始字节作为证据；它们不能被当成包含本次修复的新发布包重新使用，也不能直接修改而破坏已记录的产物哈希。

文件校验按官方非空优先级选择生效 custom CA：有效的 CODEX 覆盖不会因为未被选中的 SSL 文件不存在而被部署脚本拒绝；无效的 CODEX 覆盖不会悄悄退回 SSL 文件。相对路径和纯空白路径不会被 trim 或改写成不同配置。

显式 `SSL_CERT_DIR` 支持平台路径列表分隔符，检查每个绝对目录和 immediate 文件，跟随证书 hash symlink，不递归扫描子目录。目录列表中的空条目会因隐式依赖工作目录而被拒绝。校验兼顾原始链接父目录、中间链接目标及最终目标，防止只检查 `resolve()` 后遗漏 DynamicUser 无权遍历的目录。读取单个证书文件的父目录仅要求 traverse；被枚举的 CA 根目录同时要求 read/traverse。真正不可读的根不会因发布者是 root 而误通过。

这里的文件权限校验不是 PEM 解析替代品。现有 `configureGatewayNativeHTTP` 的真实 helper 启动验证在服务 readiness 前构造 HTTP 与 WebSocket TLS 配置；坏 PEM 的失败依然由官方实现判定，没有另写证书解析器，也没有加第二套 TLS 后端开关。

需要注意两个层次的空值规则：官方 custom CA 选择将空 `CODEX_CA_CERTIFICATE`/`SSL_CERT_FILE` 当未选中，但 `rustls-native-certs 0.8.3` 的 native root loader 使用 `var_os`，不会先过滤空 `SSL_CERT_FILE`/`SSL_CERT_DIR`。因此“空值不切 HTTP 后端”不等于“空值和完全删掉变量的 WebSocket 根加载效果相同”。本修复保留显式空值，没有擅自改变这个官方语义；生产纠正采用完全移除已确认的自动变量。

## 覆盖的边界和验证证据

| 边界 | 证据 |
| --- | --- |
| 未设置 custom CA 时不注入 | `test_default_trust_does_not_create_a_custom_ca_override` |
| 重复发布不会复活默认注入 | `test_redeployment_cannot_reintroduce_default_ssl_cert_file` |
| CODEX/SSL 单独空值、同时空值保留且无选择 | `test_empty_custom_ca_values_are_preserved_as_official_unset_selection` |
| CODEX 非空优先，SSL 仅作非空 fallback | CODEX 优先、无效 primary 不 fallback、空 primary fallback 三项测试 |
| 用户显式系统 CA 不被误删 | `test_explicit_system_bundle_is_preserved_not_guessed_to_be_legacy` |
| SSL_CERT_DIR 单/多目录不导致 custom CA 注入 | 多目录、缺失目录、空条目、immediate 文件权限测试 |
| symlink 目标文件、原始父目录、中间父目录权限 | 6 项真实 POSIX symlink/权限边界，包含合法 0711 父目录及循环链接 |
| 已存在但坏 PEM 确实失败 | `TestGatewayRuntimeAuditActualCAConfiguration` 的真实 helper 5 子项 |
| 同 UID 默认/native 与 custom CA 后端分支 | 当前真实站点 401 + HTTP/1.1 对 HTTP/2 实测；官方源码分支共同解释 |
| WS 根加载与 HTTP 后端不混为一个开关 | 官方 `ExplicitCodexTls` 源码与 helper 启动同时构造 HTTP/WS 配置；不以 HTTP 401 冒充 WSS 握手 |

本机 Windows 执行 `python -m unittest tools.test_full_release_remote -v`：37 项，31 通过，6 项 POSIX 真实权限测试明确跳过；`py_compile` 通过。主任务随后在 Linux 补跑相同 37 项，全部通过、无跳过，包含真实 chmod/symlink 语义；已逐项复核 `.local/quality-20260924/runtime-tls-deploy-tests-linux.log`。本次生产脚本 SHA-256 为 `8341bd4848d8bae85368f36ea368242861d2acccc1c58faa14b05e7a39191fa6`，测试文件为 `24c6b94f687c4e10ab32fa758f0984f5ae43ea83ac2465c5fec05234e34c04b0`。

坏 PEM 实测日志位于 `.local/gateway-audit-20260924/go-runtime-pool/tests.jsonl`，时间 `2026-09-24T09:02:06Z`：`existing_malformed_primary`、`existing_malformed_fallback`、`bad_primary_does_not_fall_back`、`valid_primary_ignores_bad_fallback`、`empty_primary_uses_fallback` 全部 PASS。测试使用真实 helper，失败路径要求没有挂接 native pool、没有遗留子进程或私有 socket；成功路径要求 HTTP/WS pool 同时配置成功。这个历史有效证据证明 PEM 配置处理，不证明此次线上账号质量。

复验部署规则的一条命令：

```sh
python -m unittest tools.test_full_release_remote -v
```

本专项未修改官方源码、vendored 源码、Rust helper 或模型请求内容，未改写糖果题结果。账号为何原生直连也可能回答 29、服务端是否存在质量路由变化，仍需由请求对照和模型响应证据继续分析。
