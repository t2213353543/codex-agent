"""Create the September fixes release note from verified production results."""
import json
from pathlib import Path

ROOT=Path(__file__).resolve().parents[1];OUT=ROOT/'.local/fixes-20260924';RUNS=ROOT/'.local/mother-benchmark'
def read(path):return json.loads(path.read_text(encoding='utf-8'))

def main():
 load=RUNS/'20260924T072842Z-load';compact=RUNS/'20260924T072801Z-compact';ws=RUNS/'20260924T073137Z-websocket'
 burst=read(load/'summary.json');runtime=read(load/'gateway-concurrency.json');logs=read(load/'gateway-request-logs.json')
 rows=[json.loads(s) for s in (load/'results.jsonl').read_text(encoding='utf-8').splitlines()]
 ids={r['request_id'] for r in rows};matched=[r for r in logs if r.get('client_request_id') in ids]
 assert len(rows)==len(matched)==160 and all(r['protocol_success'] for r in rows)
 assert runtime['peak_in_flight']==80 and runtime['runtime_stable']
 assert max(len(r.get('attempts',[])) for r in matched)==1
 compact_report=read(compact/'summary.json');assert compact_report['summary']['protocol_completed']==4
 ws_report=read(ws/'summary.json');assert all(m['success'] for m in ws_report['models'])
 live=read(OUT/'frontend/live-final.json');assert live['passed'] and len(live['browser']['checks'])==29
 deploy=read(OUT/'release/activate.json');assert deploy['deployed'] and deploy['data_before']==deploy['data_after']
 result={'deployed':True,'release_id':deploy['release_id'],'binary_sha256':deploy['verification']['health']['runtime_binary_sha256'],'helper_sha256':deploy['verification']['helper_sha256'],'load':burst,'compact':compact_report,'websocket':ws_report,'gateway_peak':80,'matched_gateway_records':160,'max_attempts':1,'runtime_stable':True,'frontend_checks':29,'data_unchanged_at_switch':True}
 (OUT/'result.json').write_text(json.dumps(result,ensure_ascii=False,indent=2),encoding='utf-8')
 answers=[]
 for row in rows:
  if row['content_kind']=='candy':answers.extend([f"## {row['model']} / {row['case']} / 答案 {row['candy_answer']}",'',row['output_text'],''])
 (OUT/'post-fix-candy-answers.md').write_text('\n'.join(answers),encoding='utf-8')
 note='''# 七项修复已上线 — 2026-09-24

北京时间 15:26:42 已发布到 [Codex Agent](http://43.225.196.170:18879)，并完成真实账号回归。账号、凭据、分组关系、请求记录和累计 Token 在切换前后精确一致；主服务与 28765 推送接收器均正常，最终账号在途请求为 0。

## 修改与结果

1. **compact 404**：本账号的 ChatGPT 订阅后端旧 `/backend-api/codex/responses/compact` 返回 404。对照 Codex 0.155 源码及真实请求，改为 `/responses` + `compaction_trigger`，收集完整 SSE 加密压缩项，再向旧客户端返回 `response.compaction` JSON。保留客户端近期消息的容量有界，输入工具调用和触发控制不重复回放，错误终态不伪装成功；其他上游基址保留原 standalone 协议。两模型压缩与续问均成功，4/4 HTTP 200；直接仅用加密压缩项续问的修复前探针也已验证记忆保留。
2. **并发限制**：删除 Go/Rust/SOCKS 的额外 32 并发限制，由账号 `concurrency` 统一控制。helper 数量限制改为空闲缓存预算，忙路由按需创建；HTTP 与 WS 同时适用。新版 Go 明确拒绝旧 32 上限 helper，避免只替换一边后仍被暗中限制。合成 96 并发全部到达上游，第 97 请求正确返回账号忙 429，`Retry-After: 1`。
3. **官方用量窗口**：列表复用官方 `/backend-api/wham/usage`。按真实 `limit_window_seconds/used_percent/reset_at` 显示，查询失败和 null 保持未知。本地预算与官方额度区分；成功缓存最多 5 分钟，跨重置时间/凭据轮换失效，手动刷新直查；账号版本冲突自动重新读元数据。线上两主题最终核对为 **7d、已用 1%、无第二窗口**，查询时间与官方快照一致。
4. **显示列复选框**：修正旧固定像素定位与新版组件百分比变换叠加导致的勾号偏移，禁用项勾选可见，保留键盘焦点。
5. **全局输入框**：移除内外双层描边，整个控件使用单层细边界，右侧后缀不再被单独隔开；普通、数字、文本域、下拉框、日期及组合输入均验证。
6. **工作空间卡片**：移除“管理工作空间 / Codex Agent”卡片及夜间重复工作区标签。
7. **仪表盘**：增加实时在途请求、失败率、平均响应耗时及样本、今日请求/Token、模型 Top 6、失败原因分布和对应日志跳转。使用真实 API，保留明细覆盖范围单独标注，未知 Token 不伪造 0。

## 真实 80 并发复测

同一个母号下的一个子号，账号并发设为 80；每模型单独运行 80 个同步起发请求（64 个短请求 + 16 道原糖果题）。`reasoning.effort=medium`，无客户端重试，无输出 Token 上限。

| 模型 | 修改前成功 / 总数 | 修改后成功 / 总数 | 修改后请求错误率 | 糖果题回答 |
|---|---:|---:|---:|---|
| gpt-5.6-sol | 32/80 | **80/80** | **0%** | **14/16 答 21**，2 次答 29 |
| gpt-6-astra | 32/80 | **80/80** | **0%** | **16/16 答 21** |

客户端和网关 admission 峰值均为 80，160 条请求与 160 条网关记录逐项匹配，每条仅一个 attempt，全部 HTTP 200 + `response.completed`。返回模型分别与请求模型一致，期间 PID/二进制不变。两模型 WebSocket 同连接双轮回归也均成功。

Sol 的 2 次 29 是本轮答案错误，与传输错误分开记录。Astra 另外有 2 条短回复多了一个句号；原短提示末尾自身有句号歧义，单列格式差异。以上为一次真实混合突发结果，不等同持续负载的长期 SLO。

## 验证与运行身份

- Windows 冻结源码全库：1,444 个测试事件通过，181 个环境依赖跳过；vet、Linux 构建通过。跳过不算通过。
- Linux/PG/Redis/native 相关 race：300 个测试事件通过；修正测试环境 Redis 前缀后，初轮失败的 47 个测试事件全部复跑通过。7 个依赖官方 CLI 可执行文件的测试未运行；真实 API/WS/compact 另有线上证据。
- Rust helper release 构建、WebSocket、请求压缩及 26 项 loopback 通过。最终源码与构建快照仅一个合成上游测试 backlog 设置不同，生产 Rust 字节相同。
- 前端 172 项单测与构建通过，双主题/多尺寸 Dashboard 和控件检查通过；增强线上浏览器 29 项通过，包括全部 55 静态文件 SHA、官方用量 DOM/查询时间和仪表盘数据逐项交叉核验。
- 初轮 Linux 全库 race 因测试 Redis 前缀不符合隔离 ACL、96 并发测试预期 503 写错（真实应 429）出现失败；另在原有大 SSE 压力用例中耗时较长，停止该轮后针对相关项复跑。没有宣称该初轮全库 race 通过。
- 首次修复后 compact 冒烟中，服务端已 200，评测器因把保留用户消息的字符串 content 当对象列表而崩溃；修正评测器后完整压缩/续问 4 条通过，原失败痕迹保留。

Release：`c6cffc50c2d83ade105b92ec6b79691af0d438ec6c02f067f3b659120b119545`。

Go SHA256：`43e1af500093ae649d71e4cfd76e95b4e791eab5968af5b11d93d5ab7ae375da`。

Rust helper SHA256：`aeb13c0efacf37aa427127e6538e1f83ca6c130e6e27a8c3af69dc1b388cfd4a`。

上线保持原数据库 schema、原管理设置和账号配置，切换写入冻结约 2.55 秒；先后暂停/恢复接收器与原工作台同步，未修改 28765 主服务。

## 证据与复跑

- `.local/fixes-20260924/result.json`：总报告。
- `.local/fixes-20260924/frontend/live-final.json`：29 项真实页面检查。
- `.local/mother-benchmark/20260924T072801Z-compact/`：压缩与续问。
- `.local/mother-benchmark/20260924T072842Z-load/`：80 并发完整请求、回答、SSE、网关记录及采样。
- `.local/fixes-20260924/post-fix-candy-answers.md`：本轮 32 个完整糖果回答。
- `.local/mother-benchmark/20260924T073137Z-websocket/`：真实 WS 双轮。

```powershell
python tools/mother_benchmark_remote.py --phase load --effort medium --timeout 300 --concurrency 80 --load-requests 80 --load-candy 16
```

公开 compact 协议参考 [OpenAI Compaction 文档](https://developers.openai.com/api/docs/guides/compaction)；订阅后端 v2 差异依据本地 0.155 官方源码及本次实际请求验证。
'''
 (ROOT/'docs/FIXES-DEPLOYED-2026-09-24.md').write_text(note,encoding='utf-8')
 print(json.dumps({'deployed':True,'load_successes':160,'load_errors':0,'gateway_peak':80,'compact_passed':4,'live_browser_checks':29}),flush=True)

if __name__=='__main__':main()
