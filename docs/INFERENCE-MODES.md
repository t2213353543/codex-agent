# 推理模式与官方托管运行时

管理设置的“推理服务”提供三种模式，使用同一网关 Key 与 `/v1/responses` 入口：

| 模式 | 推理与工具执行 |
| --- | --- |
| PowerPoint + Codex | BPS 推理；原有客户端执行本地工具 |
| 官方 Codex CLI | 官方 Codex Responses 协议；官方客户端保留完整工具循环，网关使用配置的官方 Rust 传输组件 |
| 官方 App Server | 网关启动真实官方 App Server，通过 JSON-RPC 调用；Responses 适配器把工具请求交回客户端执行 |

设置字段为 `inference_mode`，值分别为 `powerpoint_bps`、`official_codex_cli`、`official_app_server`。更新需要当前 `settings_version`；旧设置默认 PowerPoint。每个请求固定自己的模式，切换后旧的绑定会话会明确拒绝跨模式续聊，应新建客户端会话。没有自动跨模式回退。

服务端官方执行器由 `CODEX_CLI_PATH` 指向完整官方 Codex 安装中的二进制，`CODEX_RUNTIME_DIR` 是受控工作目录。发布保留 code-mode 等配套程序，不能只复制主二进制。官方 CLI 协议的 Rust 传输配置沿用 `CODEX_NATIVE_HTTP_HELPER`、对应 SHA256 与 WebSocket 设置。

## 服务器托管 CLI

`POST /v1/codex/tasks` 启动真实 `codex exec --json`，请求示例：

```json
{
  "model": "gpt-6-astra",
  "reasoning_effort": "xhigh",
  "prompt": "读取 input.txt，计算结果并写入 result.txt。",
  "files": {"input.txt": "17 + 26"}
}
```

使用普通网关 `Authorization: Bearer <key>`。返回任务 ID 和事件、文件地址。任务在独立目录执行；模型实际调用服务器工具，客户端无需代执行。

| 操作 | 接口 |
| --- | --- |
| 查询状态 | `GET /v1/codex/tasks/{id}` |
| 原生 CLI 事件流 | `GET /v1/codex/tasks/{id}/events`；支持 `Last-Event-ID` |
| 取消及子进程清理 | `POST /v1/codex/tasks/{id}/cancel` |
| 工作区目录/文件 | `GET /v1/codex/tasks/{id}/files?path=result.txt` |
| 继续已完成任务 | 创建请求加 `resume_task_id`，保留同一官方会话和工作目录 |

同一工作区同一时间只能有一个运行任务。任务绑定调用 Key 与账号身份；新请求、结果读取和后台执行持续校验授权。完成任务保留一天、最多128个，官方推理进程最多64个；重启服务会清除内存任务索引，因此当前任务列表和可恢复任务不承诺跨网关重启持久化。

## 原生 App Server

连接 `ws(s)://<gateway>/v1/codex/app-server`，使用同一 Bearer Key 和稳定的 `Session-Id`。发送官方 `initialize`，随后使用 `thread/start`、`turn/start`、`thread/resume` 等原生协议。账号登录由网关完成；客户端不获得账号令牌。

工作目录固定为该会话的服务器独立工作区。原生服务保留官方工具执行和双向审批/工具事件；客户端需实现对应 JSON-RPC 响应。管理端关闭 WebSocket 后不能新建该入口。空闲会话保留一小时；活动生成期间断开会取消其运行进程，以免释放账号并发后任务仍继续。

## Responses 桥的边界

App Server 原生 dynamic tools 只接受函数工具。网关为客户端 custom grammar 工具使用携带原始字符串的函数包装，并还原外部名字、命名空间、调用 ID 和结果。仅在 Responses 工具桥中，真实模型目录的 `tool_mode` 配置为 direct；其他模型字段、模型选择与推理档位保持原值，托管 CLI 和原生 App Server 不改工具模式。

该转换让工具继续在客户端执行，但不是协议字节透明转发。需要原生官方完整执行行为时使用托管 CLI 或原生 App Server 入口。PowerPoint 不支持的模型、服务档位或输出格式会给出明确错误，不会偷偷换模型、降低档位或忽略严格格式。

官方协议参考：[App Server](https://learn.chatgpt.com/docs/app-server)。实际验证和发布状态以本轮验收报告为准。
