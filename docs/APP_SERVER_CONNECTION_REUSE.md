# App Server 风格网络连接复用

本实现只保存网络连接和经过确认的协议状态，不运行官方 agent，不执行工具，也不保存完整会话。客户端每次仍提供权威的完整输入；中转验证前缀后才使用服务端 previous_response_id。

启用条件：请求上下文携带 appServerConnectionResult，且配置了 native transport。缺少完成确认机制的直接调用仍使用原来的每请求私有 WSS 行为。

外层接口：

    result := newAppServerConnectionResult()
    r = r.WithContext(withAppServerConnectionResult(r.Context(), result))
    // 每个账号尝试分别创建 result。
    // 完整终止事件已消费/交付、owner CAS 成功、客户端仍在线才确认。
    defer result.Confirm(success)

外层失败、客户端中断、owner CAS 冲突或账号尝试被替代必须 Confirm(false)。Confirm(true) 可以先于网络泵结束；只有有效 response.completed 的全部 SSE 字节已经被读取且外层确认成功，连接才归还。现有收集器在完整终止事件时结束，无须额外读取 HTTP EOF。提前关闭正文导致终止事件未写完时仍淘汰连接。

外层实际应在 defer 闭包中读取最终 success；以上最后一行仅表达调用约定，不应在注册 defer 时提前求值状态。

池键包括模型 provider origin/path、已准入 API key 身份、账号 owner 摘要、凭据记录版本、鉴权及账号请求头摘要、出口 proxy 配置指纹、session/thread、握手身份及路由 hint。每次租用和发送前仍执行当前请求的实时授权检查。原始请求 context、bearer、完整头和工具/历史正文不保存在空闲条目里。

同一个池键只有一个正在使用的连接租约。其他已准入请求等待这个协议租约，等待可取消；取消等待者不会关闭正在执行的请求。账号并发准入保持原样，没有新增拒绝请求的连接数或队列容量上限。空闲计时只回收网络资源。

常驻条目保留连接、固定大小稳定字段摘要、输入加已确认输出的逐项滚动 SHA256 摘要及计数、response_id 和 turn 身份/turn_state。输入项规范化比较忽略内部消息元数据，不忽略真实消息内容、工具参数或工具结果。未知请求扩展参与稳定字段比较，避免未经证明地复用上下文。

同一条连接上：

- 首次调用执行 generate:false 预热，成功后比较稳定字段及输入前缀，发送 delta 或完整输入。
- 后续请求当前缀等于上一完整输入加确认输出时，只发送新增输入和 previous_response_id，不再预热。
- 历史分支、工具/指令或其他稳定字段变化时发送完整输入，不引用旧 response_id。
- 缺少终止 response_id、响应身份变化、输出顺序/终止输出冲突、断流、错误终止、提前关闭及取消均淘汰连接。
- 已尝试发送生成后不因传输失败自动重放。已知失效的空闲 native socket 可在下一请求发送前更换；新握手或生成之前的失败仍由现有调用方策略决定。

turn_state 只在明确相同的客户端 turn_id 内复用。新 turn 或未知 turn 边界重置，不能按 session/thread 跨 turn 携带旧 token。依据固定 0.155.0 core/src/client.rs:263–283、515–528：官方可以复用 WebsocketSession，却为每个 ModelClientSession 建立新的 OnceLock turn_state。

native WebSocket 的握手 context 和连接 lifetime 分开。握手使用当前授权与截止时间；连接使用 transport 的独立服务寿命，Close 关闭全部连接。活动请求取消会淘汰连接；已经验证并交付完整终止事件后，请求 lease 的正常结束不会抢先关闭等待外层 owner 确认的连接。原始客户端取消始终使确认无效。

新增合成测试覆盖同历史续接不重复预热、历史分支完整输入、账号凭据/API key/proxy/thread 隔离、turn_state 重置、同 session 等待取消、CAS 失败、short read、缺少/变化 response_id、错误终止、自然 lease 取消、空闲回收、当前授权检查、输出前缀及慢消费者关闭。

    go test ./internal/server -run '^TestAppServerConnection|^TestAppServerTransport' -count=1

本地池测试已连续运行 40 次通过。Linux race、真实 native helper 多请求捕获及真实凭据旋转/重连必须绑定本轮冻结源码另行验收；不能沿用此前每请求私有连接版本的真实捕获结论。
