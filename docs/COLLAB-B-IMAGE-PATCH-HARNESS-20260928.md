# B 组：图片 patch 四格诊断工具交付

该工具把已交接的32340>30000调查计划变成可重复执行的合成探针。**历史交付仅完成本地验证，未执行真实BPS调用、SSH或读取凭据，不能据此声称图片超限已修复。** 当时0.4.12/0.4.13仅为交付背景；当前已部署0.4.20，不能沿用旧版本时态。

2026-09-29 04:20只读复核：指定图片证据树及共享协作板仍只找到两份offline_verified、network0/generation0回执，原harness源码/二进制与verification一致；未找到四格真实execute证据。该结论限定已查范围，不冒全工作区穷尽。实际未证项目与来源在`.local/cross-session-20260928/01a0e7ed/current-open-evidence/image-status.md`/image-status.json。当前主会话处理compact取消/失败计量，本工具保持可复用交付，不因持续协作目标自动新增图片请求。

## 交付物

根目录为 `.local/cross-session-20260928/01a0e7ed/`：

| 路径 | 内容 |
| --- | --- |
| `image-patch-fixtures/` | 四张标准RGB PNG、生成/独立解码脚本、字节与像素hash清单 |
| `image-patch-fixture-verification/` | Go官方image/png交叉解码与面板合并证明、root离线复验 |
| `image-patch-harness/` | Go标准库独立module、默认离线CLI、受限HTTP执行器、测试、说明、Windows编译产物 |
| `image-patch-harness-review/` | 旧probe反例、新harness独立红绿审查，历史结果不覆盖 |

## 本地验证结果

- Python与Go分别完整解码四张PNG，所有维度/字节hash/像素hash吻合。完整图6272×5280，左右各3136×5280；5280行左右拼接与原图逐字像素相同。512×512小图为控制。
- 七种损坏fixture反例全部拒绝：截断、缺IEND、CRC错误、压缩/解压尾随、像素hash失配、面板交换。原始资产未被修改。
- 新Go harness **13顶层/37含子项PASS**，vet/build成功。独立审查原HTTP201误成功、重复字段掩盖失败、复杂模型字段泄露三个红例修复后原样转绿，完整37项再次PASS，源码无漂移。
- root另以不存在的config路径执行默认离线命令，仍成功校验四图并报告network=0/generation=0，证明默认路径不读取配置或建立HTTP客户端。
- binary SHA `716d2a36a27727cf8e9d8fa15fa1f408a689aeeb97951609779923540a101534`；fixture manifest SHA `ae609afac5ef1dcbf88dac74518fa0c3b5287e7cce6ff44e74f56e3c0bb9d49c`。完整源码哈希见 `image-patch-harness/verification.json`。

## 一条离线复跑

```powershell
go -C .local/cross-session-20260928/01a0e7ed/image-patch-harness run .
```

该命令验证并复制唯一合成fixture到全新证据目录，只写本地计划。真实执行另须显式 `--execute --config <私有配置路径>` 或 `--config -`，本轮未调用该路径；完整配置形状和使用说明在工具README中。

## 有界实测合同

固定gpt-5.6-sol/xhigh、同账号、四格顺序：小图auto控制；完整大图auto；两个完整像素面板auto；**同一个大图file_id** high。最多四次实际生成派发，transport失败也计预算；不重试、不换模型/账号、不刷新凭据、不查询目录，redirect不跟随。正常只创建small/full/left/right四个synthetic附件，cleanup仅本轮实际成功上传得到的去重ID。

控制须HTTP200、完整唯一成功终态、匹配实际模型及固定GRID答案；后续只有明确patch400可以按预定下一格继续，鉴权/配额/其他错误/读取失败则停止。结果分别记录实际HTTP、完整性、模型存在/null/type、usage白名单计数和未知状态，保留JSON数字精度，不导出任意provider正文或未知usage扩展。

`matrix_finished`表示四格都达到可记录终点，可能包含预期400，并不代表四格全部成功。32像素网格32340/16170/256只是本地几何算术，服务端明确报告的处理后patch数单独保存。high可能改变处理分辨率，分块改变视觉/坐标上下文，都不能凭本地像素完整性宣称推理语义等价。

## 后续协作边界

主owner可在当前正式验收完成后单独安排至多四请求窗口，用服务器端配置执行并保留实际失败、返回模型、usage与cleanup回执。当前工具已具体可审，不要求为此修改生产路由、压缩客户图片或重放历史请求。服务端资格/限流失败不能通过换账号或增加请求掩盖。
