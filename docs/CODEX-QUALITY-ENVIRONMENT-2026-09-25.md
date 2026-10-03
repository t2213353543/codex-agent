# 真实运行环境与官方 CLI 身份修正

**后续上线与环境控制：** 实际组合版本 `a4777428…e20136a` 已包含[Lite 字段缺失语义修复](CODEX-GATEWAY-LITE-INSTRUCTIONS-2026-09-25.md)，本任务的等待发布在版本变化时正常停止。已完成真实 HTTPS 的同凭据 Cookie 生命周期工具验证，并执行[先行 GET／helper 三组对照的七格前缀](CODEX-QUALITY-COOKIE-CONTROL-2026-09-25.md)；因 q0 被停用没有补齐，且其窗口有其他生产流量。当前不能把预热、Cookie 或答 29 与私有算力分配直接建立因果。

**2026-09-25 后续上线更新：** 当前后端 `bb92216f…aca277ad` 已完成生产验收。导入识别官方 `user_id` fallback；刷新按当前 AT/ID 重建身份、保存旋转令牌并阻断旧身份会话；WS 连续链保持缓存键；HTTP 改键后超限在绑定前拒绝。最终 Linux 946、Windows 822 顶级通过，零失败，另有完整官方 CLI 7 项通过。八账号目录 28 对相同、官方外部令牌本地装载均成功；原 24 次答题结果保持，质量恢复仍未证明。[完整修复与验收](CODEX-GATEWAY-IDENTITY-REFRESH-2026-09-25.md)。下文旧版本号及测试数字按各自阶段保留。

本轮按用户要求优先检查运行环境、设备与指纹，再研究外部降智案例。以实际进程、固定官方 CLI 和请求发送边界为依据；不把账号间正常的会话 ID 差异当成请求损坏，也不将环境矛盾直接等同于质量问题已找到因果。

## 12:25 更新：配置写入校验与八账号实测

现已部署后端 `020fa045f6da83d6c6c0c18ded845473c35b4cfd9c11b31aee09832dd82065e1`。此前 runtime resolver 会拒绝与真实平台冲突的 enabled profile，但管理 API 仍能先保存该无效配置，导致后续请求失败。本次在 settings POST 合并配置后、持久化前调用同一 effective profile resolver：冲突立即返回 400，数据库、缓存和设置版本不变。已有无效配置仍能读取、禁用或改正；正常 CAS 冲突继续返回 409。

该问题先由回归测试复现，再修复。最终源码快照的 Windows 全套为 **797 通过、0 失败、148 跳过**；Linux race/PG/Redis/native 全套为 **921 通过、0 失败、23 opt-in 跳过**，无 DATA RACE，均通过 vet。构建使用这些测试对应的独立快照，仅将 `admin.go` 的校验补丁加入上一版生产代码；未将共享工作区其他并行变更带入发布。源码和完整测试关联见[构建清单](../.local/research-20260925/profile-admin-validation/build-manifest.json)。

北京时间 12:25:01，[生产验收](../.local/research-20260925/profile-admin-validation/runtime-smoke.json)实际确认跨平台 enabled profile POST 返回 400、配置版本保持，HTTP models 200、WS 101/正常关闭、usage 200 往返、health ready、in_flight=0。此次 smoke 不含模型 turn；原生 helper、真实 CentOS identity、TLS 环境及八账号元数据保持。

另已完成[八账号 24 次质量测试](CODEX-QUALITY-EIGHT-ACCOUNTS-2026-09-25.md)，包括绕过 Go 网关的固定请求及完整官方 CLI 直连；人工为 3 次 21、21 次 29。质量测试在本次管理校验补丁发布前完成，推理代码与 helper 不因该补丁改变。这一修复解决配置导致请求失败的问题，没有证明答题质量恢复。新增七账号历史注册/登录来源仍未知；当前环境一致性不能代替历史环境证据。以下各节保留先前阶段的部署与测试记录。

## 已发现并纠正的实际配置矛盾

2026-09-25 北京时间 04:33:17 检查生产进程，后端 `161bc6157580b950a4847e9a492174866dadf2fbef8d7842e7773ea57f888126`，原生 helper `f72e5c0ff8798c02f430f7aa36f74009dad40fade6111dad722390891239b556`。

| 维度 | 现场证据 |
| --- | --- |
| 操作系统 / 架构 | `/etc/os-release` 是 CentOS Stream 9，x86_64；内核 `5.14.0-539.el9.x86_64`。官方 UA 的 OS version 来自 os_info，不应当成 kernel version。 |
| 终端 | 服务进程没有 TERM、TERM_PROGRAM、TERM_PROGRAM_VERSION；不是交互式终端。 |
| 原生传输 | HTTP 生命周期 `cli_request`；native WS 开启；没有 SSL_CERT_FILE、SSL_CERT_DIR、CODEX_CA_CERTIFICATE。 |
| 出口配置 | 服务没有大小写 HTTP_PROXY / HTTPS_PROXY / ALL_PROXY / NO_PROXY；生产 Q0 没有绑定账号代理。此项不证明上游如何识别外部网络。 |
| 实际配置 | `codex_fingerprint_enabled=true`、profile=linux，将来源客户端 UA 覆盖成硬编码 `codex_cli_rs/0.155.0 (Linux 6.8.0; x86_64) unknown`。 |
| 确认偏差 | 硬编码值不是本机官方 CLI 的实际环境值；覆盖也会丢弃合法客户端的 originator、终端与初始化 suffix。源码此前将它标为参考模板，但实际启用仍与本轮真实环境要求冲突。 |

