# Excel、PowerPoint、Google Sheets 糖果题实测

2026-09-27 北京时间约 07:09–07:13。**统一要求直接对话后，两个账号 × 三个入口共 6 次均回答 21，并给出有效的保证策略和最小性证明。**

## 模型和实验条件

- 请求 `gpt-6-astra` / `xhigh`。六次实时 access 查询均列出 Astra 的 low、medium、high、xhigh；xhigh 是这些入口为该模型提供的最高档。响应的 model 和 reasoning.effort 字段也分别返回上述值。
- 使用用户授权的两条已启用线上测试账号，凭据仅在远端内存处理，无刷新、轮换或下载。
- 直接调用三个办公 profile 的真实 BPS 后端，使用各自工具版本；没有启动真实 Excel / PowerPoint / Google Sheets 宿主。
- 每个入口、账号使用新的 task_id / turn_id，首轮 input 只有本次题目，没有此前对话、参考答案或解法。
- 原题内容保持不变，仅把 HTML 空格和表格整理成纯文本。没有向被测模型发送“21 是正确答案”。
- 两条账号并行，每条账号按 Excel → PowerPoint → Sheets 顺序执行；每个单元只采样一次，不筛选正确答案。

## A 组：只发送原题

| 入口 | 账号 1 | 账号 2 |
| --- | --- | --- |
| Excel | 请求 read_sheets_metadata，未给数学答案 | 请求 read_sheets_metadata，未给数学答案 |
| PowerPoint | 21，推导正确，58.297 秒 | 21，推导正确，40.481 秒 |
| Google Sheets | 请求 read_sheets_metadata，未给数学答案 | 请求 read_sheets_metadata，未给数学答案 |

四次工具调用停顿属于办公执行链未完成，不能算数学答错。两份已完成的 PowerPoint 答案都正确。

## B 组：三个入口统一要求直接对话

所有入口新开会话，在完全相同的原题后统一补充：

> 请直接在对话中给出答案和推导，不要读写工作簿、演示文稿或调用办公工具。

这句话不包含任何数学答案或策略提示。

| 入口 | 账号 1 | 账号 2 | 推导核查 |
| --- | --- | --- | --- |
| Excel | **21**，51.174 秒 | **21**，45.318 秒 | 两份均正确 |
| PowerPoint | **21**，46.828 秒 | **21**，44.080 秒 | 两份均正确 |
| Google Sheets | **21**，41.905 秒 | **21**，44.655 秒 | 两份均正确 |

6 次均 HTTP 200、SSE response.completed、status completed；全部第一轮直接给出最终答案，无工具调用，也没有执行脚本预备的“宿主缺失错误续轮”分支。

所有答案都利用形状可选，提出 **9 颗圆形 + 12 颗五角星形**。12 颗星形保证苹果、桃子两种口味同时出现，9 颗圆形保证至少一个不是西瓜，因此可跨形状配对。

下界证明也有效：大部分答案构造每个形状都按西瓜→苹果→桃子出现的可行最坏顺序，两种成功配对至少分别要 9+12 或 16+5，均为 21；Excel 账号 1 使用对 20 颗形状配额分三段构造反例的等价证明。不是仅命中数字。

## 可支持的结论

**本次 Astra / xhigh 在三个 BPS 办公入口上均通过这道糖果题。** 原题引发的工具行为体现了办公 profile 的差异；增加统一对话要求后，三个入口都能直接正确解答。

这是一次单题、两账号的实际表现，不能据此证明整体智力、所有请求质量、不同入口拥有独立模型或独立额度。model / reasoning 字段是服务端返回的标识，不是对底层实际部署模型的独立鉴定。

## 完整证据

- [完整用户可见回答](../.local/office-proxy-verification/candy/answers.md)
- [原题组请求与响应记录](../.local/office-proxy-verification/candy/results.jsonl)
- [纯对话组请求与响应记录](../.local/office-proxy-verification/candy/dialogue-results.jsonl)
- [实验元数据汇总](../.local/office-proxy-verification/candy/summary.json)
- [逐份回答的独立评阅](../.local/office-proxy-verification/candy/review.json)
- [独立数学穷举与评分标准](../.local/office-proxy-verification/candy/reference/REFERENCE.md)

记录保留每次实际 request_id、请求正文哈希、题面哈希、返回的办公 prompt/tools 版本 hash、响应模型与推理档位；未保存 token 或账户私密标识。

## 一条命令复跑

在项目目录执行（会再次产生模型用量）：

```powershell
python .local/office-proxy-verification/candy/run_candy.py --dialogue --output .local/office-proxy-verification/candy/dialogue-rerun.jsonl
```

脚本固定 Astra / xhigh，不可用时记跳过，不自动降档；要求授权的两条启用账号身份匹配。输出文件使用独占创建，避免覆盖历史结果。每个请求最长等待 600 秒；请求异常、超时、工具未完成不归类为数学错误。
