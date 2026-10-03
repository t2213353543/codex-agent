<script setup>
import { computed, ref, watch } from 'vue'
import { accountManagement } from '../api/accountManagement'
import { formatTime } from '../utils/format'

const props = defineProps({ modelValue: Boolean, items: { type: Array, default: () => [] }, kind: { type: String, default: 'usage' } })
const emit = defineEmits(['update:modelValue', 'busy'])
const busy = ref(false), stop = ref(false), rows = ref([]), error = ref(''), targets = ref([]), kind = ref('usage')
const visible = computed({ get: () => props.modelValue, set: value => { if (!busy.value) emit('update:modelValue', value) } })
const label = computed(() => kind.value === 'models' ? '模型目录' : '上游额度')
watch(busy, value => emit('busy', value))
watch(() => props.modelValue, value => { if (value) { targets.value = props.items.map(row => ({ id: row.id, version: row.version, name: row.name || row.id })); kind.value = props.kind; rows.value = targets.value.map(row => ({ ...row, state: 'pending' })); error.value = ''; stop.value = false } })
const done = computed(() => rows.value.filter(row => row.state !== 'pending').length)
const canRetry = row => row.state === 'failed' && !['version_conflict', 'account_conflict', 'not_found'].includes(row.code)
async function run(retry = false) {
  if (busy.value) return
  const queue = rows.value.filter(row => row.state === 'pending' || retry && canRetry(row))
  if (!queue.length) return
  busy.value = true; stop.value = false; error.value = ''
  try {
    for (let offset = 0; offset < queue.length; offset += 10) {
      if (stop.value) break
      const batch = queue.slice(offset, offset + 10)
      for (const row of batch) { row.state = 'pending'; row.error = ''; row.snapshot = null }
      try {
        const data = await accountManagement.upstreamBatch({ kind: kind.value, items: batch.map(row => ({ id: row.id, version: row.version })), force: true })
        if (!Array.isArray(data.results) || data.results.length !== batch.length || new Set(data.results.map(row => row.id)).size !== batch.length || data.results.some(row => !batch.some(target => target.id === row.id) || typeof row.ok !== 'boolean')) throw new Error('批量查询返回不完整，未确认项请重新查询')
        for (const row of batch) { const result = data.results.find(item => item.id === row.id); Object.assign(row, { state: result.ok ? 'ok' : 'failed', code: result.code || '', error: result.error || result.snapshot?.error?.message || result.code || '', snapshot: result.snapshot || null }) }
      } catch (caught) {
        for (const row of batch) Object.assign(row, { state: 'failed', error: '本次读取未确认', code: 'read_unconfirmed' })
        error.value = caught.message || '批量读取中断，后续账号尚未查询'; break
      }
    }
  } finally { busy.value = false }
}
function windowLabel(value) { if (value == null) return '未知'; const seconds = value.limit_window_seconds; return `${seconds === 18000 ? '5h' : seconds === 604800 ? '7d' : seconds ? `${seconds / 3600}h` : '窗口'} ${value.used_percent == null ? '未知' : `${value.used_percent}%`}` }
function summary(snapshot) {
  if (!snapshot?.ok) return ''
  if (snapshot.kind === 'models') return `目录 ${snapshot.data?.models?.length ?? 0} 个模型（未验证推理）`
  return `${snapshot.data?.plan_type || '计划未知'} · ${windowLabel(snapshot.data?.rate_limit?.primary_window)} · ${windowLabel(snapshot.data?.rate_limit?.secondary_window)}`
}
</script>

<template>
  <el-dialog v-model="visible" :title="`批量读取${label}`" width="min(760px, calc(100vw - 32px))" :close-on-click-modal="false" :close-on-press-escape="!busy" :show-close="!busy" align-center append-to-body destroy-on-close class="detail-dialog">
    <p class="intro">已固定 {{ targets.length }} 个账号及其版本。确认后会逐批读取真实{{ label }}，每批最多 10 个；此操作不发送推理。账号版本变化会单项失败。</p>
    <el-alert v-if="error" :title="error" type="error" :closable="false" class="feedback" />
    <div class="batch-progress"><progress :value="done" :max="Math.max(1, targets.length)" aria-label="批量上游读取进度" /><span>{{ done }} / {{ targets.length }}</span></div>
    <div class="batch-check-results"><article v-for="row in rows" :key="row.id"><div class="result-heading"><strong>{{ row.name }}</strong><el-tag size="small" :type="row.state === 'ok' ? 'success' : row.state === 'failed' ? 'danger' : 'info'">{{ row.state === 'ok' ? '已读取' : row.state === 'failed' ? '未成功' : '尚未查询' }}</el-tag></div><p v-if="row.snapshot?.ok">{{ summary(row.snapshot) }}</p><p v-if="row.snapshot" class="muted">{{ formatTime(row.snapshot.checked_at) }} · HTTP {{ row.snapshot.http_code || '未知' }} · {{ row.snapshot.source }}</p><p v-if="row.error" class="error-text">{{ row.error }}</p></article></div>
    <p class="muted">额度缺失显示未知。模型目录只说明账号可见模型，不表示该模型已通过推理测试。停止会等待当前批次返回。</p>
    <template #footer><el-button :disabled="busy" @click="visible = false">关闭</el-button><el-button v-if="busy" :disabled="stop" @click="stop = true">{{ stop ? '当前批次后停止' : '停止后续批次' }}</el-button><el-button v-if="rows.some(canRetry) && !busy" @click="run(true)">重读未成功项</el-button><el-button type="primary" :loading="busy" :disabled="!rows.some(row => row.state === 'pending')" @click="run()">{{ done ? '继续读取待查询账号' : '确认开始读取' }}</el-button></template>
  </el-dialog>
</template>

<style scoped>.intro { margin-top: 0; font-size: 13px; line-height: 1.7; }.feedback { margin-bottom: 16px; }.batch-progress { display: flex; gap: 12px; align-items: center; font-size: 12px; margin: 16px 0; }.batch-progress progress { flex: 1; min-width: 0; accent-color: var(--el-color-primary); height: 8px; }.batch-check-results { max-height: 440px; overflow: auto; border-block: 1px solid var(--card-border); }.batch-check-results article { padding: 12px 0; border-bottom: 1px solid var(--card-border); font-size: 12px; }.result-heading { display: flex; flex-wrap: wrap; gap: 10px; align-items: center; }.result-heading strong { font-size: 13px; overflow-wrap: anywhere; }.batch-check-results p { margin: 7px 0 0; line-height: 1.6; overflow-wrap: anywhere; }.muted { color: var(--text-3); font-size: 12px; line-height: 1.7; }.error-text { color: var(--danger); }</style>
