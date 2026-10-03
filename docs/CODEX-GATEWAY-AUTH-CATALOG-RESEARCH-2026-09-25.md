# OAuth 身份、目录账号与完整 CLI 对照

本轮继续研究普通 OAuth grant 和显式会话目录刷新，确认并修复两类应用层契约差异；另用完整官方 CLI 固定 HTTP provider，对同一个账号进行原题直连/网关对照。原题答案键仍是 21，研究没有提供或注入期望答案。

## OAuth 请求使用各自的客户端身份

固定 CLI 0.155.0 的授权码交换使用 `create_raw_auth_client`，不会自动加入 Codex UA/originator 等默认身份；刷新令牌使用 `create_default_auth_client`，具有默认身份，但没有模型 provider 的 Version。网关此前把管理模型请求的 header builder 套在授权码交换上，开启 profile 后刷新还会额外添加 Version。

实际 loopback HTTP 对正常 grant 复现了这两个差异。修复按照现存两个私有调用点区分：form authorization_code 使用 raw auth 头，JSON refresh-token import 及常规 refresh 使用 default auth 身份；profile 刷新保留所选完整 UA/originator，移除 Version。raw auth 通过零值 UA entry 抑制 Go 自动 UA，而 native serializer 不输出空 UA。

不改变 grant、client_id、code_verifier、refresh_token、代理、endpoint、账号 scope 或 token 合并；PAT/session 等其他管理请求不套此新规则。未开启 profile 的短 UA 仍是既有兼容策略，不能当作完整宿主指纹。官方只在配置 residency 后加入的头也没有被臆造。

修改前两个顶级研究用例失败，修复后关联 64 项通过、5 项外部条件跳过；另有真实 Rust helper 的九种 grant/profile 子例由 Linux 全套验证。[OAuth 研究详情](quality-20260925/oauth-identity.md)记录源码位置和失败前证据。应用头对齐本身不证明提供方风险标记改变。

## 目录应尊重已存在的显式会话绑定

双账号真实 HTTP 实验先把显式 session 的推理绑定 A；第一次目录由 A 返回，随后 B 的优先级提高。旧代码的下一次同 session 目录请求切到了 B，但同会话的推理仍走 A。这会让支持显式目录 session 的客户端看到不属于该会话账号的模型配置。

修复只使用同 key、sticky 已启用且已存在的绑定；未绑定 GET、不带 session、sticky 关闭仍独立选账号，目录 GET 不建立新会话绑定。绑定账号禁用或共享绑定存储不可用时失败，不悄悄选择 B；已有绑定也控制相同会话的代理选择。

ETag 反证控制同样保留：同账号同目录正确 304，不同账号正确 ETag 变化返回新 200，client_version 或 profile 变化也没有错误复用旧 304。固定 CLI 默认目录请求传空 header map，不带 session 或 If-None-Match，因此**本缺陷是显式 session 兼容客户端边界，不声称它解释当前 CLI 原题错答**。研究与测试详见[目录身份记录](quality-20260925/models-identity.md)。

## 完整 CLI 同账号 HTTP 对照

此前多数网关实验使用手工构造的简短 Responses 正文。本轮改用完整官方 0.155.0 包，让其正常加载模型目录、生成上下文、选择 code_mode 工具格式并压缩请求。两端均使用同名 `official_http_reference` provider、name=OpenAI、相同 medium、同一原题、各自全新 home，request/stream retries 均为 0；有效配置摘要移除 base_url 后相同。直连目标为官方 Codex HTTPS，网关目标为私有 loopback `/v1`。

第一次网关夹具误将账号并发设为 1。CLI 的目录加载和推理并发触发本地 busy429，目录也未正常完成；该条没有有效答案，记录为夹具失败而非账号降智或上游限流。修正夹具并发为 4，但仍硬限制仅一次模型 POST，保持原失败记录，只补跑网关这一端，不重做或筛选直连答案。

| 有效参考 | 结果 | 时长 |
|---|---|---:|
| 完整官方 CLI HTTP 直连 | clean completed，29 | 25.979 秒 |
| 完整官方 CLI → 透明 native 网关（修正夹具） | clean completed，29 | 79.131 秒 |

