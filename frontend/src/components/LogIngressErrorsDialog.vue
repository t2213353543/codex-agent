<script setup>
import { computed, onUnmounted, ref, watch } from 'vue'
import { readIngressEvents } from '../api/ingressEvents'
import { ingressReasons, ingressReasonLabel, emptyIngressFilters, ingressInitialFilters, ingressQueryError, validateIngressPage, ingressCollectorStats } from '../utils/logIngressState'
import { createLatestRequest } from '../utils/latestRequest'
import { managedTimeParts } from '../utils/logManagementState'
import { logLocalDateInput, logTimestampInput, logLatency, logTokenCount } from '../utils/logQueryState'
const props = defineProps({ modelValue: Boolean, query: Object, timezone: String })
const emit = defineEmits(['update:modelValue'])
const opened = computed({ get: () => props.modelValue, set: value => emit('update:modelValue', value) })
const draft = ref(emptyIngressFilters()), applied = ref(emptyIngressFilters()), size = ref(50), appliedSize = ref(50)
const rows = ref([]), metadata = ref(null), cursor = ref(''), previous = ref([]), nextCursor = ref(''), hasMore = ref(false)
const loading = ref(false), error = ref(''), page = ref(1)
const latest = createLatestRequest()
let sequence = 0
const dirty = computed(() => JSON.stringify(draft.value) !== JSON.stringify(applied.value) || size.value !== appliedSize.value)
const collector = computed(() => ingressCollectorStats(metadata.value?.collector))
const count = logTokenCount
const time = stamp => { const parts = managedTimeParts(stamp, 'Asia/Shanghai'); return `${parts.date} ${parts.time}` }
const canPage = computed(() => !loading.value && !error.value && !dirty.value)
async function load(targetCursor = '', targetPrevious = [], first = false) {
  const filters = first ? { ...draft.value } : { ...applied.value }, requestedSize = first ? size.value : appliedSize.value
  const invalid = ingressQueryError(filters)
  if (invalid) { ++sequence; latest.cancel(); loading.value = false; error.value = invalid; return }
  const id = ++sequence
  loading.value = true; error.value = ''
  if (first) { applied.value = filters; appliedSize.value = requestedSize }
  try {
    await latest.run(signal => readIngressEvents(filters, requestedSize, targetCursor, { signal }), response => {
      const data = validateIngressPage(response)
      rows.value = data.items; metadata.value = data; cursor.value = targetCursor; previous.value = targetPrevious
      nextCursor.value = data.next_cursor; hasMore.value = data.has_more; page.value = targetPrevious.length + 1
    })
  } catch (cause) { if (id === sequence) error.value = cause.status === 409 ? '留存窗口已变化，旧分页不再完整。请刷新准入事件，从第一页重新读取。' : cause.message || '准入事件读取失败，请重试。' }
  finally { if (id === sequence) loading.value = false }
}
function refresh() { load('', [], true) }
function reset() { draft.value = emptyIngressFilters(); refresh() }
function next() { if (canPage.value && hasMore.value) load(nextCursor.value, [...previous.value, cursor.value]) }
function back() { if (canPage.value && previous.value.length) load(previous.value.at(-1), previous.value.slice(0, -1)) }
function closePending() { ++sequence; latest.cancel(); loading.value = false }
watch(() => props.modelValue, open => {
  closePending()
  if (open) { draft.value = ingressInitialFilters(props.query); applied.value = { ...draft.value }; rows.value = []; metadata.value = null; cursor.value = ''; previous.value = []; page.value = 1; refresh() }
})
onUnmounted(closePending)
</script>
<template>
  <el-dialog v-model="opened" title="准入错误" width="min(980px, calc(100vw - 24px))">
    <p class="ingress-scope">网关准入阶段拒绝的独立事件，不计入使用记录、Token 或请求成功率。仅继承打开窗口时的时间范围与客户端关联 ID；不会关联账号、KEY 或业务请求快照。</p>
    <form class="ingress-filters" @submit.prevent="refresh">
      <label><span>开始时间（包含）</span><input type="datetime-local" :value="logLocalDateInput(draft.from)" aria-label="准入开始时间" @input="draft.from = logTimestampInput($event.target.value)" @keydown.enter.prevent="refresh" /></label>
      <label><span>结束时间（不包含）</span><input type="datetime-local" :value="logLocalDateInput(draft.to)" aria-label="准入结束时间" @input="draft.to = logTimestampInput($event.target.value)" @keydown.enter.prevent="refresh" /></label>
      <label class="ingress-reason-filter"><span>准入原因</span><select :value="draft.reason" aria-label="准入原因" @change="draft.reason = $event.target.value"><option value="">全部原因</option><option v-for="[value,label] in ingressReasons" :key="value" :value="value">{{ label }}</option></select></label>
      <label><span>HTTP 状态</span><input :value="draft.status" aria-label="准入 HTTP 状态" inputmode="numeric" placeholder="400–599" @input="draft.status = $event.target.value" @keydown.enter.prevent="refresh" /></label>
      <label class="ingress-id-filter"><span>客户端关联 ID</span><input :value="draft.client_request_id" aria-label="准入客户端关联 ID" placeholder="入站 X-Client-Request-ID，精确匹配" @input="draft.client_request_id = $event.target.value" @keydown.enter.prevent="refresh" /></label>
      <div class="ingress-filter-actions"><el-button native-type="submit" type="primary" :loading="loading">刷新准入事件</el-button><el-button @click="reset">重置准入筛选</el-button></div>
    </form>
    <p class="ingress-note">日期按上海时区（UTC+08:00）输入和展示。{{ dirty ? '筛选尚未查询，分页已暂停。' : '' }}</p>
    <div v-if="error" class="ingress-error" role="status">{{ error }} <el-button link type="primary" @click="refresh">从第一页重试</el-button></div>
    <p v-if="loading || error && rows.length" class="ingress-note" role="status">{{ loading ? '正在读取准入事件…' : '当前查询未完成。' }}{{ rows.length ? `暂时保留上次成功加载的第 ${page} 页。` : '' }}</p>
    <div v-if="metadata" class="ingress-retention"><strong>匹配 {{ count(metadata.total) }} 条留存事件 · 当前页 {{ rows.length }} 条</strong><span>全局最近最多 {{ count(metadata.retention_limit) }} 条；已因上限淘汰 {{ count(metadata.evicted_total) }} 条。不保证留存天数，匹配数不代表历史拒绝总量。</span><small>分页固定首次查询的事件上界；新事件不混入后续页，留存淘汰导致游标失效时须刷新。读取于 {{ time(metadata.checked_at) }}。</small></div>
    <details v-if="metadata" class="ingress-collector"><summary>{{ collector.state }} · 采集队列与丢弃情况</summary><p class="ingress-note">{{ collector.processScoped ? '以下队列、丢弃和失败计数仅对应当前服务进程，重启后清零。' : '计数范围未报告，不作为历史全量。' }}未运行时仍可查看已经持久保存的事件。</p><dl><dt>队列容量 / 排队</dt><dd>{{ count(collector.counts.queue_capacity) }} / {{ count(collector.counts.queued) }}</dd><dt>等待持久化</dt><dd>{{ count(collector.counts.pending) }}</dd><dt>队列丢弃</dt><dd>{{ count(collector.counts.queue_dropped) }}</dd><dt>持久化失败次数</dt><dd>{{ count(collector.counts.persistence_failures) }}</dd><dt>保存结果未确认</dt><dd>{{ count(collector.counts.persistence_unconfirmed) }} 条；可能已保存或未保存，不等于确定丢弃。</dd><dt>关停未保存</dt><dd>{{ count(collector.counts.unpersisted_at_shutdown) }}</dd><dt>最后保存时间</dt><dd>{{ collector.lastPersistedAt ? time(collector.lastPersistedAt) : '未报告' }}</dd></dl><p v-if="collector.error" class="ingress-error">{{ collector.error }}</p></details>
    <p v-if="!loading && !error && !rows.length" class="ingress-empty">当前范围没有留存的准入错误；这不证明历史上未发生拒绝或采集丢弃。</p>
    <div class="ingress-events" :class="{ 'ingress-stale': loading || error || dirty }" :aria-busy="loading">
      <article v-for="row in rows" :key="row.id" class="ingress-event"><div class="ingress-event-heading"><time>{{ time(row.time_ms / 1000) }}</time><strong>{{ ingressReasonLabel(row.reason) }}</strong><span>HTTP {{ row.http_code }}</span></div><div class="ingress-event-meta"><span>{{ row.endpoint || '未记录标准端点' }}</span><span>客户端关联 ID：<code>{{ row.client_request_id || '未记录' }}</code></span></div><details><summary>事件详情</summary><dl><dt>事件 ID</dt><dd class="ingress-code">{{ row.id }}</dd><dt>观测原因</dt><dd>{{ row.reason }}</dd><dt>发生时间戳</dt><dd>{{ row.time_ms }} ms</dd><dt>授权耗时</dt><dd>{{ logLatency(row.authentication_ms) }}</dd><dt>来源</dt><dd>网关准入观测（gateway_ingress_v1）</dd></dl><p class="ingress-note">只保存准入事件的有限元数据，不提供请求正文、认证头、账号或 KEY 身份；事件 ID 不是业务日志 ID。</p></details></article>
    </div>
    <div class="ingress-pagination"><label>每页 <select :value="size" aria-label="准入事件每页条数" @change="size = Number($event.target.value); refresh()"><option v-for="value in [50,100,200]" :key="value" :value="value">{{ value }}</option></select> 条</label><span>第 {{ page }} 页</span><el-button :disabled="!canPage || !previous.length" @click="back">上一页准入事件</el-button><el-button :disabled="!canPage || !hasMore || !nextCursor" @click="next">下一页准入事件</el-button></div>
    <template #footer><el-button @click="opened = false">关闭准入错误</el-button></template>
  </el-dialog>
