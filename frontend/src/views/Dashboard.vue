<script setup>
import { computed, nextTick, onActivated, onDeactivated, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Tickets, Coin, CircleCloseFilled, Box, Refresh, User, CircleCheck, FolderOpened, Key } from '@element-plus/icons-vue'
import { init } from '../utils/echarts'
import { createLatestRequest } from '../utils/latestRequest'
import { createDashboardRealtimePoller } from '../utils/dashboardRealtime'
import { formatDuration, formatNumber, formatPercent, formatTime } from '../utils/format'
import { logErrorOptions } from '../utils/logManagementState'
import { readPreference, writePreference } from '../utils/preferences'
import { api, request } from '../api'
import PageHeader from '../components/PageHeader.vue'

const route = useRoute()
const router = useRouter()
const ranges = [
  { value: '1h', label: '近 1 小时', seconds: 3600 },
  { value: '6h', label: '近 6 小时', seconds: 21600 },
  { value: '24h', label: '近 24 小时', seconds: 86400 },
  { value: '7d', label: '近 7 天', seconds: 604800 },
  { value: '30d', label: '近 30 天', seconds: 2592000 },
]
const validRange = (value) => ranges.some((r) => r.value === value) ? value : '24h'
const range = ref(validRange(route.query.range))
const selectedRange = computed(() => ranges.find((r) => r.value === range.value))
const loading = ref(false)
const loadError = ref('')
const cards = ref({})
const prevCards = ref({})
const series = ref([])
const accountTop = ref([])
const status = ref([])
const recentErrors = ref([])
const breakdown = ref(null)
const topModels = computed(() => (breakdown.value?.models || []).slice(0, 6))
const errorCategories = computed(() => breakdown.value?.error_categories || [])
const categoryLabels = Object.fromEntries(logErrorOptions)
const breakdownReady = computed(() => breakdown.value?.scope === 'retained_request_details')
const realtime = ref(null)
const realtimeLoading = ref(false)
const realtimeError = ref('')
const realtimeHidden = ref(document.hidden)
const realtimeValues = computed(() => realtimeError.value ? null : realtime.value)
const realtimeNote = computed(() => realtimeError.value ? '更新失败' : realtimeHidden.value ? '后台已暂停' : !realtime.value ? '等待数据' : realtime.value.warming_up ? `已采集 ${realtime.value.coverage_seconds} 秒` : realtime.value.tokens_partial ? 'Token 部分已上报' : realtime.value.tpm === null ? 'Token 等待上报' : '最近 60 秒 · 5 秒刷新')
const realtimePoller = createDashboardRealtimePoller({
  fetch: signal => request('/overview/realtime', { signal, silent: true }),
  commit: value => { realtime.value = value; realtimeError.value = '' },
  error: error => { realtimeError.value = error.message || '实时吞吐更新失败' },
  loading: value => { realtimeLoading.value = value },
})
const averageLatency = computed(() => cards.value.latency_samples === 0 || !(cards.value.requests > 0) ? '暂无样本' : formatDuration(cards.value.avg_latency_ms, { empty: '暂无样本' }))
const displayedRange = ref(null)
const displayedLabel = ref('')
const updatedAt = ref(0)
const autoRefresh = ref(readPreference('dashboardAutoRefresh', true, value => typeof value === 'boolean'))
watch(autoRefresh, value => writePreference('dashboardAutoRefresh', value))
const hasTraffic = computed(() => Number(cards.value.requests) > 0)
const timezone = ref('服务端时区')
const retentionDays = ref(92)
const totalsScope = ref({})
const chartEl = ref(null)
const donutEl = ref(null)
const sparkEls = new Map()
const instances = new Map()
const latest = createLatestRequest()
let observer
let timer
let active = false
let requestID = 0

const kpiDefs = [
  { key: 'requests', label: '请求数', icon: Tickets, spark: (s) => s.requests },
  { key: 'tokens', label: 'Token 消耗', icon: Coin, spark: (s) => s.tokens },
  { key: 'errors', label: '失败请求', icon: CircleCloseFilled, spark: (s) => s.errors, inverse: true },
  { key: 'cache_rate', label: '缓存读取命中率', icon: Box, spark: (s) => s.cache_read_samples > 0 ? s.cache_read_rate : null, noPrev: true },
]
const resourceDefs = [
  { key: 'accounts', label: '账号总数', link: '/accounts', icon: User, tone: 'blue' },
  { key: 'accounts_enabled', label: '启用账号', link: '/accounts', icon: CircleCheck, tone: 'teal' },
  { key: 'groups', label: '分组数量', link: '/groups', icon: FolderOpened, tone: 'violet' },
  { key: 'keys', label: 'API KEY 数', link: '/keys', icon: Key, tone: 'amber' },
]
const flowDefs = [
  { key: 'success', label: '成功请求' },
  { key: 'input', label: '输入 Token', series: true },
  { key: 'output', label: '输出 Token', series: true },
  { key: 'cache_read_tokens', label: '缓存读取 Token', cacheSamples: 'cache_read_samples' },
  { key: 'cache_write_tokens', label: '缓存创建 Token', cacheSamples: 'cache_write_samples' },
  { key: 'avg_latency_ms', label: '平均耗时', latency: true },
  { key: 'peak_rpm', label: '峰值 RPM' },
  { key: 'peak_tpm', label: '峰值 TPM' },
]
function flowValue(def) {
  if (def.cacheSamples) return cards.value[def.cacheSamples] > 0 ? formatNumber(cards.value[def.key]) : '—'
  if (def.series) {
    if (!series.value.length) return Number(cards.value.requests) === 0 ? '0' : '—'
    if (series.value.some((point) => !Number.isFinite(point[def.key]))) return '—'
    return formatNumber(series.value.reduce((sum, point) => sum + point[def.key], 0))
  }
  if (def.latency) return averageLatency.value
  return formatNumber(cards.value[def.key])
}
const cacheDenominator = computed(() => {
  if (cards.value.cache_rate_has_samples === false) return 0
  if (cards.value.cache_rate_has_samples === true) return undefined
  if (Number(cards.value.requests) === 0) return 0
  const inputs = series.value.filter((point) => Number.isFinite(point.input))
  return inputs.length ? inputs.reduce((sum, point) => sum + point.input, 0) : undefined
})

