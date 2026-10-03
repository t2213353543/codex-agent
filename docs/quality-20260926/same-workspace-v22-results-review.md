本轮在同一个 workspace 内仍观察到不同用户的答案差异：new1 两次答 29，同 workspace 的 new1owner 两次明确答 21；另一 workspace 的 new2 和 new2owner 均两次答 21。这说明本题的差异不能仅由 workspace 标识解释。它不证明 owner 角色导致质量不同：角色、令牌主体及套餐权益没有被独立隔离。

共执行 8 次原生 HTTP 推理，每个用户两次。预登记顺序是 `new2,new2owner,new1,new1owner,new1owner,new1,new2owner,new2`；全部 HTTP 200、`response.completed`，没有刷新、重新授权、账号写入、自动重试或补位。实验结束于 2026-09-25 16:46:49 UTC（北京时间 9 月 26 日 00:46:49）。

| 格 | 用户标签 | 自动答案 | 独立人工注释 | 输入 | 已缓存输入 | reasoning tokens | 输出 |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 01 | new2 | 21 | — | 6879 | 0 | 2588 | 2780 |
| 02 | new2owner | 21 | — | 6879 | 6656 | 2008 | 2295 |
| 03 | new1 | 29 | — | 6879 | 0 | 509 | 680 |
| 04 | new1owner | null | 21 | 6879 | 6656 | 2070 | 2347 |
| 05 | new1owner | 21 | — | 6879 | 6656 | 2944 | 3179 |
| 06 | new1 | 29 | — | 6879 | 5888 | 516 | 715 |
| 07 | new2owner | 21 | — | 6879 | 6656 | 1872 | 2160 |
| 08 | new2 | 21 | — | 6879 | 6656 | 2070 | 2353 |

04 的原自动结果 `null / conditional_final_answer` 完整保留。独立阅读其正文可确认首句与末尾均为 21，使用 9 个圆形加 12 个五角星，并解释 20 个不能保证。人工注释单独绑定原报告 SHA `2feeb5e891bb322b26b96e4f970cbf31126c472d1c3fcc8c645bdebe9baeb753` 和 final SHA `e800fdb275a71ff740e50816154f3008587e123d87127b6f5b666d7b93a4b104`。自动评分使用执行时冻结的 `0441c9c5…`，没有为本轮改评分器。

new1 的两份 29 都沿用“任意混合取出最多 28 个仍不能配对，所以需 29”的论证，没有使用允许按形状选择的策略；同 workspace 的另一用户明确使用 9 圆形加 12 五角星。目标答案合同仍为原题及既有独立数学验证的 21，题面没有添加解题提示。03 是已缓存输入为 0 的错答，06 是已缓存输入为 5888 的错答；01 则是已缓存输入为 0 的正确答案。本轮再次不能把 cache miss 当作错答的必要或充分条件，返回 reasoning tokens 也不证明隐藏预算或硬上限。

两个 pair 的实际 workspace hash 分别相等，四个 stable user hash 互不相同；实际 AT、Authorization 和 workspace 头与预登记主体一致。owner 标签来自来源数据库的 owner/child 关系，不等于已验证的上游角色断言。两个 owner 的已解码 token claim 是 `team`；child 的既有来源证据为 `self_serve_business_prolite`，这项权益差异没有消除。JWT claim 在此作一致性对照，不能把未独立验签的解码称为签名验证。

冻结 runner 的 `current()` 并未直接使用前置目录采集器较宽松的身份解析：它仅在 `chatgpt_user_id` 为缺失或 null 时退到 `user_id`，不使用 `sub`，拒绝空值或错误类型；workspace 必须由 `chatgpt_account_id` 提供。它逐次核对网关 child、来源 active child→owner 关系、owner 存储 workspace 与实际 owner claim。来源 AT/email 仅内存传递，报告不保存原凭据。

四个账号事前目录 GET 和 usage GET 均成功且允许使用；sol 模型的 52 个字段哈希逐字段相等。模型阶段还有 4 次全组 usage 加每格前后 usage，共 20 次 usage GET，与 8 次推理分别计数。目录一致只说明公开目录一致，不保证实际质量、内部路由或计算分配相同。

本轮八格使用一份新的实际 CLI HTTP 捕获，原文 36,330 字节，SHA `0b2c339f2a2033301f7339b6c4a3ca3015c21f503561b5985df386ee539db6a7`；header 捕获 SHA `7a61334ca8cd88de1b6037cc4f4345f9505b379c421e8909d775a1d306bd2b38`。实际 IPC body、11 个正文顶层字段摘要、全部非认证头、zstd 意图在八格之间一致，每格 helper scope 独立。本轮捕获不同于 v21，不能把两批称为逐字同一请求。原题 hash、model `gpt-5.6-sol`、effort `medium` 和答案合同保持相同。

本轮实际版本固定为来源 image `74c42b0e…`、网关 `a52ad225…`、native helper `9110b869…`。冻结的执行 driver SHA 为 `ee7e006e…`；每次 `current()` 在来源容器内先核五份源码的实际 SHA，逐格读身份的前、中、后三个边界要求 source runtime、四账号代次与预登记相同。没有单独保存每次文件读取的原始 sidecar，因此该说明依据固定执行代码、成功门禁及终态控制回执，不伪称存在额外读取日志。相同 owner 下的 assignment generation 时间戳未固定，也不称它受控。

后续日志修复和 gateway/helper 联合部署发生在本轮模型终态之后，不能用新环境替代上述实验基线，也没有用后部署版本重跑来覆盖原结果。结果独立复核最终 372 项通过，包含完整响应、冻结评分、全部实际 body/header/auth、前后代次和目录证据；04 的人工注释独立于原自动报告保存。37 个服务器安全产物 SHA 与本地逐一匹配；四个原始捕获文件已清理，临时捕获凭据不存在，helper 已回收，模型 unit 为 PID 0、inactive、exit 0。八格窗口的已完成生产网关日志重叠均为 0；该观察仅覆盖已落库的完成日志，不包含外部或未完成行为。

独立产物位于 `.local/research-20260926/workspace-controls-v22/review/`：`results-final.json` 保存八格 final hash、用量、主体代次与逐项检查；`human-annotations.json` 只保存 04 的人工注释；`runner-independent-review.json` 保存执行前严格身份和门禁测试。离线复核命令：

```powershell
python .local/research-20260926/workspace-controls-v22/review/review_v22_results.py --output .local/research-20260926/workspace-controls-v22/review/results-final.json
```

可支持的结论限于本批：new1 与同 workspace 的另一个用户在原题上呈现不同输出。每个用户只有两次样本，套餐、角色、账户历史及服务端状态相互混杂，尚未找到该差异的根因。
