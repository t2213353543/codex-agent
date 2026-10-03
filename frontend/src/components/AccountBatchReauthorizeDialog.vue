<script setup>
import { computed, ref, watch } from 'vue'
import { accountManagement } from '../api/accountManagement'
import { parseBatchReauthorization } from '../utils/accountReauthorization'
import { redactImportMessage } from '../utils/importAccounts'
const props = defineProps({ modelValue: Boolean, targets: { type: Array, default: () => [] } })
const emit = defineEmits(['update:modelValue', 'busy', 'updated'])
const busy = ref(false), reading = ref(false), text = ref(''), error = ref(''), rows = ref([]), targets = ref([]), confirmed = ref(false), stopping = ref(false), started = ref(false)
let readSequence = 0
const visible = computed({ get: () => props.modelValue, set: value => { if (!busy.value) emit('update:modelValue', value) } })
const identityChanges = computed(() => rows.value.filter(row => row.allow_identity_change).length)
const counts = computed(() => Object.fromEntries(['pending', 'success', 'failed', 'unknown'].map(state => [state, rows.value.filter(row => row.state === state).length])))
const names = { pending: '未执行', success: '已替换', failed: '失败', unknown: '待核对' }
watch(busy, value => emit('busy', value))
watch(() => props.modelValue, value => { readSequence++; text.value = ''; error.value = ''; rows.value = []; confirmed.value = false; stopping.value = false; started.value = false; reading.value = false; targets.value = value ? props.targets.map(({ id, version, name }) => ({ id, version, name })) : [] })
watch(text, () => { if (!started.value) { rows.value = []; confirmed.value = false } })
async function readFile(event) {
  const file = event.target.files?.[0]; event.target.value = ''; if (!file) return
  const current = ++readSequence
  if (file.size > 32 * 1024 * 1024) { error.value = '凭据映射文件不能超过 32 MB'; return }
  reading.value = true; error.value = ''
  try { const content = await file.text(); if (current === readSequence && visible.value && !busy.value) text.value = content }
  catch { if (current === readSequence) error.value = '读取凭据映射失败' }
  finally { if (current === readSequence) reading.value = false }
}
function preview() {
  if (busy.value || started.value) return
  error.value = ''; confirmed.value = false
  try { if (new TextEncoder().encode(text.value).length > 32 * 1024 * 1024) throw new Error('凭据映射不能超过 32 MB'); rows.value = parseBatchReauthorization(text.value, targets.value) }
  catch (caught) { rows.value = []; error.value = caught.message }
}
const retryable = row => row.state === 'pending' || row.state === 'failed' && !['version_conflict', 'identity_change', 'not_found'].includes(row.code)
async function submit(retry = false) {
  if (busy.value || !confirmed.value || !rows.value.length) return
  const queue = retry ? rows.value.filter(retryable) : rows.value.filter(row => row.state === 'pending')
  busy.value = true; started.value = true; stopping.value = false; error.value = ''; text.value = ''
  try {
    for (let offset = 0; offset < queue.length; offset += 10) {
      if (stopping.value) break
      const batch = queue.slice(offset, offset + 10)
      try {
        const result = await accountManagement.reauthorizeBatch({ items: batch.map(({ id, version, credentials, allow_identity_change }) => ({ id, version, credentials, allow_identity_change })) })
        if (!Array.isArray(result.results) || result.results.length !== batch.length || new Set(result.results.map(item => item.id)).size !== batch.length || result.results.some(item => !batch.some(row => row.id === item.id) || typeof item.ok !== 'boolean')) throw new Error('服务器未返回完整逐项结果')
        for (const row of batch) {
          const item = result.results.find(item => item.id === row.id)
          row.state = item.ok ? 'success' : 'failed'; row.code = item.code || ''; row.message = item.ok ? '管理配置与历史关联保留' : redactImportMessage(item.error || item.code || '替换失败', [{ payload: { credentials: row.credentials } }])
          if (item.ok) row.credentials = null
        }
      } catch (caught) {
        for (const row of batch) { row.state = 'unknown'; row.message = '本批提交结果未确认，请刷新核对；不会自动重试' }
        error.value = redactImportMessage(caught.message || '提交中断', batch.map(row => ({ payload: { credentials: row.credentials } }))); break
      }
    }
    if (rows.value.some(row => row.state === 'success')) emit('updated')
  } finally { busy.value = false }
}
</script>
<template>
  <el-dialog v-model="visible" title="按 ID 批量替换凭据" width="min(800px, calc(100vw - 32px))" :close-on-click-modal="false" :close-on-press-escape="!busy" :show-close="!busy" align-center append-to-body destroy-on-close class="detail-dialog">
    <p class="intro">已固定 {{ targets.length }} 个账号及版本。每条映射必须使用已选账号 ID；不按名称猜测目标，替换后保留管理配置与历史关联。</p><el-alert v-if="error" :title="error" type="error" :closable="false" class="feedback" />
    <template v-if="!started"><details class="mapping-template"><summary>查看凭据映射格式与已选账号 ID</summary><pre>[{ "id": "已选账号ID", "credentials": { "access_token": "新访问令牌", "refresh_token": "新刷新令牌" }, "allow_identity_change": false }]</pre><p v-for="target in targets.slice(0, 100)" :key="target.id">{{ target.name || target.id }} · <code>{{ target.id }}</code> · v{{ target.version }}</p><p v-if="targets.length > 100">仅展示前 100 项，全部 {{ targets.length }} 项均可匹配。</p></details><label class="mapping-upload">读取 JSON 文件<input type="file" accept=".json,application/json" :disabled="busy || reading" aria-label="批量凭据映射文件" @change="readFile" /></label><el-input v-model="text" type="textarea" :rows="6" aria-label="批量凭据 ID 映射" autocomplete="off" :disabled="reading" /><el-button class="preview-button" :disabled="reading || !text.trim()" @click="preview">核对目标映射</el-button></template>
    <p v-if="rows.length" class="muted">{{ rows.length }} 项已完成本地格式与目标核对，尚未远端验证；其中 {{ identityChanges }} 项明确允许更换登录身份。</p><el-checkbox v-if="rows.length && !started" v-model="confirmed" class="mapping-confirm">我确认按这些账号 ID 替换凭据，并已核对允许更换身份的条目</el-checkbox>
    <p v-if="started" role="status">已替换 {{ counts.success }} · 失败 {{ counts.failed }} · 待核对 {{ counts.unknown }} · 未执行 {{ counts.pending }}</p><div v-if="rows.length" class="mapping-results"><article v-for="row in rows" :key="row.id"><div><strong>{{ row.name }}</strong><el-tag size="small" :type="row.state === 'success' ? 'success' : row.state === 'failed' ? 'danger' : 'info'">{{ names[row.state] }}</el-tag></div><p class="muted">{{ row.id }} · 选择版本 {{ row.version }} · {{ row.allow_identity_change ? '允许更换身份' : '仅同一身份' }}</p><p v-if="row.message">{{ row.message }}</p></article></div><p class="muted">每批最多 10 项。关闭会清除本窗口凭据；连接中断的提交可能已处理，需人工核对。已成功项不会重复执行。</p>
    <template #footer><el-button :disabled="busy" @click="visible = false">关闭</el-button><el-button v-if="busy" :disabled="stopping" @click="stopping = true">{{ stopping ? '当前批次后停止' : '停止后续批次' }}</el-button><el-button v-else-if="started && rows.some(retryable)" @click="submit(true)">执行未成功的确定项</el-button><el-button v-if="!started" type="primary" :disabled="!confirmed || !rows.length || reading" @click="submit()">确认替换 {{ rows.length }} 个账号</el-button></template>
  </el-dialog>
