# B 组：0.4.10 发布前后错误覆盖核对

本报告用于持续协作，不把完成一次候选验收等同于所有自然请求错误消失。初始依据是 `.local/cache-scope-release-20260928/natural-errors-before-scope.json`，采样时点 2026-09-28 12:20:20 UTC、运行版本 0.4.9。该文件记录进程启动以来 502 次请求、446 成功、56 错误，不能直接替代另一个排除合成请求的自然流量分母。

| 已记录分组 | 数量 | 已验证的边界 | 0.4.10 与后续核对 |
| --- | ---: | --- | --- |
| structured text.format | 20 | 本地明确拒绝；原协议 schema 直传被上游拒绝，内部格式工具另有独立设计 | 当前候选未实现 structured projection，不算修复；后续需完整 schema/工具策略/流式投影验证 |
| session account 503 | 13 | 旧 owner 不在当前授权候选集；完整明文可 failover，opaque/增量保持 owner | 两种 cache scope 行为一致；不能宣称 shared scope 修复。已复现日志 source/phase 缺失，B 组在独立候选中补分类 |
| Invalid request body 422 | 10 | 已捕获的 custom 工具输出 inline 图片有有效正负对照；上传后 file_id 请求成功 | 0.4.10 的 custom 图片真实识图与上传缓存 PASS；聚合未证明全部十条同因，上线后继续区分新 422 |
| unsupported service_tier | 8 | 当前 BPS 请求验证保留明确不支持语义 | 不算已修复；不能用静默丢弃参数替代实际服务等级支持 |
| forced tool_choice undeclared | 3 | 指定工具不在声明目录中被本地拒绝 | 不算已修复；需请求结构证据才能判断客户定义、兼容映射或目录丢失 |
| rate_limit_exceeded | 1 | 上游真实失败终态、HTTP 流已开始 | 新版保持真实错误，JSON/SSE/WS 错误交付专项通过；不意味着上游配额限制消失 |
| persistent store unsupported | 1 | store=true 或非法类型本地拒绝；聚合无法区分具体形状 | 未实现 Responses 持久化检索语义，不算已支持。后续可细分错误码，不能把续聊缓存当完整 persistent store |

## 当前候选的已完成核验

- A/B/C 合并后源码通过最终 Go/vet、Linux race/Redis、10 项真实 PostgreSQL、204 项前端单测及封存浏览器门禁。
- 最终真实 live 六个顶层用例全部 PASS：完整 HTTP、WebSocket、shared cache、模型路由、用户图片、custom 工具图片。批次 `.local/reliability-modes/live/20260928T123224.174866Z/`，exit 0。
- B 组独立补测 custom 图片的多图失败原子性、key/owner 隔离及增量续聊保存原图，14 项顶层测试 PASS；session/store 边界 14 子用例 PASS。
- acceptance 的 21 份证据 SHA 逐项一致，当前全树 source `93193eda3e9321ed3ede224cfba39fbdbdc0778bb9f628f7a47185adf85190e8`。新包 `da708caa5ae5776c79bc604d3df4286ddced2010b8440609375201f9005d58dd` 已由主会话实际激活；运行 PID775841、binary SHA `b3c8acf1694ffe1c055f85b677a537a028435a8491874c8d07db36f0deb32dca` 与已审产物一致。
- 8 条合成生产请求全部200/completed、返回用量与持久化一致；两账号各一次冷启动，整体读取74.7102%、六条热请求99.6136%。scope=api_key，账号配置保持。管理卡片包含保留历史，不能作为新版自然效果证据。核对回执 `.local/cross-session-20260928/01a0e7ed/deployment-audit.json`。

## 下一轮最小工作

B 组仅在独立 overlay 候选中准备 account-selection 的错误 source/phase 分类，保留 HTTP 503、错误正文、owner 守卫与 failover 行为。该候选不进入当前冻结树；发布后由统一整合者决定下一版。其余错误需按上表的真实能力与请求边界分别处理，不能通过提高缓存率推断已修复。

上线后以实际 process start、binary SHA 区分窗口，并把 `gateway-release-validation/*` 合成请求与自然请求分开。新 422/503/未知 usage 应使用新进程窗口的记录重新归因；不回放客户历史。
