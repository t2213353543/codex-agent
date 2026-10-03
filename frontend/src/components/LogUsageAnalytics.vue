<script setup>
import { computed, ref } from 'vue'
import { DataLine, Cpu, Coin, Clock } from '@element-plus/icons-vue'
import { logLatency, logTokenCount } from '../utils/logQueryState'
import { logPercent, rankUsageRecords, usageMetricNumber, usageMetricCoverage } from '../utils/logMetrics'
import { usageCacheRatio, usageTrendGeometry, logErrorOptions, managedTimeParts, logStepOptions } from '../utils/logManagementState'
import { observationStageTotals, observationStageAverage } from '../utils/logObservationState'
import { logSeverityOptions, severityDistribution } from '../utils/logSeverity'
import { imageTokenTotals, imageTokenTotal, imageTokenPrecisionExceeded } from '../utils/logPassiveObservation'
const props = defineProps({ stats: Object, loading: Boolean, error: String, current: Boolean, timezone: String, snapshot: Object })
defineEmits(['refresh', 'filter'])
const expanded = ref(false)
const metric = ref('total_requests')
const rankDimension = ref('accounts')
const rankMetric = ref('total_tokens')
const rankLimit = ref(50)
const ranks = computed(() => rankUsageRecords(props.stats?.[rankDimension.value] || [], rankMetric.value, rankLimit.value))
const analysisMetrics = [['total_requests', '请求数'], ['total_tokens', 'Token'], ['total_input_tokens', '输入'], ['total_output_tokens', '输出'], ['total_cached_tokens', '缓存读取'], ['total_cache_write_tokens', '缓存创建']]
const metricValue = row => usageMetricNumber(row, metric.value)
const metricFormat = value => format(value)
const geometry = computed(() => usageTrendGeometry((props.stats?.series || []).map(row => ({ ...row, [metric.value]: metricValue(row) })), metric.value, props.stats?.step))
const trendLines = computed(() => {
  const segments = []; let current = []
  for (const point of geometry.value.points) {
    if (point.y === null) { if (current.length) segments.push(current.join(' ')); current = [] }
    else current.push(`${point.x},${point.y}`)
  }
  if (current.length) segments.push(current.join(' '))
  return segments
})
const knownTrendPoints = computed(() => geometry.value.points.filter(point => point.y !== null))
const trendCoverage = computed(() => {
  if (metric.value === 'total_requests') return ''
  const unknown = geometry.value.points.filter(point => point[metric.value] === null).length
  const partial = geometry.value.points.filter(point => {
    if (point[metric.value] === null) return false
    if (metric.value === 'total_cache_write_tokens') {
      const eligible = Number.isFinite(point.cache_read_samples) && Number.isFinite(point.cache_read_unknown_requests) ? point.cache_read_samples + point.cache_read_unknown_requests : point.total_requests
      return point.cache_write_samples > 0 && point.cache_write_samples < eligible
    }
    if (metric.value === 'total_cached_tokens' && point.cache_read_samples !== undefined) return point.cache_read_samples > 0 && point.cache_read_unknown_requests > 0
    return point.usage_known_requests > 0 && point.usage_unknown_requests > 0
  }).length
  return [unknown ? `${unknown} 个时段用量未记录，曲线保留空缺` : '', partial ? `${partial} 个时段仅显示已记录合计` : ''].filter(Boolean).join('；')
})
const trendTime = time => { const parts = managedTimeParts(time, props.timezone); return `${parts.date} ${parts.time.slice(0, 5)}` }
const calendarLabel = computed(() => logStepOptions.find(([value]) => value === String(props.stats?.step))?.[1] || '自动粒度')
const rankMeasure = computed(() => ({ total_input_tokens: '输入', total_output_tokens: '输出', total_cached_tokens: '读取', total_cache_write_tokens: '创建', success_rate: '成功率' })[rankMetric.value] || 'Token')
const rankTokenMetric = computed(() => ['total_input_tokens', 'total_output_tokens', 'total_cached_tokens', 'total_cache_write_tokens'].includes(rankMetric.value) ? rankMetric.value : 'total_tokens')
const rankValue = row => rankMetric.value === 'success_rate' ? logPercent(row.total_requests > 0 ? row.success / row.total_requests : null) : format(usageMetricNumber(row, rankTokenMetric.value))
const rankCoverage = row => rankMetric.value === 'success_rate' ? '' : usageMetricCoverage(row, rankTokenMetric.value)
function rankingFilter(row) { return rankDimension.value === 'groups' ? ['group_id', row.id] : rankDimension.value === 'accounts' ? [row.id ? 'account_id' : 'account', row.id || row.name] : [row.id ? 'key_id' : 'key_name', row.id || row.name] }
const format = logTokenCount
const cacheRatio = computed(() => usageCacheRatio(props.stats))
const tokensKnown = computed(() => usageMetricNumber(props.stats, 'total_tokens') !== null)
const distributions = computed(() => [
  { key: 'model', title: '请求模型', rows: props.stats?.models || [] },
  { key: 'upstream_model', title: '上游声明模型', rows: props.stats?.upstream_models || [] },
  { key: 'sent_model', title: '实际发送模型', rows: props.stats?.sent_models || [] },
  { key: 'returned_model', title: '响应返回模型', rows: props.stats?.returned_models || [] },
  { key: 'inbound_path', title: '入站标准路径', rows: props.stats?.inbound_paths || [] },
  { key: 'upstream_path', title: '上游标准路径', rows: props.stats?.upstream_paths || [] },
  { key: 'endpoint', title: '端点', rows: props.stats?.endpoints || [] },
  { key: 'group_id', title: '成员分组', rows: props.stats?.groups || [] },
  { key: 'error_category', title: '错误类别', rows: props.stats?.error_categories || [] },
  { key: 'error_severity', title: '错误级别', ...severityDistribution(props.stats) },
].map(group => ({ ...group, rows: group.key === 'error_severity' ? group.rows : rankUsageRecords(group.rows, metric.value, 20).slice(0, 8), max: Math.max(1, ...group.rows.map(row => metricValue(row) || 0)) })))
const dimensionName = (key, name) => key === 'error_category' ? logErrorOptions.find(([value]) => value === name)?.[1] || name || '未记录' : key === 'error_severity' ? logSeverityOptions.find(([value]) => value === name)?.[1] || '级别未知' : name || '未记录'
</script>

