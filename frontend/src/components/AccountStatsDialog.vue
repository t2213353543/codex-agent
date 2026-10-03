<script setup>
import { usageCacheRatio } from '../utils/logManagementState'
import { logPercent, usageMetricNumber } from '../utils/logMetrics'
import { computed, defineAsyncComponent, onUnmounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { logManagement } from '../api/logManagement'
import { formatDuration, formatNumber, formatTime, SHANGHAI_TIMEZONE, shanghaiDayStart, shanghaiDateInput, parseShanghaiDateInput } from '../utils/format'
import { canonicalDailyTimezone, validateDailyStats, validDailyTimezone } from '../utils/accountDailyStats'
const AccountDailyStats = defineAsyncComponent(() => import('./AccountDailyStats.vue'))

const props = defineProps({ modelValue: Boolean, account: Object })
const emit = defineEmits(['update:modelValue'])
const visible = computed({ get: () => props.modelValue, set: value => emit('update:modelValue', value) })
const router = useRouter()
const target = ref(null), stats = ref(null), loading = ref(false), error = ref(''), from = ref(''), to = ref(''), preset = ref('30')
const inputTimezone = SHANGHAI_TIMEZONE
const timezone = ref(inputTimezone)
let controller, generation = 0
function range(days) {
  const end = Date.now() / 1000
  const start = shanghaiDayStart(end) - (days === 'today' ? 0 : Number(days) - 1) * 86400
  from.value = shanghaiDateInput(start); to.value = shanghaiDateInput(end)
}
watch(() => props.modelValue, value => { stop(); if (value) { target.value = { ...props.account }; stats.value = null; timezone.value = inputTimezone; preset.value = '30'; range('30'); load() } })
function stop() { generation++; controller?.abort(); loading.value = false }
onUnmounted(stop)
async function setPreset(value) { preset.value = value; range(value); await load() }
async function load() {
  stop()
  const start = parseShanghaiDateInput(from.value), end = parseShanghaiDateInput(to.value)
  if (!Number.isFinite(start) || !Number.isFinite(end) || start >= end) { error.value = '请选择结束晚于开始的完整时间区间'; return }
  if (!validDailyTimezone(timezone.value)) { error.value = '请填写明确的 IANA 统计时区，例如 Asia/Shanghai。'; return }
  timezone.value = canonicalDailyTimezone(timezone.value)
  if (end - start > 4998 * 86400) { error.value = '日统计最多选择 4998 天，请缩小范围。'; return }
  const current = generation; controller = new AbortController(); loading.value = true; error.value = ''
  try {
    const signal = controller.signal, query = { account_id: target.value.id, from: String(start), to: String(end), step: '86400', timezone: timezone.value }
    const snapshot = await logManagement.snapshot(query, { signal })
    if (current !== generation || !visible.value) return
    if (snapshot.scope !== 'retained_request_details' || typeof snapshot.as_of !== 'string' || !snapshot.as_of) throw new Error('账号统计查询快照未建立，请重试。')
    const data = await logManagement.stats({ ...query, as_of: snapshot.as_of }, { signal })
    if (current !== generation || !visible.value) return
    stats.value = validateDailyStats(data, query)
  } catch (caught) { if (current === generation && caught.name !== 'AbortError') error.value = caught.status === 409 ? '查询快照已失效，请重新查询；上次成功统计保留。' : caught.message || '账号统计读取失败' }
  finally { if (current === generation) loading.value = false }
}
const metric = value => value == null ? '未提供' : formatNumber(value)
const ratio = count => stats.value?.total_requests ? `${(count / stats.value.total_requests * 100).toFixed(1)}%` : '—'
function logs() { if (stats.value) { visible.value = false; router.push({ path: '/logs', query: { account_id: target.value.id, from: String(stats.value.from), to: String(stats.value.to), timezone: stats.value.timezone, step: '86400' } }) } }
</script>

<template>
  <el-dialog v-model="visible" title="账号使用统计" width="min(1040px, calc(100vw - 32px))" class="detail-dialog account-stats-dialog" align-center append-to-body destroy-on-close>
    <div class="stats-title"><strong>{{ target?.name }}</strong></div><p class="muted">统计基于仍保留的请求明细。已清理的明细不在此范围内；本地账号窗口累计另列于账号详情。</p>
    <div class="stats-presets"><button v-for="item in [{ value: 'today', label: '今天' }, { value: '7', label: '7 天' }, { value: '30', label: '30 天' }]" :key="item.value" type="button" :aria-pressed="preset === item.value" @click="setPreset(item.value)">{{ item.label }}</button></div>
    <p class="muted">时间输入使用上海时区 {{ inputTimezone }}，开始包含、结束不含；下方 IANA 时区决定日历日归属。</p>
    <div class="stats-dates"><label>开始时间<input type="datetime-local" :value="from" @input="from = $event.target.value; preset = ''" aria-label="账号统计开始时间" /></label><label>结束时间<input type="datetime-local" :value="to" @input="to = $event.target.value; preset = ''" aria-label="账号统计结束时间" /></label><el-button type="primary" :loading="loading" @click="load">查询统计</el-button></div>
    <label class="stats-timezone">日历统计时区（IANA）<el-input v-model="timezone" aria-label="账号日统计时区" placeholder="例如 Asia/Shanghai" @keydown.enter.prevent="load" /></label>
    <el-alert v-if="error" :title="error" type="error" :closable="false" class="feedback" /><p v-if="loading" role="status" class="muted">正在统计完整区间…</p>
    <template v-if="stats">
      <p class="muted">已应用区间：{{ formatTime(stats.from) }} 至 {{ formatTime(stats.to) }} · 更新于 {{ formatTime(stats.checked_at) }}<span v-if="error"> · 保留上次成功结果</span></p>
      <dl class="stats-totals"><div><dt>请求</dt><dd>{{ metric(stats.total_requests) }}</dd></div><div><dt>成功 / 失败 / 中断</dt><dd>{{ metric(stats.success) }} / {{ metric(stats.errors) }} / {{ metric(stats.aborted) }}</dd></div><div><dt>Token</dt><dd>{{ metric(stats.total_tokens) }}</dd></div><div><dt>平均总耗时</dt><dd>{{ stats.average_latency_ms == null ? '无样本' : formatDuration(stats.average_latency_ms) }}</dd></div></dl>
      <dl class="stats-detail"><div><dt>输入 / 输出 / 缓存读取</dt><dd>{{ metric(stats.total_input_tokens) }} / {{ metric(stats.total_output_tokens) }} / {{ metric(usageMetricNumber(stats, 'total_cached_tokens')) }}</dd></div><div><dt>用量已知 / 未知请求</dt><dd>{{ metric(stats.usage_known_requests) }} / {{ metric(stats.usage_unknown_requests) }}</dd></div><div><dt>平均首 Token 时间</dt><dd>{{ stats.average_ttft_ms == null ? '无采集样本' : formatDuration(stats.average_ttft_ms) }} <span class="muted">{{ metric(stats.ttft_samples) }} 个样本</span></dd></div><div><dt>推理 Token / 缓存创建 Token</dt><dd>{{ stats.reasoning_samples ? metric(stats.total_reasoning_tokens) : '未采集' }} / {{ stats.cache_write_samples ? metric(stats.total_cache_write_tokens) : '未采集' }}</dd></div><div><dt>缓存读取命中率</dt><dd>{{ logPercent(usageCacheRatio(stats)) }} <span class="muted">读取 / 对应输入，不含创建 · {{ metric(stats.cache_read_samples) }} 条已知 / {{ metric(stats.cache_read_unknown_requests) }} 条未知</span></dd></div></dl>
      <AccountDailyStats :stats="stats" />
      <section v-for="group in [{ key: 'models', label: '模型分布' }, { key: 'endpoints', label: '端点分布' }, { key: 'error_categories', label: '错误分布' }]" :key="group.key" class="stats-section"><h3>{{ group.label }}</h3><div v-for="item in stats[group.key] || []" :key="item.name" class="distribution-row"><span>{{ item.name || '未知' }}</span><span>{{ metric(item.total_requests) }} 次 · {{ ratio(item.total_requests) }} · {{ metric(item.total_tokens) }} Token</span></div><p v-if="!stats[group.key]?.length" class="muted">暂无对应记录</p></section>
    </template>
    <template #footer><el-button @click="visible = false">关闭</el-button><el-button type="primary" :disabled="!stats" @click="logs">查看该区间使用记录</el-button></template>
  </el-dialog>
</template>

<style scoped>
.stats-timezone { display: flex; align-items: center; flex-wrap: wrap; gap: 10px; font-size: 12px; color: var(--text-3); margin-bottom: 16px; }.stats-timezone .el-input { width: 260px; max-width: 100%; }
</style>

<style scoped>.stats-title { display: flex; align-items: center; gap: 12px; justify-content: space-between; }.stats-title strong { font-size: 17px; overflow-wrap: anywhere; }.muted { color: var(--text-3); font-size: 12px; line-height: 1.7; }.stats-presets { display: flex; gap: 6px; margin: 18px 0 14px; }.stats-presets button { border: 1px solid var(--card-border); border-radius: 4px; padding: 7px 16px; background: var(--card-bg); color: var(--text-2); cursor: pointer; font: inherit; font-size: 12px; }.stats-presets button[aria-pressed="true"] { color: var(--el-color-primary); border-color: var(--el-color-primary); background: var(--el-color-primary-light-9); }.stats-dates { display: grid; grid-template-columns: 1fr 1fr auto; align-items: end; gap: 12px; margin-bottom: 18px; }.stats-dates label { display: grid; gap: 7px; min-width: 0; color: var(--text-3); font-size: 12px; }.stats-dates input { min-width: 0; box-sizing: border-box; width: 100%; height: 32px; border: 1px solid var(--el-border-color); border-radius: 4px; padding: 0 8px; background: var(--el-fill-color-blank); color: var(--text-1); font: inherit; }.feedback { margin-bottom: 14px; }.stats-totals { display: grid; grid-template-columns: 1fr 1.4fr 1fr 1fr; margin: 20px 0; padding: 16px 0; border-block: 1px solid var(--card-border); }.stats-totals > div { min-width: 0; padding: 0 12px; border-right: 1px solid var(--card-border); }.stats-totals > div:last-child { border-right: 0; }.stats-totals dt { color: var(--text-3); font-size: 12px; }.stats-totals dd { margin: 9px 0 0; font-size: 19px; font-weight: 600; overflow-wrap: anywhere; font-variant-numeric: tabular-nums; }.stats-detail { margin: 0; font-size: 12px; }.stats-detail > div { display: grid; grid-template-columns: 170px minmax(0,1fr); padding: 10px 0; gap: 10px; border-bottom: 1px solid var(--card-border); }.stats-detail dt { color: var(--text-3); }.stats-detail dd { margin: 0; overflow-wrap: anywhere; }.stats-section { margin-top: 26px; }.stats-section h3 { font-size: 14px; margin: 0 0 12px; }.stats-chart { width: 100%; max-height: 230px; color: var(--text-2); }.stats-section summary { cursor: pointer; color: var(--text-3); font-size: 12px; margin-bottom: 8px; }.distribution-row { display: flex; justify-content: space-between; gap: 12px; padding: 10px 0; border-bottom: 1px solid var(--card-border); font-size: 12px; }.distribution-row > span { overflow-wrap: anywhere; min-width: 0; }.distribution-row > span:last-child { color: var(--text-3); text-align: right; }.empty-state { color: var(--text-3); font-size: 13px; padding: 20px 0; }@media(max-width: 600px) { .stats-title { flex-wrap: wrap; }.stats-dates { grid-template-columns: 1fr; }.stats-totals { grid-template-columns: 1fr 1fr; gap: 18px 0; }.stats-totals > div:nth-child(2) { border: 0; }.stats-detail > div { grid-template-columns: 110px minmax(0,1fr); } }</style>