function cssVar(name, fallback) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || fallback
}
function delta(def) {
  const current = Number(cards.value[def.key])
  const previous = Number(prevCards.value[def.key])
  if (!Number.isFinite(current) || !Number.isFinite(previous) || previous === 0) return null
  return (current - previous) / previous
}
function kpiValue(def) {
  return def.key === 'cache_rate' ? formatPercent(cards.value.cache_rate, cacheDenominator.value) : formatNumber(cards.value[def.key])
}
function deltaColor(def) {
  const value = delta(def)
  if (value === null || value === 0) return 'var(--text-3)'
  return (def.inverse ? value < 0 : value > 0) ? 'var(--success)' : 'var(--danger)'
}
function deltaText(def) {
  const value = delta(def)
  if (value === null) return '上时段无可比样本'
  return `${value > 0 ? '↑ ' : value < 0 ? '↓ ' : ''}${formatPercent(Math.abs(value))} 较上时段`
}
function getChart(element) {
  if (!element || !active) return null
  if (!instances.has(element)) {
    instances.set(element, init(element))
    observer?.observe(element)
  }
  return instances.get(element)
}
function setSparkRef(key, element) {
  if (element) sparkEls.set(key, element)
}
function renderSparks() {
  const colors = ['--primary', '--warning', '--danger', '--success']
  kpiDefs.forEach((def, index) => {
    const chart = getChart(sparkEls.get(def.key))
    const color = cssVar(colors[index], '#6366f1')
    chart?.setOption({
      animation: false,
      grid: { left: 0, right: 0, top: 3, bottom: 3 },
      xAxis: { type: 'category', show: false, data: series.value.map((point) => point.ts) },
      yAxis: { type: 'value', show: false },
      series: [{ type: 'line', data: series.value.map(def.spark), smooth: true, showSymbol: false,
        lineStyle: { width: 1.5, color }, areaStyle: { color, opacity: 0.12 } }],
    }, true)
  })
}
function renderChart() {
  const chart = getChart(chartEl.value)
  if (!chart) return
  const longRange = series.value.length > 0 && series.value.at(-1).ts - series.value[0].ts > 172800
  const textColor = cssVar('--text-2', '#3f3f46')
  const axisColor = cssVar('--text-3', '#a1a1aa')
  const definitions = [
    ['请求数', 'requests', '--primary', 0], ['Token', 'tokens', '--warning', 1],
    ['输入 Token', 'input', '--text-3', 1], ['输出 Token', 'output', '--el-color-primary-light-3', 1],
    ['缓存读取 Token', 'cache_read_tokens', '--success', 1], ['缓存创建 Token', 'cache_write_tokens', '--warning', 1], ['失败', 'errors', '--danger', 0],
  ]
  chart.setOption({
    textStyle: { color: textColor, fontFamily: cssVar('--el-font-family', 'sans-serif') },
    tooltip: { renderMode: 'richText', trigger: 'axis', confine: true, axisPointer: { type: 'cross' },
      backgroundColor: cssVar('--card-bg', '#fff'), textStyle: { color: textColor } },
    legend: { type: 'scroll', top: 0, textStyle: { color: textColor }, data: definitions.map(([name]) => name) },
    grid: { left: 12, right: 12, top: 58, bottom: longRange ? 55 : 15, containLabel: true },
    dataZoom: longRange ? [{ type: 'inside' }, { type: 'slider', height: 18, bottom: 4 }] : [],
    xAxis: { type: 'category', data: series.value.map((point) => formatTime(point.ts)),
      axisLine: { lineStyle: { color: axisColor } }, axisLabel: { color: textColor, hideOverlap: true } },
    yAxis: [
      { type: 'value', name: '请求', nameTextStyle: { color: textColor }, axisLabel: { color: textColor }, splitLine: { lineStyle: { color: axisColor, opacity: 0.25 } } },
      { type: 'value', name: 'Token', nameTextStyle: { color: textColor }, axisLabel: { color: textColor }, splitLine: { show: false } },
    ],
    series: definitions.map(([name, key, color, axis]) => ({ name, type: key === 'errors' ? 'bar' : 'line',
      yAxisIndex: axis, smooth: true, showSymbol: false, lineStyle: { width: 2 }, barMaxWidth: 12,
      data: series.value.map((point) => key === 'cache_read_tokens' ? point.cache_read_samples > 0 ? point[key] : null : key === 'cache_write_tokens' ? point.cache_write_samples > 0 ? point[key] : null : point[key] ?? 0), itemStyle: { color: cssVar(color, '#6366f1') } })),
  }, true)
}
function renderDonut() {
  const chart = getChart(donutEl.value)
  if (!chart) return
  const ok = Number(status.value.find((item) => item.name === '成功')?.value) || 0
  const error = Number(status.value.find((item) => item.name === '失败')?.value) || 0
  const total = ok + error
  const color = cssVar('--text-1', '#1f2937')
  chart.setOption({
    textStyle: { fontFamily: cssVar('--el-font-family', 'sans-serif') },
    tooltip: { renderMode: 'richText', trigger: 'item', confine: true },
    legend: { bottom: 0, textStyle: { color } },
    series: [{ type: 'pie', radius: ['58%', '78%'], center: ['50%', '44%'], label: { show: false },
      data: [
        { name: '成功', value: ok, itemStyle: { color: cssVar('--success', '#67c23a') } },
        { name: '失败', value: error, itemStyle: { color: cssVar('--danger', '#f56c6c') } },
      ],
    }],
    title: { text: formatPercent(total ? ok / total : null, total), subtext: '请求成功率', left: 'center', top: '36%',
      textStyle: { fontSize: total ? 26 : 18, fontWeight: 700, color }, subtextStyle: { fontSize: 12, color: cssVar('--text-3', '#9ca3af') } },
  }, true)
}
function renderAll() {
  if (!active) return
  renderChart()
  renderDonut()
  renderSparks()
}
async function load() {
  const id = ++requestID
  loading.value = true
  loadError.value = ''
  const now = Math.floor(Date.now() / 1000)
  const seconds = selectedRange.value.seconds
  const requestedRange = { from: now - seconds, to: now }
  const requestedLabel = selectedRange.value.label
  try {
    await latest.run((signal) => Promise.all([
      api.overview({ ...requestedRange, include_breakdown: 'true' }, { signal }),
      api.overview({ from: now - 2 * seconds, to: now - seconds }, { signal }),
    ]), async ([data, previous]) => {
      cards.value = data.cards || {}
      prevCards.value = previous.cards || {}
      series.value = Array.isArray(data.series) ? data.series : []
      accountTop.value = Array.isArray(data.account_top) ? data.account_top : []
      status.value = Array.isArray(data.status) ? data.status : []
      recentErrors.value = Array.isArray(data.recent_errors) ? data.recent_errors : []
      breakdown.value = data.traffic_breakdown && typeof data.traffic_breakdown === 'object' ? data.traffic_breakdown : null
      displayedLabel.value = requestedLabel
      updatedAt.value = Math.floor(Date.now() / 1000)
      const from = Number(data.range?.from)
      const to = Number(data.range?.to)
      displayedRange.value = Number.isSafeInteger(from) && from > 0 && Number.isSafeInteger(to) && to > from ? { from, to } : requestedRange
      timezone.value = typeof data.timezone === 'string' && data.timezone ? data.timezone : '服务端时区'
      retentionDays.value = Number.isFinite(data.retention_days) ? data.retention_days : 92
      totalsScope.value = data.totals_scope && typeof data.totals_scope === 'object' ? data.totals_scope : {}
      await nextTick()
      if (id === requestID) renderAll()
    })
  } catch (error) {
    if (id === requestID) loadError.value = error.message || '概览加载失败，请重试'
  } finally {
    if (id === requestID) loading.value = false
  }
}
function setRange(value) {
  router.replace({ query: { ...route.query, range: value } })
}
function refreshAll() { load(); realtimePoller.refresh() }
function realtimeVisibilityChanged() { realtimeHidden.value = document.hidden; realtimePoller.visibilityChanged(!document.hidden) }
function viewAccount(name) { router.push({ path: '/accounts', query: { search: name } }) }
function viewTraffic(filters = {}) {
  if (!displayedRange.value) return
  const { from, to } = displayedRange.value
  router.push({ path: '/logs', query: { from: String(from), to: String(to), ...filters } })
}
function shareWidth(value, total) { return `${total > 0 ? Math.min(100, Math.max(0, value / total * 100)) : 0}%` }
function viewErrors(account) {
  if (!displayedRange.value) return
  const { from, to } = displayedRange.value
  router.push({ path: '/logs', query: { status: 'failed', from: String(from), to: String(to), ...(account ? { account } : {}) } })
}
watch(() => route.query.range, (value) => {
  const next = validRange(value)
  if (range.value !== next) { range.value = next; if (active) load() }
})
function start() {
  if (active) return
  active = true
  observer = new ResizeObserver((entries) => entries.forEach(({ target }) => instances.get(target)?.resize()))
  window.addEventListener('theme-change', renderAll)
  load()
  realtimePoller.start(!document.hidden)
  document.addEventListener('visibilitychange', realtimeVisibilityChanged)
  timer = setInterval(() => { if (autoRefresh.value && !loading.value && !document.hidden) load() }, 30000)
}
function stop() {
  active = false
  ++requestID
  latest.cancel()
  realtimePoller.stop()
  document.removeEventListener('visibilitychange', realtimeVisibilityChanged)
  clearInterval(timer)
  window.removeEventListener('theme-change', renderAll)
  observer?.disconnect()
  instances.forEach((chart) => chart.dispose())
  instances.clear()
}
onMounted(start)
onActivated(start)
onDeactivated(stop)
onUnmounted(stop)
</script>

