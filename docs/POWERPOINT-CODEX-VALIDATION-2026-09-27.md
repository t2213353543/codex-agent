# PowerPoint 推理 + Codex 执行：实际兼容验证

2026-09-27。**已证明这条组合能运行真实 Codex CLI 的核心流程：本地工具、文件读写、apply_patch、MCP、并行工具、会话恢复、原生压缩后续聊、网页搜索。当前是验证原型，尚不能称为完美适配或生产完成。**

固定使用 PowerPoint BPS 上游、`gpt-6-astra` / `xhigh`，客户端是服务器上的完整官方 `codex-cli 0.155.0`。本轮模型推理没有回落到 Codex 后端；Codex负责本地执行、MCP、沙箱和会话状态。

## 实际路径

```text
官方 Codex CLI 0.155.0
  -> 独立 loopback 适配原型
  -> PowerPoint BPS /responses 与 /responses/compact
  -> 原生 function_call / custom_tool_call
  -> Codex 本地执行器 / MCP
  -> 原生工具结果回传 PowerPoint
```

原型只运行于独立临时目录，CLI采用workspace-write沙箱和单独CODEX_HOME。令牌由父探针在远端内存读取，CLI只获得loopback占位密钥。没有部署或替换生产网关，没有修改线上账号凭据。

## 三个关键问题的结论

### 1. 自定义工具：已找到原生可用路径

此前顶层 `tools` 返回422的事实仍成立，但它不代表PowerPoint不支持自定义工具。真正的边界是工具声明位置：

| 请求形状 | 结果 |
| --- | --- |
| 顶层 tools:[] | 200，但官方PowerPoint工具仍可调用，不能当成关闭工具 |
| 顶层最小function / strict function / nested function | 422 |
| input中的 additional_tools | **200，返回真实自定义function_call** |
| 回传该call_id的 function_call_output | **200，正确继续回答** |

真实CLI0.155.0恰好使用input additional_tools，其中包含namespace functions.exec（custom JavaScript工具）。保留这些声明和调用项后，PowerPoint确实产生原生custom_tool_call，CLI实际执行；不需要把模型文字伪装成工具结果。

BPS样本的工具返回省略namespace，原型根据当前已声明工具的**类型＋名称**唯一匹配恢复namespace；存在歧义时拒绝，不猜测。旧客户端顶层tools也可迁移到additional_tools，但其他版本的完整工具集仍需单独验收。

### 2. 指令：可适配，但不是完全相同的提示词环境

同一个冲突标记测试分别放在三个位置：

- 顶层instructions：请求200，实际回答遵循冲突的user文本，未遵循该标记。
- input role:developer：实际遵循指定标记。
- input role:system：实际遵循指定标记。

适配器把旧顶层instructions移到developer消息；新版CLI原生developer消息继续保留。此举已使代码任务正常完成，但BPS自身PowerPoint提示词与工具配置仍然存在，没有证明可完全删除或与Codex官方环境逐字等价。

### 3. 会话和压缩：核心已通过，WS仍需专门兼容

真实CLI完成文件任务后，以相同thread恢复，禁止工具/文件读取，正确记住任务中的随机marker。

原生压缩验证不是“要求模型总结一下”：

1. 为测试provider设置 `name="OpenAI"`，启用该CLI版本的remote compaction分支；base_url仍是独立loopback，模型上游仍是BPS。
2. 降低测试用auto compact阈值，CLI真实发送包含compaction_trigger的请求。
3. 原型转为PowerPoint `/responses/compact` 并添加BPS metadata，服务器返回200和加密compaction项目。
4. CLI消费该项目，下一请求实际携带compaction。
5. 模型正确回答压缩前随机marker；CLI正常结束，没有重新读取文件。

初版provider显示名不是OpenAI时，CLI走本地文字摘要压缩，虽能记住marker，但不计作原生压缩成功。该失败口径与修正后的成功记录均保留。

## 实际验收表

| 项目 | 本次证据 | 结论 |
| --- | --- | --- |
| 自定义函数 | 模型发出bps_probe_add，真实加法结果回传后回答5 | 通过 |
| functions.exec / shell | CLI实际运行命令，工具结果进入下一次BPS请求 | 通过 |
| 文件读写 | 输入17和26，实际创建result.json，磁盘核对sum=43和随机marker | 通过 |
| 独立读回 | 模型另外发出一条shell命令读回并assert文件 | 通过 |
| apply_patch | CLI有file_change事件，calc.py从a-b变a+b，独立Python断言成功 | 通过 |
| 会话恢复 | 同thread恢复、无工具，正确回忆随机marker | 通过 |
| 原生compact | 实际trigger、BPS200、加密compaction、CLI下一请求携带该项、回忆正确 | 通过 |
| 网页搜索 | CLI收到真实web_search事件及来源引用 | 通过 |
| MCP工具 | 隔离stdio MCP实际收到两次调用，返回43、30和随机marker | 通过 |
| 并行工具 | 模型使用Promise.allSettled；CLI两次MCP started后才出现completed | 客户端并行派发通过；fixture服务端本身顺序处理 |
| 组合后的糖果题 | 两个账号通过真实CLI，均回答21且推导有效，没有答案提示 | 2/2通过 |

