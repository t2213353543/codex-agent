# Compact 失败计量与读取取消

核对时间：2026-09-29 04:55（UTC+8）。已验证生产仍为 **0.4.20 / PID 2479613 / binary f71fc1a2…5e5c4ee**。本页运行修改在 0.4.21 最终源 `65fb8efde6f14f05…` 中保持，完整 Go/vet、新增 11 顶层/65 含子项本地回归、重新执行的 20 项真实 PG、单轮 race 和十一组 live 均已通过；实际发布回执另核。见[A 修订 Go/source 核验](../.local/collab-a-20260928/compact-release-review/REPORT-revised-go.md)及[最终合同指纹](../.local/collab-a-20260928/compact-release-review/compact-contract-final-source.json)。

后续发布状态（05:14）：原 `da72d726…` 包只完成 stage，尚未 preflight/activate。用户优先要求的“创建按读取返回默认开启”和更新不中断连接正在新的工作集中实现；这些后续代码不属于上述 `65fb…` 已验收包，须有各自新源码证据。生产身份仍以实际回执为准，不能把本页已通过的 compact 验收理解为新计费/切流方案已上线。

首轮 `814ec2b…` 的 PG 用例在脱敏 JSONL 比较原 ID 时实际失败，随后仅修测试的伪名期望并增加原 ID 不泄露检查，没有修改产品脱敏。首轮 live 另有摘要事件未观察到的失败；测试编辑途中一次括号错误只导致编译失败，没有新增 provider 调用。三类原记录独立保留，修订后的真实 PG `204449.389135Z` 已通过；不能用旧绿拼接成最终门禁。见[首轮边界](../.local/collab-a-20260928/compact-release-review/initial-gate-boundary.json)和[精确单测试文件差异](../.local/collab-a-20260928/compact-release-review/final-test-only-source-binding.json)。

完整上游 JSON 是否报告了用量，与其中压缩状态能否交付和续接，是两个不同事实。本次 native compact 和 JSON→SSE 共用 `bps.CompactResponseMetadata`：只接受有效 UTF-8 JSON 根对象、非空 ID、允许的终态 status 和基本 output/error 形状，生成仅含根 id/model/过滤后 usage 的计量视图。明确 failed 可以缺 output；其它状态缺 output 不采信。非法计数保留未知，不回退到低优先级别名补造读数。嵌套 `response`、opaque output 和任意扩展不进入这个薄视图。

Native 分支在完整合法计量视图上补充已验证根 status/error/incomplete_details 及原有服务元数据，再调用记录函数，避免通用记录逻辑自动解包嵌套 response 而偷换 ID、model 或 usage。记录函数会更新请求状态，因此所有拒绝分支随后明确设为 error；不能把“已计量”当作完成或成功。真实计量保留，上游 HTTP 与公开 502 各按实际记录，不增加重试或保存被拒绝的历史。

| 场景 | 结果和计量 |
|---|---|
| 完整、可识别的 failed/incomplete/cancelled compact JSON | 可保留真实 usage/model；缺少可用压缩状态时仍失败，不交付状态或保存 history。 |
| completed 但无实际 compact state，或 completed 同时带 error | 不能公开成功；合法根计量可保留，逻辑请求明确 error。 |
| 非法 JSON、无 ID、无效 status/output/error 形状 | 不凭片段制造可信用量或返回模型。 |
| 合法 legacy compact 省略 status，含非空 compaction.encrypted_content | 仍可成功；原生 JSON 字节保持，授权、owner 提交和历史保存按原顺序执行。 |
| 显式 status:null 或空字符串＋state | 原 native 容忍行为收紧为拒绝；不同于合法的省略 status，作者已明确记录此差异。 |

JSON→SSE 桥接在发布 done 事件和调用成功提交之前，要求 completed 响应至少有一个真实非空 compaction state。完整响应被拒且没有已有 `result.Response` 时，才通过独立计量 callback 保留过滤后 metadata；callback 不能写客户输出、标记成功或提交历史。合法成功仍保留原完整 opaque 对象，不能用薄计量视图替换公开结果。

取消处理分别覆盖等待响应头和 native compact 正文读取两阶段。仅当实际读取/传输错误携带请求 context 的取消原因、且没有已记录终态/失败时，才记为 aborted、client/canceled 或 client/timeout，停止编造 502 错误正文。后来的 context 取消不能改写已有上游 EOF、内部超时或明确失败。取消判定必须先于会修改 Status/TerminalStatus 的计量调用。

原取消复现中曾把“服务器已发送片段”误当作“客户端已进入 Body.Read”，首 after 实际命中等待响应头的另一阶段。主任务已按客户端 Body.Read 信号修正 fixture，`cancel-before.log`、`headers-before.log` 及当前正确时序的通过日志分开核；首 after 的错阶段失败仅有当时工具输出/协作记录，本组未定位到独立不可变文件，不把目前已更新的 `cancel-after.log` 说成保留了该旧失败。不把不同阶段拼成同一红绿。直接 handler 的正文取消记录可为 http_code=0、公开字节0，不声明向已经取消的客户送出了 HTTP499。

A 独立成功历史对照在已部署 .20、首私有候选及共享 metadata 候选上各两子项通过：省略 status / completed 的原生 JSON 逐字一致；input100/output7/read已知0/create20；存储的 Request.input 只保留完整压缩对象、Output为空，owner/session 不变；下一轮真实 Prepare 只有一个原对象和新输入，不含旧未压缩明文或 previous_response_id。见[兼容与历史报告](../.local/collab-a-20260928/compact-history-consumer-review/COMPATIBILITY-REPORT.md)。这些是合成本机 HTTP 对照，完整失败矩阵和取消时序由主任务原回执独立证明。

Compact 在现有缓存统计中属于 inference，不因 endpoint 或 NativeCompaction 标志变成 control。完整失败新增真实已知用量后，已知样本和计量总数可以增加；partial cancel 的未知读/写不得补零。管理 Errors 包含所有非 ok，监控则将 errors 与 aborted 分列，但非成功明细仍可含取消。这里保持既有统计口径，不宣称取消从所有错误界面消失。

发布前固定 .20 自然窗口已有 759 请求、747 成功、12 失败；加权缓存读取 84.0792%，creation 与未知值独立保留。它早于本修复上线，且包含会话提交/账号选择等本版未解决的错误，不能当作 .21 的效果验证。窗口与三条旧验收请求的时点及计量核对见[自然基线报告](../.local/collab-a-20260928/compact-release-review/predeploy-natural-window-review.md)。
