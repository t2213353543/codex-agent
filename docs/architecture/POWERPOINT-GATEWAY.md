# PowerPoint + Codex 网关维护说明

生产入口在 `internal/server/main.go` 固定创建 BPS runtime。PowerPoint 是模型推理上游，Codex 是客户端及工具执行环境；既有 Codex 传输代码保留为协议边界回归与历史维护材料，不是可用的生产回退选项。

## 代码分层

- `internal/bps/request.go`：保留原生 input、加密项目和工具 schema，将 instructions 移入 developer、顶层 tools 移入 additional_tools，识别 compaction_trigger。
- `internal/bps/tools.go`：按工具类型、名字和 namespace 校验；只在唯一匹配时补齐缺失 namespace，拒绝未声明办公工具。
- `internal/bps/stream.go`：按事件处理真实 SSE，保留文本与参数增量，验证终态，取消时关闭上游读取；完整终态前执行持久化回调。
- `internal/bps/models.go`：由实际 BPS access 目录生成客户端模型目录，本地客户端说明与上游能力分开处理。
- `internal/server/gateway_bps.go`：账号准入、代理、令牌恢复、HTTP 流式/JSON/压缩/模型目录。
- `internal/server/gateway_bps_websocket.go`：Codex WS 分流、增量历史、预热与取消，复用同一个 BPS HTTP 执行路径。
- `internal/server/gateway_bps_history.go`：API Key 与账号身份隔离的续聊缓存；生产 Redis 使用认证加密，内存路径用于测试。

## 请求和会话

客户端每次请求重新校验 Key、分组、账号启用状态和并发槽。刷新只允许同一账号身份，已有加密上下文不能漂移到其他账号。BPS task_id 由 Key、会话、账号身份共同划分，turn_id 每次生成独立。

`previous_response_id` 在网关展开成完整历史后才发到 BPS。响应链缓存最长保留一小时、全局最多512条且总字节不超过128 MiB；过期或被淘汰时明确要求客户端提交完整历史，不猜测上下文。缓存采用无明文会话ID的哈希键与加密值。

客户端 `generate:false` 只建立本地预热状态和响应ID，不调用模型。`stream_id` 在网关隔离；同一 stream 不能同时运行两个请求。一个连接最多64个活跃stream。取消请求带 response_id 时必须匹配当前执行项，迟到的取消不能终止同stream的新请求。

HTTP和WS共用账号绑定、刷新、响应验证与指标；不会以错误处理为由自动重放已接受的生成请求。

## 压缩和工具

独立 `/responses/compact` 与 CLI 的 `compaction_trigger` 都转换为 BPS compact 的 `model/input/metadata` 请求。CLI流式压缩返回 Responses SSE，旧JSON压缩返回原生JSON。压缩结果必须包含加密compaction且没有失败状态；交付前重新核对账号授权。

工具调用保持原生 function/custom 项目和 call_id，执行仍发生在客户端。上游办公profile的工具不会自动向调用者开放。工具选择和禁止并行等上游不接受的控制，通过developer约束和输出验证处理；不能伪称字段已原样转发。

## 已知上游约束

实际单字段请求已确认 BPS 拒绝顶层 `tools`、`tool_choice`、`parallel_tool_calls`、`include`、`max_output_tokens`、`text`、`service_tier` 等字段；`additional_tools`、`prompt_cache_key`和原生历史可用。网关拒绝无法表达的结构化输出、后台生成和非默认服务档位；保留可转换控制的诊断信息。

`prompt_cache_key` 在选定账号后按API Key和账号身份划分，历史缓存保存划分前的客户端值，避免续聊时重复散列。仅字段被接受不意味着所有请求都命中缓存。

`BPS_WEB_SEARCH=disabled` 增加明确请求约束并拒绝搜索返回项；上游仍由服务端办公profile运行，不能把这一机制描述成已证明其内部绝不会尝试搜索。需要完全禁止外部访问的环境应按真实网络与上游能力边界设计。

## 验证

```powershell
go test ./internal/bps ./internal/server
npm --prefix frontend test
npm --prefix frontend run build
python tools/bps_validate.py
```

最后一项产生真实模型用量，仅在授权的远端临时环境使用测试账号。发布仍需数据库、Redis、客户端、浏览器和上线健康检查证据；早期Python原型、历史冻结fixture与源码单测不能代替当前Go构建的线上验收。