<template>
  <div class="dashboard-page" :aria-busy="loading">
    <PageHeader title="运行总览" subtitle="查看运行状态，点击模型或失败原因定位对应请求">
      <el-radio-group :model-value="range" aria-label="概览时间范围" size="small" class="range-options" @change="setRange">
        <el-radio-button v-for="item in ranges" :key="item.value" :value="item.value">{{ item.label }}</el-radio-button>
      </el-radio-group>
      <el-checkbox v-model="autoRefresh" class="dashboard-auto-refresh">自动刷新 · 30 秒</el-checkbox>
      <el-button :icon="Refresh" :loading="loading" aria-label="刷新运行概览" @click="refreshAll">刷新</el-button>
    </PageHeader>
    <el-alert v-if="loadError" :title="loadError" type="error" show-icon :closable="false" class="load-error"><el-button link type="primary" :loading="loading" @click="load">重新加载</el-button></el-alert>
    <div class="dashboard-context">
      <span class="scope-label">{{ displayedLabel || selectedRange.label }} · 流量与使用情况</span>
      <div class="rates-strip" aria-label="近 5 分钟平均吞吐">
        <span class="metric-label">近 5 分钟平均</span>
        <span data-metric="rpm"><span class="rate-badge">RPM</span><strong>{{ formatNumber(cards.rpm) }}</strong><small>请求 / 分钟</small></span>
        <span data-metric="tpm"><span class="rate-badge">TPM</span><strong>{{ formatNumber(cards.tpm) }}</strong><small>Token / 分钟</small></span>
      </div>
      <span class="refresh-status"><span class="status-dot" :class="{ pending: loading }" />{{ loading ? '正在更新' : updatedAt ? `更新于 ${formatTime(updatedAt)}` : '等待数据' }}</span>
    </div>
    <div class="kpi-grid">
      <div v-for="def in kpiDefs" :key="def.key" class="kpi-card panel" :class="'metric-' + def.key">
        <div class="kpi-head">
          <div class="kpi-label">{{ def.label }}</div>
          <div class="kpi-icon" :class="'kpi-' + def.key"><el-icon :size="17"><component :is="def.icon" /></el-icon></div>
          <div class="kpi-value">{{ kpiValue(def) }}</div>
        </div>
        <div class="kpi-foot">
          <div v-if="!def.noPrev" class="kpi-delta" :style="{ color: deltaColor(def) }">{{ deltaText(def) }}</div>
          <span v-else class="kpi-note" :title="`缓存仅统计已留存明细；${cards.cache_read_samples || 0} 条读取已知，${cards.cache_read_unknown_requests || 0} 条未知`">已观测读取 / 对应输入 · 不含创建</span>
          <div :ref="(element) => setSparkRef(def.key, element)" class="kpi-spark" aria-hidden="true" />
        </div>
      </div>
    </div>
    <div class="resources-heading"><h3>资源概览</h3><span>当前配置 · 不受时间范围影响</span></div>
    <div class="resource-grid">
      <el-card v-for="def in resourceDefs" :key="def.key" shadow="never" class="resource-card" :data-tone="def.tone" :data-metric="def.key">
        <div class="metric-label">{{ def.label }}<el-button link type="primary" @click="router.push(def.link)">管理</el-button></div>
        <div class="resource-value-row"><div class="metric-value">{{ formatNumber(cards[def.key]) }}</div><div class="resource-icon" aria-hidden="true"><el-icon :size="23"><component :is="def.icon" /></el-icon></div></div>
        <div class="metric-note" v-if="def.key === 'accounts_enabled'">启用状态不代表凭据已验证</div>
        <div class="metric-note" v-else-if="def.key === 'keys'">已启用 {{ formatNumber(cards.keys_enabled) }}</div>
        <div class="metric-note" v-else>{{ def.key === 'accounts' ? '全部账号 · 不受时间范围影响' : '全部分组 · 不受时间范围影响' }}</div>
      </el-card>
    </div>
    <div class="operational-strip" aria-label="运行健康指标">
      <div class="operational-metric" data-metric="in_flight"><span class="live-indicator" aria-hidden="true" /><div><span>正在处理</span><strong>{{ formatNumber(cards.in_flight) }}<small>请求</small></strong></div><em>当前进程</em></div>
      <div class="operational-metric operational-realtime" data-metric="realtime_rates" :aria-busy="realtimeLoading" :title="realtimeError || '当前进程最近 60 秒；RPM 统计进入的推理请求，TPM 统计完成请求已上报的输入与输出 Token'">
        <span class="health-indicator tone-blue" aria-hidden="true"><el-icon><Tickets /></el-icon></span>
        <div class="realtime-content"><span>实时吞吐<span class="realtime-scope">当前进程</span></span><div class="realtime-values"><strong data-metric="realtime_rpm">{{ formatNumber(realtimeValues?.rpm) }}<small>RPM</small></strong><strong data-metric="realtime_tpm">{{ formatNumber(realtimeValues?.tpm) }}<small>TPM</small></strong></div><span class="realtime-note">{{ realtimeNote }}<time v-if="realtime?.updated_at && !realtimeError" :title="formatTime(realtime.updated_at)">{{ formatTime(realtime.updated_at).slice(-8) }}</time></span></div>
      </div>
      <div class="operational-metric" data-metric="response_latency"><span class="health-indicator tone-purple"><el-icon><Tickets /></el-icon></span><div><span>平均响应耗时</span><strong>{{ averageLatency }}</strong></div><em>{{ cards.latency_samples === undefined ? '所选范围' : `${formatNumber(cards.latency_samples)} 个耗时样本` }}</em></div>
      <div class="operational-metric" data-metric="today_requests_visible"><span class="health-indicator tone-blue"><el-icon><Coin /></el-icon></span><div><span>今日请求 / Token</span><strong>{{ formatNumber(cards.requests_today) }}<small>/ {{ formatNumber(cards.tokens_today) }}</small></strong></div><em>{{ timezone }}</em></div>
    </div>
    <div class="dashboard-chart-grid">
      <div class="panel chart-panel">
        <div class="panel-title"><div class="panel-heading"><span class="panel-icon tone-blue"><el-icon><Tickets /></el-icon></span><div><span>流量趋势</span><p class="panel-caption">请求、Token 与缓存读取、创建随时间的变化</p></div></div><span class="panel-sub soft-label">{{ displayedLabel || selectedRange.label }}</span></div>
        <div class="traffic-frame" :class="{ 'without-traffic': !hasTraffic && !loading }">
          <div ref="chartEl" class="traffic-chart" role="img" aria-label="所选时间范围的请求与 Token 流量趋势" />
          <div v-if="!hasTraffic && !loading" class="chart-empty"><el-icon :size="28"><Tickets /></el-icon><strong>这个时段还没有请求</strong><span>接入客户端后，流量与用量会显示在这里。</span><el-button link type="primary" @click="router.push('/keys')">查看接入密钥</el-button></div>
        </div>
      </div>
      <div class="panel chart-panel status-panel">
        <div class="panel-title"><div class="panel-heading"><span class="panel-icon tone-green"><el-icon><CircleCheck /></el-icon></span><div><span>请求状态分布</span><p class="panel-caption">所选时段的成功与失败</p></div></div></div>
        <div ref="donutEl" class="status-chart" role="img" aria-label="所选时间范围的请求成功率" />
      </div>
    </div>
    <div class="dashboard-breakdown-grid">
      <section class="panel model-panel">
        <div class="panel-title"><div class="panel-heading"><span class="panel-icon tone-purple"><el-icon><Box /></el-icon></span><div><span>模型使用分布</span><p class="panel-caption">按请求数排名 · 已保留的请求明细</p></div></div><span class="panel-sub soft-label">TOP 6</span></div>
        <div v-if="topModels.length" class="model-list">
          <article v-for="(model, index) in topModels" :key="model.name" class="model-row" :style="{ '--model-color': ['var(--badge-blue)', 'var(--badge-purple)', 'var(--badge-green)', 'var(--badge-amber)', 'var(--badge-red)', 'var(--badge-neutral)'][index] }">
            <div class="model-row-heading"><el-button v-if="model.name" link class="model-name" :title="model.name" @click="viewTraffic({ model: model.name })"><span class="model-dot" />{{ model.name }}</el-button><span v-else class="model-name"><span class="model-dot" />未记录模型</span><strong>{{ formatNumber(model.requests) }}<small>请求 · {{ formatPercent(model.requests / breakdown.requests, breakdown.requests) }}</small></strong></div>
            <div class="model-track"><span :style="{ width: shareWidth(model.requests, breakdown.requests) }" /></div>
            <div class="model-row-meta"><span>{{ model.usage_known_requests > 0 ? `${formatNumber(model.tokens)} Token${model.usage_known_requests < model.requests ? ' · 部分已知' : ''}` : 'Token 未知' }}</span><span>成功率 {{ formatPercent((model.requests - model.errors) / model.requests, model.requests) }}</span></div>
          </article>
        </div>
        <div v-else class="breakdown-empty"><el-icon><Box /></el-icon><span>{{ breakdownReady ? '所选范围暂无模型请求明细' : '模型统计暂不可用' }}</span></div>
        <p v-if="breakdownReady" class="breakdown-note">覆盖 {{ formatNumber(breakdown.requests) }} 条已保留明细<span v-if="breakdown.requests < cards.requests"> · 该时段共 {{ formatNumber(cards.requests) }} 次请求，明细覆盖不完整</span></p>
      </section>
      <section class="panel failure-panel">
        <div class="panel-title"><div class="panel-heading"><span class="panel-icon tone-amber"><el-icon><CircleCloseFilled /></el-icon></span><div><span>失败原因分布</span><p class="panel-caption">按错误类别汇总 · 点击查看对应日志</p></div></div><span class="failure-total">{{ breakdownReady ? formatNumber(breakdown.errors) : '—' }}<small>次失败</small></span></div>
        <div v-if="errorCategories.length" class="failure-list">
          <button v-for="category in errorCategories" :key="category.name" type="button" class="failure-row" @click="viewTraffic({ status: 'failed', error_category: category.name })"><span class="failure-label">{{ categoryLabels[category.name] || category.name || '其他错误' }}</span><span class="failure-track"><span :style="{ width: shareWidth(category.requests, breakdown.errors) }" /></span><strong>{{ formatNumber(category.requests) }}</strong><small>{{ formatPercent(category.requests / breakdown.errors, breakdown.errors) }}</small></button>
        </div>
        <div v-else class="breakdown-empty"><el-icon><CircleCheck /></el-icon><span>{{ breakdownReady ? '已保留明细中没有失败请求' : '错误分类统计暂不可用' }}</span></div>
        <p class="breakdown-note">{{ breakdownReady ? '基于请求结果分类，包含客户端中断；与日志筛选口径一致。' : '统计将在数据加载后显示。' }}</p>
      </section>
    </div>
    <details class="details-panel panel"><summary class="section-heading"><span class="section-symbol tone-blue" aria-hidden="true"><el-icon><Coin /></el-icon></span><h3>详细流量指标</h3><span>输入、输出、缓存读写与峰值吞吐</span></summary>
    <div class="flow-grid">
      <el-card v-for="def in flowDefs" :key="def.key" shadow="never" class="compact-card" :data-metric="def.key">
        <div class="metric-label">{{ def.label }}</div><div class="metric-value">{{ flowValue(def) }}</div>
        <small v-if="def.cacheSamples" class="panel-caption">已留存明细 · {{ formatNumber(cards[def.cacheSamples]) }} 条有记录</small>
      </el-card>
      <el-card shadow="never" class="compact-card" data-metric="success_rate"><div class="metric-label">请求成功率</div><div class="metric-value">{{ formatPercent(cards.requests > 0 ? cards.success / cards.requests : null, cards.requests) }}</div></el-card>
    </div>
    </details>
    <div class="dashboard-activity-grid">
      <div class="panel ranking-panel">
        <div class="panel-title"><div class="panel-heading"><span class="panel-icon tone-purple"><el-icon><User /></el-icon></span><div><span>账号流量排行</span><p class="panel-caption">了解所选时段的账号使用情况</p></div></div><span class="panel-sub soft-label">TOP 10</span></div>
        <el-table :data="accountTop" size="small" max-height="340" empty-text="所选范围暂无账号流量">
          <el-table-column type="index" label="#" width="48" />
          <el-table-column label="账号" min-width="160"><template #default="{ row }"><el-button link type="primary" class="wrap-link" @click="viewAccount(row.name)">{{ row.name }}</el-button></template></el-table-column>
          <el-table-column prop="requests" label="请求数" width="90" align="right" />
          <el-table-column label="Token" width="110" align="right"><template #default="{ row }">{{ formatNumber(row.tokens) }}</template></el-table-column>
          <el-table-column label="成功率" width="110" align="right"><template #default="{ row }">{{ formatPercent(row.requests ? (row.requests - row.errors) / row.requests : null, row.requests) }}</template></el-table-column>
          <el-table-column label="失败" width="80" align="right"><template #default="{ row }"><el-button v-if="row.errors > 0" link type="danger" :aria-label="'查看 ' + row.name + ' 的错误日志'" @click="viewErrors(row.name)">{{ formatNumber(row.errors) }}</el-button><span v-else>{{ formatNumber(row.errors) }}</span></template></el-table-column>
        </el-table>
      </div>
      <div class="panel incidents-panel">
        <div class="panel-title"><div class="panel-heading"><span class="panel-icon tone-amber"><el-icon><CircleCloseFilled /></el-icon></span><div><span>最近错误</span><p class="panel-caption">所选时段的失败请求摘要</p></div></div><el-button link type="primary" :disabled="!displayedRange" @click="viewErrors()">查看日志</el-button></div>
        <div v-if="recentErrors.length" class="incident-list">
          <article v-for="(error, index) in recentErrors" :key="error.id || index" class="incident-item">
            <div class="incident-heading"><el-button link type="primary" class="wrap-link" @click="viewErrors(error.account)">{{ error.account || '未关联账号' }}</el-button><time>{{ formatTime(error.time) }}</time></div>
            <div v-if="error.http_code || error.model || error.endpoint" class="incident-tags"><span v-if="error.http_code" class="incident-http">HTTP {{ error.http_code }}</span><span v-if="error.model" :title="error.model">{{ error.model }}</span><span v-if="error.endpoint">{{ error.endpoint }}</span></div>
            <p>{{ error.error || '请求失败，查看日志了解详情' }}</p>
          </article>
        </div>
        <div v-else class="quiet-state"><el-icon :size="24"><CircleCheck /></el-icon><strong>所选范围暂无错误记录</strong><span>{{ hasTraffic ? '有请求发生时，失败信息会汇总在这里。' : '产生请求后可在这里查看错误摘要。' }}</span></div>
      </div>
    </div>
    <details class="scope-summary panel">
      <summary class="section-heading"><span class="section-symbol tone-purple" aria-hidden="true"><el-icon><Box /></el-icon></span><h3>今日与累计</h3><span>今日（{{ timezone }}） · 展开查看长期用量</span></summary>
      <div class="summary-grid">
        <el-card shadow="never" class="compact-card" data-metric="requests_today"><div class="metric-label">今日请求</div><div class="metric-value">{{ formatNumber(cards.requests_today) }}</div></el-card>
        <el-card shadow="never" class="compact-card" data-metric="tokens_today"><div class="metric-label">今日 Token</div><div class="metric-value">{{ formatNumber(cards.tokens_today) }}</div></el-card>
        <el-card shadow="never" class="compact-card" data-metric="avg_latency_today"><div class="metric-label">今日平均耗时</div><div class="metric-value">{{ cards.requests_today > 0 ? formatDuration(cards.avg_latency_today, { empty: '暂无样本' }) : '暂无样本' }}</div></el-card>
        <el-card shadow="never" class="compact-card" data-metric="tokens_total"><div class="metric-label">累计 Token</div><div class="metric-value">{{ totalsScope.tokens_total === 'lifetime' ? formatNumber(cards.tokens_total) : '—' }}</div><div class="metric-note">持久事件累计 · 含导入基线</div></el-card>
        <el-card shadow="never" class="compact-card" data-metric="tokens_retained"><div class="metric-label">保留期 Token</div><div class="metric-value">{{ formatNumber(cards.tokens_retained ?? (totalsScope.tokens_total === 'lifetime' ? null : cards.tokens_total)) }}</div><div class="metric-note">保留期累计（最近 {{ retentionDays }} 天）</div></el-card>
      </div>
    </details>
  </div>