</template>
<style scoped>.intro { margin-top: 0; line-height: 1.7; font-size: 13px; }.feedback { margin-bottom: 14px; }.mapping-template { margin: 14px 0; color: var(--text-3); font-size: 12px; }.mapping-template summary { cursor: pointer; }.mapping-template pre { white-space: pre-wrap; overflow-wrap: anywhere; padding: 12px; background: var(--el-fill-color-light); line-height: 1.7; }.mapping-template p { overflow-wrap: anywhere; }.mapping-upload { display: flex; gap: 8px; flex-wrap: wrap; font-size: 12px; margin: 12px 0; }.mapping-upload input { max-width: 100%; min-width: 0; }.preview-button { margin: 14px 0 0; }.muted { color: var(--text-3); font-size: 12px; line-height: 1.7; }.mapping-confirm { height: auto; white-space: normal; margin: 12px 0; }.mapping-confirm :deep(.el-checkbox__label) { white-space: normal; line-height: 1.7; }.mapping-results { max-height: 330px; overflow: auto; border-block: 1px solid var(--card-border); }.mapping-results article { padding: 12px 0; border-bottom: 1px solid var(--card-border); font-size: 12px; overflow-wrap: anywhere; }.mapping-results article > div { display: flex; gap: 12px; flex-wrap: wrap; align-items: center; }.mapping-results strong { font-size: 13px; }.mapping-results p { margin: 6px 0 0; }</style>
