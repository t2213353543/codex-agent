# v11：只替换 prompt_cache_key 的完整结果复核

2026-09-25。独立离线审阅完成 **448 项检查，0项不一致**。本审阅不联网、不读取凭据、不调用模型。正式实验实际发送8个native HTTP POST，全部HTTP200；其中7个完整答案、1个SSE终态error无答案。无模型目录GET预热、无refresh、无重试。

**新UUID cache key没有让new1的已完成回答恢复21，也没有保证缓存未命中。** new2在固定key与新key下均答21；new1的两个固定key及一个成功的新key请求均答29，另一个新key请求失败而没有答案。这个小样本不证明隐藏集群或永久账号标签。

## 逐格结果

| 格 | 账号 | cache key | input tokens | cached tokens | reasoning tokens | output tokens | 答案/状态 |
|---|---|---|---:|---:|---:|---:|---|
| 01 | new2 | 固定 | 6879 | 0 | 1552 | 1766 | 21，原parser未识别，独立人工明确判定 |
| 02 | new1 | 新UUID | — | — | — | — | HTTP200后SSE error，无答案 |
| 03 | new2 | 新UUID | 6879 | 6656 | 1034 | 1241 | 21 |
| 04 | new1 | 固定 | 6879 | 5888 | 516 | 643 | 29 |
| 05 | new1 | 固定 | 6879 | 5888 | 430 | 676 | 29 |
| 06 | new2 | 新UUID | 6879 | 6656 | 2070 | 2299 | 21 |
| 07 | new1 | 新UUID | 6879 | 5888 | 516 | 814 | 29 |
| 08 | new2 | 固定 | 6879 | 6656 | 1671 | 1961 | 21 |

01原文开头明确“最少需要取出 **21 个**”，正文说明选择9个圆形加12个五角星，末尾再次明确21最少。本审阅将 `manual_annotation` 单独附入派生结果，并绑定最终文本SHA；原报告的 `answer=null` 和自动parser规则完全保留。

02只有已保留的错误code/type/message哈希和通用other标签；没有原始错误文本证据，因此不猜测它是容量、权限、cache key不支持、token撤销或降智，也不把它算成数学错答。HTTP200表示流建立，不等于响应成功。

## 输入和实际发送边界核对

正式计划在 `09:32:14Z` 写入，所有模型调用均在其后。8格执行顺序与完整镜像计划一致，new1/new2各有2个固定key、2个新key；四个新UUID分别不同，固定key四格一致，总共5个key/body变体。原始冻结正文为36,336字节、11个顶层字段，SHA `12c25dcb8577baaa2133ae44c5e9abf4a818db3e40a8096275b4c0ce8a412b78`。原题仍是相同糖果题、sol/medium、答案判据21。

对每个fresh变体核对预登记字段span的前缀/后缀原始bytes摘要一致；只有顶层prompt_cache_key value不同，另外10个字段包括input、reasoning、client_metadata、原题/指令/工具的实际native writer摘要均与捕获一致。固定key四格的全部11个字段摘要一致。没有联动session、thread、安装ID或设备信息来改变题目环境。

8格实际native IPC发送body SHA与各自计划/投影pin一致，13个非认证header的名称、原名称hash、顺序和值摘要全部相同。每格实际Authorization token摘要和workspace header摘要匹配该格登记账号；所有模型调用前后和发送前的identity epoch一致，16个前后usage GET均200且allowed。每格独立fresh helper，8个scope和IPC connection ID各自唯一，helper实际SHA为 `9110b869e65db6bf7664e39fcfe3b2ec17fa7a0207ddbeccf28d73dbc12ccce8`、lifecycle=cli_request；均正常退出、已回收、无强杀、socket已清理。

7个完整回答的返回model/effort/tier等controls一致，response ID均不同。程序结束单元MainPID=0、inactive/dead、ExecMainStatus=0。raw capture已按执行收据删除，因此本次离线审阅依靠预登记span/字段证据与实际writer摘要交叉核验，未声称重新从已删除原文逐字节重算。native IPC观察也不等于上游TLS线上抓包。

## 结论的实际范围

- 完成的新key请求全部仍有正缓存：new2两格6656、new1一格5888。新key不是强制cache miss开关。
- new2首个固定key请求cached=0仍答21；new1正缓存仍答29，再次支持此前历史反例，不能用cached0作为降智判据。
- 在这批固定正文与头下，只更换prompt_cache_key未改变两账号完成回答的分组。一个fresh格无答案必须单列，不能把实验写成8个完整数学答案。
- 两个账号并非随机抽样，固定metadata可能保留服务端状态；时间、路由和采样仍未被独立控制。没有据此识别风险标记或内部算力集群。

## 复核产物

[results-final.json](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cache-key-controls-v11/review/results-final.json) 包含逐格结果、manual annotation、输入输出和全部读取文件SHA清单；[review_results.py](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cache-key-controls-v11/review/review_results.py) 可只读重跑。

独立复核脚本另在公共证据副本上做7种mutation：实际bodyhash、实际authhash、非cache input摘要、helper cleanup、把无答案error伪记29、篡改01人工21锚点、usage epoch。全部被正确拒绝，原始报告未改。[test_review_results.py](C:/Users/Administrator/Desktop/项目/codex-agent/.local/research-20260925/cache-key-controls-v11/review/test_review_results.py)

```powershell
python .local/research-20260925/cache-key-controls-v11/review/review_results.py
python .local/research-20260925/cache-key-controls-v11/review/test_review_results.py
```