</template>

<style scoped>
.dashboard-page { min-width: 0; }
.operational-strip { display:grid; grid-template-columns:repeat(4,minmax(0,1fr)); gap:12px; margin-bottom:14px; }
.operational-metric { display:flex; align-items:center; gap:10px; min-width:0; border:1px solid var(--card-border); border-radius:8px; padding:11px 12px; background:var(--card-bg); position:relative; }
.operational-metric > div { display:flex; flex-direction:column; gap:5px; min-width:0; }
.operational-metric > div > span { color:var(--text-3); font-size:10px; }
.operational-metric strong { color:var(--text-1); font-size:17px; font-weight:600; font-variant-numeric:tabular-nums; overflow-wrap:anywhere; }
.operational-metric strong small { font-weight:400; font-size:10px; color:var(--text-3); margin-left:5px; }
.operational-metric em { position:absolute; right:10px; top:12px; font-size:8px; color:var(--text-3); font-style:normal; }
.operational-realtime { gap:8px; }
.operational-realtime .realtime-content { flex:1; gap:3px; }
.realtime-scope { float:right; font-size:8px; }
.realtime-values { display:flex; align-items:baseline; gap:13px; flex-wrap:wrap; }
.operational-realtime .realtime-values strong { font-size:16px; line-height:1.3; white-space:nowrap; }
.operational-realtime .realtime-values small { margin-left:4px; font-size:9px; }
.operational-realtime .realtime-note { display:flex; justify-content:space-between; gap:4px; font-size:8px; line-height:1.4; }
.realtime-note time { white-space:nowrap; font-variant-numeric:tabular-nums; }
.live-indicator,.health-indicator { display:grid; place-items:center; flex:0 0 26px; height:26px; border-radius:7px; font-size:13px; background:var(--badge-green-soft); color:var(--badge-green); }
.live-indicator::before { content:''; width:7px; height:7px; background:var(--badge-green); border-radius:50%; box-shadow:0 0 0 4px color-mix(in srgb,var(--badge-green) 10%,transparent); }
.health-indicator.tone-red { background:var(--badge-red-soft); color:var(--badge-red); }
.dashboard-breakdown-grid { display:grid; grid-template-columns:minmax(0,1.2fr) minmax(0,1fr); gap:14px; margin-bottom:12px; }
.dashboard-breakdown-grid > .panel { min-width:0; margin-bottom:0; padding:16px; }
.model-list { display:grid; grid-template-columns:repeat(2,minmax(0,1fr)); gap:17px 22px; }
.model-row { min-width:0; }
.model-row-heading { display:flex; align-items:center; justify-content:space-between; gap:8px; }
.model-row-heading .model-name { display:inline-flex; align-items:center; gap:6px; color:var(--text-1); font-size:11px; max-width:100%; min-width:0; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; padding:0; }
.model-name :deep(span) { min-width:0; overflow:hidden; text-overflow:ellipsis; }
.model-dot { display:inline-block; width:6px; height:6px; flex-shrink:0; margin-right:6px; border-radius:50%; background:var(--model-color); }
.model-row-heading strong { flex-shrink:0; color:var(--text-2); font-weight:600; font-size:11px; }
.model-row-heading small { display:block; font-size:8px; font-weight:400; margin-top:3px; color:var(--text-3); text-align:right; }
.model-track { height:5px; margin:7px 0 6px; border-radius:4px; background:var(--el-fill-color-light); overflow:hidden; }
.model-track > span { display:block; height:100%; border-radius:inherit; background:var(--model-color); min-width:2px; }
.model-row-meta { display:flex; justify-content:space-between; gap:7px; font-size:9px; color:var(--text-3); }
.breakdown-note { margin:15px 0 0; color:var(--text-3); font-size:9px; line-height:1.6; }
.breakdown-empty { display:flex; align-items:center; justify-content:center; gap:8px; min-height:95px; font-size:11px; color:var(--text-3); }
.failure-total { color:var(--danger); font-size:20px; font-variant-numeric:tabular-nums; white-space:nowrap; }
.failure-total small { font-size:9px; color:var(--text-3); margin-left:5px; font-weight:400; }
.failure-list { display:flex; flex-direction:column; gap:6px; max-height:206px; overflow:auto; }
.failure-row { display:grid; grid-template-columns:minmax(85px,1fr) minmax(25px,1fr) 34px 43px; align-items:center; gap:9px; min-height:29px; width:100%; padding:4px 6px; border:0; border-radius:5px; cursor:pointer; text-align:left; background:transparent; font-size:10px; color:var(--text-2); }
.failure-row:hover { background:var(--el-fill-color-light); }
.failure-label { overflow-wrap:anywhere; }
.failure-track { height:5px; border-radius:3px; background:var(--el-fill-color-light); }
.failure-track > span { display:block; height:100%; border-radius:inherit; background:var(--badge-red); }
.failure-row strong,.failure-row small { font-size:10px; font-variant-numeric:tabular-nums; text-align:right; }
.failure-row small { color:var(--text-3); }
.incident-tags { display:flex; gap:5px; margin:5px 0 0; flex-wrap:wrap; }
.incident-tags > span { max-width:100%; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; padding:2px 5px; border-radius:4px; background:var(--badge-neutral-soft); color:var(--badge-neutral); font-size:9px; }
.incident-tags > .incident-http { background:var(--badge-red-soft); color:var(--badge-red); }
@media(max-width:1200px) { .operational-metric em { display:none; }.model-list { grid-template-columns:1fr; gap:12px; } }
@media(max-width:767px) { .operational-strip { grid-template-columns:repeat(2,minmax(0,1fr)); gap:8px; }.operational-metric { padding:10px; gap:7px; }.operational-metric strong { font-size:15px; }.dashboard-breakdown-grid { grid-template-columns:1fr; }.model-list { grid-template-columns:repeat(2,minmax(0,1fr)); } }
@media(max-width:480px) { .model-list { grid-template-columns:1fr; }.operational-metric { align-items:flex-start; }.live-indicator,.health-indicator { flex-basis:21px; height:21px; }.operational-metric strong small { display:block; margin:3px 0 0; } }
.dashboard-eyebrow { margin-bottom: 8px; color: var(--text-3); font-size: 9px; font-weight: 600; letter-spacing: 1.8px; }
.dashboard-page :deep(.page-header) { align-items: center; margin-bottom: 27px; }
.dashboard-page :deep(.page-header-title) { font-size: 27px; font-weight: 650; letter-spacing: -.9px; }
.dashboard-page :deep(.page-header-sub) { color: var(--text-3); font-size: 12px; margin-top: 8px; }
.dashboard-page :deep(.page-header-actions) { gap: 10px; }
.dashboard-context, .resources-heading { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 10px; margin: 0 0 14px; }
.scope-label { margin: 0; color: var(--text-2); font-size: 11px; font-weight: 500; }
.refresh-status { display: flex; align-items: center; gap: 7px; color: var(--text-3); font-size: 10px; }
.status-dot { width: 5px; height: 5px; border-radius: 50%; background: var(--success); }
.status-dot.pending { background: var(--warning); }
.kpi-grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 16px; margin-bottom: 16px; }
.kpi-card { display: block; min-width: 0; border: 1px solid var(--card-border); border-radius: 12px; padding: 19px 21px 16px; background: var(--card-bg); box-shadow: none; }
.kpi-head { display: grid; grid-template-columns: minmax(0, 1fr) auto; align-items: center; gap: 0 12px; }
.kpi-icon { display: grid; place-items: center; width: 29px; height: 29px; border-radius: 8px; color: var(--primary); background: var(--primary-soft); }
.metric-tokens .kpi-icon { color: var(--warning); background: color-mix(in srgb, var(--warning) 10%, var(--card-bg)); }
.metric-errors .kpi-icon { color: var(--danger); background: color-mix(in srgb, var(--danger) 10%, var(--card-bg)); }
.metric-cache_rate .kpi-icon { color: var(--success); background: color-mix(in srgb, var(--success) 10%, var(--card-bg)); }
.kpi-label { color: var(--text-2); font-size: 12px; font-weight: 500; }
.kpi-value { grid-column: 1 / -1; margin-top: 15px; color: var(--text-1); font-size: 31px; font-weight: 600; line-height: 1.2; letter-spacing: -1.1px; font-variant-numeric: tabular-nums; overflow-wrap: anywhere; }
.kpi-foot { display: flex; align-items: end; justify-content: space-between; gap: 8px; min-height: 34px; margin-top: 10px; }
.kpi-delta { display: block; min-width: 0; max-width: 62%; margin: 0; padding-bottom: 3px; font-size: 9px; line-height: 1.6; white-space: normal; text-align: left; }
.kpi-note { color: var(--text-3); font-size: 9px; line-height: 1.6; padding-bottom: 3px; }
.kpi-spark { width: 35%; min-width: 42px; height: 31px; flex-shrink: 0; margin: 0; }
.rates-strip { display: flex; flex-wrap: wrap; align-items: center; gap: 12px 27px; padding: 12px 20px; margin-bottom: 22px; border-radius: 9px; box-shadow: none; background: var(--card-bg); }
.rates-strip > span { display: inline-flex; align-items: baseline; gap: 8px; font-size: 10px; color: var(--text-2); }
.rates-strip > .metric-label { padding-right: 27px; border-right: 1px solid var(--card-border); font-size: 10px; color: var(--text-3); }
.rates-strip strong { font-size: 14px; font-weight: 600; font-variant-numeric: tabular-nums; color: var(--text-1); }
.rates-strip small { font-size: 10px; color: var(--text-3); }
.dashboard-chart-grid, .dashboard-activity-grid { display: grid; grid-template-columns: minmax(0, 1.9fr) minmax(0, 1fr); gap: 20px; margin-bottom: 20px; }
.dashboard-chart-grid > .panel, .dashboard-activity-grid > .panel { min-width: 0; margin-bottom: 0; padding: 22px; border-radius: 12px; box-shadow: none; }
.panel-title { min-height: 34px; gap: 12px; margin-bottom: 22px; font-size: 13px; font-weight: 600; }
.panel-sub { flex-shrink: 0; font-size: 10px; }
.panel-caption { margin: 6px 0 0; color: var(--text-3); font-size: 10px; font-weight: 400; line-height: 1.5; }
.traffic-frame { position: relative; }
.traffic-chart, .status-chart { height: 280px; min-width: 0; }
.status-panel { background: var(--card-bg); }
.without-traffic .traffic-chart { opacity: .15; }
.chart-empty { position: absolute; inset: 0; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 10px; color: var(--text-3); font-size: 11px; text-align: center; padding: 20px; }
.chart-empty .el-icon { color: var(--primary); }
.chart-empty strong, .quiet-state strong { color: var(--text-2); font-size: 12px; font-weight: 500; }
.flow-grid, .summary-grid, .resource-grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 0; }
.resource-grid { gap: 16px; margin-bottom: 22px; }
.summary-grid { grid-template-columns: repeat(5, minmax(0, 1fr)); }
.resource-card, .compact-card { min-width: 0; border: 1px solid var(--card-border); border-radius: 10px; background: var(--card-bg); box-shadow: none; }
.resource-card :deep(.el-card__body) { padding: 18px 20px; }
.resource-value-row { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin: 10px 0 7px; }
.resource-icon { display: grid; place-items: center; width: 33px; height: 33px; border: 1px solid var(--card-border); border-radius: 9px; color: var(--text-3); background: var(--bg); }
.metric-label { display: flex; align-items: center; justify-content: space-between; gap: 8px; font-size: 11px; color: var(--text-2); }
.resource-card .metric-label :deep(.el-button) { font-size: 10px; font-weight: 400; }
.metric-value { color: var(--text-1); font-size: 25px; font-weight: 600; font-variant-numeric: tabular-nums; line-height: 1.5; letter-spacing: -.7px; overflow-wrap: anywhere; }
.metric-note { margin-top: 4px; font-size: 10px; line-height: 1.65; color: var(--text-3); }
.compact-card { border: 0; border-radius: 0; background: transparent; }
.compact-card :deep(.el-card__body) { padding: 17px 15px; }
.compact-card .metric-value { margin-top: 7px; font-size: 19px; letter-spacing: -.4px; }
.details-panel, .scope-summary { border-radius: 10px; margin-bottom: 22px; padding: 0 21px; box-shadow: none; }
.details-panel[open] .flow-grid, .scope-summary[open] .summary-grid { padding-bottom: 6px; border-top: 1px solid var(--card-border); }
.section-heading { display: flex; align-items: center; flex-wrap: wrap; gap: 8px 18px; padding: 17px 0; cursor: pointer; list-style: none; }
.section-heading::-webkit-details-marker { display: none; }
.section-heading::after { content: '+'; margin-left: auto; color: var(--text-3); font-size: 17px; font-weight: 400; }
details[open] > .section-heading::after { content: '−'; }
.section-heading h3, .resources-heading h3 { margin: 0; color: var(--text-1); font-size: 12px; font-weight: 600; }
.section-heading > span, .resources-heading > span { color: var(--text-3); font-size: 10px; }
.resources-heading { margin-top: 26px; margin-bottom: 14px; }
.ranking-panel :deep(.el-table) { font-size: 11px; }
.ranking-panel :deep(.el-table__header-wrapper th) { font-size: 10px; font-weight: 500; color: var(--text-3); }
.ranking-panel :deep(.el-table .el-button) { font-size: 11px; }
.ranking-panel :deep(.el-table__cell) { padding: 12px 0; }
.incident-list { max-height: 300px; overflow-y: auto; }
.incident-item { position: relative; margin-left: 3px; padding: 0 0 16px 16px; border-left: 1px solid var(--card-border); }
.incident-item::before { content: ''; position: absolute; top: 7px; left: -3px; height: 5px; width: 5px; border-radius: 50%; background: var(--danger); }
.incident-item:last-child { padding-bottom: 0; }
.incident-heading { display: flex; align-items: baseline; justify-content: space-between; flex-wrap: wrap; gap: 6px 12px; }
.incident-heading :deep(.el-button) { font-size: 11px; }
.incident-heading time { color: var(--text-3); font-size: 9px; }
.incident-item p { display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical; overflow: hidden; overflow-wrap: anywhere; margin: 7px 0 0; font-size: 11px; line-height: 1.75; color: var(--text-2); }
.quiet-state { min-height: 176px; display: flex; align-items: center; justify-content: center; flex-direction: column; gap: 11px; padding: 15px; text-align: center; color: var(--text-3); font-size: 10px; line-height: 1.7; }
.quiet-state .el-icon { display: grid; place-items: center; box-sizing: content-box; width: 24px; height: 24px; padding: 13px; border: 1px solid var(--card-border); border-radius: 50%; color: var(--success); background: var(--bg); margin-bottom: 3px; }
.load-error { margin-bottom: 16px; }
.wrap-link { white-space: normal; height: auto; text-align: left; overflow-wrap: anywhere; }
.range-options { max-width: 100%; flex-wrap: wrap; gap: 0; }
.range-options :deep(.el-radio-button__inner) { padding: 8px 11px; font-size: 10px; }
@media (min-width: 1700px) { .traffic-chart, .status-chart { height: 320px; } .kpi-card { padding: 22px 24px 18px; } .kpi-value { font-size: 35px; } }
@media (max-width: 1200px) { .kpi-grid { gap: 12px; } .kpi-card { padding: 16px; } .kpi-value { font-size: 27px; } .kpi-foot { align-items: center; } .kpi-spark { width: 28%; min-width: 30px; } .summary-grid { grid-template-columns: repeat(3, minmax(0, 1fr)); } .dashboard-page :deep(.page-header) { align-items: flex-start; } }
@media (max-width: 1000px) { .kpi-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } .dashboard-chart-grid, .dashboard-activity-grid { grid-template-columns: minmax(0, 1fr); } .status-chart { height: 245px; } .resource-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } .kpi-foot { align-items: end; } .kpi-spark { width: 36%; } }
@media (max-width: 700px) { .dashboard-page :deep(.page-header) { margin-bottom: 22px; } .dashboard-page :deep(.page-header-title) { font-size: 24px; } .dashboard-page :deep(.page-header-actions) { flex-wrap: wrap; } .dashboard-context { gap: 8px; } .flow-grid, .summary-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); } .resource-grid { gap: 12px; } .metric-value { font-size: 22px; } .rates-strip { gap: 10px 20px; padding: 12px 16px; } .rates-strip > .metric-label { width: 100%; border-right: 0; padding-right: 0; } .traffic-chart { height: 270px; } .section-heading > span { width: calc(100% - 25px); line-height: 1.6; } .section-heading { position: relative; } .section-heading::after { position: absolute; right: 0; top: 15px; } .dashboard-chart-grid > .panel, .dashboard-activity-grid > .panel { padding: 18px; } .dashboard-chart-grid, .dashboard-activity-grid { gap: 16px; margin-bottom: 16px; } .panel-title { margin-bottom: 19px; } }
@media (max-width: 480px) { .kpi-grid { gap: 10px; } .kpi-card { padding: 14px 13px 12px; } .kpi-head { gap: 6px; } .kpi-label { font-size: 11px; } .kpi-icon { width: 25px; height: 25px; border-radius: 6px; } .kpi-value { font-size: 25px; margin-top: 13px; } .kpi-foot { gap: 4px; min-height: 35px; margin-top: 7px; } .kpi-delta { font-size: 8px; max-width: 64%; } .kpi-note { font-size: 8px; } .kpi-spark { width: 30%; height: 28px; min-width: 24px; } .resource-card :deep(.el-card__body) { padding: 14px; } .resource-icon { width: 28px; height: 28px; } .resource-icon .el-icon { font-size: 18px; } .compact-card :deep(.el-card__body) { padding: 13px 8px; } .refresh-status { font-size: 9px; } .range-options :deep(.el-radio-button__inner) { padding: 8px 9px; font-size: 9px; } .dashboard-chart-grid > .panel, .dashboard-activity-grid > .panel { padding: 16px; } .panel-caption { font-size: 9px; } .panel-sub { font-size: 9px; } .details-panel, .scope-summary { padding: 0 16px; } .metric-note { font-size: 9px; } }

