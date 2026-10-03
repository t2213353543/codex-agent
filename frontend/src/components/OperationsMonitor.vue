<script setup>
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { api } from '../api'
import { useManagementRead } from '../utils/management'
import { init } from '../utils/echarts'
import { formatDuration, formatNumber, formatPercent, formatTime } from '../utils/format'
import { logErrorOptions } from '../utils/logManagementState'
import { readPreference, writePreference } from '../utils/preferences'

const { loading, error, read } = useManagementRead()
const ranges = [['15m', '近 15 分钟'], ['1h', '近 1 小时'], ['6h', '近 6 小时'], ['24h', '近 24 小时']]
const range = ref('1h')
const data = ref(null)
const displayedRange = ref('')
const current = ref(false)
const autoRefresh = ref(readPreference('operationsAutoRefresh', true, value => typeof value === 'boolean'))
const throughputEl = ref(null)
const latencyEl = ref(null)
const chartInstances = new Map()
const categoryLabels = Object.fromEntries(logErrorOptions)
const totals = computed(() => data.value?.traffic || {})
const runtime = computed(() => data.value?.runtime || {})
const host = computed(() => runtime.value.host || {})
const minutes = computed(() => data.value ? (data.value.to - data.value.from) / 60 : 0)
const requestLink = computed(() => ({ path: '/logs', query: { from: String(data.value?.from || ''), to: String(data.value?.to || '') } }))
const failureLink = computed(() => ({ ...requestLink.value, query: { ...requestLink.value.query, status: 'failed' } }))
const errorLink = row => ({ path: '/logs', query: { request_id: String(row.id) } })
const byteSize = value => Number.isFinite(value) ? value >= 1024 ** 3 ? `${(value / 1024 ** 3).toFixed(2)} GiB` : `${(value / 1024 ** 2).toFixed(1)} MiB` : '—'
const latency = value => formatDuration(value, { empty: '暂无样本' })
const rate = value => Number.isFinite(value) && minutes.value > 0 ? formatNumber(Number((value / minutes.value).toFixed(2))) : '—'
const percent = (value, total) => total > 0 ? Math.min(100, Math.max(0, value / total * 100)) : 0
let interval
let observer
let disposed = false

async function load() {
  const requestedRange = range.value
  current.value = false
  await read(signal => api.operations({ range: requestedRange }, { signal, silent: true }), result => {
    if (result?.scope !== 'retained_request_details' || result?.runtime?.scope !== 'current_process' || !Number.isFinite(result?.traffic?.requests) || !Number.isFinite(result.checked_at) || !(result.step_seconds > 0) || !Array.isArray(result.series) || !Array.isArray(result.latency_histogram) || !Array.isArray(result.error_categories) || !Array.isArray(result.recent_errors)) throw new Error('运维监控数据不完整，请重新刷新')
    data.value = result
    displayedRange.value = ranges.find(([value]) => value === requestedRange)?.[1] || ''
    current.value = true
  })
  await nextTick()
  renderCharts()
}