MCP首次测试被本地工具审批配置阻止，实际服务端调用数为0，正确记录为失败。随后仅在该隔离测试配置中预先批准fixture工具，并标注其read-only性质；两次MCP请求实际执行成功。没有更改用户全局MCP配置，也没有把模型算出的数值误认成MCP结果。

## WebSocket边界

另用官方BPS前端同样的WebSocket协议测试：

| 项目 | 结果 |
| --- | --- |
| 同socket完整历史双轮 | 101握手，两个完成响应，记忆正确 |
| BPS断线续传 | response.created之后断开，重连收到resumed并完成指定输出 |
| BPS取消 | 收到basispoints.response.cancelled；确认BPS取消，不证明底层计费的精确停止时刻 |
| Codex previous_response_id | request_rejected |
| Codex generate:false预热 | request_rejected |
| Codex stream_id | request_rejected |

BPS恢复同一个生成流，与Codex下一轮增量历史续接不是一件事。完整CLI-WS到BPS-WS的双向状态适配器尚未实现，当前真实CLI验证采用HTTP。

## 为什么还不能称为完美适配

1. 当前HTTP原型先收齐BPS响应，再把原生items封装成CLI可消费的SSE；它验证工具/内容兼容，**没有实现逐token实时透传、背压和完整取消传播**。
2. WS增量参数不通用，需要保留完整历史、响应链和重放游标并转换协议。直接改base_url不能解决。
3. 当前只发送已确认的BPS字段，尚未保持Codex所有tool_choice、parallel_tool_calls、text格式、include、cache key、service tier等控制语义。不能把某个并行任务成功扩张为所有参数都透明支持。
   本次还观察到CLI配置web_search=disabled时，BPS办公profile仍可搜索；这具体证明了配置语义尚未完全对齐。
4. BPS办公提示词仍在；空tools不会关闭其办公工具。原型拒绝转交客户端未声明的办公调用。
5. 多账号会话隔离、过期刷新、断网恢复、高并发、长工具历史、多模态、全部MCP工具与多代理完整流程尚未做生产验收。

因此可下的工程结论是：**PowerPoint作为唯一模型上游，Codex作为执行与会话客户端，这个方案具有实际可行性，且核心链路已经跑通；完整产品适配仍有明确工作量。** 无需把推理请求切回Codex后端才能使用本地工具。

两次糖果题的成功说明组合在本次测试没有重现29的错误，但不能证明账号永久“满血”、底层模型绝无路由变化或所有题目质量稳定。保留相同账号、题目、档位、请求路径与失败样本，才能继续评价质量。

## 脚本与证据

目录：`.local/office-proxy-verification/powerpoint-codex/`。

- `native_adapter.py`：原生请求适配、工具声明迁移、namespace校验和SSE封装；无账号访问/命令执行。
- `remote_cli.py`：隔离真实CLI验证；`run_validation.py`负责打包与复跑。
- `field-results.jsonl`：工具位置与指令位置测试。
- `additional-tools-native-results.jsonl`：原生自定义工具成功及空tools仍有办公工具的证据。
- `cli-live-results.jsonl`：直接透传422。
- `cli-native-extended-results.jsonl`：真实文件、apply_patch、搜索与两账号糖果题。
- `cli-candy-answers.md`：组合后的两份糖果题完整回答。
- `cli-native-compact-results.jsonl`：实际CLI原生压缩与恢复。
- `cli-mcp-approved-results.jsonl`：实际MCP与并行工具。
- `ws-live-results.jsonl`、`ws-review.json`：WS原始结果与取消事件分类修正。
- `final-review.json`：独立执行证据验收；`summary.json`：最终能力汇总。
- `text_bridge.py`、相关失败日志：早期有损JSON文本方案保留作证据，最终主路径不依赖它制造工具调用。

从项目目录复跑核心验证（会再次产生模型用量，结果文件须新命名）：

```powershell
python .local/office-proxy-verification/powerpoint-codex/run_validation.py --suite core --output core-rerun.jsonl
```

MCP与WS分别选择 `--suite mcp`、`--suite ws`。未将原型部署为生产服务。
