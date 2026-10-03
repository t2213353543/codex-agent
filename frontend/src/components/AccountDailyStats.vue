<script setup>
import { formatNumber } from '../utils/format'
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { init } from '../utils/echarts'
import { accountDailySummary, dailyNumber, dailyTrendValue } from '../utils/accountDailyStats'
const props = defineProps({ stats: { type: Object, required: true } })
const mode = ref('active'), metric = ref('requests'), page = ref(1), chartElement = ref(null)
const summary = computed(() => accountDailySummary(props.stats, mode.value))
const metrics = [['requests', '请求'], ['tokens', 'Token']]
const selectedLabel = computed(() => metrics.find(([key]) => key === metric.value)[1])
const rows = computed(() => summary.value.days.slice((page.value - 1) * 31, page.value * 31))
const pages = computed(() => Math.max(1, Math.ceil(summary.value.days.length / 31)))
let chart, resizeObserver
async function draw() {
  await nextTick()
  if (!chartElement.value) return
  if (!chart) chart = init(chartElement.value)
  const style = getComputedStyle(document.documentElement), text = style.getPropertyValue('--text-3') || '#788390', border = style.getPropertyValue('--card-border') || '#dce3ec'
  const values = summary.value.days.map(row => dailyTrendValue(row, metric.value))
  const color = { requests: '#409eff', tokens: '#38a591' }[metric.value]
  chart.setOption({ animation: false, grid: { left: 55, right: 18, top: 26, bottom: 48 }, tooltip: { trigger: 'axis', renderMode: 'richText', formatter: points => { const item = points[0]; if (!item) return ''; const observation = values[item.dataIndex]; return `${summary.value.days[item.dataIndex].date}\n${selectedLabel.value}：${observation.text}${observation.state === 'partial' ? '（已记录部分）' : observation.state === 'unknown' ? '（未知）' : ''}` } }, xAxis: { type: 'category', data: summary.value.days.map(row => row.date), axisLabel: { color: text, hideOverlap: true }, axisLine: { lineStyle: { color: border } } }, yAxis: { type: 'value', min: 0, axisLabel: { color: text }, splitLine: { lineStyle: { color: border } } }, series: [{ name: selectedLabel.value, type: 'line', data: values.map(value => value.value), connectNulls: false, symbol: 'circle', symbolSize: 5, showSymbol: summary.value.days.length <= 62, lineStyle: { color, width: 2 }, itemStyle: { color } }] }, true)
  chart.resize()
}
watch(() => props.stats, () => { page.value = 1; draw() })
watch(metric, draw)
onMounted(() => { draw(); resizeObserver = new ResizeObserver(() => chart?.resize()); if (chartElement.value) resizeObserver.observe(chartElement.value); window.addEventListener('theme-change', draw) })
onUnmounted(() => { resizeObserver?.disconnect(); chart?.dispose(); chart = null; window.removeEventListener('theme-change', draw) })
const peakText = value => value.days.length ? value.days.join('、') : '无请求日'
</script>