function renderCharts() {
  if (disposed || !data.value) return
  const style = getComputedStyle(document.documentElement)
  const color = name => style.getPropertyValue(name).trim()
  const textColor = color('--text-3') || '#8b8f9c'
  const borderColor = color('--border') || '#343740'
  const primary = color('--el-color-primary') || '#8278db'
  const success = color('--badge-blue') || '#4b89d8'
  const danger = color('--el-color-danger') || '#e57777'
  const timeLabel = value => new Date(value * 1000).toLocaleTimeString('zh-CN', { timeZone: 'Asia/Shanghai', hour: '2-digit', minute: '2-digit', second: data.value.step_seconds < 60 ? '2-digit' : undefined, hour12: false })
  const series = data.value.series
  const base = {
    animation: false, textStyle: { color: textColor },
    grid: { top: 42, left: 6, right: 12, bottom: 8, containLabel: true },
    legend: { top: 0, itemWidth: 16, itemHeight: 8, itemGap: 12, textStyle: { color: textColor, fontSize: 11 } },
    tooltip: { trigger: 'axis', renderMode: 'richText', valueFormatter: value => value == null ? '暂无样本' : formatNumber(Number(Number(value).toFixed(2))) },
    xAxis: { type: 'category', data: series.map(point => timeLabel(point.time)), axisLabel: { color: textColor, hideOverlap: true }, axisLine: { lineStyle: { color: borderColor } }, axisTick: { show: false } },
  }
  function draw(element, option) {
    if (!element) return
    let chart = chartInstances.get(element)
    if (!chart) { chart = init(element); chartInstances.set(element, chart); observer?.observe(element) }
    chart.setOption(option, true)
  }
  const axis = name => ({ type: 'value', name, min: 0, nameTextStyle: { color: textColor }, axisLabel: { color: textColor, formatter: value => formatNumber(value) }, splitLine: { lineStyle: { color: borderColor, type: 'dashed' } } })
  draw(throughputEl.value, { ...base, color: [primary, success, danger], yAxis: [axis('RPM'), { ...axis('TPM'), splitLine: { show: false } }], series: [
    { name: '请求 RPM', type: 'line', showSymbol: false, data: series.map(point => point.requests * 60 / data.value.step_seconds) },
    { name: 'Token TPM', type: 'line', showSymbol: false, yAxisIndex: 1, data: series.map(point => point.tokens * 60 / data.value.step_seconds) },
    { name: '失败 RPM', type: 'line', showSymbol: false, lineStyle: { type: 'dashed' }, data: series.map(point => point.errors * 60 / data.value.step_seconds) },
  ] })
  draw(latencyEl.value, { ...base, tooltip: { ...base.tooltip, valueFormatter: latency }, color: [primary, success], yAxis: axis('ms'), series: [
    { name: '平均总耗时', type: 'line', showSymbol: true, showAllSymbol: true, symbolSize: 5, connectNulls: false, data: series.map(point => point.average_latency_ms) },
    { name: '平均首 Token', type: 'line', showSymbol: true, showAllSymbol: true, symbolSize: 5, connectNulls: false, data: series.map(point => point.average_ttft_ms) },
  ] })
}

watch(range, load)
watch(autoRefresh, value => writePreference('operationsAutoRefresh', value))
onMounted(() => {
  observer = new ResizeObserver(entries => { for (const { target } of entries) chartInstances.get(target)?.resize() })
  window.addEventListener('theme-change', renderCharts)
  interval = setInterval(() => { if (autoRefresh.value && !loading.value && document.visibilityState === 'visible') load() }, 30000)
  load()
})
onBeforeUnmount(() => {
  disposed = true
  clearInterval(interval)
  window.removeEventListener('theme-change', renderCharts)
  observer?.disconnect()
  for (const chart of chartInstances.values()) chart.dispose()
  chartInstances.clear()
})
</script>

