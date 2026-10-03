# B 组：32340 > 30000 图片 patch 错误复核

已定位实际失败：0.4.10 自然请求记录29324，2026-09-28 12:58:43 UTC，requested/sent=`gpt-5.6-sol`，HTTP SSE 在上游响应头阶段返回400，一次上游attempt、10631ms；returned model和usage均未报告。错误中的32340/30000原值完整保留，不能将其归类为缓存率不足或自动重试失败。

本组一次READ ONLY查询只取固定20分钟窗口内该精确错误的元数据，未读取提示词、图片、凭据，也没有重放或发送模型请求。原始证据 `.local/cross-session-20260928/01a0e7ed/image-patch-review/production-patch-metadata.json`。其中 prepared body2003字节、input_items3均不能推出图片原尺寸或数量；当时没有尺寸/detail/逐图计数观察字段。

## 单图限制与参数证据

实际打开的 [OpenAI Images and vision](https://developers.openai.com/api/docs/guides/images-vision#image-input-requirements) 明确将30,000上限定义为每张图片经过预处理后的patch数，而非请求内图片patch总和。对应 `gpt-5.6-sol` 的公开表中auto沿用original尺寸策略，high会在尺寸与patch预算内缩放。公开API契约仍需与当前BPS行为区分。

固定官方PowerPoint bundle的图片writer仅证明上传文件后使用file_id、缺省detail补auto、显式detail保留。未发现可证实的lossless绕限字段。无损压缩不改变像素尺寸；file_id引用和上传缓存也不证明上游无需处理图像。high可能降低分辨率，切成多张虽可保留像素却改变坐标与视觉上下文，均不能称为已验证的等价自动修复。

此前两次422捕获的8张1020×1320和1张2048×822属于不同请求，不是本次400的原图证据。不能将旧捕获拼接进本条日志推算尺寸或累计预算。

## 已完成本地验证

| 验证边界 | 实际结果 |
| --- | --- |
| JSON/SSE/WS收到上游400 | 状态和原错误保留，记录requested/sent、returned未知，一次generation attempt，不自动换号重试 |
| 有效PNG上传 | 上传字节/hash与原尺寸一致，没有resize/reencode |
| 同图在user/custom或同一请求多处出现 | 仅一次上传，保留每处file_id引用 |
| previous_response_id续聊 | 保留原图和新增图片，不删除重复历史；已缓存图不再次上传 |
| 显式low/high/auto | 原值保留；省略/null补auto |
| 无损重编码同像素不同字节 | 两次上传，当前附件cache key基于字节hash；这不是patch预算变化证据 |

请求形状套件2顶层+9子例PASS，674文件冻结错误交付套件2顶层/5含子例PASS，均零失败/跳过。详见本组 `image-patch-review/request-shape/REPORT.md` 和 `error-boundary/REPORT.md`。这里只证明上传次数、请求引用和协议行为，不宣称已测得上游图像patch计数。

## 可以直接交接的下一步

已准备零联网合成诊断计划与算术校验脚本 `image-patch-review/protocol/diagnostic_plan.py`，尚未执行推理：固定 `gpt-5.6-sol` 小图附件控制、6272×5280单张auto（自选32340原始网格）、同像素拆为两张3136×5280（各16170）、同大图high，最多4次无重试对照。该计划用新生成图片，不恢复客户图；能分别检验BPS单图/累计边界和显式detail是否生效。

主会话可在当前structured真实验证窗口结束后选择执行这个有界计划。没有大图BPS实证前，不自动缩图、删图、改detail或把分块作为默认转换。若再次发生自然错误，应先补每图尺寸/有效detail/引用次数的纯结构观察；现有历史记录无法补回这些缺失字段。

完整协议证据、官方来源和四格计划见 `.local/cross-session-20260928/01a0e7ed/image-patch-review/protocol/README.md`。本任务完成了主会话指定的只读复核，未修改运行代码或生产设置，未宣布图片超限已经修复。
