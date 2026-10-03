# Codex App Server 与兼容接口推理实测

2026-09-27，两个已授权 ChatGPT 测试账号，官方 `codex-cli 0.155.0`，模型 `gpt-6-astra`。本轮没有复现糖果题答29：正式样本中，App Server、PowerPoint及App Server兼容桥共12次原题回答均为21，且推导成立。

这证明本次App Server路径能够正常完成这些任务，不证明账号永久不降智，也不能将差异归因于JSON-RPC协议本身。

## 实测结果

| 路径 | 档位 | 有效答题 | 原糖果题 | 其他题 |
| --- | --- | --- | --- | --- |
| 官方App Server，ChatGPT登录，内置openai provider | xhigh | 10/10数学正确 | 两账号各2次，均21 | 明确形状规则21；可自适应变式17；条件概率166 |
| PowerPoint BPS直接请求，对照 | xhigh | 10/10数学正确 | 两账号各2次，均21 | 同上，均正确 |
| 实际HTTP Chat Completions → App Server → 官方Codex后端 | max | 8/8数学正确 | 两账号各2次，均21 | 明确形状规则21；严格固定配额变式20 |

先做的一次App Server pilot也答21，不计入上述正式28次。所有样本都从新会话开始，没有发送参考答案。提示包含原题、简明推导/不调用工具要求和`ANSWER=<整数>`格式；原题版本额外要求说明规则解释，没有提示21或9圆+12星。

原题正确方案是9个圆形与12个五角星形。12星必同时有苹果和桃子，9圆必有非西瓜，可配成目标组合。合法对手顺序“每种形状先西瓜、再苹果、最后桃子”同时封住20个以内的两种配对；模型输出的充分性与下界均经人工复核。

## 确认实际运行路径

- 二进制SHA256：`660e159a49e823ac8e5986cb238f73158ce4b957d40d9292f8de90862644b501`。
- 通过`initialize`、`account/login/start(type=chatgptAuthTokens)`、`account/read(refreshToken=false)`真实加载服务器现有账号；不触发刷新，不导出令牌到工作站。
- 独立临时`CODEX_HOME`及空工作目录；环境没有继承API Key、模型provider或代理覆盖。`config/read`确认`model_provider=openai`、无`openai_base_url`或provider的`base_url`覆盖，三个系统配置文件不存在。
- 两账号实时`model/list`均列出Astra及`low/medium/high/xhigh/max/ultra`。`thread/start`返回请求模型、provider和推理档位，`turn/start`再次显式指定；没有观察到reroute或工具调用。兼容桥用例还记录了空`instructionSources`。
- 已读取的0.155源码说明：外部ChatGPT令牌与普通ChatGPT登录共用Codex provider，默认后端为`https://chatgpt.com/backend-api/codex`。App Server不是一个单独的“满血模型入口”。模型名及档位的返回值不能独立证明服务端内部模型身份。
- `max`用于较高的单模型推理对照；0.155打包Astra的`ultra`涉及多代理委派、单次调用映射到xhigh，未混入此次禁工具单模型实验。之前将xhigh一概称最高档并不严谨。

公开协议参考：[OpenAI App Server文档](https://learn.chatgpt.com/docs/app-server)。该文档提供登录、thread/turn与流事件协议，具体质量结论来自本次真实调用。

## 兼容封装是否丢信息

本次确实启动了真实loopback HTTP服务，并由HTTP客户端调用`/v1/chat/completions`，不是仅把结果离线改成JSON。

桥将单条user文本、`model`、`reasoning_effort`映射到App Server；逐个`item/agentMessage/delta`写出OpenAI SSE `choices[0].delta.content`。发送端累计文本与App Server完成消息完全相等才发`finish_reason=stop`和`[DONE]`。HTTP客户端再独立重组并逐字比较。

8次均获得成功终态、真实增量、完整文本、stop及DONE；未观察到模型切换或工具事件，临时HTTP服务及App Server进程均清理完成。本结论仅覆盖单条用户文本与流式返回，不代表多轮、压缩、自定义工具、所有Responses字段或完整生产代理已适配。

## 失败与题库纠正

初版BPS对照缺少此前验证过的User-Agent/Accept请求头，产生10次HTTP403，无模型答案。补齐请求头后，同批题目重新执行10次均成功。原始失败保留且不计为答错；未来运行遇到同路由错误会停用该路由，避免重复无效请求。

改数值题最初写“可预定形状数量”，未禁止途中调整。初版参考答案20只适用于固定配额，而两个渠道都提出了更好的自适应17方案。独立minimax确认原题自适应最优21、变式自适应最优17，分别检查29,340和15,960个状态。随后max桥测试用严格固定配额的新题面，正确答案20（11圆+9星）。不能跨两个题面把17判错或比较正确率。

max桥的8次答题及8次流校验全部完成，进程和目录清理成功，但脚本末尾`/api/settings`管理状态检查返回401，使该轮整体退出码为1。因此不能说整个脚本退出成功。独立复查`/health/live`和`/health/ready`均200，`/api/settings`仍401；未改变管理凭据，也未为该状态改动生产。此后置检查不影响已经保存的答题与文本一致性证据。

App Server内置Codex提示与PowerPoint办公提示不同，输入上下文量不同；这不是只改变协议的因果实验。题目数量少，重复题不等同独立能力样本，没有统计依据宣称永久满血或两路总体能力完全相同。29在完全不能控制形状的解释下是正确值，应核对题意后再判断错误。

## 证据与复跑

所有时间目录使用UTC：

- `.local/appserver-quality/20260927T034121Z/`：pilot。
- `.local/appserver-quality/20260927T034243Z/`：App Server xhigh 10次成功、初版BPS 10次403。
- `.local/appserver-quality/20260927T034436Z/`：修复请求头后的BPS xhigh 10次成功。
- `.local/appserver-quality/20260927T034825Z/`：max兼容桥8次答题及逐流校验；脚本快照和hash；后置管理校验失败。
- `.local/appserver-quality/summary.json`、`answer-review.md`：逐项成绩、协议有效性与独立数学复核。
- `.local/appserver-quality/adaptive-verification.json`、`production-after.json`：独立minimax和事后健康检查。

维护入口：[题库及运行范围](../integration/app_server_quality/README.md)。从项目根目录复跑：

```powershell
python tools/quality_app_server_probe.py --route bridge --effort max
```

该命令使用明确固定变式规则的当前题库，与早期自适应题有意区分。此次仅新增隔离测试工具和证据，没有部署App Server代理，也没有将线上PowerPoint推理路由切走。
