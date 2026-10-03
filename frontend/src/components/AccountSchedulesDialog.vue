<script setup>
import { computed, onUnmounted, ref, watch } from 'vue'
import { ElMessageBox } from 'element-plus'
import { accountManagement } from '../api/accountManagement'
import { formatTime } from '../utils/format'
import { clearScheduleDraft, definiteScheduleRejection, readScheduleDraft, rememberScheduleDraft } from '../utils/accountScheduleDraft'

const props = defineProps({ modelValue: Boolean, account: Object })
const emit = defineEmits(['update:modelValue'])
const busy = ref(false)
const loading = ref(false)
const visible = computed({ get: () => props.modelValue, set: value => { if (!busy.value) emit('update:modelValue', value) } })
const target = ref(null)
const plans = ref([])
const error = ref('')
const editing = ref(null)
const editorVisible = ref(false)
const form = ref({})
const historyPlan = ref(null)
const history = ref([])
const historyLoading = ref(false)
const creationAttempt = ref(null)
const editorLocked = computed(() => busy.value || !editing.value && Boolean(creationAttempt.value))
let listGeneration = 0, historyGeneration = 0, listController, historyController
const kinds = { usage: '上游额度读取', models: '模型目录读取', stream: '普通流式推理', compact: 'compact 推理' }
const defaults = () => ({ kind: 'usage', model: '', cron: '0 * * * *', timezone: 'Asia/Shanghai', enabled: true, max_results: 50, confirm_inference: false })
const inference = computed(() => ['stream', 'compact'].includes(form.value.kind))
function stopReads() {
  listGeneration++; historyGeneration++; listController?.abort(); historyController?.abort()
  loading.value = false; historyLoading.value = false
}
watch(() => props.modelValue, value => {
  stopReads()
  if (value) {
    target.value = { ...props.account }; plans.value = []; error.value = ''; editorVisible.value = false; historyPlan.value = null; history.value = []
    creationAttempt.value = readScheduleDraft(target.value.id)
    if (creationAttempt.value) edit()
    load()
  }
})
onUnmounted(stopReads)
async function load() {
  listController?.abort(); listController = new AbortController()
  const current = ++listGeneration
  loading.value = true; error.value = ''
  try { const result = await accountManagement.schedules(target.value.id, { signal: listController.signal }); if (current === listGeneration && visible.value) { if (!Array.isArray(result?.plans)) throw new Error('计划列表格式无效'); plans.value = result.plans } }
  catch (caught) { if (current === listGeneration && caught.name !== 'AbortError') error.value = caught.message || '读取计划失败' }
  finally { if (current === listGeneration) loading.value = false }
}
function edit(plan = null) { editing.value = plan ? { ...plan } : null; form.value = plan ? { kind: plan.kind, model: plan.model || '', cron: plan.cron, timezone: plan.timezone, enabled: plan.enabled, max_results: plan.max_results, confirm_inference: false } : creationAttempt.value ? { ...defaults(), ...creationAttempt.value } : defaults(); editorVisible.value = true; error.value = '' }
async function cancelEdit() {
  if (!editing.value && creationAttempt.value) {
    try { await ElMessageBox.confirm('原创建请求可能已经成功。清除草稿不会删除服务器上的计划；再次创建会是新的操作。请先刷新计划列表核对。', '清除未确认创建草稿', { type: 'warning', confirmButtonText: '已核对，清除草稿', cancelButtonText: '继续保留' }) }
    catch { return }
    clearScheduleDraft(target.value.id); creationAttempt.value = null
  }
  editorVisible.value = false
}
function payload(plan, enabled = plan.enabled) { return { kind: plan.kind, ...(plan.model ? { model: plan.model } : {}), cron: plan.cron, timezone: plan.timezone, enabled, max_results: Number(plan.max_results), ...(['stream', 'compact'].includes(plan.kind) && enabled ? { confirm_inference: true } : {}) } }
async function save() {
  if (busy.value) return
  error.value = ''
  if (inference.value && !form.value.model.trim()) { error.value = '推理计划必须填写真实模型 ID'; return }
  if (inference.value && form.value.enabled && !form.value.confirm_inference) { error.value = '启用推理计划前请确认会产生实际请求'; return }
  if (form.value.cron.trim().split(/\s+/).length !== 5) { error.value = 'Cron 必须包含分钟、小时、日、月、星期五个字段'; return }
  if (!form.value.timezone.trim()) { error.value = '请输入 UTC 或有效的 IANA 时区'; return }
  busy.value = true
  try {
    const body = payload({ ...form.value, model: form.value.model.trim(), cron: form.value.cron.trim(), timezone: form.value.timezone.trim() })
    if (editing.value) await accountManagement.updateSchedule(editing.value.id, { ...body, version: editing.value.version })
    else {
      if (!creationAttempt.value) creationAttempt.value = rememberScheduleDraft(target.value.id, { ...body, request_id: crypto.randomUUID() })
      const result = await accountManagement.createSchedule(target.value.id, { ...creationAttempt.value })
      if (!result?.id || result.account_id !== target.value.id || !Number.isInteger(result.version) || result.version < 1) throw new Error('服务器未返回可确认的创建结果')
      clearScheduleDraft(target.value.id); creationAttempt.value = null
    }
    editorVisible.value = false
    await load()
  } catch (caught) {
    if (!editing.value && creationAttempt.value) {
      if (definiteScheduleRejection(caught)) { clearScheduleDraft(target.value.id); creationAttempt.value = null; error.value = caught.message || '创建请求被拒绝，请核对输入' }
      else error.value = caught.status === 409 ? '原创建请求发生冲突，计划可能已修改或删除。请刷新列表核对；原请求与参数已保留。' : '创建结果未确认，计划可能已经存在。原请求与参数已保留，重读不会新建第二个计划。'
    } else error.value = caught.status === 409 ? '计划已被修改，请重新读取后编辑；当前输入已保留。' : caught.message || '保存计划失败'
  }
  finally { busy.value = false }
}
async function toggle(plan) {
  if (busy.value) return
  const enabled = !plan.enabled
  if (enabled && ['stream', 'compact'].includes(plan.kind)) {
    try { await ElMessageBox.confirm('启用后会按计划实际发送推理请求，可能消耗上游额度。确认启用？', '启用定时推理', { confirmButtonText: '确认启用', cancelButtonText: '取消' }) }
    catch { return }
  }
  busy.value = true; error.value = ''
  try { await accountManagement.updateSchedule(plan.id, { ...payload(plan, enabled), version: plan.version }); await load() }
  catch (caught) { error.value = caught.message || '更新计划状态失败' }
  finally { busy.value = false }
}
async function remove(plan) {
  if (busy.value) return
  try { await ElMessageBox.confirm('删除后不再安排后续检查，正在执行的检查可能已经发出。', '删除定时计划', { type: 'warning', confirmButtonText: '删除计划', cancelButtonText: '取消' }) }
  catch { return }
  busy.value = true; error.value = ''
  try { await accountManagement.deleteSchedule(plan.id, plan.version); if (historyPlan.value?.id === plan.id) historyPlan.value = null; await load() }
  catch (caught) { error.value = caught.message || '删除计划失败' }
  finally { busy.value = false }
}
async function showHistory(plan) {
  historyController?.abort(); historyController = new AbortController()
  const current = ++historyGeneration
  historyPlan.value = plan; history.value = []; historyLoading.value = true; error.value = ''
  try { const result = await accountManagement.scheduleResults(plan.id, { signal: historyController.signal }); if (current === historyGeneration && visible.value && historyPlan.value?.id === plan.id) { if (!Array.isArray(result?.results)) throw new Error('运行历史格式无效'); history.value = result.results } }
  catch (caught) { if (current === historyGeneration && caught.name !== 'AbortError') error.value = caught.message || '读取运行历史失败' }
  finally { if (current === historyGeneration) historyLoading.value = false }
}
const statusNames = { ok: '成功', failed: '失败', unknown: '结果未知', skipped: '已跳过' }
</script>