前后 user/workspace、AT 摘要、分配及 OAuth 更新时间一致；两端均正常载入相同版本目录，目标模型为 `code_mode_only`，无可见工具事件、警告、reroute 或回合错误。网关观察到两个成功目录 GET 和一次推理 POST，未触发额外请求拒绝。该 POST 从 CLI 以 zstd 输入，实际 native IPC 保持 zstd 选择、相同 bearer/workspace、`gpt-5.6-sol/medium`，回显 completed/default。所有 helper 正常回收，临时 AT 副本删除。

网关 ingress 解压后的正文为 36,243 字节，native IPC 准备后的正文为 36,636 字节；记录为不同序列化摘要，**没有声称两者逐字节一致**。直连出站正文没有捕获，独立 CLI invocation 的 session/cache/元数据也不同；此对照不是只改变 TLS 的字节级 A/B。已有合成 CLI 压缩/字段保真测试提供另一个层面的契约证据，但不能替本次补造抓包。

这项对照补上了“完整 CLI 经网关真实请求”证据。两端仍是 29；没有因此证实内部集群、稳定准确率或账号恢复。旧夹具失败保留，不能合并为第三条数学错答。

原始和派生证据位于 `.local/research-20260925/cli-http-pair/{result,corrected,pair-review.json}`。用于实验的 gateway test binary 固定在上一部署 `cdc07d09…39752` 的源码，加上 test-only loopback server；本轮 OAuth/目录补丁与实验分开，不把先前实验改称新生产版本质量验收。

## 最终集成与上线

OAuth 头部与显式会话目录修复已上线，当前后端 SHA256 为 `33f4535faa9ca3cf928568c3e3c0d48939ec9337095995705ae0eccb1dcdbe5e`。无在途请求时切换；账号 metadata、应用环境、前端、监听及原生 TLS helper 保持一致，没有数据库 migration。

| 最终验收 | 实际结果 |
|---|---|
| Linux race + PostgreSQL/Redis + 真实 Rust helper | **880 顶级通过、0 失败、22 项明确 opt-in 跳过**；2562 个含子项终态，286.631 秒，零 DATA RACE，vet 通过 |
| OAuth 原生实际 HTTP | 九种 grant/profile 叶子用例全部通过，包含父组共 12 条终态；raw auth 的 UA 不存在与 default auth 的 Version 不存在均由真实服务端检查 |
| 官方 CLI 独立回归 | **7 顶级通过、0 失败、0 跳过**，13 个含子项终态，33.670 秒；实际工具执行与合成上游回答 |
| Windows 全套 | **758 通过、0 失败、145 个外部条件跳过**，40.262 秒；vet 和 Linux 构建通过 |
| 发布后验证 | HTTP models 200、9 个模型；WS 101/accept 校验通过，关闭后 in-flight=0，readiness=ready |
| 源码与清理 | 两批冻结源码与工作区、构建输入一致；两次 CLI 网关夹具正常退出，临时 AT 删除，捕获的 helper/wrapper PID 均已消失 |

Linux opt-in 跳过包含真实账号实验服务器，不能计为已通过；实际账号证据仍按上文三次 CLI invocation（两条有效、一条夹具无效）分开。发布后 WS 只验证握手与关闭，不发模型回合。完整 CLI 质量实验基于此前 cdc 后端；不能把它反向变成本次 33f 后端的质量恢复验收。

最终证据目录：`.local/gateway-audit-20260924/go-authcatalog-20260925-final/`、`go-cli-authcatalog-20260925/`；运行和发布记录：`.local/research-20260925/auth-catalog-continuation/` 的 build-manifest、activation-before、activation-result、runtime-smoke、experiment-cleanup。服务覆盖文件为 `zzz-auth-catalog-20260925.conf`，单独撤销它并重载/重启可回到上一后端，账号数据无需回滚。

离线复验本轮边界：

```powershell
go test . -run '^TestResearch(OAuthIdentity|Models)' -count=1 -timeout=120s
```

本轮确切修复了正常 OAuth grant 的应用头偏差和已有显式会话的目录 owner 漂移；有效完整 CLI 原题直连/网关均为 29。原题稳定通过目标继续保留，不以应用头和目录契约修复代替模型质量证明。