<template>
  <section class="usage-analytics" :class="{ 'usage-stale': !current }" aria-label="全部筛选结果统计" :aria-busy="loading">
    <div class="usage-scope"><div><strong>筛选结果汇总</strong><span>{{ loading ? '正在统计全部匹配记录…' : !current ? '上次统计结果，等待当前查询更新' : '全部匹配的留存明细 · 不随分页变化' }}</span></div><button type="button" class="analytics-toggle" :aria-expanded="expanded" aria-controls="usage-analysis" @click="expanded = !expanded">{{ expanded ? '收起分析' : '趋势与分布' }} <span aria-hidden="true">{{ expanded ? '−' : '+' }}</span></button></div>
    <div v-if="error" class="usage-error" role="status"><span>{{ error }}</span><el-button link type="primary" @click="$emit('refresh')">重试统计</el-button></div>
    <p v-if="stats?.checked_at" class="analysis-checked">统计生成于 {{ trendTime(stats.checked_at) }} · {{ timezone || 'Asia/Shanghai' }} · {{ calendarLabel }}；{{ snapshot?.as_of ? '与列表、导出共享固定记录快照；并发清理的详情可能不可用。' : '当前未取得固定查询快照。' }}</p>
    <div class="usage-summary">
      <div class="usage-stat stat-requests"><span class="usage-stat-icon" aria-hidden="true"><el-icon><DataLine /></el-icon></span><span>请求总数</span><strong>{{ stats ? format(stats.total_requests) : '—' }}</strong><small v-if="stats"><b class="summary-success">{{ format(stats.success) }} 成功</b> · <b class="summary-errors">{{ format(stats.errors) }} 失败/中断</b></small><small v-else>全量统计加载后显示</small></div>
      <div class="usage-stat stat-tokens"><span class="usage-stat-icon" aria-hidden="true"><el-icon><Cpu /></el-icon></span><span>输入 + 输出 Token<template v-if="tokensKnown && stats?.usage_unknown_requests"> · 已记录合计</template></span><strong>{{ tokensKnown ? format(stats.total_tokens) : '—' }}</strong><small v-if="stats">{{ tokensKnown ? '输入 ' + format(stats.total_input_tokens) + ' · 输出 ' + format(stats.total_output_tokens) : '尚无已确认的用量' }}<template v-if="stats.usage_unknown_requests"> · {{ format(stats.usage_unknown_requests) }} 条未记录用量</template></small></div>
      <div class="usage-stat stat-cache"><span class="usage-stat-icon" aria-hidden="true"><el-icon><Coin /></el-icon></span><span>缓存读取命中率</span><strong>{{ logPercent(cacheRatio) }}</strong><small v-if="stats">{{ stats.cache_read_samples > 0 ? '读取 ' + format(stats.cache_read_tokens) + ' Token' : '读取用量未知' }}{{ cacheRatio === null ? ' · 无可计算样本' : ' · 读取 / 输入，不含创建' }}<template v-if="stats.cache_read_unknown_requests"> · {{ format(stats.cache_read_unknown_requests) }} 条读取未知</template></small></div>
      <div class="usage-stat stat-latency"><span class="usage-stat-icon" aria-hidden="true"><el-icon><Clock /></el-icon></span><span>平均总耗时</span><strong>{{ stats?.total_requests ? logLatency(stats.average_latency_ms) : '—' }}</strong><small>{{ stats?.ttft_samples ? '平均首字 ' + logLatency(stats.average_ttft_ms) + ' · ' + format(stats.ttft_samples) + ' 条采样' : '首字延迟尚无采样' }}</small></div>
    </div>
    <div v-if="expanded" id="usage-analysis" class="usage-analysis">
      <div class="usage-observation"><div><span>推理 Token</span><strong>{{ format(stats?.total_reasoning_tokens) }}</strong><small>{{ format(stats?.reasoning_samples) }} 条有记录 · 包含在输出内</small></div><div><span>缓存创建 Token</span><strong>{{ format(usageMetricNumber(stats, 'total_cache_write_tokens')) }}</strong><small>{{ format(stats?.cache_write_samples) }} 条有记录 · 不计入读取命中率</small></div><div><span>首字延迟采样</span><strong>{{ stats?.ttft_samples ? logLatency(stats.average_ttft_ms) : '—' }}</strong><small>{{ format(stats?.ttft_samples) }} 条有记录</small></div></div>
      <div class="usage-observation usage-image-observation"><div v-for="[samples,total,label,overflow] in imageTokenTotals" :key="samples"><span>{{ label }}</span><strong>{{ stats?.[overflow] === true ? '合计超出支持范围' : imageTokenPrecisionExceeded(stats?.[total]) ? '超出前端精确显示范围' : format(imageTokenTotal(stats,samples,total)) }}</strong><small>{{ format(stats?.[samples]) }} / {{ format(stats?.total_requests) }} 条请求有记录</small></div></div><p class="observation-stage-note">图像数值仅汇总已观测明细，不再计入总 Token。模型分解与图像工具来源在逐条详情中区分；缺少图片字段不表示没有图片用量。</p>
      <div class="usage-observation usage-stage-observation"><div v-for="[samples,average,label] in observationStageTotals" :key="samples"><span>平均{{ label }}</span><strong>{{ logLatency(observationStageAverage(stats,samples,average)) }}</strong><small>{{ format(stats?.[samples]) }} 条实际采样</small></div></div><p class="observation-stage-note">阶段可能重叠，不能相加为总耗时。缺少采样保持未知；模型与路径分布仅统计明确观测值，不回填请求模型或推断路径。</p>
      <div class="analysis-toolbar"><span>统计仅覆盖留存明细；历史已清理记录不计入。</span><div class="analysis-metric" role="group" aria-label="分析指标"><button v-for="item in analysisMetrics" :key="item[0]" type="button" :class="{ active: metric === item[0] }" :aria-pressed="metric === item[0]" @click="metric = item[0]">{{ item[1] }}</button></div></div>
      <div class="usage-trend"><div class="trend-heading"><strong>{{ metric === 'total_requests' ? '请求趋势' : 'Token 趋势' }}</strong><span>{{ knownTrendPoints.length ? '最高 ' + metricFormat(geometry.max) : geometry.points.length && metric !== 'total_requests' ? '当前范围尚无已记录用量' : '当前范围暂无趋势数据' }}</span></div><svg v-if="geometry.points.length" viewBox="0 0 600 155" role="img" :aria-label="metric === 'total_requests' ? '全部筛选请求趋势' : '全部筛选 Token 趋势'" preserveAspectRatio="none"><line x1="20" y1="20" x2="580" y2="20" class="chart-guide" /><line x1="20" y1="75" x2="580" y2="75" class="chart-guide" /><line x1="20" y1="130" x2="580" y2="130" class="chart-guide" /><polyline v-for="(line, index) in trendLines" :key="index" :points="line" class="chart-line" /><circle v-for="point in knownTrendPoints" :key="point.time" :cx="point.x" :cy="point.y" r="3" class="chart-point"><title>{{ trendTime(point.time) }} · {{ metricFormat(point[metric]) }}{{ usageMetricCoverage(point, metric) ? ' · ' + usageMetricCoverage(point, metric) : '' }}</title></circle></svg><div v-if="geometry.points.length" class="trend-times"><span>{{ trendTime(geometry.points[0].time) }}</span><span>{{ trendTime(geometry.points.at(-1).time) }}</span></div><p v-if="trendCoverage" class="metric-coverage trend-coverage">{{ trendCoverage }}</p></div>