<template>
  <el-dialog v-model="visible" title="定时检查与测试" width="min(840px, calc(100vw - 32px))" :close-on-click-modal="false" :close-on-press-escape="!busy" :show-close="!busy" align-center append-to-body destroy-on-close class="detail-dialog">
    <p class="intro"><strong>{{ target?.name }}</strong> · 已创建 {{ plans.length }} / 20 个计划</p>
    <el-alert v-if="error" :title="error" type="error" :closable="false" class="feedback" />
    <p class="muted">额度与目录计划只读上游；推理计划需要单独确认。已停用账号会跳过周期检查。计划不会改写账号的业务状态。</p>
    <div class="schedule-toolbar"><el-button type="primary" :disabled="busy || !creationAttempt && plans.length >= 20" @click="edit()">{{ creationAttempt ? '查看未确认创建' : '创建计划' }}</el-button><el-button :loading="loading" :disabled="busy" @click="load">刷新计划</el-button></div>
    <div v-if="editorVisible" class="schedule-editor">
      <h3>{{ editing ? '编辑计划' : '创建计划' }}</h3>
      <el-alert v-if="!editing && creationAttempt" title="此创建请求尚未确认。参数已锁定，重读沿用原操作 ID；关闭后仍保留在本浏览器会话中。" type="warning" :closable="false" class="feedback" />
      <el-form label-position="top" :disabled="editorLocked"><div class="schedule-form-grid"><el-form-item label="检查类型"><el-select v-model="form.kind" class="full-width" aria-label="计划检查类型"><el-option v-for="(label, key) in kinds" :key="key" :label="label" :value="key" /></el-select></el-form-item><el-form-item v-if="inference" label="真实模型 ID"><el-input v-model="form.model" aria-label="计划模型 ID" placeholder="例如上游目录中的模型 ID" /></el-form-item><el-form-item label="Cron（分 时 日 月 周）"><el-input v-model="form.cron" aria-label="计划 Cron" placeholder="0 * * * *" /></el-form-item><el-form-item label="时区"><el-input v-model="form.timezone" aria-label="计划时区" placeholder="Asia/Shanghai 或 UTC" /></el-form-item><el-form-item label="保留运行结果"><el-input-number v-model="form.max_results" :min="1" :max="100" :precision="0" aria-label="计划保留结果数" /></el-form-item><el-form-item label="启用"><el-switch v-model="form.enabled" aria-label="计划启用状态" /></el-form-item></div><p class="muted">示例：每小时 0 * * * *；每 15 分钟 */15 * * * *；每天 09:00 为 0 9 * * *。服务器校验时区和表达式。</p><el-checkbox v-if="inference && form.enabled" v-model="form.confirm_inference" class="plan-confirm">我确认此计划会周期性发送推理请求，可能消耗上游额度</el-checkbox></el-form>
      <div class="schedule-toolbar"><el-button :disabled="busy" @click="cancelEdit">{{ !editing && creationAttempt ? '清除未确认草稿' : '取消编辑' }}</el-button><el-button type="primary" :loading="busy" @click="save">{{ !editing && creationAttempt ? '重读原创建结果' : '保存计划' }}</el-button></div>
    </div>
    <p v-if="loading" role="status" class="muted">正在读取计划…</p><p v-else-if="!plans.length" class="empty-state">还没有定时计划。创建后会显示下次执行时间。</p>
    <article v-for="plan in plans" :key="plan.id" class="schedule-plan"><div class="plan-heading"><strong>{{ kinds[plan.kind] || plan.kind }}</strong><el-tag :type="plan.running ? 'warning' : plan.enabled ? 'success' : 'info'" size="small">{{ plan.running ? '正在执行' : plan.enabled ? '已启用' : '已停用' }}</el-tag><span v-if="plan.model" class="muted">{{ plan.model }}</span></div><p class="plan-expression"><code>{{ plan.cron }}</code><span>{{ plan.timezone }}</span></p><p class="muted">下次：{{ plan.enabled ? formatTime(plan.next_run_at) : '已暂停' }} · 上次：{{ formatTime(plan.last_run_at) }} · 保留 {{ plan.max_results }} 条</p><div class="plan-actions"><el-button size="small" :disabled="busy" @click="edit(plan)">编辑</el-button><el-button size="small" :disabled="busy" @click="toggle(plan)">{{ plan.enabled ? '停用' : '启用' }}</el-button><el-button size="small" @click="showHistory(plan)">运行历史</el-button><el-button size="small" type="danger" plain :disabled="busy" @click="remove(plan)">删除</el-button></div></article>
    <section v-if="historyPlan" class="schedule-history"><h3>运行历史 · {{ kinds[historyPlan.kind] }}</h3><p class="muted">最近最多 50 条。进程中断的运行可能标记为“结果未知”；系统不会因此自动重放推理。</p><p v-if="historyLoading" role="status">正在读取…</p><p v-else-if="!history.length" class="empty-state">暂无运行记录</p><details v-for="run in history" :key="run.id"><summary><el-tag :type="run.status === 'ok' ? 'success' : run.status === 'failed' ? 'danger' : 'warning'" size="small">{{ statusNames[run.status] || run.status }}</el-tag> {{ formatTime(run.started_at || run.scheduled_at) }}</summary><p class="muted">计划时间 {{ formatTime(run.scheduled_at) }} · 完成 {{ formatTime(run.finished_at) }}</p><pre v-if="run.snapshot">{{ JSON.stringify(run.snapshot, null, 2) }}</pre><p v-else class="muted">此项没有完整检查结果。</p></details></section>
    <template #footer><span class="footer-note">执行计划使用所填时区；此页时间按上海时区显示。</span><el-button :disabled="busy" @click="visible = false">关闭</el-button></template>
  </el-dialog>
