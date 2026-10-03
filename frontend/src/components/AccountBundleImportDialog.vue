<script setup>
import { computed, ref, watch } from 'vue'
import { accountManagement } from '../api/accountManagement'
import { redactImportMessage } from '../utils/importAccounts'
import AccountResourceRestorePanel from './AccountResourceRestorePanel.vue'
import AccountLeaseRestorePanel from './AccountLeaseRestorePanel.vue'
import { validLeaseManagement, validLeaseRestore } from '../utils/accountLeaseBackup.js'
import { accountBundleInput } from '../utils/accountBundleInput.js'

const props = defineProps({ modelValue: Boolean, groups: { type: Array, default: () => [] }, proxies: { type: Array, default: () => [] } })
const emit = defineEmits(['update:modelValue', 'busy', 'updated'])
const loading = ref(false)
const resourcesBusy = ref(false)
const leasesBusy = ref(false), leaseReceipt = ref(null)
const reading = ref(false)
const visible = computed({ get: () => props.modelValue, set: (value) => { if (!loading.value && !resourcesBusy.value && !leasesBusy.value) emit('update:modelValue', value) } })
const bundle = ref(null)
const filename = ref('')
const replace = ref(false)
const groupMapping = ref({})
const proxyMapping = ref({})
const results = ref([])
const previewed = ref(false)
const submitted = ref(false)
const error = ref('')
let readGeneration = 0
const groupIDs = computed(() => [...new Set((bundle.value?.accounts || []).flatMap((account) => account.groups || []))])
const proxyIDs = computed(() => [...new Set((bundle.value?.accounts || []).flatMap((account) => account.proxies || []))])
const hasFailures = computed(() => results.value.some((result) => result.status === 'failed'))
const stateNames = { added: '新增', replaced: '替换凭据', duplicate: '重复跳过', failed: '失败' }
watch([loading, resourcesBusy, leasesBusy], ([value, resource, lease]) => emit('busy', value || resource || lease))
watch([replace, groupMapping, proxyMapping], () => { previewed.value = false; submitted.value = false; results.value = []; leaseReceipt.value = null }, { deep: true })
watch(() => props.modelValue, (value) => { if (!value) clear() })
function clear() { readGeneration++; bundle.value = null; filename.value = ''; groupMapping.value = {}; proxyMapping.value = {}; results.value = []; previewed.value = false; submitted.value = false; error.value = ''; replace.value = false; reading.value = false; leaseReceipt.value = null; leasesBusy.value = false }
async function readFile(event) {
  const file = event.target.files?.[0]
  event.target.value = ''
  if (!file || loading.value || resourcesBusy.value || leasesBusy.value) return
  clear()
  if (file.size > 32 * 1024 * 1024) { error.value = '备份文件不能超过 32 MB'; return }
  const generation = ++readGeneration
  reading.value = true
  try {
    const data = accountBundleInput(JSON.parse(await file.text()))
    if (generation !== readGeneration || !visible.value) return
    if (data?.type !== 'codex-agent-accounts' || data?.version !== 1 || !Array.isArray(data.accounts)) throw new Error('请选择 Codex 可迁移凭据备份（version 1）；元数据清单不能恢复登录')
    if (!data.accounts.length) throw new Error('备份中没有账号')
    if (data.resources && (data.resources.version !== 1 || !Array.isArray(data.resources.groups) || !Array.isArray(data.resources.proxies))) throw new Error('备份中的关联资源格式无效')
    if (!validLeaseManagement(data.management, data.accounts.length)) throw new Error('备份租期配置无效或包含旧实例运行状态，不可恢复')
    bundle.value = data
    filename.value = file.name
    groupMapping.value = Object.fromEntries(groupIDs.value.map((id) => [id, props.groups.some((group) => group.id === id) ? id : '']))
    proxyMapping.value = Object.fromEntries(proxyIDs.value.map((id) => [id, props.proxies.some((proxy) => proxy.id === id) ? id : '']))
  } catch (caught) { error.value = caught instanceof SyntaxError ? '文件不是有效 JSON' : caught.message || '文件读取失败' }
  finally { if (generation === readGeneration) reading.value = false }
}
async function run(dryRun) {
  if (!bundle.value || loading.value || resourcesBusy.value || leasesBusy.value || !dryRun && (!previewed.value || submitted.value)) return
  error.value = ''
  loading.value = true
  const mappings = (value) => Object.fromEntries(Object.entries(value).filter(([, id]) => id))
  try {
    const result = await accountManagement.importBundle({ data: bundle.value, dry_run: dryRun, replace: replace.value, group_mapping: mappings(groupMapping.value), proxy_mapping: mappings(proxyMapping.value) })
    if (!Array.isArray(result?.results) || result.results.length !== bundle.value.accounts.length || result.dry_run !== dryRun) throw new Error('服务器返回的恢复结果不完整，请刷新核对')
    const rows = bundle.value.accounts.map((account) => ({ payload: account }))
    results.value = result.results.map((row) => ({ index: row.index, name: row.name, status: row.status, message: redactImportMessage(row.error || row.code || (row.warnings || []).join('；'), rows) }))
    previewed.value = dryRun
    submitted.value = !dryRun
    if (!dryRun) emit('updated')
    if (bundle.value.management?.leases?.length) {
      if (!validLeaseRestore(result.lease_restore, bundle.value.management.leases.length)) { leaseReceipt.value = null; error.value = '账号结果已返回，但租期回执不完整；请核对账号，租期恢复未确认。' }
      else leaseReceipt.value = result.lease_restore
    }
  } catch (caught) { error.value = redactImportMessage(caught.message || '恢复备份失败', bundle.value.accounts.map((account) => ({ payload: account }))); previewed.value = false }
  finally { loading.value = false }
}
function applyResourceMappings(value) { groupMapping.value = { ...groupMapping.value, ...value.groups }; proxyMapping.value = { ...proxyMapping.value, ...value.proxies } }
</script>

