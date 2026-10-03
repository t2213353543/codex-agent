# 账号官方窗口的美元估算

账号列表与上游额度详情在官方百分比旁显示本地 USD 参考金额。后端从真实留存请求计算 `(input_tokens - cached_tokens) × 输入价 + cached_tokens × 缓存价 + output_tokens × 输出价`，按官方 `reset_at - limit_window_seconds` 至当前时间的窗口汇总。官方百分比、credits 和 spend control 均不用于推算美元金额。

模型优先取 returned model，其次 upstream、sent、requested；支持明确的日期模型后缀、实际返回的 priority/fast/flex 档位，以及目录定义的长上下文价格。金额在 `local_window_costs` 中独立返回，官方 `data` 原样保留。官方缓存命中仍重新查询本地日志，批次中所有账号及窗口共用一次扫描。读取错误不显示金额。

价格基础来自本机 `sub2api/current-source/backend/resources/model-pricing/model_prices_and_context_window.json`，算法参考其 `account_usage_service.go` 的 WindowStats 和 `billing_service.go`。摘取的输入、缓存、输出及长上下文价格在 `internal/server/account_window_prices.json` 内保存来源 SHA-256。2026-09-27 根据 [OpenAI 官方价格](https://developers.openai.com/api/docs/pricing)补齐 GPT-6 Astra / Sol / Luna、更新 GPT-5.6 Sol 优惠价，并移除官方未提供的 Pro 缓存价格。版本是日期与实际内嵌价格文件 SHA-256 前缀，可审计价格变更。

该金额是按当前参考价回算的本地 Token 估算，不是官方扣费、订阅余额或历史账单。已清理日志及其他客户端的用量不在范围内；窗口重置时间缺失或已过期时不猜测区间。缺少 usage、未知模型/档位、未可靠拆分的图片或 cache-write Token 都列入无法估算次数；部分已知时显示“已知部分”，全部未知时不显示 `$0`。工具调用费用不在 Token 估算范围内。

验证：`go test ./internal/server -run 'TestManagedAccountRequestUSD|TestManagedAccountWindowCosts|TestPGManagedAccountWindowCosts|TestManagedUsageBatchOfficialCache' -count=1`。PG 验证需要项目现有的隔离 `TEST_DATABASE_URL`。

前端验证：在 `frontend` 运行 `node --test tests/account-official-usage.test.js`；构建后运行 `node tests/account-official-usage-browser.mjs`，覆盖金额、未知状态、强制刷新、分组焦点展开、行 hover 颜色及导入配置字段。
