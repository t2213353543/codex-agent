# App Server 推理质量验证

使用两个已授权测试账号、官方 Codex 0.155.0，隔离 CODEX_HOME 与工作目录。令牌只在服务器进程内使用，不刷新账号、不部署服务。每题建立独立会话，答案键不发送给模型。

从项目根目录复跑 OpenAI 兼容文本接口：

```powershell
python tools/quality_app_server_probe.py --route bridge --effort max
```

直接 App Server 与 PowerPoint 的 xhigh 对照使用 `python tools/quality_app_server_probe.py`。`--pilot` 仅运行首账号的首题 App Server；`--case <id>` 可选择题目。每个完整运行额外重复第一题一次。

`cases.json` 的改数值糖果题明确要求固定两种形状的配额，答案20。早期实验题未禁止自适应，正确最优值为17，不能混用答案键。原题按能够选择形状解释为21；不能控制形状时的29并不能单独证明模型质量问题。

工具分工：`tools/quality_app_server_probe.py` 在本地调度、保存证据；`quality_app_server_remote.py` 负责服务器上隔离登录、逐题调用和清理；`quality_app_server_bridge.py` 是仅支持单条用户文本的loopback Chat Completions/SSE验证桥，不是完整生产代理。

有效答案要求成功终态、无工具调用/模型切换错误，App Server增量与完成文本一致。桥接额外要求HTTP客户端重组文本与App Server原文完全一致，收到`stop`及`[DONE]`。请求失败或超时单列，不能算模型答错；数值正确仍须复核证明。

所有结果写入 `.local/appserver-quality/<UTC时间>/`，包含请求题面、脚本快照、脱敏答案及运行摘要。App Server与PowerPoint内置提示不同，本实验比较实际路径表现，不证明协议造成质量差异，也不保证长期模型质量。
