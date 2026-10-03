# 当前验收入口

当前版本0.4.0提供PowerPoint、官方Codex CLI和App Server模式，见[本轮上线与验收](docs/RELIABILITY-MODES-2026-09-27.md)、[三模式接入](docs/INFERENCE-MODES.md)及[项目说明](README.md)。

- 本地代码验收：`python tools/acceptance.py`
- 当前 Go 网关与真实 Codex 验收：`python tools/bps_validate.py`
- 三模式真实运行时验收：`python tools/reliability_live.py`
- 官方窗口计价对照：`python tools/verify_account_window_parity.py`
- 前端设置与接入验收：[浏览器报告](frontend/test-results/settings/results.json)

完整入口、环境变量与证据边界见[工具索引](tools/README.md)。没有 live 配置而跳过的测试不能记为真实推理通过，测试通过也不代表已部署。

本文件原 2026-09-17 单账号部署记录已原样归档为[历史验收](docs/history/2026-09/VERIFICATION-2026-09-17.md)，不描述当前状态。