</template>

<style scoped>.intro { margin-top: 0; overflow-wrap: anywhere; line-height: 1.7; }.muted, .footer-note { color: var(--text-3); font-size: 12px; line-height: 1.7; }.feedback { margin-bottom: 14px; }.schedule-toolbar, .plan-actions, .plan-heading { display: flex; gap: 8px; flex-wrap: wrap; align-items: center; }.schedule-toolbar { margin: 16px 0; }.schedule-toolbar .el-button + .el-button, .plan-actions .el-button + .el-button { margin: 0; }.schedule-editor { padding: 16px; margin: 16px 0; border: 1px solid var(--card-border); background: var(--el-fill-color-light); }.schedule-editor h3, .schedule-history h3 { font-size: 14px; margin: 0 0 16px; }.schedule-form-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 0 16px; }.full-width { width: 100%; }.plan-confirm { white-space: normal; height: auto; }.plan-confirm :deep(.el-checkbox__label) { white-space: normal; line-height: 1.7; }.schedule-plan { border-top: 1px solid var(--card-border); padding: 18px 0; font-size: 13px; }.plan-heading strong, .plan-heading .muted { overflow-wrap: anywhere; }.plan-expression { display: flex; gap: 14px; color: var(--text-2); }.empty-state { color: var(--text-3); font-size: 13px; padding: 16px 0; }.schedule-history { margin-top: 16px; border-top: 1px solid var(--card-border); padding-top: 20px; }.schedule-history details { border-bottom: 1px solid var(--card-border); padding: 12px 0; font-size: 12px; }.schedule-history summary { cursor: pointer; }.schedule-history pre { max-height: 240px; overflow: auto; white-space: pre-wrap; overflow-wrap: anywhere; padding: 12px; background: var(--el-fill-color-light); }.footer-note { margin-right: 16px; }@media(max-width: 550px) { .schedule-form-grid { grid-template-columns: 1fr; }.footer-note { display: block; text-align: left; margin: 0 0 12px; } }</style>
