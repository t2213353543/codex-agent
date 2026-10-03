# Codex / ChatGPT 质量与环境相关原始报告

检索与原文阅读日期：2026-09-25（Asia/Shanghai）。本轮只查公开材料，没有调用用户账号、模型或修改网关代码。选取 10 个来源，区分官方已说明的行为、作者实际观察与作者对原因的推测。

目前最直接的官方证据是：**ChatGPT 的异常登录活动可能导致临时模型访问降级**。公开原发材料也有 **UA/网络变化影响 Cloudflare 准入** 的具体观察。但本轮没有取得“只修改 UA、device ID 或 IP 标签，即使相同 Codex 请求从 29 恢复 21”的受控证据；更没有官方公开确认 516 代表某个低计算集群或固定账户标签。

## 1. 官方：异常登录可能触发临时访问降级

[OpenAI：模型功能访问问题](https://help.openai.com/en/articles/10258669-troubleshooting-model-feature-access-issues)。页面显示 **Updated: 25 days ago**，不擅自换算发布日期。产品分类是 **ChatGPT**，没有限定 Codex CLI 或具体模型。

官方把多次登录失败、未知位置访问列为安全异常例子，并说明可能造成 temporary downgrade；还列出权限、用量及账户变更等访问限制原因。这确认安全/登录环境与访问限制存在联系，**未描述 UA/TLS 指纹规则、Codex 服务端路由或推理 token 上限**。该页语境包含通知、模型/智能选项不可用，不能据此断言所有降级都是不提示的后台换模型。

本地用途：支持检查真实登录链的 device/cookie/身份一致性，以及实际限制通知；不能单凭这页把一次错答定性为风控。主任务已浏览原文，保存 `.playwright-mcp/page-2026-09-24T20-51-05-821Z.yml`。

## 2. 原发经历：地区/时区调整后短期恢复，但未隔离变量

[Linux DO：账号环境调整经历](https://linux.do/t/topic/2896640)，**2026-09-13**，作者 kafukasun。作者描述 Pro 20x 账号，在 ChatGPT 网页、官方 Codex 与 Hermes 间对比，未提供准确模型版本。

其经历是登出静置、换美国住宅出口仍首轮正常次轮异常；改电脑区域与时区并再次静置后，官方 Codex 约一小时表现正常，换回 Hermes 又异常。作者开头明确不确定效果是否永久或通用。没有相同正文、请求头、TLS、模型返回字段或逐轮评分记录，多因素同时变化；“Codex 特殊指纹”是作者解释，**没有官方确认**。

本地用途：先无模型捕获真实 CLI，查 locale/TZ 是否进入请求及通过何种字段进入，避免凭帖子为网关编造美国地区/时区。该经历只提供候选变量。主任务浏览快照 `.playwright-mcp/page-2026-09-24T20-57-11-315Z.yml` 已核对正文。

## 3. 一手 UA/网络矩阵：观察到 403 → 401，不是答案质量恢复

[Codex #17860](https://github.com/openai/codex/issues/17860)，**2026-04-15**，Codex CLI **0.120.0 / ChatGPT OAuth / Ubuntu 24.04 WSL2**。原作者报告同账号、同代理节点，Linux 被 Cloudflare challenge 阻断而 macOS 可用；TLS 栈导致阻断是作者推断，不能把 binary strings 数量当握手证据。

更具体的[2026-05-17 原发评论](https://github.com/openai/codex/issues/17860#issuecomment-4469462357)报告：macOS+家宽可用、Crostini Linux+家宽被阻、同 Linux+手机热点可用；curl 改 Chrome-like UA 后从 403 challenge 变为 401。其他评论也有 macOS 同样失败的反例。**未见已读评论中 OpenAI 确认其 TLS 根因；401 更不证明已完成认证或推理。**

本地用途：支持把出口、应用头、TLS 与响应状态分层记录；真实 UA 修复应先验准入/握手，不能把 403 消失当成质量问题已解决。

## 4. 糖果题最早相关讨论：第三方路径差异，也存在反例

[CLIProxyAPI 原讨论 #3937](https://github.com/router-for-me/CLIProxyAPI/discussions/3937)，**2026-06-20**，由原 issue #3936 转换。原作者 haowang02 使用 **GPT-5.5 / macOS / 新加坡出口**，比较官方交互 CLI、SDK、CPA、OpenCode，报告第三方路径更容易出现 29 和 reasoning 516。

原文没有发布固定整份 body/header/TLS 的配对捕获；“账号/IP 无风控”是作者自述，不是服务端字段。讨论同时有直接官方客户端也出现 516、第三方仍可正确的报告。[CPA 维护者的评论](https://github.com/router-for-me/CLIProxyAPI/discussions/3937#discussioncomment-17446592)把工具与系统提示差异作为另一个候选原因；CPA 维护者不是 OpenAI 官方。**没有确认某个 UA/device/IP 标签导致降级。**

本地用途：这正是同题研究的原始线索，支持固定全请求而非只固定题面。现有 schema 4 与 exact-request 摘要可以检查其未控制的工具、instructions、metadata 和 session 差异；不照搬帖子中的因果结论。

## 5. 评测脚本作者确认 exec 与交互 CLI 有 UA 差异，仍保留随机性解释

[codex-candy-eval #4](https://github.com/haowang02/codex-candy-eval/issues/4)，**2026-06-29**。报告者称同题同 high，脚本失败、单独交互提问成功；准确 CLI 版本未提供。

作者在[当天回复](https://github.com/haowang02/codex-candy-eval/issues/4#issuecomment-4833185320)指出脚本走 `codex exec`，与交互模式不同，至少 UA 不同；同时明确可能是随机性。没有只改变 UA 的实验，也没有逐字节请求对照。**这属于评测项目作者说明，不是 OpenAI 承认 UA 质量分流。**

本地用途：既有 CLI/SDK 比较必须记录实际 originator、UA、系统提示和工具目录。真实环境 header 修复验收应区别交互/exec 的合法身份，不为了“统一指纹”强行改成同一个字符串。

## 6. 官方 Desktop 也能复现同题 29/516

[Codex #29353](https://github.com/openai/codex/issues/29353)，**2026-06-21 UTC 发帖，正文测试日期 2026-06-22 Asia/Shanghai**。条件为 **官方 Codex Desktop / GPT-5.5 / xhigh / 新会话**，具体应用版本未写。

作者列出 5 次相同英文题：2 次答 29、reasoning 516、直接 final；3 次答 21，reasoning 为 3904–7766，并有 commentary。中文题也有同类结果。它说明作者在官方客户端观察到了相同现象，不能把第三方网关当作必要条件。**issue 为 closed / not planned；可见评论没有官方机制解释，关闭不等于证实或修复。**

本地用途：与我们绕过本网关仍可出现 29 的参考一致；阶段、usage、答案应独立记录。不能把“先发 commentary”当保证正确的协议要求，也不能通过插入提示改变原题来冒充修复。

## 7. 大样本 token 聚集报告：相关性强于单例，仍非预算证明

[Codex #30364](https://github.com/openai/codex/issues/30364)，**2026-06-27**。作者汇总其 **2026-02-01 至 06-27 Codex token_count 日志**，报告 390,195 条记录、865 会话，以及 GPT-5.5 的 516/1034/1552 聚集。

作者明确表示不能证明隐藏推理被截断。数据混合模型、月份和任务，不是固定请求随机对照；正文没有原始全量记录供本次独立复算。issue 状态是 closed/completed，但本次匿名 HTML 只取得前 12 条评论，后续 timeline 未完整取得，**不能将关闭状态写成官方确认预算机制或已恢复**。

本地用途：可以按实际响应归属、阶段和完整 usage 统计分布；必须与独立任务评分分开。当前新增 reasoning/summary/encrypted-content 摘要可研究同 token 是否同内容；密文 hash 不同仍可能来自加密随机性。

## 8. 有具体请求级复现的 effort 丢失：模型目录回退

[Codex #30697](https://github.com/openai/codex/issues/30697)，**2026-06-30**，**CLI 0.142.4 / 第三方 Responses-compatible provider / `provider/group/gpt-5.5`**。

作者给出配置和实际出站捕获：本地配置 xhigh，但发送 `reasoning: null`；多段模型 slug 没匹配到元数据，fallback 的 reasoning capability 为 false。其添加准确 model_catalog_json 后恢复发送 effort。**这是用户的可检验请求级报告；issue open，唯一可见自动去重机器人回复不是官方根因确认。**

本地用途：解释为何应比较实际 prepared/native 请求，不只看 UI/config。我们已经检查固定官方目录与 reasoning 实际字段；不把旧版本、自定义 slug 的具体问题扩展成所有 OAuth 账号的隐藏降档。

## 9. 请求模型与响应 model 字段不一致的一手报告

[Codex #11189](https://github.com/openai/codex/issues/11189)，**2026-02-09**，**CLI 0.99.0-alpha.10 / Pro / macOS**。作者配置 GPT-5.3-Codex，却在 `response.created.response.model` 观察到 GPT-5.2 dated ID。

[次日一条对照评论](https://github.com/openai/codex/issues/11189#issuecomment-3877459715)报告同命令在个人与工作账号返回不同 model 字段。这比让模型自报名字更具体，但仍缺固定整份请求、权重级服务证明。issue closed/completed；本次只读到前 12 条匿名可见评论，**不能据关闭状态补写官方解释**。

本地用途：保留 created/in-progress/terminal 的 model 字段、请求 ID hash 与请求 model。它提供可检查的差异类型，不证明当前同名返回就没有服务端变化，也不证明所有自报名称差异都是替换模型。

## 10. Astra 容量事件之后质量下降：作者也明确样本和上下文限制

[Codex #44851](https://github.com/openai/codex/issues/44851)，**2026-09-11**，**ChatGPT Pro / GPT-6 Astra / CLI core 0.153.4 / macOS**。作者对比 09-08/09-09 与容量错误后的 09-11 鹈鹕 SVG 任务，报告输出规模、推理量和自检行为降低；检查了所请求 model、effort、tier，并用干净 `codex exec` 重试验证现象。

作者主动列明样本小、生成任务波动、桌面外壳与技能列表变化、部分 prompt/project 不同，且无法确认真正服务模型。**已读 7 条评论无 OpenAI 根因确认，自动去重不算确认。**

本地用途：保留容量/协议错误与完成答案的不同状态；比较正常完成样本时记录工具和正文差异。不因容量错误先发生就把后续低 token 或小文件认定为永久账号降级。

## 对环境/IP/设备假说的可用结论

- **官方已说明的范围**：ChatGPT 安全异常可能影响模型功能访问；没有公开到本次找到的材料中的 UA、did、JA3/JA4 到 reasoning budget 的映射规则。
- **有具体观察、但终点不同**：UA/出口可改变 403 challenge/401 状态；这是准入差异，尚不是已认证成功响应的质量差异。
- **有第一手质量经历、控制仍不充分**：地区/时区、客户端、交互/exec、账号与容量事件都有人报告关联；未得到只改单一环境字段、保持完整 body/session/auth 状态可比的公开质量恢复证明。
- **本地下一步对应**：先修并核验真实 runtime UA 与登录 device/cookie 一致性；无模型捕获验证字段真实性、同一值生命周期、HTTP/WS 一致。需要质量验证时再沿用预登记的完整请求和隐私安全响应证据，不能用帖子代替真实判定或新增隐藏标签。

## 获取方式与证据边界

`web.run` 两次返回后端 400；随后使用公开 GitHub search API 找到原帖，再下载 GitHub 原始 HTML 与嵌入的正文/评论 JSON 阅读，不停留在搜索摘要。GitHub core API 遇匿名限流后使用公开页面；部分长 timeline 只有首批评论，已逐条披露。官方帮助页及 Linux DO 页采用主任务真实浏览器快照；不是绕过登录获取私有内容。

原始页面与解析文本保存在 `.local/research-20260925/public-quality-sources/`。没有把全文转贴到报告，没有将同一讨论的跟帖人数当作独立受控实验，也没有将 GitHub 的 bug 标签、CONTRIBUTOR 身份或机器人评论当作 OpenAI 官方确认。本次没有找到可直接归因于当前网关用户的公开事件公告；不能据此推断不存在服务端问题。
