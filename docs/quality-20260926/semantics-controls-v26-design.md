本轮诊断检验当前 new1/new2 的原题分歧是否对“取出前可以选择形状”这一语义敏感，而不是直接测量普遍能力或服务端隐藏算力。任何澄清题正确答案都只能记诊断成功，绝不能回填为原题恢复。

历史不是空白：2026-09-24 q4 已完成四次真实请求，原题direct/transparent分别29/29，澄清形状选择21，完全盲抽29。旧报告 integrity_valid=true、四个完整终态；旧身份SHA9f59d814253ef32f688f7972f8cb09c1a50ab29f508f850b4ee8dc2b413d02eb不同于当前new1/new2。对本地已保存plan/report/results/summary搜索 clarified_shape_choice/blind_control，仅发现这组q4语义实测，未发现当前两新用户对应控制。证据细目及文件哈希在v26/history-math-review.json，不把这次计划描述为首次发明该实验。

数学结论依赖可操作能力。原句“不同的形状靠手感可以分辨”，按允许在袋内先摸清形状再决定取出解释，可以选择圆形/星形，不能选择口味。“活动前决定数目”并不禁止预先定好形状配额。若反而解释成只能取出后辨别，或者必须完全无选择地摸取，就成为不同的盲抽问题。原评分合同保持21；澄清控制只消除这项歧义，不更改原评分合同。

独立下界用四个最大失败区域核验，无需依赖模型答案：尚未配出目标时，圆/星数量分别不能超过(8,17)、(24,4)、(17,10)、(15,11)中的至少一个矩形。这四种对应圆仅西瓜、星仅西瓜、两形状均无苹果、两形状均无桃子的极大失败结构。枚举25×18形状配额，最小同时落在四个失败区域之外的总数为21，唯一固定配额为9圆+12星。完全盲抽最大失败集合为24圆+4星西瓜=28，四个区域的最大总数也是28，因此保证数29。另实跑既有独立201600组成动态规划，允许取后观察口味再选择下一形状也仍需最坏21；盲抽最坏29。全部答案与证明只在离线证据，不进入模型上下文。

最小新干预沿用已审核的两句能力替换：clarified_shape_choice 明确取出前能在袋内分辨并选择形状、不能辨别或选择口味；blind_control 明确形状和口味均不能预先辨别或选择。数字序列、原表、目标、“活动前决定数目”和原instructions不变；没有21、29、9+12或算法/抽屉原理等解题提示。建议根任务已定的2账号×3variant×2镜像，原题同批对照，全部fresh完整firstturn；单格最多一次，不刷新/换号/补格。题面改变本身是干预，非original benchmark恢复。

新增project_prompt_variant(body,captured_head,variant_id)仅作用原CLI完整body中恰好一处与原题UTF-8 hash cd771ba38f8f75696ba703c7d3e91e5273588c91cb8c4145e3ce4b077c075c3f完全相等的user/input_text字符串token。允许同请求已有system/developer/environment项；拒重复匹配、非user角色、错误content类型及任意层JSON重复成员。baseline完全返回原字节，包括原字符串转义。两个诊断variant只重新编码这个字符串，prefix/suffix逐字一致；JSON大整数、指数词法、未知字段、itemID、session/cache/metadata、instructions和headers次序/值均不改。head只更新body_sha256/body_bytes。

8项测试覆盖UTF-8/ASCII转义及escaped成员名、多user/context、重复键、额外原题匹配、布尔model、异常JSON、巨大数字/负零词法、原head错误/凭据拒绝、超限与baseline exactnoop。工具是pure offline，不读真实凭据、不发送请求。工具与依赖冻结hash见v26/projection-manifest.json；三种纯题目hash分别baseline cd771ba3…、clarified31b13fe2…、blind375f30e1…。

若new1原题29而澄清21、盲抽29，只能支持该账号在当前上下文对能力语义敏感；原题仍错，不排除其他账号差异。如果澄清仍29，也只能说明澄清不足以改变本次输出，不能推出风控降档。若blind21则其对约束的遵守存在另一问题。协议失败、限流、epoch变化或缺终态均不记数学错答，不以重试选择漂亮结果。
