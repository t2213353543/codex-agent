# HTTP取消与WS交付生命周期：源码已修，尚未部署

后续工作区已叠加[背压与错误截断修复](CODEX-STREAM-BACKPRESSURE-CANDIDATE-2026-09-27.md)，本页保留400文件版本的独立历史证据。

本轮基于上一轮未部署的397文件续接候选，非直接从线上二进制推断源码。最终400文件均逐SHA核对，2个生产文件改变，新增3个测试文件并调整1个既有测试。全部已同步工作区；本轮无远端操作、真实模型/OAuth请求或部署，也没有重试之前被平台拒绝的远端构建和在线状态观测。

## HTTP 401恢复取消优先

原处理器在两个失败出口会吞掉调用者取消：首401后恢复预检已返回context错误，或GetBody在返回error/nil/NoBody的同时取消，仍返回原401与nil错误。实际TCP红例还覆盖已经开始的合成授权返回503时发生取消/超时。

`gateway_upstream.go`仅新增8行：这些失败出口若原请求context已取消，关闭原401响应体并返回该context错误。GetBody返回的临时body仍关闭。未取消的授权失败或克隆失败继续保留原401状态与诊断正文；非401不恢复，最多一次401重试。已经获准执行的旋转授权独立完成并先保存合法新token，再停止取消请求的重放，不会因下游离开而丢新凭据。

原基线6个红叶已保留；作者197个本机定向通过事件、独立4顶层＋4子用例通过。这个问题是取消归因及资源所有权错误，不宣称原路径必然永久泄漏——正常调用者原本仍能关闭返回的401响应体。

## WS上游完成与下游交付分离

真实双TCP测试在Upgrade后向最终帧写入注入常规`io.ErrClosedPipe`：客户端没有收到完成帧，但旧网关已在Write前持久记录ok。现在仍在上游终态出现时退休ID、释放推理槽位并推进队首；终态记录从pending分离，待真实下游Write返回后通过一次性回调落账。

完成帧写失败记为aborted/client/response_write；真实上游TerminalStatus、response ID、usage与握手HTTP101保留。原failed/incomplete的上游错误不被随后客户端失败覆盖。直接观察器测试入口保持立即commit，生产泵使用实际write结果；成功Write只证明传输接受，不等于客户端应用层确认。

阻塞终态写入期间新的请求仍可准入；帧顺序不变，旧ID不能消费新turn；abort只清理仍pending请求，不重复释放或记录已分离终态。作者重复20次共220通过事件、相邻重复3次237通过/3个native跳过，独立5个场景×20轮通过。全部为本机Go WS合成TCP，不冒称本轮已运行Rust或Linux race。

## Native HTTP请求Body：无需生产修改

读取本机Go1.26.7标准库确认RoundTripper负责关闭请求Body，包括错误返回，Client.Do不会为自定义RoundTripper补救。项目现有native Go包装器已经用Once与取消回调满足本轮检查，故未改该生产文件。

新增回归覆盖9个返回分支的request Close恰一次、30轮取消与5个response.Close并发、Client.Timeout打断阻塞request Read。它使用预建Go helper记录与本地TCP IPC模拟端，不启动Rust、不经过TLS或真实上游HTTP。独立重跑3顶层＋9子用例全部通过。

## 全套验证及测试调整

第一轮合并检查3043通过、382跳过、1失败，vet亦报错，源归档及日志完整保留。失败测试在客户端Read返回后立即读LastFor，而终态记录现在必须等待服务端Write返回，故可能读到上一轮。仅改测试等待本轮精确response ID，保留原5秒截止与全部缓存/用量断言；连续100次通过。另将测试里的250ms deadline使用独立CancelFunc并明确defer，不改变行为断言。独立400文件归档比较确认生产两文件未变，没有用削弱断言掩盖生产回归。

最终Windows全套 **3044通过、382明确跳过、0失败**，vet通过。本地Windows与Linux交叉编译成功；Linux运行、race、native及隔离数据库等合并验证仍未完成，不能由交叉编译代替。候选不部署，最后已确认的线上版本仍为`6ecdf93f…`（上一轮2026-09-25 21:22:21 UTC读取；本轮没有重新读取线上进程）。

- 最终源码：`31e80d1ccd7b35d289a92464d063f9c4db0ce257026cbf13b4e0be251edf3ddf`
- Windows本地产物：`0febdb547de982c6103900b31f6233f1ab2c37dce9b16f6a0925f0eb1c752e46`
- Linux交叉编译产物：`4f11669cf19a52a41c15904dbdb5a5bd90d95112c5b3a88653d02626f2d3b7a4`

[完整本地证据](../.local/research-20260926/request-lifecycle/summary.json) · [HTTP独审](../.local/research-20260926/request-lifecycle/review/http-peer-review.json) · [WS独审](../.local/research-20260926/request-lifecycle/review/ws-delivery-peer-review.json) · [测试调整独审](../.local/research-20260926/request-lifecycle/review/test-boundary-adjustment.json) · [源码同步](../.local/research-20260926/request-lifecycle/shared-source-sync.json)

在冻结目录本地复核：`go test -count=1 -timeout=12m ./...`。本轮修复不证明账号被路由到弱集群或原题答案已恢复；原目标仍未完成。
