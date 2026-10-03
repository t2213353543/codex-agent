# 架构索引

- [目录、构建和测试结构迁移](STRUCTURE-MIGRATION.md)：运行入口、Go 包、SQL 嵌入、跨语言测试、源码清单与冻结快照。
- [API 契约](../API-CONTRACT.md)：客户端与管理后台接口。
- [当前 PowerPoint 网关](POWERPOINT-GATEWAY.md)：生产请求、会话、工具与验证边界。
- [历史原生 Codex 传输](../CODEX-NATIVE-TRANSPORT.md)：旧上游组件与调查材料，不是当前 PowerPoint 推理回退通道。
- [PowerPoint/Codex 验证](../POWERPOINT-CODEX-VALIDATION-2026-09-27.md)：已验证能力和适配边界。
- [BPS 网关代码审查](BPS-REVIEW.md)：独立发现、修复复验和仍需覆盖的边界。

实现以 `internal/server/`、`internal/bps/`、`migrations/` 为准。这里描述组织方式；具体协议与行为变更仍由相应实现和测试约束。
