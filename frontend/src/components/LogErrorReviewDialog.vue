<script setup>
import { computed, onUnmounted, ref, watch } from 'vue'
import { logManagement } from '../api/logManagement'
import { createLatestRequest } from '../utils/latestRequest'
import { errorReviewQuery, reviewRecordRef } from '../utils/logErrorReviewState'
import { managedTimeParts } from '../utils/logManagementState'
import { getLogFailure } from '../utils/logFailure'
import LogSeverityBadge from './LogSeverityBadge.vue'
import { logSeverityOptions } from '../utils/logSeverity'
const props = defineProps({ modelValue: Boolean, filters: Object, timezone: String })
const emit = defineEmits(['update:modelValue', 'detail'])
const opened = computed({ get: () => props.modelValue, set: value => emit('update:modelValue', value) })
const resolved = ref('false'), size = ref(20), rows = ref([]), total = ref(null), loading = ref(false), error = ref(''), cursor = ref(''), nextCursor = ref(''), hasMore = ref(false), previous = ref([]), checkedAt = ref(0)
const latest = createLatestRequest()
const loadedPage = ref(1)
const severity = ref('')
let sequence = 0
const time = value => { const parts = managedTimeParts(value, props.timezone); return `${parts.date} ${parts.time}` }
async function load(reset = false) {
  if (reset) { cursor.value = ''; previous.value = [] }
  const id = ++sequence
  loading.value = true; error.value = ''
  try {
    await latest.run(signal => logManagement.errors(errorReviewQuery(props.filters, resolved.value, cursor.value, size.value, severity.value), { signal }), data => {
      if (!Array.isArray(data?.items) || !Number.isSafeInteger(data.total) || data.total < 0) throw new Error('错误处理列表响应格式无效')
      rows.value = data.items; total.value = data.total; hasMore.value = data.has_more === true; nextCursor.value = data.next_cursor || ''; checkedAt.value = data.checked_at || 0; loadedPage.value = previous.value.length + 1
    })
  } catch (cause) { if (id === sequence) error.value = cause.status === 409 ? '处理状态集合已变化，旧分页不再有效。请刷新错误列表，从第一页重新读取。' : cause.message || '错误处理列表加载失败' }
  finally { if (id === sequence) loading.value = false }
}
function changeResolved(event) { resolved.value = event.target.value; load(true) }
function changeSeverity(event) { severity.value = event.target.value; load(true) }
function next() { if (loading.value || error.value || !hasMore.value || !nextCursor.value) return; previous.value.push(cursor.value); cursor.value = nextCursor.value; load() }
function back() { if (loading.value || !previous.value.length) return; cursor.value = previous.value.pop(); load() }
function detail(row) { opened.value = false; emit('detail', row.record) }
watch(() => props.modelValue, open => {
  ++sequence; latest.cancel(); loading.value = false
  if (open) { severity.value = props.filters?.error_severity || ''; rows.value = []; total.value = null; checkedAt.value = 0; load(true) }
})
watch(() => JSON.stringify(props.filters), () => { if (props.modelValue) { severity.value = props.filters?.error_severity || ''; load(true) } })
onUnmounted(() => { ++sequence; latest.cancel() })
</script>
<template>
  <el-dialog v-model="opened" title="错误处理" width="min(940px, calc(100vw - 24px))">
    <p class="review-list-note">沿用当前已应用的账号、模型、时间等筛选，仅显示留存的失败与中断请求。处理状态实时查询，不沿用主列表的固定快照；处理标记不改变请求结果或用量。</p>
    <div class="review-list-toolbar"><label><span>处理状态</span><select :value="resolved" aria-label="错误处理列表状态" @change="changeResolved"><option value="false">未处理</option><option value="true">已处理</option><option value="">全部</option></select></label><label><span>错误级别</span><select :value="severity" aria-label="错误处理列表级别" @change="changeSeverity"><option value="">全部级别</option><option v-for="[value,label] in logSeverityOptions" :key="value" :value="value">{{ label }}</option></select></label><span>{{ total === null ? '正在读取匹配数量' : `匹配 ${total} 条 · 当前页 ${rows.length} 条` }}</span><el-button :loading="loading" @click="load(true)">刷新错误列表</el-button></div>
    <p class="review-list-note">级别来自管理规则，不是上游原生分级。此窗口更改级别筛选仅作用于错误处理列表；关闭后主查询不变。</p>
    <div v-if="error" class="review-list-error" role="status">{{ error }} <el-button link type="primary" @click="load()">重试错误列表</el-button></div>
    <p v-if="loading" class="review-list-note" role="status">正在读取当前处理状态…{{ rows.length ? `暂时显示上次成功加载的第 ${loadedPage} 页。` : '' }}</p>
    <p v-else-if="error && rows.length" class="review-list-note">显示上次成功加载的第 {{ loadedPage }} 页，当前查询尚未完成。</p>
    <p v-else-if="!error && !rows.length" class="review-list-empty">当前筛选没有{{ resolved === 'true' ? '已处理' : resolved === 'false' ? '未处理' : '' }}的错误记录。</p>
    <div class="review-list" :class="{ 'review-list-stale': loading || error }" :aria-busy="loading">
      <article v-for="item in rows" :key="reviewRecordRef(item.record)" class="review-list-row">
        <div class="review-list-identity"><time>{{ time(item.record.time) }}</time><strong :title="item.record.account || item.record.account_id">{{ item.record.account || item.record.account_id || '未记录账号' }}</strong><span>{{ item.record.model || '未记录模型' }} · {{ item.record.endpoint || '未记录端点' }}</span></div>
        <div class="review-list-result"><div class="review-result-heading"><span>{{ item.record.status === 'aborted' ? '中断' : '失败' }} · HTTP {{ item.record.http_code ?? '—' }}</span><LogSeverityBadge :record="item.record" /></div><strong>{{ getLogFailure(item.record)?.label || '请求失败' }}</strong><p :title="item.record.error">{{ item.record.error || '未记录错误信息' }}</p></div>
        <div class="review-list-status"><span :class="{ 'review-done': item.review?.resolved === true }">{{ item.review?.resolved === true ? '已处理' : item.review?.resolved === false ? '未处理' : '状态未知' }}</span><small v-if="item.review?.updated_at">{{ time(item.review.updated_at) }}</small><el-button link type="primary" :disabled="loading || !!error" :aria-label="'处理记录 ' + reviewRecordRef(item.record)" @click="detail(item)">查看与处理</el-button></div>
      </article>
    </div>
    <div class="review-list-pagination"><label>每页 <select :value="size" aria-label="错误列表每页条数" @change="size = Number($event.target.value); load(true)"><option :value="20">20</option><option :value="50">50</option><option :value="100">100</option></select> 条</label><span>第 {{ previous.length + 1 }} 页</span><el-button :disabled="loading || !previous.length" @click="back">上一页错误</el-button><el-button :disabled="loading || !!error || !hasMore || !nextCursor" @click="next">下一页错误</el-button></div>
    <p v-if="checkedAt" class="review-list-note">状态读取于 {{ time(checkedAt) }}（{{ timezone || 'UTC' }}）。处理变更后刷新列表，已清理的明细不再显示。</p>
    <template #footer><el-button @click="opened = false">关闭错误处理</el-button></template>
  </el-dialog>
