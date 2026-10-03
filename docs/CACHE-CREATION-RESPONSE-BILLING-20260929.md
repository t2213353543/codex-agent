# 缓存创建按读取返回

`cache_creation_as_read_enabled` 已随 .22 上线，默认开启；旧配置没有该字段时也按开启读取，显式 false 会保存并生效。设置页通过同名开关控制。2026-09-29 06:37（北京时间）先成功切到 .22；07:26 又完成认证错误修复版本 .23 的真实SSE流中切换，当前PID3337873、release `37450c1b…`。07:27:54实际线上回执确认开关true、两次生成及本地负例/CSV验收均通过，设置版本27未变。旧.22随后排空退休，旧.20仍保留。此前两次等待回执保留；已取消的“旧实例预留”方案与本次认证修复 .23 是不同工作，预留方案未进入生产。

启用时，已知且合法的原生 usage 在写给客户的最后一步进行转换：

| 计数 | 上游真实值 / 内部记录 | 对外返回 |
|---|---:|---:|
| input_tokens | 100 | 100 |
| output_tokens | 10 | 10 |
| cached_tokens | 80 | 87 |
| cache_write_tokens | 7 | 0 |

读取量与创建量合并用于下游读取计费桶；总输入、总输出不变。内部 `StreamResult`、观察回调、请求记录、缓存统计和历史仍使用上游真实值，CSV 也应保留真实 read/write，不能将对外87/0反写为内部80/7的替代。

当前转换 helper 只处理唯一原生 `usage.input_tokens` 和 `input_tokens_details.cached_tokens/cache_write_tokens`，要求三个计数都是非负 int64，read不大于input，write不大于input-read。这样加法不会溢出或超过总输入。缺字段、null、非法数值、重复/有歧义字段、alias-only及无法确认的计量保持原字节，不猜成零；write已经为零时不重写。未知计量仍未知。

变换只编辑两个计数字段的字节范围，保留其余正文与密文。普通 SSE 的观察/完成回调先处理原事件，再转换实际写出；structured内部缓冲关闭转换，投影及记录之后才对公开事件转换。原生JSON/compactJSON在最终write使用显式 `ExportResponseCacheCreationAsRead` 根对象入口，SSE使用事件入口；不能仅因为一个原生响应的未知扩展叫 `response` 就去改扩展内部的用量。C 已在真实本机 compact 消费者复现旧实现改错对象，并在主任务修订后用同一 fixture 验证根87/0、内部真实80/7、扩展原字节均保持，见[C 对照报告](../.local/coordination-20260928/team-c/phase41/dual-usage-gateway-review/REPORT.md)。各模式的实际接入与失败路径覆盖仍由主任务集成测试证明，不能凭helper测试推定所有入口已完成。

部署验收也需同步这一区别。原 `postdeploy.py` 的 exact_usage 将公开缓存计数直接等于内部记录，面对100/80/7→100/87/0会假失败。新验收应明确开关前后为true，对公开值与内部真实记录按投影规则对账，同时单独检查内部CSV的真实计量。不能为了旧谓词通过关闭用户要求的新默认开关，也不能放掉unknown、越界或配置未生效检查。A 的私有predicate修订与纯本地验证另列在协作记录，实际生成/发布仍由主任务唯一执行。

A 已交[最小验收补丁](../.local/collab-a-20260928/billing-postdeploy-review/postdeploy-usage-projection.patch)与[10组纯函数证据](../.local/collab-a-20260928/billing-postdeploy-review/review.json)：正确87/0通过、未投影80/7在默认true下拒绝，CSV仍要求真实80/7，开关前后必须是布尔true。补丁保留原请求预算和身份/配置守卫，没有运行部署或生成请求。它只验证标准原生 smoke 的期望计量，不从已解析记录反推原始JSON的重复键等信息；最终实例unit/端口适配需随主发布方案另核。

主会话后续的实际 `postdeploy.py`（SHA `f9e8a41f…`）已采用上述对账边界，并另写严格原生计量谓词：必须明确知道读写量、计数是非负int64且读写之和不超输入，再按显式开关比较投影。CSV单独核真实计量，结束重新检查开关仍为true。实例验证读取当前registry、systemd PID、可执行文件SHA、listen以及入口generation/address，并在请求前后比对；解密配置使用已核验的新实例环境。它与A私有函数实现有差异，不能直接借用A旧10组/13组测试结论，见[实际采用审查](../.local/collab-a-20260928/billing-hot-release-review/postdeploy-adoption-review.md)。这仍是源码审查，真实上线回执另核。

双实例无中断发布是同轮独立工作。2026-09-29 05:42（北京时间）的实际入口接入回执支持：旧应用PID未变、一条预先建立的普通HTTP keepalive在接入前后均返回200，新建本机IPv4/IPv6健康请求成功；这不等于已完成新应用切流，也不证明所有在途模型SSE/WS均连续，见[入口回执审查](../.local/collab-a-20260928/billing-hot-release-review/bootstrap-review.md)。新的unit/PID、入口目标和共享状态仍需实际切流验收；旧固定 `codex-agent` MainPID＋18879请求的回执不能直接认证新的实例拓扑，见[验收身份边界](../.local/collab-a-20260928/release-drain-review/receipt-consumer-review.md)。

06:21 的首轮观察为271次本机健康请求成功、旧入口恢复，未观察到实例切换。06:26 的第二轮观察为421次本机与132次外部健康请求成功，但本机仍只有旧generation；外部健康回执不含generation，不能独立认证切流。第二轮45秒排空观察末尾还有4个入口活动请求与4条旧后端连接，不能据汇总判断协议或请求身份。实际两轮原件与范围见[观察报告](../.local/collab-a-20260928/billing-hot-release-review/external-source-scope-review.md)，不得将它们当作 .23 的执行证据。

06:37 的最终观察另有94次本机health全ready，单一client连接地址元组保持，实际观测到legacy→新实例两个generation，old PID未变，最大健康延迟3.24ms。它支持这条已建立连接跨本次切流可用，不扩展为所有模型SSE/WS或外部业务都经过了同样验证；此前外部132次health属于未切流窗口，不能挪用到最终窗口。