北京时间 04:38:41，已通过管理 API 的 settings CAS 关闭此模板，设置版本 3→4。前后只改变 fingerprint enabled 和设置版本，官方环境 UA 发起 `/v1/models` 返回 200 / 9 项，health ready，没有发模型推理。保留正常 CLI 传入的 UA/originator/version，安装、会话、窗口 ID 按客户端事实透传，不用伪造设备编号替代。

证据：`.local/research-20260925/environment-fidelity/{runtime,template-disabled}.json`。这是已执行的配置修复，不是未执行建议。

随后又发现并修复了一个残留边界：即使模板默认关闭，管理员重新开启 `codex_fingerprint_enabled` 时旧代码仍可能选择硬编码 Linux/macOS profile。现在只要启动时加载了 verified runtime identity，启用 profile 必须匹配真实平台并复用 capture 的 UA/originator/version；跨平台 profile 直接拒绝，不再回退到伪造模板。runtime resolver、管理端 profile 列表、账号 runtime 诊断和 models query 已统一使用同一 effective snapshot。该补丁已上线，网关 SHA `f9e56bb2d040a574b3703b4c474ea2f89badaabc16e70f63eab14c61d87e4687`，focused fingerprint/runtime tests 16 pass、0 fail、1 个明确 CLI opt-in skip，运行时 HTTP/WS/usage smoke 通过。

## 官方运行身份来自实际进程

同机使用生产 UID/GID 61787，在独立无网络 namespace 中执行固定官方 CLI 0.155.0，仅发送 app-server `initialize`。官方二进制静态及 `/proc/<pid>/exe` SHA 均为 `660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`。进程正常退出，没有模型、OAuth 或远端连接。

初始化 client 名称使用官方源码列为 NON_ORIGINATING 的 `codex-backend`，因此不改默认 originator 或新增 suffix；直接读取 `get_codex_user_agent()` 的结果：

```text
codex_cli_rs/0.155.0 (CentOS 9.0.0; x86_64) unknown
```

这不是将 uname 结果手工拼入 UA，也不是预设 CentOS 字符串。默认库身份与普通 exec 的 `codex_exec/... (codex_exec; 0.155.0)` 是不同的合法初始化路径：前者供网关自产请求使用，后者供入站 exec 保留。详细源码在 `.local/codex-source-0.155.0/codex-rs/login/src/auth/default_client.rs:164` 和 `app-server/src/request_processors/initialize_processor.rs:18`、`:125`、`:179`。捕获记录为 `official-runtime-identity.json`。

已补齐官方源码使用的全部终端环境变量后再次离线采集：20 个终端相关变量在实际 service env 中全部不存在，UA 相同；初次记录按 SHA 归档。这排除了采集白名单缺失导致假 `unknown` 的解释。

网关自产 models/usage、非 CLI 请求和 OAuth refresh 已接入这份真实身份。配置在启动时检查严格 JSON、版本、平台、源 CLI 文件 SHA，并发布不可变的 UA/originator 对；每次请求不读文件或启动子进程。OAuth code exchange 的 raw client 仍按固定官方源码发送原始认证请求。操作系统发行版或终端环境改变时需重新捕获，本机 OS/arch 一致和文件 SHA 本身不是任意 UA 来源的密码学证明。

## 新后端已上线

北京时间 **2026-09-25 04:55:34** 完成生产验收，后端 SHA256 `9468c12bdaf556e4f36492e56d202c5d0e7e5838e989edc14213972e17646de3`。原生 helper 及 TLS 环境保持，服务只新增 `CODEX_GATEWAY_IDENTITY_FILE`，指向 root-owned 只读的实际 CLI capture；来源 CLI 以固定 SHA 的只读副本保留。

| 验证 | 结果 |
| --- | --- |
| Linux 完整 race/PG/Redis/native | 912 个顶级通过，0 失败，23 个 opt-in 跳过；2680 个含子项终态，333.921 秒，无 DATA RACE，vet 通过。 |
| Windows 完整测试 | 790 顶级通过，0 失败，146 个条件不满足跳过；2548 个含子项终态，41.492 秒，vet 通过。 |
| 追加实际 Rust HTTP/WS TLS 身份抓取 | 1 个顶级、3 个含子项通过，0 跳过；fallback/native 各 HTTP、WS、管理 GET，共六条实际请求验证 UA/来源、attestation 与 metadata。测试还检查无 Go 传输兜底，race/vet 通过。 |
| 完整官方 CLI 链路 | 7 项通过，0 跳过；13 个含子项终态，14.840 秒；HTTP、WS、工具、连续交互链路检查通过。 |
| 生产验收 | HTTP models 200/9项、WS 101及正常关闭、management usage 200往返一致、in_flight=0、health=ready；没有模型 turn。 |

