<script setup>
import { computed, onUnmounted, ref, watch } from 'vue'
import { logManagement } from '../api/logManagement'
import { createLatestRequest } from '../utils/latestRequest'
import { formatTime } from '../utils/format'
import { errorReviewPayload, reviewAllowed, reviewRecordRef, validErrorReview } from '../utils/logErrorReviewState'
const props = defineProps({ record: Object, active: Boolean })
const emit = defineEmits(['changed'])
const review = ref(null), loading = ref(false), saving = ref(false), error = ref(''), saveError = ref(''), conflict = ref(false), missing = ref(false), saved = ref(false)
const draft = ref({ resolved: false, note: '' })
const latest = createLatestRequest()
let sequence = 0
const allowed = computed(() => reviewAllowed(props.record))
const changed = computed(() => review.value && (review.value.resolved !== draft.value.resolved || review.value.note !== draft.value.note))
async function load(preserveDraft = false) {
  const id = ++sequence, target = reviewRecordRef(props.record)
  loading.value = true; error.value = ''; missing.value = false; saved.value = false
  if (!preserveDraft) { review.value = null; draft.value = { resolved: false, note: '' }; saveError.value = ''; conflict.value = false }
  try {
    await latest.run(signal => logManagement.review(target, { signal }), data => {
      if (!validErrorReview(data?.review)) throw new Error('处理状态响应格式无效')
      review.value = data.review
      if (!preserveDraft) draft.value = { resolved: data.review.resolved, note: data.review.note }
      conflict.value = false; saveError.value = ''
    })
  } catch (cause) { if (id === sequence) { missing.value = cause.status === 404; error.value = missing.value ? '原始记录已清理或不存在，无法更新处理状态。' : cause.message || '处理状态加载失败' } }
  finally { if (id === sequence) loading.value = false }
}
async function save() {
  if (saving.value || loading.value || missing.value || conflict.value || !changed.value) return
  let body
  try { body = errorReviewPayload(review.value, draft.value) } catch (cause) { saveError.value = cause.message; return }
  const target = reviewRecordRef(props.record), id = sequence
  saving.value = true; saveError.value = ''; saved.value = false
  try {
    const data = await logManagement.reviewUpdate(target, body)
    if (id !== sequence || !props.active || target !== reviewRecordRef(props.record)) return
    if (!validErrorReview(data?.review)) throw new Error('保存结果未确认，请重新读取处理状态核对。')
    review.value = data.review; draft.value = { resolved: data.review.resolved, note: data.review.note }; saved.value = true
    emit('changed', { record: target, review: data.review })
  } catch (cause) {
    if (id !== sequence) return
    conflict.value = cause.status === 409; missing.value = cause.status === 404
    saveError.value = conflict.value ? '处理状态已被其他会话更新。草稿已保留，请重新读取后核对并保存。' : missing.value ? '原始记录已清理或不存在，未保存处理状态。' : cause.message || '保存未确认，请重新读取状态核对。'
  } finally { if (id === sequence) saving.value = false }
}
watch([() => props.active, () => reviewRecordRef(props.record), () => allowed.value], ([active, , canReview]) => {
  ++sequence; latest.cancel(); saving.value = false; loading.value = false
  if (active && canReview) load()
}, { immediate: true })
onUnmounted(() => { ++sequence; latest.cancel() })
</script>
<template>
  <section v-if="allowed" class="error-review-panel" aria-label="错误处理状态" :aria-busy="loading || saving">
    <div class="review-heading"><h3>错误处理</h3><span v-if="review" class="review-state" :class="{ resolved: review.resolved }">{{ review.resolved ? '已处理' : '未处理' }}</span></div>
    <p class="review-note">这是管理员的处理标记，原始请求仍保留失败或中断状态，不会重新发送请求或改写用量。</p>
    <p v-if="loading" role="status" class="review-note">正在读取处理状态…</p>
    <div v-if="error" class="review-error" role="status">{{ error }} <el-button v-if="!missing" link type="primary" :disabled="loading || saving" @click="load(Boolean(review))">重试处理状态</el-button></div>
    <template v-if="review">
      <p class="review-note">版本 {{ review.version }}<template v-if="review.updated_at"> · {{ formatTime(review.updated_at) }}<br />管理员会话指纹：<span class="review-actor">{{ review.updated_by || '未记录' }}</span></template></p>
      <p v-if="review.resolved && review.resolved_at" class="review-note">标记已处理于 {{ formatTime(review.resolved_at) }} · 会话 <span class="review-actor">{{ review.resolved_by || '未记录' }}</span></p>
      <label class="review-checkbox"><input type="checkbox" :checked="draft.resolved" :disabled="loading || saving || missing" @change="draft.resolved = $event.target.checked; saved = false" /><span>标记为已处理</span></label>
      <label class="review-comment"><span>处理备注（最多 1000 字）</span><el-input v-model="draft.note" type="textarea" :rows="3" maxlength="1000" aria-label="错误处理备注" placeholder="记录处理过程，避免填写凭据或请求正文" :disabled="loading || saving || missing" @input="saved = false" /></label>
      <div v-if="saveError" class="review-error" role="status">{{ saveError }}</div><p v-if="saved" class="review-success" role="status">处理状态已保存。</p>
      <div class="review-actions"><el-button :disabled="loading || saving || missing" @click="load(true)">重新读取状态</el-button><el-button type="primary" :loading="saving" :disabled="loading || missing || conflict || !changed" @click="save">保存处理状态</el-button></div>
      <details v-if="review.history.length" class="review-history"><summary>处理历史 · {{ review.history_total ?? review.history.length }} 次变更</summary><p v-if="review.history_truncated" class="review-note">仅显示最近 50 次，累计变更数以服务端为准。</p><ol><li v-for="entry in review.history" :key="entry.version"><strong>{{ entry.resolved ? '已处理' : '未处理' }}</strong><span> · 版本 {{ entry.version }} · {{ formatTime(entry.at) }}</span><p class="review-actor">会话 {{ entry.actor || '未记录' }}</p><p v-if="entry.note" class="review-history-note">{{ entry.note }}</p></li></ol></details>
    </template>
  </section>
