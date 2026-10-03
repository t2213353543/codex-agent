<script setup>
import { computed, ref, watch } from 'vue'
import { ElMessageBox } from 'element-plus'
import { api } from '../api'
import { accountManagement } from '../api/accountManagement'
import { runAccountBatches } from '../utils/accountManagement'
import { accountEditForm, validateAccountConfiguration } from '../utils/accountFields'

const props = defineProps({ modelValue: Boolean, targets: { type: Array, default: () => [] }, groups: { type: Array, default: () => [] }, proxies: { type: Array, default: () => [] }, initialAction: { type: String, default: 'edit' } })
const emit = defineEmits(['update:modelValue', 'busy', 'updated'])
const visible = computed({ get: () => props.modelValue, set: (value) => { if (!working.value) emit('update:modelValue', value) } })
const action = ref('edit')
const working = ref(false)
const stopping = ref(false)
const targets = ref([])
const fields = ref([])
const form = ref(accountEditForm())
const results = ref([])
const error = ref('')
const labels = { enabled: '启用状态', concurrency: '并发容量', priority: '优先级', weight: '权重', notes: '备注', tags: '标签', groups: '分组', proxies: '代理' }
const actionLabels = { edit: '编辑配置', enable: '启用账号', disable: '停用账号', tags: '替换标签', bind: '追加绑定代理', unbind: '清空代理（直连）', refresh: '刷新令牌', delete: '删除账号' }
const counts = computed(() => Object.fromEntries(['success', 'failed', 'unknown', 'pending'].map((state) => [state, results.value.filter((result) => result.state === state).length])))
const states = { success: '成功', failed: '失败', unknown: '待核对', pending: '未执行' }
const retryable = computed(() => results.value.filter((result) => result.state === 'pending' || result.state === 'failed' && !['version_conflict', 'not_found'].includes(result.code)))
const doneCount = computed(() => results.value.filter((result) => result.state !== 'pending').length)
watch(() => props.modelValue, (value) => {
  if (!value) return
  targets.value = props.targets.map((target) => ({ ...target }))
  action.value = props.initialAction
  fields.value = []
  form.value = accountEditForm()
  results.value = []
  error.value = ''
  stopping.value = false
})
watch(working, (value) => emit('busy', value))
function buildPatch() {
  if (action.value === 'enable') return { enabled: true }
  if (action.value === 'disable') return { enabled: false }
  if (action.value === 'tags') return { tags: [...form.value.tags] }
  if (action.value === 'unbind') return { proxies: [] }
  if (action.value !== 'edit') return undefined
  if (!fields.value.length) throw new Error('请勾选至少一个需要修改的字段')
  const validation = validateAccountConfiguration(form.value)
  if (validation) throw new Error(validation)
  return Object.fromEntries(fields.value.filter(key => Object.hasOwn(labels, key)).map((key) => [key, Array.isArray(form.value[key]) ? [...form.value[key]] : form.value[key]]))
}
async function run(retry = false) {
  if (working.value) return
  error.value = ''
  const selected = retry ? retryable.value.map(({ id }) => targets.value.find((target) => target.id === id)).filter(Boolean) : targets.value
  if (!selected.length) return
  let patch
  try {
    patch = buildPatch()
    if (action.value === 'bind' && !form.value.proxies.length) throw new Error('请选择需要追加的代理')
    if (action.value === 'delete') await ElMessageBox.confirm(`将删除 ${selected.length} 个账号，历史使用记录保留。`, '确认批量删除', { type: 'warning', confirmButtonText: '删除所选账号', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger' })
  } catch (caught) { if (!['cancel', 'close'].includes(caught)) error.value = caught.message; return }
  working.value = true
  stopping.value = false
  const previous = retry ? results.value.filter((result) => !selected.some((target) => target.id === result.id)) : []
  const updateProgress = (next) => { results.value = [...previous, ...next] }
  try {
    if (action.value === 'bind') {
      // Adding keeps each account's existing proxies. The original selection
      // version still guards the update against changes made after selection.
      const next = []
      for (const target of selected) {
        if (stopping.value) break
        try {
          const detail = await api.accountDetail(target.id)
          const response = await accountManagement.actions({ action: 'update', items: [{ id: target.id, version: target.version }], patch: { proxies: [...new Set([...(detail.account?.proxies || []), ...form.value.proxies])] } })
          const result = response.results?.find((item) => item.id === target.id)
          next.push({ ...target, ...result, state: result?.ok === true ? 'success' : result?.ok === false ? 'failed' : 'unknown', message: result?.ok ? '已添加代理' : result?.code === 'version_conflict' ? '版本已变化，请重新选择' : result?.error || '未返回明确结果，请刷新核对' })
        } catch (caught) { next.push({ ...target, state: 'unknown', message: caught.message || '结果未确认，请刷新核对' }); stopping.value = true }
        updateProgress(next)
      }
      const completed = new Set(next.map((item) => item.id))
      updateProgress([...next, ...selected.filter((target) => !completed.has(target.id)).map((target) => ({ ...target, state: 'pending', message: '尚未执行' }))])
    } else {
      await runAccountBatches({ items: selected, action: ['refresh', 'delete'].includes(action.value) ? action.value : 'update', patch, send: accountManagement.actions, stopped: () => stopping.value, onProgress: updateProgress })
    }
    emit('updated')
  } finally { working.value = false }
}
</script>

<template>
  <el-dialog v-model="visible" title="批量管理" width="min(700px, calc(100vw - 32px))" :close-on-click-modal="false" :close-on-press-escape="!working" :show-close="!working" class="detail-dialog account-bulk-dialog" align-center append-to-body destroy-on-close>
    <p class="bulk-intro">已固定选择 <strong>{{ targets.length }}</strong> 个账号。每个账号独立校验版本并返回结果。</p>
    <el-alert v-if="error" :title="error" type="error" :closable="false" />
    <el-select v-model="action" class="full-width" aria-label="批量操作" :disabled="working || results.length > 0"><el-option v-for="(label, value) in actionLabels" :key="value" :label="label" :value="value" /></el-select>
    <div v-if="action === 'edit'" class="bulk-fields">
      <p class="muted">只更新勾选的字段。标签、分组与代理会完整替换；留空将清除关联。</p>
      <div v-for="(label, key) in labels" :key="key" class="bulk-field">
        <el-checkbox :model-value="fields.includes(key)" :disabled="working || results.length > 0" @change="value => fields = value ? [...fields, key] : fields.filter(field => field !== key)">{{ label }}</el-checkbox>
        <div v-if="fields.includes(key)" class="bulk-field-value">
          <el-switch v-if="key === 'enabled'" v-model="form.enabled" :disabled="working || results.length > 0" :aria-label="`批量${label}`" />
          <el-select v-else-if="['groups', 'proxies', 'tags'].includes(key)" v-model="form[key]" multiple filterable :allow-create="key === 'tags'" default-first-option class="full-width" :aria-label="`批量${label}`" :disabled="working || results.length > 0" placeholder="留空将清除"><el-option v-for="item in key === 'groups' ? groups : key === 'proxies' ? proxies : form.tags.map(tag => ({ id: tag, name: tag }))" :key="item.id" :label="item.name" :value="item.id" /></el-select>
          <el-input v-else-if="key === 'notes'" v-model="form.notes" type="textarea" :rows="2" aria-label="批量备注" :disabled="working || results.length > 0" />
          <el-input-number v-else v-model="form[key]" :min="key === 'priority' ? -Number.MAX_SAFE_INTEGER : 1" :max="['concurrency', 'weight'].includes(key) ? 1000 : Number.MAX_SAFE_INTEGER" :precision="0" controls-position="right" :aria-label="`批量${label}`" :disabled="working || results.length > 0" />
        </div>
      </div>
    </div>
    <el-select v-if="action === 'bind'" v-model="form.proxies" multiple class="full-width bulk-extra" aria-label="批量追加代理" :disabled="working || results.length > 0"><el-option v-for="proxy in proxies" :key="proxy.id" :label="proxy.name" :value="proxy.id" /></el-select>
    <el-select v-if="action === 'tags'" v-model="form.tags" multiple filterable allow-create default-first-option class="full-width bulk-extra" aria-label="批量替换标签" placeholder="输入后回车；留空会清除" :disabled="working || results.length > 0"><el-option v-for="tag in form.tags" :key="tag" :label="tag" :value="tag" /></el-select>
    <p v-if="action === 'refresh'" class="muted">每批最多 10 个账号；没有刷新令牌的账号会返回失败原因。此操作不发送模型推理。</p>
    <div v-if="working || results.length" class="bulk-progress" aria-live="polite"><progress :value="doneCount" :max="targets.length || 1" aria-label="批量操作进度" /><span>成功 {{ counts.success }} · 失败 {{ counts.failed }} · 待核对 {{ counts.unknown }} · 未执行 {{ targets.length - doneCount }}</span><span v-if="stopping && working">当前批次结束后停止</span></div>
    <el-table v-if="results.length" :data="results" max-height="300" size="small" class="bulk-results"><el-table-column label="账号" min-width="130"><template #default="{ row }">{{ row.name || row.id }}</template></el-table-column><el-table-column label="结果" width="80"><template #default="{ row }"><el-tag :type="row.state === 'success' ? 'success' : row.state === 'failed' ? 'danger' : 'warning'" size="small">{{ states[row.state] }}</el-tag></template></el-table-column><el-table-column prop="message" label="说明" min-width="220" /></el-table>
    <p v-if="counts.unknown" class="muted">待核对项可能已在服务器执行，请刷新后核对；不会自动重试。</p>
    <template #footer><el-button :disabled="working" @click="visible = false">关闭</el-button><el-button v-if="working" :disabled="stopping" @click="stopping = true">{{ stopping ? '正在停止' : '停止后续批次' }}</el-button><el-button v-else-if="results.length" :disabled="!retryable.length" @click="run(true)">重试失败 / 未执行（{{ retryable.length }}）</el-button><el-button v-else :type="action === 'delete' ? 'danger' : 'primary'" @click="run()">执行{{ actionLabels[action] }}</el-button></template>
  </el-dialog>
</template>

<style scoped>
.full-width { width: 100%; }.bulk-intro { margin: 0 0 16px; line-height: 1.7; }.muted { color: var(--text-3); font-size: 12px; line-height: 1.7; }.bulk-fields { margin-top: 14px; }.bulk-field { display: grid; grid-template-columns: 140px minmax(0, 1fr); gap: 8px; align-items: center; padding: 9px 0; border-bottom: 1px solid var(--card-border); }.bulk-field-value { min-width: 0; }.bulk-extra { margin-top: 14px; }.bulk-progress { margin: 18px 0; display: grid; gap: 8px; font-size: 12px; color: var(--text-2); }.bulk-results :deep(.cell) { overflow-wrap: anywhere; }.el-alert { margin-bottom: 14px; }@media(max-width: 600px) { .bulk-field { grid-template-columns: 1fr; gap: 2px; }.bulk-field-value { padding-left: 24px; }.bulk-field-value .el-input-number { width: 100%; } }
</style>
