# 附件取消与集中断线调查

## 已复现的本地缺陷

附件上传在收到 HTTP 200 后，JSON 解码和等待末尾 EOF 的两个分支把 context.Canceled/DeadlineExceeded 转成了“无有效文件引用”502。共享同一附件上传的健康 follower 因此无法识别 leader 已取消，被一起误报502。

最小修复：上述两个错误分支先返回真实 ctx.Err；真正的坏JSON、额外JSON、无文件引用和无取消的截断仍使用原响应错误。沿用既有 cancelled-leader 接手规则，不新增等待、不重试HTTP错误、不重发模型生成。

真实HTTP fixture在上游先返回200后发送半JSON，或发送完整JSON但不结束body，分别触发取消和期限到期。旧代码4个场景全部复现leader/follower502；修后健康follower200，共两次上传（旧leader取消、新follower自己的一次）和一次生成。实际取消原因、缓存只存完整成功结果、失败不缓存及原账号授权边界均验证。27个顶层、72个含子用例通过，独立审查无新增P1/P2。证据 `.local/root-cause-20260930/attachment-boundary-overlay/verification.json`。

完整回归中的取消cause测试增加客户端Body.Read同步屏障：服务端Flush只证明已发出数据，不代表客户端已进入Decode。修正后的严格原断言连续20次通过；未修改产品逻辑。最终精确候选全量go test、go vet和真实PG/Redis回归全部通过，源码清单 `325222ede7239d59bc69c4f028da251b1ffe2bd706f389d6e046caceebe6cc91`。

23:05两节点已激活release `b259f49fe4e760416883a6170e50f51bdf04bea59b0e0ed267a184102c5031fe`，真实运行binary均为 `c041ace712fa96ad836b2318227ca3476d6e61e959abeac124a4e959430f6ffd`。只改变一个产品文件及其测试，UI/迁移不变。12项主节点页面/管理/健康检查全200；两个Sub2容器网络的健康和capabilities全200、共享配置一致。原缓存率85%及关闭限流冷却保留。23:06在再次验证空账号池、无活跃租约且阻止并发导入的窗口，旧6ccb代正常退出，两节点draining_remaining=0。证据 `.local/error-repair-20260930/drain-peer-verified-150654.json`、`.local/root-cause-20260930/empty-pool-attachment-status.json`。

相关用例复跑（工作区根目录）：

```powershell
go -C .local/redis-memory-20260930/release-source test ./internal/server -run '^TestAttachmentBoundary' -count=1
```

## 历史13个连接重置的边界

11个bps_protocol错误与2个attachment错误具有完全相同的TCP端点元组哈希 `2fad698226b66912`。开始时间为12:01:50至12:04:12 UTC，结合记录latency后的失败时间集中在12:04:15.666至12:04:16.433；原created_at只有秒精度，不能将这个范围当作毫秒级网络事件时间。

源地址匹配副节点app214公网接口，端口40834；对端公网443与当前bps.openai.com DNS A匹配，附件URL为 `/basispoints/api/attachments`。不是本地WARP/SOCKS代理监听。现有记录支持一条共享上游连接重置导致多路请求一起失败，不能把下游request_type=websocket当成上游使用WebSocket。历史attempt.phase=request，11个流已有HTTP200，10个已有TTFT。

两节点目标时间窗口内没有应用停止/重启、NetworkManager/firewalld重载或内核网络异常的日志证据。当前DNS及当前代理配置只能辅助归属，不能独自证明历史网络状态；具体为何对端发RST仍未定因。没有通过重放已输出请求或更改协议掩盖失败，也不声称本次附件取消修复解决了这13次RST。

精确历史unit814ea在副节点11:39:17 UTC已启动，目标窗口无该unit生命周期事件。4份历史transport源码与当时manifest吻合，BPS与附件使用共享Go客户端；HTTP/2多路复用是强推断，因为原记录没有协议/ALPN字段。三条natural-monitor记录均为panic_count=0，不是程序崩溃。最终只读审计见 `connection-reset-audit-summary.json`。

证据：`.local/root-cause-20260930/read_remaining_error_boundaries.json`、`connection-reset-audit*.json`。

## 加密内容错误

三条c62563错误最终都为上游HTTP400，多个HTTP200 attempt对应附件上传，不是多次模型生成。请求和实际模型均gpt-6-sol，credential_version为1；最多4个attempt的原记录可能已截断后面的生成请求。

新增隔离writer验证通过：一份密文附带图片经过附件转换后，仅1次上传、1次生成；Authorization及workspace一致，decoded密文和大整数保持不变，仍被识别为opaque，未当明文迁移。历史缺密文来源、producing owner、owner proof查询和当时wire证据，因此不推定网关改坏密文或换了账号，也不添加未经证实的schema拒绝规则。证据 `.local/root-cause-20260930/encrypted-review/review.json`。

母号held恢复策略、注册状态、429处理及生产等待均保持不变。Loophub的已验证候选仍待用户对注册维护窗口的选择。