<template>
  <el-dialog v-model="visible" title="恢复账号备份" width="min(720px, calc(100vw - 32px))" :close-on-click-modal="false" :close-on-press-escape="!loading && !resourcesBusy && !leasesBusy" :show-close="!loading && !resourcesBusy && !leasesBusy" @closed="clear" align-center append-to-body destroy-on-close class="detail-dialog">
    <p class="intro">恢复本系统导出的账号、关联资源和租期配置。普通 auth.json / Sub2API 文件请使用“导入账号”。</p>
    <el-alert v-if="error" :title="error" type="error" :closable="false" class="feedback" />
    <label class="backup-file">选择备份文件 <input type="file" accept=".json,application/json" :disabled="loading || reading || resourcesBusy || leasesBusy" aria-label="账号备份文件" @change="readFile" /></label>
    <p v-if="reading" role="status">正在读取文件…</p>
    <template v-if="bundle">
      <p class="bundle-summary">{{ filename }} · {{ bundle.accounts.length }} 个账号</p>
      <p v-if="bundle.management?.leases?.length" class="muted">含 {{ bundle.management.leases.length }} 项独立租期配置；本次确认恢复账号不会自动应用租期，之后需单独预览并确认。</p>
      <el-checkbox v-model="replace" :disabled="loading || submitted || resourcesBusy">允许替换同名账号的凭据</el-checkbox>
      <p class="muted">新增账号恢复原配置与统计窗口；同名替换保留本地已有管理配置。原文件中的关联 ID 不会自动匹配到其他资源。若备份包含资源定义，可先按下面第一步恢复；否则请先创建本地分组和代理，再映射到对应资源。</p>
      <div v-if="groupIDs.length || proxyIDs.length" class="mapping-list">
        <h4>资源关联映射</h4>
        <label v-for="id in groupIDs" :key="`group-${id}`"><span>原分组 <code>{{ id }}</code></span><el-select v-model="groupMapping[id]" :disabled="loading || submitted || resourcesBusy" clearable placeholder="选择本地分组" :aria-label="`备份分组映射 ${id}`"><el-option v-for="group in groups" :key="group.id" :label="group.name" :value="group.id" /></el-select></label>
        <label v-for="id in proxyIDs" :key="`proxy-${id}`"><span>原代理 <code>{{ id }}</code></span><el-select v-model="proxyMapping[id]" :disabled="loading || submitted || resourcesBusy" clearable placeholder="选择本地代理" :aria-label="`备份代理映射 ${id}`"><el-option v-for="proxy in proxies" :key="proxy.id" :label="proxy.name" :value="proxy.id" /></el-select></label>
      </div>
      <AccountResourceRestorePanel v-if="bundle.resources" :resources="bundle.resources" :group-mapping="groupMapping" :proxy-mapping="proxyMapping" :disabled="loading || submitted" @busy="resourcesBusy = $event" @mapping="applyResourceMappings" @updated="emit('updated')" />
      <el-alert v-if="previewed" :title="hasFailures ? '预览包含失败项，请先修正资源关联或文件。成功项可单独执行恢复。' : '预览完成，尚未写入账号。'" :type="hasFailures ? 'warning' : 'success'" :closable="false" class="feedback" />
      <el-alert v-if="submitted" title="恢复已执行，请核对逐项结果。关闭窗口后可重新导入失败项。" type="info" :closable="false" class="feedback" />
      <el-table v-if="results.length" :data="results" size="small" max-height="300"><el-table-column prop="index" label="#" width="50" /><el-table-column prop="name" label="账号" min-width="140" /><el-table-column label="结果" width="100"><template #default="{ row }">{{ stateNames[row.status] || '未知' }}</template></el-table-column><el-table-column prop="message" label="说明" min-width="180" /></el-table>
      <AccountLeaseRestorePanel v-if="leaseReceipt" :receipt="leaseReceipt" :disabled="loading || resourcesBusy" @busy="leasesBusy = $event" @updated="emit('updated')" />
    </template>
    <template #footer><el-button :disabled="loading || resourcesBusy || leasesBusy" @click="visible = false">关闭</el-button><el-button :disabled="!bundle || submitted || loading || resourcesBusy || leasesBusy" @click="run(true)">生成预览</el-button><el-button type="primary" :loading="loading" :disabled="!previewed || submitted || resourcesBusy || leasesBusy" @click="run(false)">确认恢复</el-button></template>
  </el-dialog>
</template>

<style scoped>.intro { margin-top: 0; line-height: 1.7; }.backup-file { display: flex; gap: 10px; flex-wrap: wrap; margin: 18px 0; font-size: 13px; }.backup-file input { min-width: 0; max-width: 100%; }.bundle-summary { font-size: 13px; overflow-wrap: anywhere; }.muted { color: var(--text-3); font-size: 12px; line-height: 1.7; }.mapping-list { margin: 18px 0; }.mapping-list h4 { font-size: 13px; }.mapping-list label { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); align-items: center; gap: 10px; margin: 10px 0; font-size: 12px; }.mapping-list code { overflow-wrap: anywhere; }.mapping-list .el-select { width: 100%; }.feedback { margin: 14px 0; }:deep(.el-table .cell) { overflow-wrap: anywhere; }@media(max-width: 500px) { .mapping-list label { grid-template-columns: 1fr; } }</style>