</template>
<style scoped>
.review-result-heading { display:flex;align-items:center;flex-wrap:wrap;gap:5px 9px; }.review-result-heading > span:first-child { color:var(--text-3);font-size:12px; }
.review-list-note { color: var(--text-3); font-size: 12px; line-height: 1.8; margin: 0 0 14px; }.review-list-toolbar, .review-list-toolbar label, .review-list-pagination, .review-list-pagination label { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; }.review-list-toolbar { margin: 16px 0; font-size: 12px; color: var(--text-2); }.review-list-toolbar > span { color: var(--text-3); }.review-list-toolbar > .el-button { margin-left: auto; }.review-list-toolbar select, .review-list-pagination select { height: 30px; border: 1px solid var(--el-border-color); border-radius: 4px; background: var(--card-bg); color: var(--text-2); font: inherit; padding: 0 7px; }.review-list-error { color: var(--danger, #d9363e); font-size: 12px; line-height: 1.8; margin-bottom: 12px; }.review-list-stale { opacity: .65; }.review-list-empty { padding: 28px 0; text-align: center; color: var(--text-3); font-size: 12px; }.review-list-row { display: grid; grid-template-columns: minmax(0, 1.15fr) minmax(0, 1.5fr) 132px; gap: 20px; padding: 15px 0; border-top: 1px solid var(--card-border); color: var(--text-2); font-size: 12px; line-height: 1.7; }.review-list-identity, .review-list-result, .review-list-status { display: flex; flex-direction: column; min-width: 0; gap: 3px; }.review-list-identity time, .review-list-identity > span, .review-list-result > span, .review-list-status small { color: var(--text-3); font-size: 11px; }.review-list-identity strong, .review-list-result strong { font-weight: 500; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }.review-list-identity > span { overflow-wrap: anywhere; }.review-list-result p { display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical; overflow: hidden; margin: 0; overflow-wrap: anywhere; color: var(--text-3); }.review-list-status { text-align: right; align-items: flex-end; }.review-list-status > span { color: var(--warning, #a66c11); }.review-list-status > span.review-done { color: var(--success, #529b2e); }.review-list-pagination { justify-content: flex-end; padding: 15px 0; border-top: 1px solid var(--card-border); color: var(--text-3); font-size: 12px; }.review-list-pagination .el-button + .el-button { margin-left: 0; }.review-list-pagination label { margin-right: auto; }@media(max-width:600px) { .review-list-row { grid-template-columns: minmax(0, 1fr); gap: 10px; }.review-list-status { flex-direction: row; flex-wrap: wrap; align-items: center; text-align: left; gap: 6px 10px; }.review-list-status .el-button { margin-left: auto; }.review-list-identity time, .review-list-result > span { font-size: 12px; }.review-list-toolbar { align-items: flex-start; }.review-list-toolbar > span { flex-basis: 100%; order: 3; }.review-list-pagination label { flex-basis: 100%; }.review-list-row strong { white-space: normal; overflow-wrap: anywhere; } }
</style>
