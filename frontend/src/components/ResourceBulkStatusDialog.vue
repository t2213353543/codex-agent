<script setup>
import { computed, onBeforeUnmount, ref } from 'vue'
import { onBeforeRouteLeave } from 'vue-router'
import { ElMessage } from 'element-plus'
import { resourceStatusSnapshot, runResourceStatusBatch } from '../utils/resourceManagement'

const props = defineProps({
  resourceLabel: { type: String, required: true },
  resources: { type: Array, required: true },
  update: { type: Function, required: true },
  reload: { type: Function, required: true },
})
const emit = defineEmits(['visibility', 'result'])
const visible = ref(false)
const enabled = ref(false)
const items = ref([])
const results = ref([])
const working = ref(false)
const refreshed = ref(false)
const notice = ref('')
let disposed = false
const action = computed(() => enabled.value ? '启用' : '停用')
const failed = computed(() => results.value.filter((row) => !row.ok))
const succeeded = computed(() => results.value.filter((row) => row.ok).length)
const displayRows = computed(() => items.value.map((row) => results.value.find((result) => result.id === row.id) || { ...row, message: '待确认', ok: undefined }))

function open(rows, target) {
  if (working.value) return
  items.value = resourceStatusSnapshot(rows)
  if (!items.value.length) return
  enabled.value = target
  results.value = []
  refreshed.value = false
  notice.value = ''
  visible.value = true
  emit('visibility', true)
}
function close() {
  if (working.value) return
  visible.value = false
  emit('visibility', false)
}
async function run(retry = false) {
  if (working.value || (retry && !refreshed.value) || (!retry && results.value.length)) return
  const targets = retry ? items.value.filter((row) => failed.value.some((failure) => failure.id === row.id)) : items.value
  working.value = true
  refreshed.value = false
  notice.value = ''
  try {
    await runResourceStatusBatch(targets, enabled.value, props.update, {
      shouldContinue: () => !disposed,
      onResult: (result) => {
        if (disposed) return
        results.value = [...results.value.filter((row) => row.id !== result.id), result]
        if (result.ok) emit('result', result)
      },
    })
    if (!disposed) {
      const loaded = await props.reload()
      if (!loaded) notice.value = '操作结果已保留，但列表刷新失败。请关闭后刷新列表核对状态。'
    }
  } finally { working.value = false }
}
async function refreshFailed() {
  if (working.value || !failed.value.length) return
  working.value = true
  notice.value = ''
  try {
    if (!await props.reload()) {
      notice.value = '读取最新列表失败，未提交任何重试。请稍后再刷新。'
      return
    }
    if (disposed) return
    const latest = resourceStatusSnapshot(props.resources)
    const failureIDs = new Set(failed.value.map((row) => row.id))
    items.value = items.value.map((row) => failureIDs.has(row.id) ? latest.find((item) => item.id === row.id) || { ...row, version: undefined } : row)
    results.value = results.value.map((row) => {
      if (row.ok) return row
      const item = items.value.find((entry) => entry.id === row.id)
      return { ...row, ...item, message: item.version ? '已读取最新状态，请核对后重试' : '资源已不存在或缺少版本，不会提交更新' }
    })
    refreshed.value = true
    notice.value = '失败项已更新为最新状态，请核对后点击「仅重试失败项」。已成功的资源不会再次提交。'
  } finally { working.value = false }
}
onBeforeRouteLeave((to) => {
  if (working.value && to.name !== 'login') { ElMessage.warning('批量操作进行中，请等待结果后切换页面'); return false }
})
onBeforeUnmount(() => { disposed = true })
defineExpose({ open })
</script>

<template>
  <el-dialog :model-value="visible" :title="`批量${action}${resourceLabel}`" width="min(700px, calc(100vw - 24px))" class="detail-dialog" align-center append-to-body :close-on-click-modal="false" :close-on-press-escape="!working" :show-close="!working" @update:model-value="(value) => { if (!value) close() }">
    <p class="batch-intro">将{{ action }}选中的 {{ items.length }} 个{{ resourceLabel }}。请核对下方名单后确认。</p>
    <el-alert v-if="!enabled && !results.length" type="warning" :closable="false" :title="resourceLabel === '密钥' ? '停用后，使用这些密钥的新请求将被拒绝。' : '停用后，这些代理将不再作为账号的可用代理出口。'" show-icon />
    <el-alert v-if="notice" :title="notice" type="info" :closable="false" show-icon class="batch-notice" />
    <p v-if="results.length" class="batch-summary" aria-live="polite">已处理 {{ results.length }}/{{ items.length }} 项，成功 {{ succeeded }} 项，失败 {{ failed.length }} 项</p>
    <el-table :data="displayRows" max-height="360" class="batch-table" row-key="id">
      <el-table-column prop="name" label="名称" min-width="140" />
      <el-table-column label="当前状态" width="100"><template #default="{ row }">{{ row.enabled ? '启用' : '停用' }}</template></el-table-column>
      <el-table-column label="处理结果" min-width="240"><template #default="{ row }"><span :class="{ 'batch-error': row.ok === false }">{{ row.message }}</span></template></el-table-column>
    </el-table>
    <template #footer>
      <div class="batch-footer">
        <el-button :disabled="working" @click="close">{{ results.length ? '关闭' : '取消' }}</el-button>
        <el-button v-if="failed.length && !refreshed" :loading="working" :disabled="working" @click="refreshFailed">刷新失败项</el-button>
        <el-button v-if="failed.length && refreshed" type="primary" :loading="working" :disabled="working" @click="run(true)">仅重试失败项</el-button>
        <el-button v-if="!results.length" type="primary" :loading="working" :disabled="working" @click="run(false)">确认{{ action }}</el-button>
      </div>
    </template>
  </el-dialog>
</template>

<style scoped>
.batch-intro { margin-top: 0; line-height: 1.6; }
.batch-notice, .batch-table { margin-top: 14px; }
.batch-summary { margin: 16px 0 0; }
.batch-error { color: var(--danger); }
.batch-footer { display: flex; justify-content: flex-end; flex-wrap: wrap; gap: 8px; }
.batch-footer .el-button + .el-button { margin-left: 0; }
:deep(.el-table .cell) { overflow-wrap: anywhere; }
</style>