</template>
<style scoped>
.ingress-scope,.ingress-note,.ingress-retention,.ingress-empty{font-size:12px;color:var(--text-3);line-height:1.8;margin:0 0 14px}.ingress-filters{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:12px}.ingress-filters label{display:grid;gap:6px;min-width:0;font-size:12px;color:var(--text-2)}.ingress-filters input,.ingress-filters select,.ingress-pagination select{box-sizing:border-box;height:34px;min-width:0;width:100%;padding:0 8px;border:1px solid var(--el-border-color);border-radius:4px;background:var(--card-bg);color:var(--text-2);font:inherit;font-size:12px}.ingress-id-filter{grid-column:span 2}.ingress-filter-actions{grid-column:span 2;display:flex;gap:8px;align-items:end}.ingress-filter-actions .el-button+.el-button{margin-left:0}.ingress-filters+.ingress-note{margin-top:8px}.ingress-error{font-size:12px;color:var(--danger);line-height:1.8;margin:10px 0}.ingress-retention{display:grid;gap:4px;border-top:1px solid var(--card-border);padding-top:14px}.ingress-retention strong{font-weight:500;color:var(--text-2)}.ingress-retention small{font-size:11px}.ingress-collector{margin:12px 0 18px;font-size:12px;color:var(--text-2)}.ingress-collector summary,.ingress-event summary{cursor:pointer;color:var(--primary);font-size:12px;line-height:1.8}.ingress-collector dl,.ingress-event dl{display:grid;grid-template-columns:145px minmax(0,1fr);gap:8px 12px;font-size:12px;margin:12px 0}.ingress-collector dt,.ingress-event dt{color:var(--text-3)}.ingress-collector dd,.ingress-event dd{margin:0;overflow-wrap:anywhere}.ingress-event{border-top:1px solid var(--card-border);padding:13px 0;color:var(--text-2);font-size:12px;line-height:1.8}.ingress-event-heading{display:grid;grid-template-columns:170px minmax(0,1fr) auto;gap:12px}.ingress-event-heading strong{font-weight:500}.ingress-event-heading>span{color:var(--danger)}.ingress-event-meta{display:flex;gap:8px 20px;flex-wrap:wrap;color:var(--text-3);margin:5px 0}.ingress-event-meta code,.ingress-code{font-family:ui-monospace,monospace;font-size:11px;overflow-wrap:anywhere}.ingress-stale{opacity:.65}.ingress-pagination{display:flex;align-items:center;justify-content:flex-end;flex-wrap:wrap;gap:8px;padding-top:14px;border-top:1px solid var(--card-border);font-size:12px;color:var(--text-3)}.ingress-pagination label{display:flex;align-items:center;gap:6px;margin-right:auto}.ingress-pagination select{width:72px}.ingress-pagination .el-button+.el-button{margin-left:0}.ingress-empty{text-align:center;padding:24px 0}@media(max-width:700px){.ingress-filters{grid-template-columns:repeat(2,minmax(0,1fr));gap:10px}.ingress-filter-actions{align-items:center;flex-wrap:wrap}.ingress-event-heading{grid-template-columns:minmax(0,1fr) auto;gap:3px 8px}.ingress-event-heading time{grid-column:1/-1}.ingress-event-meta{display:grid;gap:3px}.ingress-pagination label{flex-basis:100%}.ingress-event dl,.ingress-collector dl{grid-template-columns:105px minmax(0,1fr)}.ingress-retention small{font-size:12px}}
@media(max-width:700px){.ingress-reason-filter{grid-column:1/-1}}
</style>