<template>
  <section class="account-daily-stats" aria-label="账号日统计摘要">
    <div class="daily-heading"><h3>日统计摘要</h3><div class="daily-mode" aria-label="日均分母"><button v-for="option in [['active','按活跃日'],['calendar','按所选自然日']]" :key="option[0]" type="button" :aria-pressed="mode === option[0]" @click="mode = option[0]">{{ option[1] }}</button></div></div>
    <p class="daily-note">{{ stats.timezone }} · {{ summary.calendarDays }} 个所选自然日 · {{ summary.activeDays }} 个活跃日。日均分母为 {{ summary.divisor }} 天；活跃日指仍有留存请求的日期。首末日仅统计已选时间段，不将部分日折算成 24 小时。</p>
    <dl class="daily-overview"><div><dt>日均请求</dt><dd>{{ dailyNumber(summary.averageRequests) }}</dd></div><div><dt>日均已记录 Token</dt><dd>{{ formatNumber(summary.averageTokens) }}<small v-if="summary.tokenPartial">存在未记录用量</small></dd></div><div><dt>最高请求日</dt><dd>{{ summary.peakRequests.value === null ? '无请求日' : dailyNumber(summary.peakRequests.value) + ' 次' }}<small>{{ peakText(summary.peakRequests) }}</small></dd></div><div><dt>最高 Token 日</dt><dd>{{ summary.peakTokens.value === null ? '无请求日' : formatNumber(summary.peakTokens.value) }}<small>{{ summary.tokenPartial ? '已记录部分 · ' : '' }}{{ peakText(summary.peakTokens) }}</small></dd></div></dl>
    <section class="daily-today"><h4>今日在所选区间内</h4><p v-if="summary.today">{{ summary.today.date }} · {{ dailyNumber(summary.today.total_requests) }} 次请求 · {{ formatNumber(summary.today.total_tokens) }} 已记录 Token</p><p v-else class="daily-note">所选区间不包含今日。</p></section>
    <div class="daily-heading"><h3>每日趋势</h3><label>指标 <select :value="metric" aria-label="账号日趋势指标" @change="metric = $event.target.value"><option v-for="[key, label] in metrics" :key="key" :value="key">{{ label }}</option></select></label></div>
    <p class="daily-note">未记录的 Token 留空，不连线补零；部分覆盖仅表示已记录用量。请求与 Token 使用同一查询快照及日历日。</p>
    <div ref="chartElement" class="daily-chart" role="img" :aria-label="`${selectedLabel}日历日趋势，${summary.calendarDays}天，时区${stats.timezone}`" />
    <details class="daily-table-details"><summary>查看逐日明细（{{ summary.calendarDays }} 天）</summary><div class="daily-table-scroll"><table><thead><tr><th>日期</th><th>请求</th><th>已记录 Token</th></tr></thead><tbody><tr v-for="row in rows" :key="row.date"><td>{{ row.date }}</td><td>{{ dailyNumber(row.total_requests) }}</td><td>{{ formatNumber(row.total_tokens) }}<small v-if="row.usage_unknown_requests">用量未知 {{ row.usage_unknown_requests }}</small></td></tr></tbody></table></div><div v-if="pages > 1" class="daily-pagination"><el-button :disabled="page <= 1" @click="page--">上一页</el-button><span>{{ page }} / {{ pages }}</span><el-button :disabled="page >= pages" @click="page++">下一页</el-button></div></details>
  </section>
</template>

<style scoped>
.account-daily-stats { margin-top: 24px; border-top: 1px solid var(--card-border); padding-top: 20px; }.daily-heading { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; }.daily-heading h3 { font-size: 14px; margin: 0; }.daily-heading label { font-size: 12px; }.daily-mode { display: flex; }.daily-mode button { border: 1px solid var(--card-border); padding: 6px 10px; background: var(--card-bg); color: var(--text-2); cursor: pointer; font: inherit; font-size: 12px; }.daily-mode button[aria-pressed=true] { color: var(--el-color-primary); border-color: var(--el-color-primary); }.daily-note,small { color: var(--text-3); font-size: 12px; line-height: 1.8; }.daily-overview { display: grid; grid-template-columns: repeat(2,minmax(0,1fr)); margin: 18px 0; gap: 16px; }.daily-overview dt { color: var(--text-3); font-size: 12px; }.daily-overview dd { margin: 8px 0 0; font-size: 18px; font-variant-numeric: tabular-nums; overflow-wrap: anywhere; }small { display: block; }.daily-today { padding: 18px 0; font-size: 12px; }.daily-heading select { min-height: 32px; max-width: 100%; border: 1px solid var(--card-border); border-radius: 4px; background: var(--card-bg); color: var(--text-2); font: inherit; padding: 0 8px; }.daily-chart { width: 100%; height: 260px; min-width: 0; }.daily-table-details summary { cursor: pointer; font-size: 12px; margin: 12px 0; }.daily-table-scroll { overflow: auto; }.daily-table-scroll table { width: 100%; border-collapse: collapse; font-size: 12px; }.daily-table-scroll th,.daily-table-scroll td { padding: 6px 10px; text-align: left; border-bottom: 1px solid var(--card-border); }.daily-table-scroll th { color: var(--text-3); font-weight: 500; }.daily-pagination { display: flex; align-items: center; justify-content: flex-end; gap: 12px; margin-top: 12px; font-size: 12px; }
.daily-today h4 { font-size: 13px; margin: 0 0 10px; }.daily-table-scroll table { min-width: 0; }.daily-table-scroll th,.daily-table-scroll td { white-space: normal; overflow-wrap: anywhere; }
@media(max-width:600px) { .daily-chart { height: 230px; } }
</style>
