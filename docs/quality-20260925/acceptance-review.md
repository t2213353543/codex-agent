# 当前任务验收复核（总体未完成）

**2026-09-25 后续上线更新：** 当前后端 `bb92216f…aca277ad` 已完成生产验收。导入识别官方 `user_id` fallback；刷新按当前 AT/ID 重建身份、保存旋转令牌并阻断旧身份会话；WS 连续链保持缓存键；HTTP 改键后超限在绑定前拒绝。最终 Linux 946、Windows 822 顶级通过，零失败，另有完整官方 CLI 7 项通过。八账号目录 28 对相同、官方外部令牌本地装载均成功；原 24 次答题结果保持，质量恢复仍未证明。[完整修复与验收](../CODEX-GATEWAY-IDENTITY-REFRESH-2026-09-25.md)。下文旧版本号及测试数字按各自阶段保留。

后文 12:25 验收是前一阶段历史快照，当前版本与计数以上述最终记录为准。

更新至 2026-09-25 北京时间 12:25 生产验收，以及本轮八账号实验。此前 06:40 的“只有一个可用账号、没有新模型结果”是历史状态，已经被本轮实际测试取代。

## 验收条件与范围

1. 以实际官方 CLI、运行环境和发送边界检查 HTTP/WS TLS、UA、设备及会话字段；Linux 已验证范围不能扩展成所有平台完全一致。
2. 多账号对照要求独立 user/identity/token；不同用户可以共享 workspace，不能因共享 workspace 就认定是同一账号。
3. 固定请求实验保持 model、effort、完整 body、非认证头、CLI/helper、出口配置与重试策略。普通官方 CLI 新会话的自然字段变化应单独记录。
4. 原题答案键保持 21；完整响应的明确最终答案才评分。自动无法识别的句式可另附人工判读，不覆盖冻结评分或原始报告。
5. 重复用户/凭据、调用前身份变化或缺失门禁证据应在推理前拦截；常规 usage 允许不等于特定模型显式授权。
6. 不能把账号相关性、历史答案、事后额度或社区经历当作隐藏算力分配机制的因果证据。

## 当前证据

| 条件 | 状态 |
| --- | --- |
| 网关真实应用身份与传输 | 静态 Linux 6.8 模板已关闭，实际 CentOS 官方 CLI capture 已加载；此前 Linux HTTP/WSS 对照有效，跨平台及完整 wire 一致性仍未全覆盖。 |
| 28765 登录设备连续性 | auth-device-v2 已上线并验证当前代码行为；不能据此倒推每个账号历史注册或签发时的设备环境。 |
| usage 诊断 | workspace limit、model availability 和缓存边界保留；本轮前后 usage 允许，sol 的明确可用性仍为 not_observed。 |
| 指纹配置写入边界 | 新增管理 POST 校验；平台冲突在持久化前返回 400，版本与配置不变。历史无效配置仍可读取、禁用和修复。 |
| 多账号门禁 | 本轮八个 user、identity、token 各自独立，共六个 workspace；每格前后验证凭据 epoch。 |
| 有效多账号复测 | 已完成 16 次固定 native IPC 请求及 8 次完整官方 CLI 直连调用，均有最终答案；人工统计 3 次 21、21 次 29。 |
| 自动评分边界 | 原始自动统计 3 正确、15 错误、6 无法判分；六格人工均明确为 29。两套结果并列保留。 |
| 历史来源环境 | q0 可匹配当前来源记录；g1–g7 在现有 28765 注册库和 Team 元数据没有匹配，历史环境未知。 |
| 降智根因与恢复 | 未证明。q0 和 g7 均出现对错变化；官方直连也错答，不能把 Go 转发损坏当作所有错答的解释，也不能排除历史环境影响。 |

## 已部署修复与测试关联

当前后端 SHA256 为 `020fa045f6da83d6c6c0c18ded845473c35b4cfd9c11b31aee09832dd82065e1`。相对上一版 `f9e56bb2…d87e4687`，本次生产源码只改变 `admin.go` 的 settings POST 校验。构建从已经通过完整测试的独立源码快照进行，不包含共享工作区后来出现的其他推理代码变更。

- Windows：797 顶级通过、0 失败、148 跳过，2601 个含子测试终态；vet 通过。
- Linux race/PG/Redis/native：921 顶级通过、0 失败、23 opt-in 跳过，2735 个含子测试终态；348.666 秒，无 DATA RACE，vet 通过。跳过不计为通过。
- 生产：HTTP models 200/9 项、WS 101 且正常关闭、usage 200 往返、health ready；跨平台 enabled profile 实际 POST 返回 400 且配置未变。该冒烟验证没有模型 turn。
- 八账号部署前后元数据摘要相同。三个实验任务退出码均为 0、MainPID=0；12:25:53 实查临时凭据和原始捕获体/头均已删除。

## 证据入口

- [八账号逐次报告](../CODEX-QUALITY-EIGHT-ACCOUNTS-2026-09-25.md)：身份、请求、评分、直连补测与结论边界。
- [环境修正与本次管理边界补丁](../CODEX-QUALITY-ENVIRONMENT-2026-09-25.md)。
- [机器汇总](../../.local/research-20260925/live-account-controls-v3/review-summary.json)及[实际清理检查](../../.local/research-20260925/live-account-controls-v3/cleanup-verified.json)。
- [构建与测试清单](../../.local/research-20260925/profile-admin-validation/build-manifest.json)及[生产验收](../../.local/research-20260925/profile-admin-validation/runtime-smoke.json)。

旧 exact-account-control/start.py 是针对旧 q5/q6 额度状态的历史入口，不作为本轮重跑入口。八账号实验使用独立冻结工具和事前计划；原始结果不应被后续执行覆盖。