<template>
  <section class="operations-monitor" aria-label="运维监控">
    <div class="ops-heading">
      <div class="ops-heading-title"><h2>运维监控</h2><p>请求质量、吞吐趋势与服务资源</p></div>
      <div class="ops-controls">
        <el-select v-model="range" size="small" aria-label="运维统计范围" class="ops-range"><el-option v-for="[value,label] in ranges" :key="value" :label="label" :value="value" /></el-select>
        <el-checkbox v-model="autoRefresh">自动刷新 · 30 秒</el-checkbox>
        <el-button size="small" :loading="loading" @click="load">刷新监控</el-button>
      </div>
    </div>
    <el-alert v-if="error" :title="`${error}${data ? '；下方保留上次成功结果。' : ''}`" type="error" :closable="false" show-icon />
    <div v-if="!data && loading" class="panel ops-placeholder" role="status">正在读取运维监控…</div>
    <template v-if="data">
      <div class="ops-scope"><span>{{ displayedRange }} · {{ formatTime(data.from) }} — {{ formatTime(data.to) }}（上海时间）</span><span>{{ current ? '更新于' : '上次成功更新于' }} {{ formatTime(data.checked_at) }}</span></div>
      <div class="ops-kpis">
        <div class="panel ops-kpi"><span>成功率</span><strong>{{ formatPercent(totals.success / totals.requests, totals.requests) }}</strong><small>{{ formatNumber(totals.success) }} 成功 / {{ formatNumber(totals.requests) }} 请求</small></div>
        <div class="panel ops-kpi"><span>平均请求吞吐</span><strong>{{ rate(totals.requests) }} <em>RPM</em></strong><small>{{ formatNumber(totals.errors) }} 失败，含 {{ formatNumber(totals.aborted) }} 次中断</small></div>
        <div class="panel ops-kpi"><span>平均 Token 吞吐</span><strong>{{ rate(totals.tokens) }} <em>TPM</em></strong><small>共 {{ formatNumber(totals.tokens) }} Token</small></div>
        <div class="panel ops-kpi"><span>当前处理请求</span><strong>{{ formatNumber(runtime.in_flight) }}</strong><small>当前进程已准入的请求</small></div>
        <div class="panel ops-kpi"><span>平均请求耗时</span><strong>{{ latency(totals.average_latency_ms) }}</strong><small>{{ formatNumber(totals.latency_samples) }} 个耗时样本</small></div>
        <div class="panel ops-kpi"><span>平均首 Token 时间</span><strong>{{ latency(totals.average_ttft_ms) }}</strong><small>{{ formatNumber(totals.ttft_samples) }} 个首 Token 样本</small></div>
      </div>
      <div class="ops-grid ops-trends">
        <section class="panel ops-card"><h3>吞吐与失败趋势</h3><div ref="throughputEl" class="ops-chart" role="img" aria-label="请求和 Token 吞吐及失败趋势图" /><p>每个点为 {{ data.step_seconds }} 秒内的平均速率。</p></section>
        <section class="panel ops-card"><h3>请求耗时与首 Token 趋势</h3><div ref="latencyEl" class="ops-chart" role="img" aria-label="请求耗时和首 Token 时间趋势图" /><p>无观测样本的区间留空。</p></section>
      </div>
      <div class="ops-grid ops-distributions">
        <section class="panel ops-card"><h3>请求耗时分布</h3><div v-for="bucket in data.latency_histogram" :key="bucket.label" class="ops-bar-row"><span>{{ bucket.label }}</span><el-progress :percentage="percent(bucket.requests, totals.latency_samples)" :show-text="false" /><b>{{ formatNumber(bucket.requests) }}</b></div><p>仅统计已记录的耗时样本，包含实际测得的 0 ms。</p></section>
        <section class="panel ops-card"><h3>错误分布</h3><div v-for="category in data.error_categories" :key="category.name" class="ops-bar-row"><span>{{ categoryLabels[category.name] || category.name }}</span><el-progress :percentage="percent(category.requests, totals.errors)" :show-text="false" status="exception" /><b>{{ formatNumber(category.requests) }}</b></div><div v-if="!data.error_categories.length" class="ops-empty">当前时间范围暂无失败请求</div><router-link :to="failureLink">查看失败请求 →</router-link></section>
      </div>
      <section class="panel ops-card ops-resource-card">
        <div class="ops-title-row"><h3>服务资源与存储</h3><el-tag size="small" :type="runtime.metrics_status === 'ok' ? 'success' : 'danger'">{{ runtime.metrics_status === 'ok' ? '用量写入正常' : '用量写入异常' }}</el-tag></div>
        <dl class="ops-resources">
          <div><dt>主机 CPU 利用率</dt><dd>{{ Number.isFinite(host.cpu_percent) ? `${host.cpu_percent.toFixed(1)}%` : host.cpu_status === 'warming_up' ? '等待下次采样' : '未采集' }}</dd></div><div><dt>主机内存使用率</dt><dd>{{ Number.isFinite(host.memory_available_bytes) && host.memory_total_bytes > 0 ? formatPercent(1 - host.memory_available_bytes / host.memory_total_bytes) : '未采集' }}</dd></div><div><dt>主机可用内存</dt><dd>{{ byteSize(host.memory_available_bytes) }}</dd></div><div><dt>主机总内存</dt><dd>{{ byteSize(host.memory_total_bytes) }}</dd></div>
          <div><dt>Go 堆已分配</dt><dd>{{ byteSize(runtime.heap_bytes) }}</dd></div><div><dt>Go 堆占用</dt><dd>{{ byteSize(runtime.heap_in_use_bytes) }}</dd></div><div><dt>Go 运行时保留内存</dt><dd>{{ byteSize(runtime.go_reserved_bytes) }}</dd></div><div><dt>栈内存</dt><dd>{{ byteSize(runtime.stack_bytes) }}</dd></div>
          <div><dt>可用 CPU / 并行线程上限</dt><dd>{{ runtime.cpu_cores }} / {{ runtime.go_max_procs }}</dd></div><div><dt>Goroutine</dt><dd>{{ formatNumber(runtime.goroutines) }}</dd></div><div><dt>GC 次数 / 最近暂停</dt><dd>{{ formatNumber(runtime.gc_count) }} / {{ latency(runtime.last_gc_pause_ms) }}</dd></div><div><dt>待写入 / 累计查询失败</dt><dd>{{ formatNumber(runtime.pending_writes) }} / {{ formatNumber(runtime.read_failures) }}</dd></div>
        </dl>
        <div class="ops-pools">
          <div><h4>PostgreSQL 连接池</h4><template v-if="runtime.postgres_pool"><p>使用中 {{ runtime.postgres_pool.in_use }} · 空闲 {{ runtime.postgres_pool.idle }} · 总连接 {{ runtime.postgres_pool.total }} / {{ runtime.postgres_pool.max }}</p><p>累计等待 {{ formatNumber(runtime.postgres_pool.wait_count) }} 次 · 取消获取 {{ formatNumber(runtime.postgres_pool.canceled_acquires) }} 次</p></template><p v-else>当前进程未配置 PostgreSQL 连接池</p></div>
          <div><h4>Redis 连接池</h4><template v-if="runtime.redis_pool"><p>总连接 {{ runtime.redis_pool.total }} · 空闲 {{ runtime.redis_pool.idle }}</p><p>累计复用 {{ formatNumber(runtime.redis_pool.hits) }} · 新建需求 {{ formatNumber(runtime.redis_pool.misses) }} · 等待超时 {{ formatNumber(runtime.redis_pool.timeouts) }}</p></template><p v-else>当前进程未配置 Redis 连接池</p></div>
        </div>
        <p>主机指标来自操作系统，容器环境可能反映宿主机资源。<template v-if="host.cpu_sample_seconds">CPU 为最近 {{ Number(host.cpu_sample_seconds.toFixed(1)) }} 秒的平均利用率。</template>Go 资源与连接池属于当前进程；累计计数随进程重启归零。</p>
      </section>
      <section class="panel ops-card ops-errors"><div class="ops-title-row"><h3>近期失败请求</h3><router-link :to="failureLink">查看使用记录 →</router-link></div><el-table :data="data.recent_errors" size="small" empty-text="当前时间范围暂无失败请求"><el-table-column label="请求时间（上海）" min-width="165"><template #default="{ row }">{{ formatTime(row.time) }}</template></el-table-column><el-table-column prop="model" label="模型" min-width="150" show-overflow-tooltip /><el-table-column label="HTTP" width="70"><template #default="{ row }">{{ row.http_code || '—' }}</template></el-table-column><el-table-column label="原因" min-width="115"><template #default="{ row }">{{ categoryLabels[row.category] || row.category }}</template></el-table-column><el-table-column label="记录" width="64" fixed="right"><template #default="{ row }"><router-link v-if="row.id != null" :to="errorLink(row)" :aria-label="`查看失败记录 ${row.id}`">详情</router-link><span v-else>—</span></template></el-table-column></el-table></section>
      <p class="ops-footnote">统计范围为已保留的请求明细；清理记录会影响这些指标。Token 已观测 {{ formatNumber(totals.usage_known_requests) }} / {{ formatNumber(totals.requests) }} 次请求，缺失用量不会估算补齐。<router-link :to="requestLink">查看全部请求 →</router-link></p>
    </template>
    <el-empty v-else-if="!loading" description="暂无运维监控数据"><el-button @click="load">重新读取</el-button></el-empty>
  </section>
