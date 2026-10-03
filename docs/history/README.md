# 历史材料

这里保存已被当前说明替代的原文。按日期记录的部署状态、模型可用性、测试数量、源码位置和界面截图只适用于当时版本；当前入口以[文档索引](../README.md)及[PowerPoint 架构](../architecture/POWERPOINT-GATEWAY.md)为准。

| 原位置 | 归档位置 | 处理方式 |
| --- | --- | --- |
| 根 `REVIEW-2026-09-17.md` | [2026-09 审查](2026-09/REVIEW-2026-09-17.md) | 原文移动，验收脚本基线引用更新 |
| 根 `VERIFICATION.md` | [2026-09 验收](2026-09/VERIFICATION-2026-09-17.md) | 原文移动，原位置保留当前验收入口 |
| `docs/GATEWAY-SETTINGS.md` | [旧设置说明](2026-09/GATEWAY-SETTINGS-2026-09-18.md) | 原文复制归档，活动文档更新为 BPS |
| `frontend/FRONTEND-HANDOFF.md` | [旧前端交接](2026-09/FRONTEND-HANDOFF-2026-09-17.md) | 原文复制归档，活动文档更新为 BPS |

归档文件原始字节已校验保持不变，因此其中相对链接仍按表中原位置解释。不要通过修改历史证据消除与当前实现的差异；新事实写入当前文档和新验收报告。

`docs` 根的其他日期报告及 `audit-*`、`quality-*` 目录继续保留原路径。它们可能被交付清单、源码冻结摘要或外部链接引用，不做批量搬迁。后续归档先查引用，再登记原位置，最后验证哈希。

早期 Python BPS 原型仍保存在 `.local/office-proxy-verification`，是私有调查证据，不是发布模块。当前可维护实现位于 `internal/bps`、`internal/server`，真实验收入口是 `tools/bps_validate.py`。`.local` 的既有凭据、数据、捕获和验证证据不参与文档归档。

根目录旧 `codex-agent.exe` 已移动到 `.local/build/legacy-root-codex-agent.exe`，保留原字节且未覆盖已有文件。它不是当前发布产物；当前版本应从源码构建到 `.local/build/`。
