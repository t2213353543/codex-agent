# 慢客户端、响应期限与错误截断：源码候选

本轮基于上一轮未部署的400文件请求生命周期候选，合并后403文件，生产改动局限于4个文件并新增3个测试文件；全部已核对旧SHA后同步工作区，整个403文件源与冻结候选一致。本机Windows全套3096项通过、382项明确跳过、vet通过，Windows编译与Linux交叉编译成功。Linux运行、race及真实Rust集成仍未执行，本轮没有SSH、在线模型、重新授权或部署，也没有重试之前被平台拒绝的操作。

## WS背压不再计为上游闲置

串行响应泵执行上游Read后再向下游Write；Write被慢消费者阻塞时，它无法读取任何上游lane。旧闲置观察器仍计时，真实双TCP测试中4MiB以上消息或终态写阻塞超过10秒，会将正常活动请求误记为upstream response_idle并关闭连接，即使下一轮上游已发送完成。

候选只在实际下游Write期间暂停闲置时钟，成功后排除该阶段的等待时间；旧队首已消耗的闲置预算保持，新准入或新晋升队首只排除它存在的那段暂停，不额外获赠时间。失败写先记真实client写错误再取消，不唤醒旧闲置期限抢占错误来源。60秒独立写预算、请求总context以及真正的上游闲置仍有效。既有native队列容量和读上限不变。

独立验证包括“10秒预算已耗6秒，恢复后仍仅余4秒”、新队首、非队首不提前计时、重复暂停/恢复不重复加时，以及实际阻塞写被父context取消并释放。原始长阻塞红绿证据与小范围精确时序测试分别保留，不把本机Go IPC测试冒称真实Rust网络。

## HTTP写期限只覆盖实际写出

旧writeStreamPipe每块写入设60秒deadline，却在完成交付后继续保留它。上游思考时间过长时，HTTP/2的deadline计时器可主动重置流，后来重新设deadline也不能恢复；HTTP/1若下一次只有干净EOF，处理器的结束chunk也可能落在已过期deadline而让客户端看到截断。HTTP/1若还有下一块，重新设期限本来就可能成功，这个区别保留。

候选在每次上游Read前清除下游写期限，并在函数退出时清除；每次真正Write/Flush仍设置原60秒期限，写入错误仍返回。真实loopback HTTP/1与TLS HTTP/2测试只将60秒映射为40毫秒、上游间隔150毫秒，证明原三个失败边界修复；慢Write注入150毫秒延迟后，实际HTTP写/Flush仍超时。该测试不是实测生产HTTP/2入口，也不等于TCP发送缓冲已压满。

## 截断的HTTP错误响应不再表现为完整结束

旧错误转发会吞掉上游短Content-Length读错误，并将超过2MiB的错误正文通过LimitReader裁剪后正常结束。Responses、compact、models的6个真实TCP场景中，下游都错误看到cleanEOF。

候选保持最多转发2MiB、最多64KiB诊断前缀，只读取额外1字节区分恰好到上限与超限，不将探测字节发给客户端；最多100次空读取后报错。上游读错、超限、下游短写等实际失败返回给处理器，在保存原上游错误状态与诊断后中断HTTP连接。完整小错误、恰好2MiB错误及普通错误码/多值元数据保持；成功JSON/compact/models已有写失败记录保护，经验证无需生产修改。

错误响应的限额探测不是无限读。现有请求/上游读期限继续约束等待；本轮没有额外改默认模型、TLS、设备画像或账号凭据。

## 验证范围

- 最终源码SHA：`fe59b73353860ac51b2fa7b405511cdb2d201ce8a6f68f4ced99a734b5fa02aa`，403个声明源文件。
- Windows完整3096通过、382跳过、0失败；vet通过。
- Windows本地产物：`9de701c44d7e92d967c293395378cdef72491dc9cdab1c953d51bf41bcddb59a`。
- Linux交叉编译产物：`fe0075bcbef3688aac0334920176fe537b8be858edc3037ce9c98c2f5a958a3a`，仅编译，不代表运行验证。
- 原生产最后确认版本仍为`6ecdf93f…`，最后远端读取为2026-09-25 21:22:21 UTC；本轮不宣称已重新核验线上。

本轮解决可复现的传输中断和错误完整性问题，没有调用真实模型复测，不证明此前29/23错答的账号状态或服务端根因。

[候选清单](../.local/research-20260927/stream-backpressure/candidate-manifest.json) · [完整本机测试](../.local/research-20260927/stream-backpressure/windows-final-summary.json) · [HTTP期限独审](../.local/research-20260927/write-deadline-reference/candidate-peer-review.json) · [WS独审](../.local/research-20260927/stream-backpressure/review/ws-backpressure-peer-review.json)

冻结目录本机复核：`go test -count=1 -timeout=12m ./...`。

[最终本地交付清单](../.local/research-20260927/stream-backpressure/summary.json) · [403文件合并独审](../.local/research-20260927/stream-backpressure/review/combined-source-review.json) · [HTTP错误截断独审](../.local/research-20260927/stream-backpressure/http-peer-review/independent-review.json) · [工作区同步](../.local/research-20260927/stream-backpressure/shared-source-sync.json)
