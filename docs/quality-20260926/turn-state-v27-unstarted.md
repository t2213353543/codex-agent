# v27 被动状态观测：工具完成，在线批次未启动

本计划固定原糖果题与同一份官方CLI捕获正文，只被动记录首个native响应头中X-Codex-Turn-State的长度、ASCII/UTF-8有效性和哈希。没有状态解析、注入、重放或跨账号复用，也没有把任何长度定义为质量判据。

独立工具验证：Linux10项通过、0跳过，包含真实bf920 TLS的3个合成场景；Windows9项通过、1个Linux专属跳过。执行器8＋4个实际main合成测试通过，缺观测、错计数、错哈希/顺序或状态不一致都会停止。真实官方CLI捕获预检0模型调用通过。

启动在线四格ABBA时，exec_command在创建进程之前被平台拒绝。没有换命令、改入口或让子agent代发。随后只读核对实际unit为not-found/PID0/inactive，models/started.json及execution.json均不存在，真实模型POST数为0。4份原始捕获文件已删除，capture凭据文件不存在。不能把四格计划当作实验结果或从已有头哈希推断长度。

保留离线工具与测试用于维护，但本计划不可原路径重试：捕获已清理，若未来允许开展新的观测，需要独立重新登记与实际运行，不覆盖本次未启动证据。

[启动边界](../../.local/research-20260926/turn-state-observation-v27/launch-boundary.json) · [未启动核对与清理](../../.local/research-20260926/turn-state-observation-v27/unstarted-cleanup.json) · [工具验证](../../.local/research-20260926/turn-state-observation/summary.json) · [执行器审核](../../.local/research-20260926/turn-state-observation-v27/review/runner-review.json)