<div class="usage-distributions"><section v-for="group in distributions" :key="group.key"><h3>{{ group.title }}分布 <small>前 {{ group.rows.length }} 项</small></h3><p v-if="group.key === 'upstream_model'" class="distribution-note">仅统计明确观测到的上游模型；缺失记录不以请求模型补齐，因此可能少于请求总数。</p><p v-if="group.key === 'group_id'" class="distribution-note">按请求当时的账号成员分组快照统计；同一请求可属于多组，分组之和不等于去重总数。</p><p v-if="group.note" class="distribution-note">{{ group.note }}</p><p v-if="!group.rows.length && group.key !== 'error_severity'" class="distribution-empty">当前范围暂无记录</p><button v-for="row in group.rows" :key="row.name" type="button" class="distribution-row" :disabled="!current || !row.name" :title="'筛选' + group.title + '：' + dimensionName(group.key, row.name)" @click="$emit('filter', group.key, group.key === 'group_id' ? row.id : row.name)"><span class="distribution-name">{{ dimensionName(group.key, row.name) }}</span><strong>{{ metricFormat(metricValue(row)) }}</strong><small v-if="usageMetricCoverage(row, metric)" class="metric-coverage distribution-coverage">{{ usageMetricCoverage(row, metric) }}</small><span class="distribution-track"><i :style="{ width: `${Math.max(0, (metricValue(row) || 0) / group.max * 100)}%` }" /></span></button></section></div>
<section class="usage-ranking"><div class="ranking-toolbar"><h3>用量排行</h3><label><span class="ranking-label">维度</span><select :value="rankDimension" aria-label="排行维度" @change="rankDimension = $event.target.value"><option value="accounts">账号</option><option value="keys">KEY</option><option value="groups">分组</option></select></label><label><span class="ranking-label">排序</span><select :value="rankMetric" aria-label="排行排序" @change="rankMetric = $event.target.value"><option value="total_tokens">总 Token</option><option value="total_requests">请求数</option><option value="total_input_tokens">输入 Token</option><option value="total_output_tokens">输出 Token</option><option value="total_cached_tokens">缓存读取 Token</option><option value="total_cache_write_tokens">缓存创建 Token</option><option value="success_rate">成功率</option></select></label><select :value="rankLimit" aria-label="排行数量" @change="rankLimit = Number($event.target.value)"><option v-for="limit in [20, 50, 100, 200]" :key="limit" :value="limit">前 {{ limit }} 项</option></select></div><p v-if="!ranks.length" class="distribution-empty">当前范围暂无{{ rankDimension === 'accounts' ? '账号' : rankDimension === 'groups' ? '分组' : 'KEY' }}排行</p><div v-for="(row, index) in ranks" :key="row.id || row.name" class="ranking-row"><span class="ranking-position">{{ index + 1 }}</span><button type="button" class="ranking-name" :disabled="!current || (!row.id && !row.name)" :title="'筛选：' + (row.name || row.id)" @click="$emit('filter', ...rankingFilter(row))">{{ row.name || row.id || '未记录' }}</button><span><small>请求</small>{{ format(row.total_requests) }}</span><span><small>{{ rankMeasure }}</small>{{ rankValue(row) }}</span><small v-if="rankCoverage(row)" class="metric-coverage ranking-coverage">{{ rankCoverage(row) }}</small></div><p v-if="ranks.length" class="ranking-note">点击名称可筛选使用记录。未记录的用量指标置后，成功率按成功请求数除以请求总数。</p></section>
    </div>
  </section>
