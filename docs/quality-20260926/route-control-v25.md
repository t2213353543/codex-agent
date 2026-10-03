v25 检查同一个账号使用主机直连与当前源 OAuth 代理时，固定原题请求是否出现不同结果。它控制请求正文、非认证头、账号凭据、原生 helper 和实际进程环境，只改变显式路由；不修改生产网关设置或重新授权。

已完成的只读路由证据表明：new1/new2 当前源配置均使用同一个 SOCKS5h WARP 路由，源容器域名解析到私有代理地址，宿主可连接该地址。两源请求的 Cloudflare trace 为 WARP on、IPv6、US/LAX、同一出口哈希；宿主直连为 WARP off、IPv4、US/LAX、不同出口哈希。此处只有 3 次不带授权头的 trace GET，证明当前观察，不证明历史 OAuth 授权当时使用的出口。trace 的源客户端与主机 Python 客户端也不同，不能把该 GET 当作 native 模型请求的 TLS 抓包。

固定 bf920 helper 的 HTTP 编译路径不支持直接 SOCKS，即使配置解析允许 SOCKS URL。因此新探针以一次性 HTTP CONNECT→SOCKS5h 字节隧道连接同一目标；TLS仍端到端由原生helper处理。真实隔离loopback验证12项全通过，包含直连、HTTP CONNECT和SOCKS5h的POST与usage、目标解密后无Proxy-Authorization、精确body/headers与拒绝后零直连回退。正式探针不继承代理/CA环境，不增加CA参数；验证CA只在测试fixture内使用。

首个 models 批次已终止，实际模型 POST=0。控制核心预留过一个回调名额，但工具在启动helper之前因 runtime codex_cli_rs 与真实捕获 codex_exec 应用身份不一致而拒绝；helpers=[]、native_dispatch_count=0、model_post_attempts=0。已完成的6个usage GET（4组前、1格前、1格后）均成功并清理。该报告与started标记原样保留，不覆盖或继续。

独立 models-v2 在任何新usage/model请求前调用原校验器，但附加的runtime→codex_exec字符串推导仍漏掉真实官方UA末尾的 `(codex_exec; 0.155.0)`，因此前置拒绝并停止。v2 实际usage=0、模型=0，没有恢复原run。合成main10项加独立4项虽通过，未能覆盖真实捕获UA后缀，故这些通过不能替代实际preflight。下一版改为精确固定真实captured-application安全投影及原head哈希，不再猜测构造UA，仍不改原body/head。两个停止尝试报告均保留。

预登记模型顺序为 new2直连、new1 OAuth代理、new1直连、new2 OAuth代理，再完全逆序；最多8次模型POST，各账号各路由2次。任何协议失败、epoch/route变化或清理/持久化异常停止，不重试补格。最终 models-v3 已完成8次模型POST；每格HTTP200、protocol completed、helper/隧道清理通过。原自动答案3格为null，以下人工附注单独绑定原输出与report哈希，没有回写原评分。

本对照仍不能单独识别“出口IP”“远端DNS”“WARP网络”中哪一项影响服务端，也看不到历史风险状态或隐藏算力分配。账号角色/权益不因代理切换而变；token使用量和一道题答案不是普遍模型能力测量。生产并发请求需要独立记录为混杂因素，不能用它推定错答原因。

最终验收239项机器检查全部通过，8格均为同一固定正文SHA `41dcbb703b584cb6a846c1139866bde999416a92132254b7dbb736f120f8765b`，同一组13个非认证头哈希；只在内存绑定各自认证字段。8格输入均6879 tokens，返回model为gpt-5.6-sol、effort为medium；HTTP均1.1。两arm实际actor均UID0、同宿主network namespace（不冒称生产UID61787）。4个代理格都观察到一次本地CONNECT、一次代理连接、一次SOCKS CONNECT，目标ATYP3及chatgpt.com摘要匹配，桥不终止TLS、没有回退且全部socket/listener/thread关闭。

| 账号 | 主机直连两次 | 当前源 OAuth/WARP 路由两次 |
| --- | --- | --- |
| new2 | 21、21〔08人工附注〕 | 21〔04人工附注〕、21 |
| new1 | 29、29〔06人工附注〕 | 29、29 |

| 格 | 账号/路由 | 原自动答案 | 单独人工结论 | reasoning tokens |
| --- | --- | ---: | ---: | ---: |
| 01-direct-new2 | new2/direct | 21 | — | 1544 |
| 02-oauth_route-new1 | new1/oauth_route | 29 | — | 732 |
| 03-direct-new1 | new1/direct | 29 | — | 575 |
| 04-oauth_route-new2 | new2/oauth_route | null | 21 | 1421 |
| 05-oauth_route-new2 | new2/oauth_route | 21 | — | 2588 |
| 06-direct-new1 | new1/direct | null | 29 | 516 |
| 07-oauth_route-new1 | new1/oauth_route | 29 | — | 516 |
| 08-direct-new2 | new2/direct | null | 21 | 1034 |

04的输出首末均明确21并给出9圆形+12星形；06末尾明确boxed29；08首末明确最少21。三个附注分别绑定UTF-8原文SHA：04 `31f1c8f016dd3da6bc010d22e014b543dd41bab609a9801d10c928113c388987`、06 `03cb03d120e6d029b1cc9d5838a6e4d9728ffc97c4cfb9ddbf65f65d20853f9e`、08 `13cec7595e0add59064e879dec376aef9a2480e6a0b28c2571f353e21e116f9e`。这只是明确数值结论复核，原模型文本、自动提取失败状态和原评分均保留。

当前路由切换没有改变本题的账号差异：new2两路均21，new1两路均29。在这次当前凭据/当前网络对照内，没有观察到把模型请求改走OAuth代理就即时修复new1的效果。不能据此排除历史授权环境对账号状态的影响，也不能证明不存在服务端差异路由或配额因素。

期末source route/profile/runtime完整摘要与本次plan相同，账号epoch逐格前后相同；8格窗口内已完成的生产网关日志重叠数均0（不覆盖外部、未完成或未持久化活动）。五个实验worker PID0且inactive，四份raw capture已删、凭据文件不存在。独立重算67份服务器登记公开文件SHA全部匹配。完整证据为 `.local/research-20260926/route-controls-v25/review/result-v3-review.json`、三份manual-answer附注、route-final.json和cleanup-verified.json。所有后验检查离线完成，不依赖随后部署改变的运行时，也没有再发模型或GET。

2026-09-26后续核对执行器命名：实际最终worker为`codex-quality-route-v35c`（字符串替换导致的命名笔误），launcher及collect查询使用此实际名称；cleanup脚本曾将其写为`v25c`。另行只读查询实际`v35c`现为not-found、PID0、inactive，原collect结束状态也为PID0/exit0；逐helper的回收/套接字检查及全部模型结果不受该命名错误影响。原回执未覆盖，补充证据为[unit-name-correction.json](../../.local/research-20260926/route-controls-v25/review/unit-name-correction.json)。后续批次固定显式unit名称。
