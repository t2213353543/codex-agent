<script setup>
import { onUnmounted, ref } from 'vue'
import { logManagement } from '../api/logManagement'
import { createLatestRequest } from '../utils/latestRequest'
import { logTokenCount } from '../utils/logQueryState'
import { formatTime } from '../utils/format'
const loading = ref(false), error = ref(''), data = ref(null)
const latest = createLatestRequest()
let sequence = 0
async function load() {
  const id = ++sequence; loading.value = true; error.value = ''
  try { await latest.run(signal => logManagement.coverage({ signal }), result => { if (!Number.isSafeInteger(result?.detail_count) || result.detail_count < 0 || result.scope !== 'retained_request_details') throw new Error('数据范围响应无效'); data.value = result }) }
  catch (cause) { if (id === sequence) error.value = cause.message || '数据范围查询失败' }
  finally { if (id === sequence) loading.value = false }
}
function toggle(event) { if (event.target.open) load(); else { ++sequence; latest.cancel(); loading.value = false } }
onUnmounted(() => { ++sequence; latest.cancel() })
</script>
<template>
  <details class="log-coverage" @toggle="toggle"><summary>数据范围与留存说明</summary><p v-if="loading" role="status">正在查询实际留存范围…</p><p v-else-if="error" role="status">{{ error }} <el-button link type="primary" @click="load">重试数据范围</el-button></p><template v-else-if="data"><dl><dt>当前全部留存明细</dt><dd>{{ logTokenCount(data.detail_count) }} 条 · {{ data.storage === 'postgres' ? 'PostgreSQL' : data.storage === 'file' ? '文件存储' : data.storage }}</dd><dt>实际记录区间</dt><dd>{{ data.detail_count && data.first_record_at ? formatTime(data.first_record_at) + ' 至 ' + formatTime(data.last_record_at) : '暂无留存记录' }}</dd><dt>留存规则</dt><dd>手动清理与存储修剪决定；没有保证的留存天数。</dd><dt>累计事件</dt><dd>{{ data.cumulative_events_preserved ? '清理明细后累计事件仍保留；明细维度不能从累计值恢复。' : '文件模式不提供独立累计事件保留保证。' }}</dd><dt>导出字段版本</dt><dd>{{ data.export_schema || '未报告' }}</dd><dt>查询时间</dt><dd>{{ data.checked_at ? formatTime(data.checked_at) : '未报告' }}</dd></dl><p>此处显示当前整个明细库的可用范围，不是当前筛选总量；最早记录不证明历史完整。已删除的逐条模型、账号等明细不能恢复。</p></template></details>
</template>
<style scoped>
.log-coverage { color: var(--text-3); font-size: 12px; line-height: 1.8; padding: 0 0 14px; }.log-coverage summary { color: var(--primary, #409eff); cursor: pointer; width: fit-content; }.log-coverage dl { display: grid; grid-template-columns: 140px minmax(0, 1fr); gap: 8px 14px; border: 1px solid var(--card-border); border-radius: 6px; padding: 16px; margin: 12px 0; background: var(--card-bg); }.log-coverage dd { margin: 0; color: var(--text-2); overflow-wrap: anywhere; }.log-coverage p { margin: 10px 0; }@media(max-width:500px) { .log-coverage dl { grid-template-columns: 1fr; gap: 3px; padding: 12px; }.log-coverage dd { margin-bottom: 8px; } }
</style>
