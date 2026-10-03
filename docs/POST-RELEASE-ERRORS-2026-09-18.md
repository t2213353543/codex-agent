# 上线后被动错误复核

观察窗口：2026-09-18 12:00:54.850574 至12:25 +08。运行binary仍为a66，live/ready均200。此流程只读取生产日志元数据、错误来源/阶段和资源聚合，不发送模型请求，不导出原始错误正文或账号标识。

## 实际记录

| 记录来源与阶段 | 数量 | 结论 |
| --- | ---: | --- |
| HTTP200、记录status=ok、绑定代理 | 125 | 该请求记录成功，不能推算同时在途或上游容量 |
| 上游http_status 401，无代理 | 2 | 认证失败；另次布尔提示采样包含expired，不能单凭401判断所有账号撤销 |
| 本地account_selection 503、models | 1 | 匹配没有启用可选账号，不是上游503过载证据 |
| 上游http_status 400、绑定代理 | 1 | 上游请求拒绝，未凭任意错误文字假定具体参数或环境根因 |
| HTTP200、记录error、上游response_event | 1 | 上下文窗口相关失败，不能因传输200算成功 |

共130条，在这个观察窗口没有记录到HTTP502/429。流量并非本次受控压测，未控制账号、提示、并发、客户端重试或出口条件；没有基线可比，因此**不据此宣布502/503/429已解决，也不证明更换IP或传输恢复模型能力**。

记录：`.local/gateway-environment/20260918T042501Z/inventory.json`。早一条12:17聚合129请求与此时130请求不矛盾：后续新增1条models本地拒绝。这些请求并非本线程压测工具产生。

## 诊断工具修正

`inventory_remote.py` 以前仅按错误文字子串分类，新版本读取并保留白名单 `error_source/error_phase`，保持来源未知时不推断：

- 明确代理403/407/429与上游认证/限流分开；通用JSON `type=proxy_error` 本身不能证明代理出口失败。
- JSON `error.code/type` 的已知语义先于偶然出现在message中的authentication/transport单词。
- 客户端取消不再被外层connection失败描述吞掉；HTTP200的流内终态错误仍按实际record.status处理。
- 泛化 `server_error` 不等于overload；上游response_event错误与HTTP错误分别保留。
- 新 `--since` 支持时区明确的部署时刻，查询继续只读、5秒SQL截止、最多50000条；不知道的字段映射other，不导出任意内容。

15个合成回归方法通过，包括mock执行实际聚合主程序，确认成功/失败取记录状态而不是HTTP200。此修改只改独立诊断工具，不修改已发布Go行为或配置。

复跑当前发布窗口：

```powershell
$env:CODEX_AGENT_SSH_PROXY='http://127.0.0.1:7897'
$env:CODEX_AGENT_CLASH_REGION='hk'
python tools/gateway_environment.py --since 2026-09-18T04:00:54.850574Z
```

剩余高并发验证仍是30/50/70/100受控实测及同任务、同账号条件的错误率/延迟对照；此前平台拒绝的启动动作没有以被动数据或合成请求替代。
