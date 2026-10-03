# 主节点 Codex CLI

用户要求在主节点安装 Codex，沿用当前配置，并将服务地址和密钥替换为 CC Switch 中 Codex 类型的 `666AI pro` 条目。操作已完成，不改变本机当前 Codex 配置或 CC Switch 选中项，不修改或重启现有业务服务。

## 安装结果

- 节点：`43.225.196.170:12865`，root，Linux x86_64。
- 官方 Codex CLI：`0.155.0`，命令 `/usr/local/bin/codex`。
- 由[官方独立安装器](https://learn.chatgpt.com/docs/codex/cli)安装到 `/root/.codex/packages/standalone`，无需修改系统 OpenSSL/OpenSSH 包。
- 当前配置源：`D:/ChatGPT/CodexHome/config.toml`；主节点配置 `/root/.codex/config.toml`，认证 `/root/.codex/auth.json`，文件权限均为 `0600`。
- 服务地址：`https://666ai.top/v1`；密钥来自本机 CC Switch 数据库的 Codex `666AI pro` 条目，原服务商内嵌 bearer 已移除。密钥不记录在此文档或验收报告中。
- 模型 `gpt-6-astra`、推理强度 `ultra`、原上下文设置及其他非平台专属参数保留。
- 用户后续要求完全权限：主节点默认配置已设 `sandbox_mode = "danger-full-access"`、`approval_policy = "never"`，CLI 配置校验通过；原文件已在同目录私密备份，其他设置及认证未改变。新启动的 Codex 会话读取此默认配置。
- 配置引用的指令文件已复制至 `/root/.codex/prompts/cc-compliance.md`。
- Linux 不加载 Windows 通知程序、Windows 沙箱和桌面设置、Windows 项目信任路径、本机插件市场路径；Windows 专用 node_repl/cua_repl MCP 禁用。插件标志保留，但桌面插件缓存未迁移。
- 保留 Playwright MCP，补齐官方 Node.js `v24.21.0` LTS、npm/npx `11.19.0`；安装在 `/opt/codex-node-v24.21.0/runtime` 并校验官方 SHA256。原配置命令 `npx @playwright/mcp@latest --version` 返回 `0.0.81`。未安装图形浏览器或修改系统包。

## 验证

`codex --version`、配置解析、登录状态全部通过；模型列表接口认证返回 HTTP 200，列出配置模型。

真实 `codex exec` 使用当前模型及 `ultra` 配置，在临时目录、只读沙箱内返回预设标记，2.65 秒完成，无命令执行。input 12761、cached input 3968、output 8；测试仅临时关闭不需要的 Playwright MCP，不修改持久配置。此前验证脚本未关闭 SSH 标准输入，CLI 停在等待输入阶段；修正为 `stdin=DEVNULL` 后通过，不属于上游请求超时。

脱敏证据：`.local/main-node-codex/config-sync-report.json`、`verification-report.json`、`smoke-report.json`。

主节点进入项目目录后运行 `codex` 即可。工作站可用 `python tools/main_node_codex.py verify` 重验安装、配置、认证文件匹配及权限；`python tools/main_node_codex.py smoke` 可重跑真实最小会话。
