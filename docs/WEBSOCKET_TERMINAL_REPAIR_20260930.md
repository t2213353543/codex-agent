# WebSocket 终止帧修复

## 已确定的本地问题

BPS 复用连接的单 reader 将最后一帧放入单槽 inbox，随后读到 peer EOF 并关闭 dead 通道。消费者原先在 inbox 和 dead 上同时 select，可能随机选到 dead，返回 UnexpectedEOF 并丢掉已经收到的 completed。实际 reader 的128次复现中丢失79次；另一轮67次。

现在只有选择到 dead 时再非阻塞读取已入队帧；队列为空仍为 UnexpectedEOF。不等待新帧，不重发生成，不把未完成响应判为成功，死亡连接不回池。

连接池原先吞掉所有 basispoints.* 帧，包含解析器已支持的 basispoints.response.cancelled。现只把该取消终止别名交给原解析器，结果仍是取消失败；metadata、heartbeat、resume 以及未知内部控制继续隐藏。

## 验证

21个顶层、46个含子用例通过，覆盖真实loopback WebSocket单次握手/单次create后立即关闭、completed/done/failed/incomplete/cancelled、半截响应、客户端取消和旧连接复用。独立审查无剩余P1/P2。证据 `.local/root-cause-20260930/remaining-protocol-overlay/verification.json`。

完整回归暴露两个既有测试自身问题，均只修改测试：

- HTTP hop header测试共用5s期限，小于已存在4s默认冷却加最多1.5s抖动。每次请求改用依据现有上限加网络余量的独立测试期限，全部状态/头/call count断言保留。三轮中5.18s和5.09s正常完成，证实旧期限会误报。
- 租约取消测试只等待lease.Done即检查另一个route的cause。实现同一临界区先取消lease再取消route，前一个channel关闭不代表后一行已执行。测试改为有界等待route.Done后校验原typed cause，没有改生产取消顺序或延迟。

生产等待、429处理、母号held恢复和缓存率均未更改。

最终源码清单为 `3aa5b6ed13e3d37ba80ea8bbaa1bda010ef08bc259cc23ec6f67828d17db4ed6`。精确候选完整go test通过（5490含子用例通过，725项外部依赖门控跳过），go vet通过；另行实际隔离PG/Redis回归全部通过而不是将本机PG跳过当作成功，取消同步场景额外连续10次通过。最终证据在候选 `.local/verification/evidence/{go,vet,pg}/result.json`。

22:40两节点已上线release `2c88147bd8a97015a48ab3e72f236635a79315168cc96360ae954514a0209e8c`，真实运行binary均为 `6ccb5155b4394eb550d32dbab2892ab9674c9084eac0f5181139087bdf3563ae`；未增加迁移，仍27项。主节点12项健康/管理/页面检查全200；两节点Sub2容器网络访问 `http://172.18.0.1:18879` 的健康与管理员协议接口全200，运行配置与存储identity一致，缓存读取比例仍85%、rate_limit_cooldown_enabled仍false。证据 `.local/error-repair-20260930/drain-peer-verified-144138.json`。

## 旧代退出与未解决证据边界

22:30两节点旧671dea代正常退出，exit status均0，无SIGKILL。仅在账号池持续为空且没有在途租约的现场进行：最后账号删除12:51 UTC，旧代13:11 UTC启动；短事务持有accounts SHARE锁阻止并发导入，stop前与exit后双向确认事务仍持锁，再更新运行注册表。此前确认的所有旧代均已退出，管理协议不再混用缺少排空保护的旧进程。证据 `.local/root-cause-20260930/empty-pool-ws-status.json`。

22:41使用同一已审查的空池流程，将此轮旧1dc3f代也正常退休，两个exit status均0、draining_remaining均0。当前全部生成入口使用新6ccb代，没有旧长连接继续走未修复的终止帧代码。证据 `.local/root-cause-20260930/empty-pool-terminal-status.json`。

历史13个connection reset分为11个bps_protocol和2个attachment，前者10个已有TTFT。它们缺少peer端帧事实，不能说全部由本次竞态造成。此轮另有3个包含encrypted的历史错误处于response_headers，不能仅凭关键词等同invalid_encrypted_content。历史invalid_encrypted_content和update_plan缺关键身份/实际声明证据，保持真实失败，不删除密文、伪造工具结果或重放已输出请求；已部署有限诊断供下一次真实失败区分原因。

Loophub调用端镜像已验证但尚未上线：100个注册worker仍运行，暂停派发及人工恢复注册的维护选择待用户答复。母号恢复任务保持原样。

23:06后续：两节点已部署附件取消传播修复binary `c041ace712fa96ad836b2318227ca3476d6e61e959abeac124a4e959430f6ffd`，继承本页全部WS修复且旧代均退出；详见 `docs/ATTACHMENT_CANCEL_AND_CONNECTION_RESET_20260930.md`。
