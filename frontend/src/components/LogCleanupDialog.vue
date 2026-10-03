<script setup>
import { computed, onUnmounted, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { logManagement } from '../api/logManagement'
import { createLatestRequest } from '../utils/latestRequest'
import { formatTime } from '../utils/format'
import { logLocalDateInput, logTimestampInput, logTokenCount } from '../utils/logQueryState'
import { managedFilterLabels, managedFilterKeys } from '../utils/logManagementState'
import { cleanupCanCancel, cleanupLabels, cleanupProgress, cleanupRangeError, cleanupSnapshot, validCleanupJob, validCleanupPreview } from '../utils/logCleanupState'
const props = defineProps({ modelValue: Boolean, query: { type: Object, required: true } })
const emit = defineEmits(['update:modelValue', 'changed'])
const opened = computed({ get: () => props.modelValue, set: value => emit('update:modelValue', value) })
const filters = ref({})
const from = ref(''), to = ref('')
const preview = ref(null), previewLoading = ref(false), previewError = ref('')
const confirmed = ref(false), creating = ref(false), createError = ref('')
const jobs = ref([]), total = ref(0), page = ref(1), jobsLoading = ref(false), jobsError = ref('')
const cancelTarget = ref(null), canceling = ref(false), cancelError = ref('')
const now = ref(Math.floor(Date.now() / 1000))
const previewRequest = createLatestRequest(), listRequest = createLatestRequest()
let previewSequence = 0, listSequence = 0, timer, createID = ''
const deletedSeen = new Map()
const snapshot = computed(() => cleanupSnapshot(filters.value, from.value, to.value))
const chips = computed(() => managedFilterKeys.filter(key => !['from', 'to'].includes(key) && filters.value[key]).map(key => ({ key, label: managedFilterLabels[key], value: filters.value[key] })))
const previewValid = computed(() => validCleanupPreview(preview.value, now.value))
const canCreate = computed(() => confirmed.value && previewValid.value && preview.value.total > 0 && !creating.value)
const formatCount = logTokenCount

function clearPreview() {
  ++previewSequence
  previewRequest.cancel()
  preview.value = null
  confirmed.value = false
  previewError.value = ''
  createError.value = ''
  previewLoading.value = false
  createID = ''
}
watch([from, to], clearPreview)

async function previewCleanup() {
  clearPreview()
  now.value = Math.floor(Date.now() / 1000)
  const invalid = cleanupRangeError(snapshot.value, now.value)
  if (invalid) { previewError.value = invalid; return }
  const id = ++previewSequence
  const requested = snapshot.value
  previewLoading.value = true
  try {
    await previewRequest.run(signal => logManagement.cleanupPreview(requested, { signal }), data => {
      if (!validCleanupPreview(data)) throw new Error('清理预览无效或已过期，请重新预览')
      preview.value = data
      createID = crypto.randomUUID()
    })
  } catch (error) { if (id === previewSequence) previewError.value = error.message || '预览失败，请重试' }
  finally { if (id === previewSequence) previewLoading.value = false }
}

function acceptJob(job) {
  if (!validCleanupJob(job)) throw new Error('清理任务响应格式无效，请刷新任务列表确认')
  const previous = deletedSeen.get(job.id)
  if ((previous !== undefined && job.deleted_rows > previous) || (previous === undefined && job.deleted_rows > 0)) emit('changed')
  deletedSeen.set(job.id, job.deleted_rows || 0)
  return job
}
async function loadJobs() {
  const id = ++listSequence
  jobsLoading.value = true
  jobsError.value = ''
  try {
    await listRequest.run(signal => logManagement.cleanupJobs(page.value, { signal }), data => {
      if (!Array.isArray(data?.items) || !Number.isSafeInteger(data.total) || data.total < 0) throw new Error('任务列表响应格式无效')
      const items = data.items.map(acceptJob)
      jobs.value = items
      total.value = data.total
    })
  } catch (error) { if (id === listSequence) jobsError.value = error.message || '任务列表加载失败' }
  finally { if (id === listSequence) jobsLoading.value = false }
}

async function createCleanup() {
  now.value = Math.floor(Date.now() / 1000)
  if (!canCreate.value) { if (preview.value && !previewValid.value) createError.value = '预览已过期，请重新预览后确认。'; return }
  creating.value = true
  createError.value = ''
  const body = { preview_id: preview.value.preview_id, confirmation_token: preview.value.confirmation_token, request_id: createID }
  try {
    const result = await logManagement.cleanupCreate(body)
    acceptJob(result?.job)
    preview.value = null
    confirmed.value = false
    page.value = 1
    await loadJobs()
    emit('changed')
    ElMessage.success('已创建清理任务，可在下方查看进度')
  } catch (error) { createError.value = error.message || '任务创建未确认，请刷新任务列表或重试；重试将复用同一请求标识。' }
  finally { creating.value = false }
}

async function cancelCleanup() {
  if (canceling.value || !cleanupCanCancel(cancelTarget.value)) return
  canceling.value = true
  cancelError.value = ''
  try {
    const result = await logManagement.cleanupCancel(cancelTarget.value.id)
    acceptJob(result?.job)
    cancelTarget.value = null
    await loadJobs()
    emit('changed')
    ElMessage.success('已提交取消清理请求')
  } catch (error) { cancelError.value = error.message || '取消失败，请重试' }
  finally { canceling.value = false }
}

function changePage(delta) { page.value = Math.max(1, page.value + delta); loadJobs() }
function stop() { clearInterval(timer); timer = undefined; ++previewSequence; ++listSequence; previewRequest.cancel(); listRequest.cancel() }
watch(() => props.modelValue, value => {
  stop()
  if (!value) { clearPreview(); return }
  filters.value = { ...props.query }
  from.value = props.query.from || ''
  to.value = props.query.to || String(Math.floor(Date.now() / 60000) * 60)
  now.value = Math.floor(Date.now() / 1000)
  clearPreview()
  page.value = 1
  loadJobs()
  timer = setInterval(() => { now.value = Math.floor(Date.now() / 1000); if (!document.hidden && !jobsLoading.value) loadJobs() }, 10000)
})
onUnmounted(stop)
</script>

<template>
  <el-dialog v-model="opened" title="清理请求明细" width="min(850px, calc(100vw - 24px))" :close-on-click-modal="!creating" :close-on-press-escape="!creating" :show-close="!creating">
    <div class="cleanup-scope"><strong>仅清理请求明细</strong><p>选中明细删除后不能恢复；累计用量与账号额度会保留。使用记录列表、同条件统计及明细导出将不再包含已清理记录。</p></div>
    <section class="cleanup-prepare"><h3>1. 预览清理范围</h3><p class="cleanup-help">使用打开窗口时的已应用筛选。时间按上海时区输入。先指定明确时间范围，再获取服务端预览；预览本身不会删除记录。</p><p v-if="filters.as_of && !snapshot.as_of" class="cleanup-help">清理日期与当前查询不同，将按新日期重新预览明细；原列表快照不用于本次删除范围。</p>
      <div v-if="chips.length" class="cleanup-chips"><span v-for="chip in chips" :key="chip.key" :title="chip.value">{{ chip.label }}：{{ chip.value }}</span></div><p v-else class="cleanup-help">除时间范围外，未限定账号、KEY 或其他条件。</p>
      <div class="cleanup-dates"><label><span>开始时间（包含）</span><input type="datetime-local" :value="logLocalDateInput(from)" :disabled="creating" aria-label="清理开始时间" @input="from = logTimestampInput($event.target.value)" /></label><label><span>结束时间（不包含）</span><input type="datetime-local" :value="logLocalDateInput(to)" :disabled="creating" aria-label="清理结束时间" @input="to = logTimestampInput($event.target.value)" /></label><el-button :loading="previewLoading" :disabled="creating" @click="previewCleanup">预览匹配记录</el-button></div>
      <el-alert v-if="previewError" :title="previewError" type="error" :closable="false" />
      <div v-if="preview" class="cleanup-confirmation"><h3>2. 确认 {{ formatCount(preview.total) }} 条请求明细</h3><p class="cleanup-help">此预览固定匹配的记录范围，有效至 {{ formatTime(preview.expires_at) }}；之后新产生的请求不会被纳入该任务。</p><p v-if="!previewValid" class="cleanup-expired" role="status">预览已过期，请重新预览。</p><p v-else-if="preview.total === 0" class="cleanup-help">当前条件没有可清理的记录。</p><label v-else class="cleanup-checkbox"><input type="checkbox" :checked="confirmed" :disabled="creating || !previewValid" @change="confirmed = $event.target.checked" /><span>我确认删除以上 {{ formatCount(preview.total) }} 条请求明细，并保留累计用量与额度。</span></label><el-button type="danger" :disabled="!canCreate" :loading="creating" @click="createCleanup">创建清理任务</el-button></div>
      <el-alert v-if="createError" :title="createError" type="error" :closable="false" />
    </section>
    <section class="cleanup-history"><div class="cleanup-history-heading"><h3>清理任务</h3><span>每 10 秒更新</span><el-button link type="primary" :loading="jobsLoading" @click="loadJobs">刷新任务</el-button></div><el-alert v-if="jobsError" :title="jobsError + (jobs.length ? '；以下保留上次结果。' : '')" type="error" :closable="false" />
      <div v-if="!jobs.length" class="cleanup-empty">{{ jobsLoading ? '正在加载任务…' : jobsError ? '任务暂时无法加载，请重试。' : '尚无清理任务' }}</div>
      <article v-for="job in jobs" :key="job.id" class="cleanup-job"><div class="cleanup-job-heading"><span :class="'cleanup-status cleanup-status-' + job.status">{{ cleanupLabels[job.status] }}</span><time>{{ formatTime(job.created_at) }}</time><el-button v-if="cleanupCanCancel(job)" link type="danger" @click="cancelTarget = job; cancelError = ''">取消任务</el-button></div><div class="cleanup-job-progress"><strong>已清理 {{ formatCount(cleanupProgress(job).deleted) }} / {{ formatCount(cleanupProgress(job).total) }} 条</strong><span>剩余 {{ formatCount(cleanupProgress(job).remaining) }} 条</span></div><div v-if="cleanupProgress(job).percentage !== null" class="cleanup-progress-track" role="progressbar" :aria-label="'任务 ' + job.id + ' 清理进度'" :aria-valuenow="Math.round(cleanupProgress(job).percentage)" aria-valuemin="0" aria-valuemax="100"><i :style="{ width: cleanupProgress(job).percentage + '%' }" /></div><dl class="cleanup-job-meta"><dt>任务 ID</dt><dd>{{ job.id }}</dd><dt>创建人</dt><dd>{{ job.created_by || '未记录' }}</dd><template v-if="job.filters?.from || job.filters?.to"><dt>时间范围</dt><dd>{{ formatTime(Number(job.filters.from)) }} 至 {{ formatTime(Number(job.filters.to)) }}</dd></template><template v-if="job.finished_at"><dt>结束时间</dt><dd>{{ formatTime(job.finished_at) }}</dd></template></dl><p v-if="job.error" class="cleanup-job-error">{{ job.error }}</p></article>
      <div v-if="total > 10" class="cleanup-pagination"><span>共 {{ formatCount(total) }} 个任务 · 第 {{ page }} 页</span><el-button size="small" :disabled="jobsLoading || page <= 1" @click="changePage(-1)">上一批任务</el-button><el-button size="small" :disabled="jobsLoading || page * 10 >= total" @click="changePage(1)">下一批任务</el-button></div>
    </section>
    <template #footer><el-button :disabled="creating" @click="opened = false">关闭</el-button></template>
  </el-dialog>
  <el-dialog :model-value="Boolean(cancelTarget)" title="取消清理任务" width="min(440px, calc(100vw - 24px))" :close-on-click-modal="!canceling" :close-on-press-escape="!canceling" :show-close="!canceling" @update:model-value="value => { if (!value && !canceling) cancelTarget = null }"><p class="cleanup-help">停止任务后续的删除操作。已清理的 {{ formatCount(cancelTarget?.deleted_rows) }} 条明细不会恢复。</p><el-alert v-if="cancelError" :title="cancelError" type="error" :closable="false" /><template #footer><el-button :disabled="canceling" @click="cancelTarget = null">继续任务</el-button><el-button type="danger" :loading="canceling" @click="cancelCleanup">确认取消任务</el-button></template></el-dialog>
</template>

<style scoped>
.cleanup-scope { padding: 13px 15px; border: 1px solid var(--card-border); border-left: 3px solid var(--warning, #d99721); border-radius: 4px; background: var(--el-fill-color-light); }.cleanup-scope strong { font-size: 13px; font-weight: 500; color: var(--text-1); }.cleanup-scope p, .cleanup-help { color: var(--text-3); font-size: 12px; line-height: 1.8; margin: 7px 0 0; }
.cleanup-prepare { padding: 18px 0; border-bottom: 1px solid var(--card-border); }h3 { margin: 0; color: var(--text-2); font-size: 13px; font-weight: 500; }.cleanup-chips { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 12px; }.cleanup-chips span { font-size: 11px; padding: 3px 7px; background: var(--el-fill-color-light); border-radius: 3px; max-width: 100%; overflow-wrap: anywhere; }
.cleanup-dates { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)) auto; align-items: end; gap: 12px; margin: 15px 0; }.cleanup-dates label { display: flex; flex-direction: column; gap: 7px; color: var(--text-2); font-size: 12px; }.cleanup-dates input { height: 32px; width: 100%; min-width: 0; box-sizing: border-box; padding: 0 8px; border: 1px solid var(--el-border-color); border-radius: 4px; background: var(--card-bg); color: var(--text-1); font: inherit; }.cleanup-confirmation { margin-top: 15px; padding: 14px; border: 1px solid var(--card-border); border-radius: 4px; }.cleanup-checkbox { display: flex; align-items: flex-start; gap: 7px; font-size: 12px; line-height: 1.8; color: var(--text-2); margin: 13px 0; }.cleanup-checkbox input { margin-top: 5px; accent-color: var(--danger); }.cleanup-expired { font-size: 12px; color: var(--danger); }.cleanup-confirmation + .el-alert { margin-top: 10px; }
.cleanup-history { padding-top: 18px; }.cleanup-history-heading { display: flex; align-items: center; gap: 10px; margin-bottom: 12px; }.cleanup-history-heading span { font-size: 11px; color: var(--text-3); margin-right: auto; }.cleanup-empty { padding: 22px; text-align: center; font-size: 12px; color: var(--text-3); }.cleanup-job { border: 1px solid var(--card-border); border-radius: 5px; padding: 13px 15px; margin-bottom: 10px; }.cleanup-job-heading { display: flex; align-items: center; flex-wrap: wrap; gap: 12px; }.cleanup-status { font-size: 12px; color: var(--text-2); }.cleanup-status-running { color: var(--primary, #409eff); }.cleanup-status-succeeded { color: var(--success); }.cleanup-status-failed { color: var(--danger); }.cleanup-job-heading time { font-size: 11px; color: var(--text-3); }.cleanup-job-heading .el-button { margin-left: auto; }.cleanup-job-progress { display: flex; align-items: baseline; justify-content: space-between; gap: 10px; margin-top: 12px; font-size: 12px; color: var(--text-2); font-variant-numeric: tabular-nums; }.cleanup-job-progress strong { font-weight: 500; }.cleanup-job-progress > span { font-size: 11px; color: var(--text-3); }.cleanup-progress-track { height: 3px; border-radius: 3px; overflow: hidden; background: var(--el-fill-color); margin-top: 8px; }.cleanup-progress-track i { display: block; height: 100%; background: var(--primary, #409eff); }.cleanup-job-meta { display: grid; grid-template-columns: 65px minmax(0, 1fr); gap: 4px 10px; color: var(--text-3); font-size: 11px; line-height: 1.7; margin: 10px 0 0; }.cleanup-job-meta dd { margin: 0; overflow-wrap: anywhere; }.cleanup-job-error { margin: 9px 0 0; color: var(--danger); white-space: pre-wrap; overflow-wrap: anywhere; font-size: 12px; line-height: 1.7; }.cleanup-pagination { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; font-size: 12px; color: var(--text-3); }.cleanup-pagination > span { margin-right: auto; }.cleanup-pagination .el-button + .el-button { margin-left: 0; }
@media(max-width:600px) { .cleanup-dates { grid-template-columns: minmax(0, 1fr); }.cleanup-dates input { height: 36px; }.cleanup-job { padding: 12px; }.cleanup-job-heading { gap: 8px; }.cleanup-job-heading time { flex: 1; }.cleanup-job-progress { flex-wrap: wrap; gap: 5px; }.cleanup-history-heading { gap: 7px; }.cleanup-job-meta { grid-template-columns: 56px minmax(0, 1fr); }.cleanup-job-meta dd { font-size: 11px; } }
</style>