</template>

<style scoped>
.metric-coverage { color: var(--text-3); font-size: 11px; font-weight: 400; line-height: 1.7; }.trend-coverage { margin: 7px 0 0; }.distribution-coverage { grid-column: 1 / -1; }.ranking-coverage { grid-column: 2 / -1; text-align: right; margin-top: -7px; }
.usage-stage-observation { border-top:1px solid var(--card-border);padding-top:14px;padding-bottom:6px; }.observation-stage-note { color:var(--text-3);font-size:12px;line-height:1.8;margin:2px 0 16px; }
.analysis-checked, .distribution-note { color: var(--text-3); font-size: 11px; line-height: 1.7; margin: 8px 0 0; }.distribution-note { margin: -4px 0 8px; }.analysis-metric { flex-wrap: wrap; }
.usage-analytics { border-bottom: 1px solid var(--card-border, #e4eaf3); background: var(--el-fill-color-lighter, #fafbfd); padding: 16px 20px; }
.usage-stale { opacity: .74; }
.usage-scope, .usage-scope > div, .analysis-toolbar, .trend-heading, .trend-times { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
.usage-scope > div { justify-content: flex-start; flex-wrap: wrap; gap: 6px 12px; }
.usage-scope strong { color: var(--text-2); font-size: 12px; font-weight: 500; }
.usage-scope span, .analysis-toolbar, .trend-heading > span, .trend-times { color: var(--text-3); font-size: 11px; }
.analytics-toggle { border: 0; padding: 2px 0; background: transparent; color: var(--primary, #409eff); font: inherit; font-size: 12px; white-space: nowrap; cursor: pointer; }
.usage-summary { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 20px; margin-top: 17px; }
.usage-summary > div { min-width: 0; display: flex; flex-direction: column; gap: 5px; border-left: 1px solid var(--card-border); padding-left: 20px; }
.usage-summary > div:not(.usage-stat):first-child { border: 0; padding-left: 0; }
.usage-summary span { color: var(--text-3); font-size: 12px; }
.usage-summary strong { color: var(--text-1); font-size: 22px; line-height: 1.5; font-weight: 600; font-variant-numeric: tabular-nums; overflow-wrap: anywhere; }
.usage-summary small { color: var(--text-3); font-size: 11px; line-height: 1.6; }
.usage-summary small b { font-weight: 400; }.summary-success { color: var(--success, #529b2e); }.summary-errors { color: var(--danger, #d9363e); }
.usage-error { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; font-size: 12px; color: var(--danger); padding-top: 10px; }
.usage-observation { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 16px; padding: 0 0 17px; }.usage-observation > div { display: grid; gap: 5px; }.usage-observation span { color: var(--text-3); font-size: 12px; }.usage-observation strong { color: var(--text-2); font-size: 16px; font-weight: 500; font-variant-numeric: tabular-nums; }.usage-observation small { color: var(--text-3); font-size: 11px; line-height: 1.6; }
.usage-ranking { padding-top: 18px; margin-top: 18px; border-top: 1px solid var(--card-border); }.ranking-toolbar { display: flex; align-items: center; flex-wrap: wrap; gap: 10px; margin-bottom: 10px; }.ranking-toolbar h3 { color: var(--text-2); font-size: 12px; font-weight: 500; margin: 0 auto 0 0; }.ranking-toolbar label { display: flex; align-items: center; gap: 6px; }.ranking-label { color: var(--text-3); font-size: 11px; }.ranking-toolbar select { height: 28px; max-width: 100%; border: 1px solid var(--el-border-color); border-radius: 4px; background: var(--card-bg); color: var(--text-2); padding: 0 6px; font: inherit; font-size: 12px; }.ranking-row { display: grid; grid-template-columns: 24px minmax(0, 1fr) 100px 120px; gap: 12px; align-items: center; padding: 10px 0; border-bottom: 1px solid var(--card-border); font-size: 12px; font-variant-numeric: tabular-nums; color: var(--text-2); }.ranking-position { color: var(--text-3); }.ranking-name { display: block; min-width: 0; text-align: left; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; border: 0; padding: 0; background: none; color: var(--primary, #409eff); font: inherit; cursor: pointer; }.ranking-name:disabled { cursor: default; }.ranking-row > span:not(.ranking-position) { text-align: right; }.ranking-row small { color: var(--text-3); margin-right: 7px; font-size: 11px; }.ranking-note { color: var(--text-3); font-size: 11px; line-height: 1.7; margin: 10px 0 0; }
.usage-analysis { margin-top: 18px; border-top: 1px solid var(--card-border); padding-top: 16px; }
.analysis-toolbar { flex-wrap: wrap; }.analysis-metric { display: flex; gap: 2px; }.analysis-metric button { border: 0; border-radius: 4px; padding: 5px 9px; font: inherit; background: transparent; color: var(--text-3); cursor: pointer; }.analysis-metric button.active { color: var(--primary, #409eff); background: var(--primary-soft, #ecf5ff); }
.usage-trend { padding: 16px 0 18px; }.trend-heading strong { font-size: 12px; font-weight: 500; color: var(--text-2); }.usage-trend svg { width: 100%; height: 160px; margin-top: 5px; overflow: visible; }.chart-guide { stroke: var(--card-border); stroke-dasharray: 3 5; }.chart-line { stroke: var(--primary, #409eff); fill: none; stroke-width: 2; vector-effect: non-scaling-stroke; }.chart-point { fill: var(--primary, #409eff); }.trend-times { font-variant-numeric: tabular-nums; }
.usage-distributions { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 24px; }.usage-distributions h3 { display: flex; justify-content: space-between; gap: 10px; margin: 0 0 12px; font-size: 12px; color: var(--text-2); font-weight: 500; }.usage-distributions h3 small { color: var(--text-3); font-size: 11px; font-weight: 400; }.distribution-empty { color: var(--text-3); font-size: 12px; }.distribution-row { display: grid; grid-template-columns: minmax(0, 1fr) auto; gap: 5px 12px; padding: 5px 0 8px; width: 100%; border: 0; background: transparent; font: inherit; font-size: 12px; text-align: left; color: var(--text-2); cursor: pointer; }.distribution-row:disabled { cursor: default; }.distribution-name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }.distribution-row strong { font-weight: 400; font-variant-numeric: tabular-nums; }.distribution-track { grid-column: 1 / -1; display: block; height: 3px; background: var(--el-fill-color); border-radius: 2px; }.distribution-track i { display: block; height: 100%; background: var(--primary, #409eff); opacity: .6; border-radius: 2px; }
button:focus-visible { outline: 2px solid var(--primary, #409eff); outline-offset: 3px; }
@media(max-width:700px) { .usage-analytics { padding: 14px; }.usage-scope { align-items: flex-start; }.usage-scope > div { display: grid; gap: 5px; }.usage-summary { grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 17px 12px; }.usage-summary > div { padding-left: 12px; }.usage-summary > div:nth-child(3) { border: 0; padding-left: 0; }.usage-summary strong { font-size: 20px; }.usage-summary small { font-size: 11px; }.usage-distributions { grid-template-columns: 1fr; gap: 18px; }.usage-trend svg { height: 130px; } }
@media(max-width:700px) { .usage-observation { gap: 10px; }.usage-observation span { font-size: 11px; }.usage-observation strong { font-size: 14px; }.ranking-toolbar h3 { flex-basis: 100%; }.ranking-row { grid-template-columns: 18px repeat(2, minmax(0, 1fr)); gap: 6px; }.ranking-name { grid-column: 2 / -1; }.ranking-row > span:not(.ranking-position) { text-align: left; display: grid; gap: 3px; padding-left: 0; }.ranking-row > span:nth-of-type(2) { grid-column: 2; }.ranking-row small { margin-right: 0; } }
.usage-analytics { padding: 13px 17px 15px; background: var(--card-bg); }
.usage-scope { gap: 10px; }
.usage-scope > div { gap: 8px; }
.usage-scope strong { display: inline-flex; align-items: center; gap: 6px; font-size: 11px; font-weight: 550; }
.usage-scope strong::before { content: ''; width: 5px; height: 5px; border-radius: 2px; background: var(--badge-blue,var(--primary)); }
.usage-scope span { font-size: 9px; }
.analytics-toggle { display: inline-flex; align-items: center; gap: 6px; font-size: 10px; padding: 3px 7px; border: 1px solid var(--card-border); border-radius: 5px; color: var(--text-2); background: var(--bg); }
.analytics-toggle:hover { color: var(--primary); border-color: var(--primary); }
.analysis-checked { font-size: 9px; line-height: 1.6; margin: 6px 0 0; }
.usage-summary { gap: 12px; margin-top: 12px; }
.usage-summary > .usage-stat { position: relative; min-width: 0; display: grid; grid-template-columns: minmax(0,1fr); gap: 4px; border: 1px solid var(--card-border); border-radius: 8px; padding: 12px 13px 11px; background: var(--card-bg); overflow: hidden; }
.usage-summary > .usage-stat::after { content: ''; position: absolute; height: 2px; left: 0; right: 0; top: 0; background: var(--stat-color); opacity: .55; }
.usage-stat { --stat-color: var(--badge-blue,var(--primary)); --stat-soft: var(--badge-blue-soft,var(--primary-soft)); }
.stat-tokens { --stat-color: var(--badge-purple,var(--primary)); --stat-soft: var(--badge-purple-soft,var(--primary-soft)); }
.stat-cache { --stat-color: var(--badge-amber,var(--warning)); --stat-soft: var(--badge-amber-soft,var(--bg)); }
.stat-latency { --stat-color: var(--badge-green,var(--success)); --stat-soft: var(--badge-green-soft,var(--primary-soft)); }
.usage-summary .usage-stat-icon { position: absolute; right: 12px; top: 12px; width: 29px; height: 29px; display: grid; place-items: center; background: var(--stat-soft); color: var(--stat-color); border-radius: 7px; border: 1px solid color-mix(in srgb,var(--stat-color) 12%,transparent); font-size: 15px; }
.usage-summary .usage-stat > span:not(.usage-stat-icon) { font-size: 10px; color: var(--text-3); padding-right: 30px; }
.usage-summary .usage-stat > strong { font-size: 24px; font-weight: 600; letter-spacing: -.6px; line-height: 1.25; padding-right: 31px; margin-top: 2px; }
.usage-summary .usage-stat > small { font-size: 9px; line-height: 1.6; margin-top: 2px; }
.usage-analysis { margin-top: 14px; padding-top: 14px; }
.usage-observation { gap: 10px; padding-bottom: 12px; }
.usage-observation > div { gap: 4px; padding: 10px 12px; border: 1px solid var(--card-border); border-radius: 7px; background: var(--bg); }
.usage-observation span { font-size: 10px; }.usage-observation strong { font-size: 15px; }.usage-observation small { font-size: 9px; }
.observation-stage-note { font-size: 9px; line-height: 1.7; margin: 0 0 12px; }
.analysis-toolbar { font-size: 10px; }
.analysis-metric { padding: 3px; background: var(--bg); border: 1px solid var(--card-border); border-radius: 6px; }
.analysis-metric button { font-size: 10px; padding: 4px 8px; }
.usage-trend { padding: 13px 0 15px; }.usage-trend svg { height: 140px; }.trend-heading strong { font-size: 11px; }
.usage-distributions { gap: 18px; }.usage-distributions h3 { font-size: 11px; }.distribution-row { font-size: 11px; }
.usage-ranking { margin-top: 14px; padding-top: 14px; }.ranking-row { padding: 8px 0; font-size: 11px; }.ranking-row small { font-size: 10px; }
@media (max-width: 1100px) { .usage-summary { gap: 9px; }.usage-summary > .usage-stat { padding: 11px; }.usage-summary .usage-stat-icon { top: 11px; right: 10px; width: 25px; height: 25px; font-size: 13px; }.usage-summary .usage-stat > strong { font-size: 22px; padding-right: 22px; }.usage-summary .usage-stat > span:not(.usage-stat-icon) { font-size: 9px; padding-right: 23px; } }
@media (max-width: 700px) { .usage-analytics { padding: 12px; }.usage-scope strong { font-size: 10px; }.usage-scope > div { gap: 4px; }.usage-scope span { font-size: 8px; }.analytics-toggle { font-size: 9px; padding: 4px 6px; }.analysis-checked { font-size: 8px; }.usage-summary { grid-template-columns: repeat(2,minmax(0,1fr)); gap: 8px; margin-top: 10px; }.usage-summary > .usage-stat { padding: 11px 10px; border: 1px solid var(--card-border); }.usage-summary > .usage-stat:nth-child(3) { border: 1px solid var(--card-border); padding-left: 10px; }.usage-summary .usage-stat-icon { width: 23px; height: 23px; top: 10px; right: 9px; font-size: 12px; border-radius: 6px; }.usage-summary .usage-stat > span:not(.usage-stat-icon) { font-size: 9px; padding-right: 22px; }.usage-summary .usage-stat > strong { font-size: 22px; padding-right: 0; }.usage-summary .usage-stat > small { font-size: 8px; }.usage-observation > div { padding: 9px; }.usage-observation span { font-size: 9px; }.usage-observation strong { font-size: 13px; }.usage-observation small { font-size: 8px; }.usage-distributions { gap: 16px; }.usage-trend svg { height: 120px; } }
</style>
