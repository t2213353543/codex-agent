# 账号状态公开线索与观测边界

检索日期2026-09-26（本地时间），以下实际打开原始发布页面，不将搜索摘要或社区猜测当成根因。

1. OpenAI Support于2026-09-14公开说明：账号活动可能影响某些模型/功能的临时可用性，并会重新评估；未公开检查细节。这证实存在账号级限制这一可能性，不识别本项目new1/new2是否受限，也没有给出当前请求的模型替换或算力证据。
来源：https://community.openai.com/t/gpt-5-6-sol-repeatedly-hits-selected-model-is-at-capacity-in-codex-august-31-2026/1393895/11

2. 官方帮助页列出workspace权限、用量、安全疑虑与账号变更等可能因素；它是排障分类，不是本批账号诊断。读取没有触发重新登录、密码重置或账号安全设置改变。
来源：https://help.openai.com/en/articles/10258669-troubleshooting-model-feature-access-issues

3. ccodex-sleep-state原作者README讨论的是X-Codex-Turn-State字符长度/封装形状，不是HTTP 292/312状态码；作者明确说明长度不是OpenAI公布的质量指标。个人与Team/Business规则不同，不能把个人长度经验直接套用本批prolite账号。此工具的成功叙述也不能作为本网关恢复证明；没有安装或运行其注入功能。
来源：https://github.com/gylive/ccodex-sleep-state

4. 一位用户公开报告五个Pro账号中两个持续输出较差，并提交同提示词图形对照；该用户同样区分可观察差异与内部机制猜测。帖子为一手经验，不是随机对照或官方模型识别结论。其GPT-5.3历史路由说法需另核原评论；本轮打开的GitHub issue只呈现用户正文，没有获取到所引官方评论，不据此推广到gpt-5.6-sol。
来源：https://www.reddit.com/r/codex/comments/1wmk8i7/five_pro_20x_accounts_two_persistently_degraded/
来源：https://github.com/openai/codex/issues/11189

本地核验：v25/v26/v15的19份HTTP报告全部HTTP200，均保存turn-state头名及值的哈希，但没有长度，不能从哈希还原292/312等。下一步只做首个响应头的被动长度/哈希观测，原题和请求不变，不注入、解密、跨账号复用或采集后重放state。长度与答案如相关，也仍需独立因果验证，不能将长度当作官方标签。