完整快照先冻结生产代码，再添加独立 native wire 测试；三个 Linux 批次的生产文件 hash 一致，没有把追加测试写成已在先前全套里执行。[构建清单](../.local/research-20260925/environment-fidelity/build-manifest.json)、[激活记录](../.local/research-20260925/environment-fidelity/activation-result.json)和[运行验收](../.local/research-20260925/environment-fidelity/runtime-smoke.json)保留完整证据。源码实现与校验范围见[应用身份专项](quality-20260925/runtime-application-identity.md)。

## 28765 登录设备连续性修复已上线

网关环境修正后，又把 28765 登录源头的确定性设备状态缺陷纳入生产：

1. `chatgpt.com` warmup 读取到的 `oai-did` 现在写回同一 AuthFlow 结果；登录 URL 优先使用发送给 `chatgpt.com` 的同域 Cookie，不再由 stale result 另生 B。
2. `auth.openai.com` 返回的新 Cookie 仍由该域服务端决定；缺少新 Cookie 时沿用当前事务的有效 ID，不凭空切换设备。
3. OAuth 后 fresh device 只有在 workspace、email/user 身份或同一 token epoch 验证后才用于即时 usage；不再错误使用旧 child 设备或 owner 设备。所有 header、phone、Sentinel、已有凭证入口都拒绝控制字符和异常设备值。

离线红测先重现 A→B→A 设备漂移与 fresh device 错绑，绿测覆盖同域 Cookie、跨域隔离、OAuth 身份核验及异常值。带备份部署的候选 `20260925-auth-device-v1` 通过构建后切换；随后加入统一异常值守门的 `20260925-auth-device-v2` 再次切换。当前容器镜像 `sha256:2cd8ff0a…ca3ceb`，health/storage=postgres，设备连续性测试在生产容器内返回 0，未发登录、模型或账号动作；调度开关恢复，数据库未迁移，备份已保留。证据为 `.local/research-20260925/source-environment/device-v2-deploy.json`、`device-red.log`、`device-green.log`、`device-production.patch`。

这修复的是网关上游登录环境的设备身份不连贯，不是已证明的“低算力集群”开关。下一次新增账号质量实验必须同时记录同域 device cookie、出口、locale/TZ、实际 UA、请求 ID 与响应 usage，不能只看答案数字。

## 设备与会话字段的正常生命周期

[设备与会话源码核查](quality-20260925/environment-metadata.md)已确认：installation_id 是 CODEX_HOME 持久 UUID，window/context-window/root-turn 由会话与压缩窗口生命周期管理，均不是硬件序列号。官方还会有意使直接 metadata header 与 body 中完整 metadata 快照有所不同。网关不得为了表面一致而固定所有账号会话 ID、重造设备标识或把正常投影差异判为损坏。

先前多账号实验每格临时 CODEX_HOME 导致 installation_id 随格变化，属于隔离实验的明确条件；它不能代表用户长期使用的同一安装环境。未来真实环境控制应保持适当的安装连续性，同时允许正常的会话/turn变化。

## 后续网上资料核对

环境矛盾已纠正后，开始读取官方帮助与原始社区报告。官方明确说明安全异常（例如重复登录失败或未知地点访问）可能导致暂时能力降级，但页面范围是 ChatGPT 访问/能力限制，没有公布 Codex 的 TLS/UA 评分规则或低算力集群分配机制。[官方说明](https://help.openai.com/en/articles/10258669-troubleshooting-model-feature-access-issues)

原发糖果讨论报告第三方调用出现 29/516，另有作者在官方客户端也复现；因此不能直接以帖子标题认定“网关请求必然降档”。[原始讨论](https://github.com/router-for-me/CLIProxyAPI/discussions/3937)

9 月 13 日的一手经历称改变电脑区域/时区后官方客户端表现改善，换回 Hermes 又变差。作者自己说明不确定持续性与普适性，且没有提供单变量请求/指纹日志；这是值得对照的线索，不能据此批量伪造所有账号的地区或改服务器时区。[原发文章](https://linux.do/t/topic/2896640)

## 当前结论边界

已证实并停止生产里的静态应用身份覆盖。既有同平台 TLS 对照与此次 runtime 环境检查支持当前原生传输实现范围内的一致性；不会据此声称所有客户端 OS、网络层指纹、设备可信状态都相同。是否有环境历史导致账号长期质量差异仍需证据，环境修正本身不能宣称四次 29 已恢复。