</template>
<style scoped>
.error-review-panel { border-top: 1px solid var(--card-border); padding-top: 18px; margin-top: 18px; color: var(--text-2); font-size: 12px; }.review-heading { display: flex; align-items: center; justify-content: space-between; gap: 12px; }.review-heading h3 { margin: 0; color: var(--text-1); font-size: 13px; font-weight: 500; }.review-state { color: var(--warning, #a66c11); }.review-state.resolved, .review-success { color: var(--success, #529b2e); }.review-note { color: var(--text-3); font-size: 12px; line-height: 1.8; margin: 10px 0; }.review-actor { overflow-wrap: anywhere; font-variant-numeric: tabular-nums; }.review-checkbox { display: flex; align-items: center; gap: 7px; margin: 12px 0; }.review-checkbox input { margin: 0; accent-color: var(--primary, #409eff); }.review-comment { display: grid; gap: 8px; }.review-error { color: var(--danger, #d9363e); line-height: 1.7; margin: 10px 0; }.review-actions { display: flex; justify-content: flex-end; gap: 8px; margin-top: 12px; }.review-actions .el-button + .el-button { margin-left: 0; }.review-history { margin-top: 18px; }.review-history summary { cursor: pointer; color: var(--text-3); }.review-history ol { padding-left: 20px; }.review-history li { margin: 13px 0; line-height: 1.7; }.review-history li strong { font-weight: 500; }.review-history li p { margin: 3px 0; }.review-history-note { white-space: pre-wrap; overflow-wrap: anywhere; }@media(max-width:500px) { .review-actions { flex-wrap: wrap; }.review-actions .el-button { flex: 1; } }
</style>