/* Compact operational overview: preserve each metric while reducing scanning distance. */
.dashboard-page :deep(.page-header) { margin-bottom: 12px; }
.dashboard-page :deep(.page-header-title) { font-size: 23px; }
.dashboard-page :deep(.page-header-sub) { margin-top: 5px; font-size: 11px; }
.dashboard-context { gap: 9px 18px; margin-bottom: 10px; }
.scope-label { font-size: 10px; }
.refresh-status { margin-left: auto; font-size: 9px; }
.rates-strip { display: flex; flex-wrap: wrap; gap: 10px 16px; margin: 0; padding: 0; border: 0; background: transparent; }
.rates-strip > .metric-label { width: auto; padding-right: 0; border: 0; font-size: 9px; }
.rates-strip > span { gap: 5px; font-size: 9px; }
.rates-strip strong { font-size: 11px; }
.rates-strip small { display: none; }
.rate-badge { padding: 2px 5px; border-radius: 4px; background: var(--badge-blue-soft, var(--primary-soft)); color: var(--badge-blue, var(--primary)); font-size: 8px; font-weight: 600; letter-spacing: .4px; }
[data-metric='tpm'] .rate-badge { background: var(--badge-purple-soft, var(--primary-soft)); color: var(--badge-purple, var(--primary)); }
.kpi-grid { gap: 12px; margin-bottom: 12px; }
.kpi-card { position: relative; padding: 12px 15px 9px; border-radius: 9px; }
.kpi-head { gap: 0 9px; }
.kpi-label { font-size: 11px; }
.kpi-icon { width: 25px; height: 25px; border-radius: 7px; background: var(--badge-blue-soft, var(--primary-soft)); color: var(--badge-blue, var(--primary)); }
.metric-tokens .kpi-icon { background: var(--badge-purple-soft, var(--primary-soft)); color: var(--badge-purple, var(--primary)); }
.metric-cache_rate .kpi-icon { background: var(--badge-green-soft, var(--primary-soft)); color: var(--badge-green, var(--success)); }
.kpi-value { margin-top: 3px; font-size: 27px; line-height: 1.15; letter-spacing: -.7px; }
.kpi-foot { min-height: 23px; margin-top: 5px; }
.kpi-spark { height: 23px; width: 32%; }
.kpi-delta, .kpi-note { padding-bottom: 1px; font-size: 8px; }
.resources-heading { margin: 0 0 7px; }
.resources-heading h3 { font-size: 10px; color: var(--text-2); font-weight: 500; }
.resources-heading > span { font-size: 9px; }
.resource-grid { gap: 0; margin-bottom: 14px; border: 1px solid var(--card-border); border-radius: 9px; overflow: hidden; background: var(--card-bg); }
.resource-card { border: 0; border-right: 1px solid var(--card-border); border-radius: 0; }
.resource-card:last-child { border-right: 0; }
.resource-card :deep(.el-card__body) { position: relative; padding: 10px 14px 10px 51px; }
.resource-card .metric-label { font-size: 10px; }
.resource-card .metric-label :deep(.el-button) { padding: 0; min-height: 0; font-size: 9px; }
.resource-value-row { margin: 2px 0 0; }
.resource-value-row .metric-value { font-size: 21px; line-height: 1.2; }
.resource-icon { position: absolute; left: 13px; top: 15px; width: 27px; height: 27px; border: 0; border-radius: 7px; background: var(--badge-blue-soft, var(--primary-soft)); color: var(--badge-blue, var(--primary)); }
.resource-icon :deep(svg) { width: 16px; height: 16px; }
.resource-card[data-tone='teal'] .resource-icon { background: var(--badge-green-soft, var(--primary-soft)); color: var(--badge-green, var(--success)); }
.resource-card[data-tone='violet'] .resource-icon { background: var(--badge-purple-soft, var(--primary-soft)); color: var(--badge-purple, var(--primary)); }
.resource-card[data-tone='amber'] .resource-icon { background: var(--badge-amber-soft, var(--primary-soft)); color: var(--badge-amber, var(--warning)); }
.resource-card .metric-note { margin-top: 4px; font-size: 9px; }
.dashboard-chart-grid, .dashboard-activity-grid { gap: 14px; margin-bottom: 12px; }
.dashboard-chart-grid > .panel, .dashboard-activity-grid > .panel { padding: 14px 16px; border-radius: 10px; }
.panel-title { min-height: 29px; margin-bottom: 12px; font-size: 12px; }
.panel-heading { display: flex; align-items: center; gap: 9px; min-width: 0; }
.panel-icon { display: grid; place-items: center; width: 28px; height: 28px; flex-shrink: 0; border-radius: 7px; font-size: 15px; }
.tone-blue { background: var(--badge-blue-soft, var(--primary-soft)); color: var(--badge-blue, var(--primary)); }
.tone-green { background: var(--badge-green-soft, var(--primary-soft)); color: var(--badge-green, var(--success)); }
.tone-purple { background: var(--badge-purple-soft, var(--primary-soft)); color: var(--badge-purple, var(--primary)); }
.tone-amber { background: var(--badge-amber-soft, var(--primary-soft)); color: var(--badge-amber, var(--warning)); }
.panel-caption { margin-top: 4px; font-size: 9px; }
.soft-label { padding: 4px 7px; border: 1px solid var(--card-border); border-radius: 5px; background: var(--bg); font-size: 9px; }
.traffic-chart, .status-chart { height: 260px; }
.details-panel, .scope-summary { padding: 0 15px; margin-bottom: 12px; border-radius: 8px; }
.section-heading { gap: 8px; padding: 11px 0; }
.section-heading h3 { font-size: 11px; }
.section-heading > span:not(.section-symbol) { margin-left: 6px; font-size: 9px; }
.section-symbol { display: inline-grid; place-items: center; width: 21px; height: 21px; flex-shrink: 0; border-radius: 5px; font-size: 12px; }
.ranking-panel :deep(.el-table__cell) { padding: 9px 0; }
.incident-item { padding-bottom: 13px; }
.quiet-state { min-height: 140px; }
@media (max-width: 1100px) { .dashboard-context { gap: 7px 14px; }.refresh-status { margin-left: 0; }.resource-card :deep(.el-card__body) { padding-left: 46px; }.resource-icon { left: 11px; width: 25px; height: 25px; }.resource-card .metric-note { font-size: 8px; } }
@media (max-width: 1000px) { .resource-grid { grid-template-columns: repeat(4,minmax(0,1fr)); }.resource-card :deep(.el-card__body) { padding: 10px 12px; }.resource-icon { position: static; width: 23px; height: 23px; }.resource-card .metric-note { font-size: 8px; }.resources-heading { margin-top: 2px; } }
@media (max-width: 700px) { .kpi-grid { gap: 9px; }.kpi-card { padding: 11px 13px 9px; }.dashboard-context > .scope-label { flex: 1 1 auto; }.rates-strip { order: 3; width: 100%; padding: 5px 0; }.refresh-status { margin-left: auto; }.resource-grid { grid-template-columns: repeat(2,minmax(0,1fr)); }.resource-card:nth-child(2) { border-right: 0; }.resource-card:nth-child(-n+2) { border-bottom: 1px solid var(--card-border); }.resource-card :deep(.el-card__body) { padding: 10px 13px 10px 46px; }.resource-icon { position: absolute; top: 17px; left: 12px; }.resource-card .metric-note { font-size: 9px; }.dashboard-chart-grid > .panel, .dashboard-activity-grid > .panel { padding: 13px; }.section-heading > span:not(.section-symbol) { width: 100%; margin-left: 29px; padding-right: 20px; }.section-heading::after { top: 9px; }.section-heading { gap: 5px 8px; }.traffic-chart, .status-chart { height: 240px; } }
@media (max-width: 480px) { .kpi-value { font-size: 25px; }.kpi-foot { min-height: 25px; }.kpi-icon { width: 24px; height: 24px; }.panel-icon { width: 25px; height: 25px; }.panel-caption { font-size: 8px; }.soft-label { font-size: 8px; padding: 3px 5px; }.resource-card .metric-note { font-size: 8px; }.refresh-status { font-size: 8px; }.scope-label { font-size: 9px; } }
</style>
