<script setup>
import { formatDate } from '../utils/format'
import { computed, ref, watch } from 'vue'
import { ElMessageBox } from 'element-plus'
import { accountManagement, downloadAccountJSON } from '../api/accountManagement'
import { accountSelectionTarget } from '../utils/accountManagement'
import { validLeaseManagement } from '../utils/accountLeaseBackup.js'
import { accountBundleInput } from '../utils/accountBundleInput.js'

const props = defineProps({ modelValue: Boolean, selection: { type: Array, default: () => [] }, filters: { type: Object, default: () => ({}) }, total: Number })
const emit = defineEmits(['update:modelValue'])
const visible = computed({ get: () => props.modelValue, set: (value) => { if (!loading.value) emit('update:modelValue', value) } })
const mode = ref('selected')
const credentials = ref(false)
const resources = ref(false), proxyCredentials = ref(false)
const leases = ref(false)
const loading = ref(false)
const error = ref('')
const done = ref('')
const items = ref([])
const filters = ref({})
const total = ref(0)
watch(() => props.modelValue, (value) => {
  if (!value) return
  items.value = props.selection.map(({ id, version }) => ({ id, version }))
  filters.value = { ...props.filters }
  total.value = props.total
  mode.value = items.value.length ? 'selected' : 'filtered'
  credentials.value = false
  resources.value = false; proxyCredentials.value = false
  leases.value = false
  error.value = ''; done.value = ''
})
watch(credentials, value => { if (!value) { resources.value = false; proxyCredentials.value = false; leases.value = false } })
watch(resources, value => { if (!value) proxyCredentials.value = false })
async function download() {
  if (loading.value || mode.value === 'selected' && !items.value.length) return
  error.value = ''; done.value = ''
  const includeCredentials = credentials.value
  const includeResources = includeCredentials && resources.value
  const includeProxyCredentials = includeResources && proxyCredentials.value
  const includeLeases = includeCredentials && leases.value
  if (includeCredentials) {
    try { await ElMessageBox.confirm('备份包含可登录账号的访问与刷新令牌，请妥善保存下载文件。确认导出当前选定范围的凭据？', '导出可迁移凭据', { type: 'warning', confirmButtonText: '确认导出凭据', cancelButtonText: '取消' }) }
    catch { return }
  }
  if (includeProxyCredentials) {
    try { await ElMessageBox.confirm('此文件还将包含关联代理的用户名和密码。确认将这些代理登录凭据一并写入下载文件？', '导出代理登录凭据', { type: 'warning', confirmButtonText: '确认包含代理密码', cancelButtonText: '取消' }) }
    catch { return }
  }
  loading.value = true
  try {
    const data = accountBundleInput(await accountManagement.export({ ...accountSelectionTarget(items.value, filters.value, mode.value), ...(includeResources ? { include_resources: true } : {}), ...(includeProxyCredentials ? { include_proxy_credentials: true, confirm_proxy_credentials: true } : {}), ...(includeLeases ? { include_leases: true } : {}) }, includeCredentials))
    if (!data || !Array.isArray(data.accounts)) throw new Error('服务器返回的导出数据不完整')
    if (includeCredentials && data.type !== 'codex-agent-accounts') throw new Error('服务器返回的备份格式不正确，已阻止下载')
    if (includeResources && (!data.resources || data.resources.version !== 1 || !Array.isArray(data.resources.groups) || !Array.isArray(data.resources.proxies))) throw new Error('服务器未返回完整关联资源，已阻止下载')
    if (includeLeases && (!data.management || !validLeaseManagement(data.management, data.accounts.length))) throw new Error('服务器未返回有效租期备份段，已阻止下载')
    downloadAccountJSON(data, `codex-accounts-${includeCredentials ? 'credentials' : 'metadata'}-${formatDate(Date.now() / 1000)}.json`)
    done.value = `已生成 ${data.accounts.length} 个账号的${includeCredentials ? '凭据备份' : '元数据清单'}。`
    credentials.value = false
  } catch (caught) { error.value = caught.status === 409 ? '账号在选择后已修改，请关闭窗口并重新选择，再导出。' : caught.message || '导出失败，请重试' }
  finally { loading.value = false }
}
</script>

<template>
  <el-dialog v-model="visible" title="导出账号" width="min(520px, calc(100vw - 32px))" :close-on-click-modal="false" :close-on-press-escape="!loading" :show-close="!loading" align-center append-to-body destroy-on-close class="detail-dialog">
    <el-alert v-if="error" :title="error" type="error" :closable="false" class="feedback" />
    <el-alert v-if="done" :title="done" type="success" :closable="false" class="feedback" />
    <label class="export-label">导出范围</label>
    <el-select v-model="mode" aria-label="账号导出范围" class="full-width" :disabled="loading"><el-option :label="`已选账号（${items.length}）`" value="selected" :disabled="!items.length" /><el-option :label="`当前筛选的全部账号（${total ?? '—'}）`" value="filtered" /></el-select>
    <p class="muted">筛选范围已固定为打开窗口时的条件。默认清单包含账号配置与统计，不含登录凭据。</p>
    <el-checkbox v-model="credentials" :disabled="loading">导出可迁移凭据备份</el-checkbox>
    <p v-if="credentials" class="credential-note">包含敏感访问和刷新令牌，可用“恢复备份”导入。元数据清单不能恢复账号登录。</p>
    <div v-if="credentials" class="resource-options"><el-checkbox v-model="leases" :disabled="loading">包含账号独立租期配置</el-checkbox><p v-if="leases" class="muted">仅备份租期截止、到期停用和延期恢复策略，不含令牌到期或原实例的停用所有权。恢复账号后须独立确认租期。</p></div>
    <div v-if="credentials" class="resource-options"><el-checkbox v-model="resources" :disabled="loading">包含所选账号关联的分组与代理定义</el-checkbox><el-checkbox v-if="resources" v-model="proxyCredentials" :disabled="loading">同时包含关联代理的用户名和密码</el-checkbox><p class="muted">未包含资源定义时，跨实例需先创建目标资源再映射。仅导出定义但不含密码的认证代理需要映射到本地现有代理，不能直接还原认证。</p></div>
    <template #footer><el-button :disabled="loading" @click="visible = false">关闭</el-button><el-button type="primary" :loading="loading" :disabled="mode === 'selected' && !items.length" @click="download">生成并下载</el-button></template>
  </el-dialog>
</template>

<style scoped>.resource-options .el-checkbox { display: flex; white-space: normal; height: auto; margin: 12px 0; }.resource-options :deep(.el-checkbox__label) { white-space: normal; line-height: 1.7; }.full-width { width: 100%; }.export-label { display: block; margin: 0 0 8px; font-size: 13px; }.muted, .credential-note { font-size: 12px; line-height: 1.7; color: var(--text-3); }.credential-note { color: var(--warning); }.feedback { margin-bottom: 14px; }</style>