</template>

<style scoped>
.operations-monitor{margin-top:16px;min-width:0}.operations-monitor .panel{margin-bottom:0}
.ops-heading,.ops-heading-title,.ops-controls,.ops-title-row,.ops-scope{display:flex;align-items:center;justify-content:space-between;gap:8px 12px;flex-wrap:wrap}
.ops-heading{margin-bottom:8px}.ops-heading-title{justify-content:flex-start}.ops-heading h2{font-size:15px;margin:0;white-space:nowrap}
.ops-heading p,.ops-card p,.ops-footnote{color:var(--text-3);font-size:11px;line-height:1.6}.ops-heading p{margin:0}
.ops-controls{justify-content:flex-end;gap:6px 10px}.ops-range{width:124px}.ops-controls :deep(.el-checkbox){height:28px;margin-right:0}.ops-controls :deep(.el-checkbox__label){font-size:11px}
.ops-scope{font-size:11px;color:var(--text-3);margin:8px 0;line-height:1.5}
.ops-kpis{display:grid;grid-template-columns:repeat(6,minmax(0,1fr));gap:9px}
.ops-kpi{padding:12px;display:flex;flex-direction:column;gap:5px;min-width:0}
.ops-kpi>span,.ops-kpi small{font-size:11px;line-height:1.5;color:var(--text-3)}
.ops-kpi strong{font-size:21px;font-weight:650;line-height:1.3;color:var(--text-1);font-variant-numeric:tabular-nums;overflow-wrap:anywhere}
.ops-kpi em{font-size:11px;font-style:normal;font-weight:400;color:var(--text-3)}
.ops-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:12px;margin-top:12px}
.ops-card{padding:13px 15px;min-width:0}.ops-card h3{font-size:13px;margin:0 0 9px}.ops-card h4{font-size:12px;margin:0 0 4px}.ops-title-row h3{margin:0}
.ops-chart{height:210px;width:100%;min-width:0}.ops-card p{margin:6px 0 0}
.ops-bar-row{display:grid;grid-template-columns:minmax(85px,110px) minmax(0,1fr) minmax(32px,max-content);align-items:center;gap:9px;margin:9px 0;font-size:12px;line-height:1.4}
.ops-bar-row>span{overflow-wrap:anywhere}.ops-bar-row b{text-align:right;font-weight:500;font-variant-numeric:tabular-nums}
.ops-empty{padding:15px 0;color:var(--text-3);font-size:12px}.ops-card a,.ops-footnote a{font-size:11px;color:var(--el-color-primary)}
.ops-resource-card,.ops-errors{margin-top:12px}
.ops-resources{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:0 16px;margin:10px 0}
.ops-resources>div{display:flex;align-items:baseline;justify-content:space-between;gap:8px;min-width:0;border-bottom:1px solid var(--card-border);padding:8px 0}
.ops-resources dt{font-size:11px;color:var(--text-3);line-height:1.5}.ops-resources dd{font-size:12px;text-align:right;margin:0;overflow-wrap:anywhere;font-variant-numeric:tabular-nums}
.ops-pools{padding-top:4px;display:grid;grid-template-columns:1fr 1fr;gap:10px 24px}.ops-pools p{margin:3px 0 0}
.ops-errors :deep(.el-table){margin-top:9px}.ops-errors :deep(.el-table .cell){line-height:1.5}.ops-errors :deep(.el-table__cell){padding-top:6px;padding-bottom:6px}
.ops-footnote{margin:8px 0 0}.ops-footnote a{margin-left:12px}.ops-placeholder{padding:20px;text-align:center;color:var(--text-3)}
@media(max-width:1250px){.ops-kpis{grid-template-columns:repeat(3,minmax(0,1fr))}.ops-resources{grid-template-columns:repeat(3,minmax(0,1fr))}}
@media(max-width:1000px){.ops-resources{grid-template-columns:repeat(2,minmax(0,1fr))}.ops-chart{height:220px}}
@media(max-width:850px){.ops-grid{grid-template-columns:1fr}.ops-chart{height:210px}}
@media(max-width:600px){.ops-heading,.ops-controls{align-items:flex-start;justify-content:flex-start}.ops-heading-title{gap:4px 10px}.ops-card{padding:12px}.ops-kpis{grid-template-columns:repeat(2,minmax(0,1fr));gap:8px}.ops-kpi{padding:10px}.ops-kpi strong{font-size:20px}.ops-pools{grid-template-columns:1fr}.ops-resources{gap:0 12px}.ops-resources>div{display:block;padding:7px 0}.ops-resources dd{margin-top:3px;text-align:left}.ops-bar-row{grid-template-columns:95px minmax(0,1fr) minmax(28px,max-content);gap:8px}}
</style>
