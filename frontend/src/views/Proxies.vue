<script setup>
import { formatDuration } from '../utils/format'
import { formatTime } from '../utils/format'
import ConsoleIcon from '../components/ConsoleIcon.vue'
import { computed, nextTick, onBeforeUnmount, onMounted, ref, toRef, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '../api'
import PageHeader from '../components/PageHeader.vue'
import ResourcePageLayout from '../components/ResourcePageLayout.vue'
import ResourcePageSummary from '../components/ResourcePageSummary.vue'
import ResourceDetailsDialog from '../components/ResourceDetailsDialog.vue'
import ResourceEmptyState from '../components/ResourceEmptyState.vue'
import ResourceMobileList from '../components/ResourceMobileList.vue'
import ResourceBulkStatusDialog from '../components/ResourceBulkStatusDialog.vue'
import { downloadResourceCSV, sortResources } from '../utils/resourceManagement'
import { useResourceFormDialog } from '../utils/resourceFormDialog'
import { proxyCheckPassed, managementFilters, useManagementRead } from '../utils/management'

const { loading, error, read, cancel } = useManagementRead()
const hasLoaded = ref(false)
const proxies = ref([])
const page = ref(1)
const pageSize = toRef(managementFilters.proxies, 'pageSize', 20)
const sortOrder = toRef(managementFilters.proxies, 'sort', '')
const formRef = ref()
const nameInput = ref()
const validating = ref(false)
const detailID = ref('')
const detail = computed(() => proxies.value.find((row) => row.id === detailID.value))
const detailVisible = ref(false)
function showDetail(row) { detailID.value = row.id; detailVisible.value = true }
const router = useRouter()
const table = ref()
const selected = ref([])
const bulkDialog = ref()
const bulkVisible = ref(false)
const busy = computed(() => !!pending.value || !!testing.value || bulkVisible.value)
const search = toRef(managementFilters.proxies, 'search')
const typeFilter = toRef(managementFilters.proxies, 'type')
const statusFilter = toRef(managementFilters.proxies, 'status', '')
const checkFilter = ref('')
const dialogVisible = ref(false)
const editing = ref(null)
const pending = ref('')
const testing = ref('')
const testResult = ref(null)
const testResults = ref({})
const clearPassword = ref(false)
let testController
const form = ref({ name: '', type: 'http', host: '', port: null, username: '', password: '' })
const { capture: captureDraft, close: closeEditor } = useResourceFormDialog({
  readDraft: () => ({ ...form.value, clearPassword: clearPassword.value }), isBusy: () => busy.value || validating.value, hide: () => { dialogVisible.value = false },
})
const filtered = computed(() => {
  const kw = search.value.trim().toLowerCase()
  return proxies.value.filter((p) => (!kw || String(p.name || '').toLowerCase().includes(kw) || String(p.host || '').toLowerCase().includes(kw)) && (!typeFilter.value || p.type === typeFilter.value) && (!statusFilter.value || p.enabled === (statusFilter.value === 'enabled')) && (!checkFilter.value || (checkFilter.value === 'unchecked' ? !testResults.value[p.id] : testResults.value[p.id]?.ok === (checkFilter.value === 'passed'))))
})
const sorted = computed(() => sortResources(filtered.value, sortOrder.value))
const paginated = computed(() => sorted.value.slice((page.value - 1) * pageSize.value, page.value * pageSize.value))
watch([filtered, pageSize], () => { page.value = Math.min(page.value, Math.max(1, Math.ceil(filtered.value.length / pageSize.value))) })
watch([search, typeFilter, statusFilter, checkFilter, pageSize, sortOrder], () => { page.value = 1 })
function clearSelection() { selected.value = []; table.value?.clearSelection() }
function updateSelection(ids) {
  selected.value = ids
  for (const row of paginated.value) table.value?.toggleRowSelection(row, ids.includes(row.id))
}
watch([page, pageSize, sortOrder], clearSelection)
const selectedIDs = computed(() => new Set(selected.value))
const selectedRows = computed(() => filtered.value.filter((row) => selectedIDs.value.has(row.id)))
watch([search, typeFilter, statusFilter, checkFilter], clearSelection)
function resetFilters() { search.value = ''; typeFilter.value = ''; statusFilter.value = ''; checkFilter.value = '' }
function proxyAddress(row) { const host = String(row.host || ''); return `${host.includes(':') && !host.startsWith('[') ? `[${host}]` : host}:${row.port}` }
function clearTestResult(id) { delete testResults.value[id]; if (testResult.value?.id === id) testResult.value = null }
function openBulk(enabled) {
  if (busy.value || !selectedRows.value.length) return
  cancel()
  bulkDialog.value.open(selectedRows.value, enabled)
}
function applyBulkResult(result) {
  const row = proxies.value.find((item) => item.id === result.id)
  if (row) { row.enabled = result.enabled; row.version = result.version }
}
async function load() {
  let loaded = false
  await read((signal) => api.proxies({ signal }), (data) => {
    // Older servers include passwords. Never retain them in display/edit state.
    proxies.value = (data.proxies || []).map(({ password, ...proxy }) => ({ ...proxy, has_password: proxy.has_password ?? Boolean(password) }))
    for (const [id, result] of Object.entries(testResults.value)) {
      if (!proxies.value.some((row) => row.id === id && row.version === result.version)) clearTestResult(id)
    }
    loaded = true
    hasLoaded.value = true
  })
  return loaded
}
const summary = computed(() => [
  { label: '全部代理', value: proxies.value.length, hint: '已配置的网络出口', action: '', active: !statusFilter.value },
  { label: '已启用', value: proxies.value.filter((row) => row.enabled).length, hint: '可作为账号代理出口', action: 'enabled', active: statusFilter.value === 'enabled' },
  { label: '已停用', value: proxies.value.filter((row) => !row.enabled).length, hint: '暂不参与代理选择', action: 'disabled', active: statusFilter.value === 'disabled' },
])
function openCreate() {
  formRef.value?.clearValidate()
  editing.value = null
  form.value = { name: '', type: 'http', host: '', port: null, username: '', password: '' }
  clearPassword.value = false
  error.value = ''
  captureDraft()
  dialogVisible.value = true
}
function openEdit(row) {
  formRef.value?.clearValidate()
  editing.value = row
  form.value = { name: row.name, type: row.type, host: row.host, port: row.port, username: row.username || '', password: '' }
  clearPassword.value = false
  error.value = ''
  captureDraft()
  dialogVisible.value = true
}
async function save() {
  if (busy.value || validating.value) return
  validating.value = true
  let valid = false
  try { valid = await formRef.value?.validate().catch(() => false) } finally { validating.value = false }
  if (!valid) return
  const payload = { name: form.value.name.trim(), type: form.value.type, host: form.value.host.trim(), port: Number(form.value.port), username: form.value.username }
  if (form.value.password && !clearPassword.value) payload.password = form.value.password
  if (editing.value) {
    payload.version = editing.value.version
    if (clearPassword.value) payload.clear_password = true
  }
  cancel()
  pending.value = 'save'
  error.value = ''
  try {
    if (editing.value) await api.proxyUpdate(editing.value.id, payload)
    else await api.proxyCreate(payload)
    if (editing.value) clearTestResult(editing.value.id)
    ElMessage.success(editing.value ? '代理已更新' : '代理已添加')
    dialogVisible.value = false
    form.value.password = ''
    await load()
  } catch (cause) {
    if (cause?.status === 409) {
      dialogVisible.value = false
      await load()
      error.value = '数据已被修改，列表已刷新。请重新打开编辑核对最新内容。'
    } else error.value = cause?.message || '保存失败，请重试'
  } finally { pending.value = '' }
}
async function remove(row, event) {
  if (busy.value) return
  const trigger = event?.currentTarget || document.activeElement
  pending.value = row.id
  try {
    try {
      await ElMessageBox.confirm(`确定删除代理「${row.name}」吗？${Number(row.accounts) || 0} 个绑定账号将解除此代理。`, '删除确认', {
        type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger',
      })
    } catch { return }
    cancel()
    error.value = ''
    await api.proxyDelete(row.id)
    clearTestResult(row.id)
    ElMessage.success('代理已删除')
    await load()
  } catch (cause) {
    error.value = cause?.message || '删除失败，请重试'
  } finally {
    pending.value = ''
    // The confirmation closes while row actions are disabled. Restore focus
    // after Vue has enabled the original control again, including on cancel.
    await nextTick()
    if (trigger instanceof HTMLElement && trigger.isConnected && !trigger.disabled) trigger.focus({ preventScroll: true })
  }
}
async function toggle(row, value) {
  if (busy.value) return
  if (!Number.isSafeInteger(row.version) || row.version <= 0) {
    await load()
    error.value = '缺少有效数据版本，已尝试刷新列表。请核对最新状态后操作。'
    return
  }
  cancel()
  pending.value = row.id
  error.value = ''
  try {
    const result = await api.proxyUpdate(row.id, { enabled: value, version: row.version })
    row.enabled = value
    if (result.version !== undefined) row.version = result.version
    else await load()
    ElMessage.success(value ? '代理已启用' : '代理已停用')
  } catch (cause) {
    if (cause?.status === 409) {
      await load()
      error.value = '代理已被修改，列表已刷新。请核对最新状态后重新操作。'
    } else error.value = cause?.message || '状态更新失败，请重试'
  } finally { pending.value = '' }
}
async function testProxy(row) {
  if (busy.value) return
  testing.value = row.id
  clearTestResult(row.id)
  testResult.value = null
  const controller = new AbortController()
  testController = controller
  try {
    const result = await api.proxyTest({ id: row.id }, { signal: controller.signal })
    if (controller.signal.aborted) return
    const code = Number(result.http_code)
    const passed = proxyCheckPassed(result)
    const message = passed
      ? `检测通过：出口 IP ${result.ip}，延迟 ${formatDuration(result.latency_ms, { empty: '未知' })}`
      : `检测未通过（HTTP ${code || '未知'}）：${result.error || (code < 200 || code >= 300 ? '目标未返回成功状态' : '未取得有效出口 IP')}`
    const states = { ok: '通过', failed: '失败', not_checked: '未检查' }
    const stages = { connection: '连接', authentication: '鉴权', http: '目标 HTTP', exit_ip: '出口 IP', complete: '完成' }
    const details = ['connection', 'authentication', 'http', 'exit_ip']
      .filter((key) => Object.hasOwn(states, result[key]))
      .map((key) => `${stages[key]}：${states[result[key]]}`)
    testResult.value = {
      id: row.id, version: row.version, ok: passed, name: row.name, message, details,
      ip: passed ? result.ip : '', latency: result.latency_ms != null && Number.isFinite(Number(result.latency_ms)) ? formatDuration(result.latency_ms) : '延迟未知',
      stage: stages[result.stage] || result.stage || '', errorCode: result.error_code || '',
      time: result.checked_at ? formatTime(result.checked_at) : `${formatTime(Date.now() / 1000)}（本地接收）`,
    }
    testResults.value[row.id] = testResult.value
    if (passed) ElMessage.success('代理检测通过')
    else ElMessage.error('代理检测未通过，详见检测结果')
  } catch (cause) {
    if (cause?.name !== 'AbortError') {
      testResult.value = { id: row.id, version: row.version, ok: false, name: row.name, message: cause?.message || '检测失败，请重试', details: [], time: `${formatTime(Date.now() / 1000)}（本地接收）` }
      testResults.value[row.id] = testResult.value
    }
  } finally { if (testController === controller) { testing.value = ''; testController = undefined } }
}
function cancelTest() { testController?.abort(); testing.value = ''; testController = undefined; ElMessage.info('已取消检测') }
function validateHost(rule, value, callback) {
  const host = String(value || '').trim()
  if (!host) return callback(new Error('请输入代理服务器 IP 或域名'))
  if (/\s|[\/@?#]/.test(host) || host.split(':').length === 2 && /:\d+$/.test(host)) return callback(new Error('主机只填写 IP 或域名，协议和端口请在对应字段填写'))
  callback()
}
function validatePort(rule, value, callback) {
  callback(Number.isInteger(Number(value)) && Number(value) >= 1 && Number(value) <= 65535 ? undefined : new Error('请输入 1–65535 之间的整数端口'))
}
onMounted(load)
onBeforeUnmount(() => { testController?.abort(); form.value.password = '' })
</script>

<template>
  <ResourcePageLayout v-loading="loading">
    <PageHeader :title="$route.meta.title" :subtitle="$route.meta.subtitle">
      <el-button :disabled="busy" type="primary" :icon="'Plus'" @click="openCreate">添加代理</el-button>
      <el-button aria-label="刷新代理" :disabled="busy" :icon="'Refresh'" circle @click="load" />
    </PageHeader>

    <ResourcePageSummary v-if="hasLoaded" :items="summary" :disabled="busy" @select="(status) => statusFilter = status" />

    <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon class="management-error" />
    <div v-if="testing" class="proxy-testing-notice" role="status"><span>正在检测 {{ proxies.find((row) => row.id === testing)?.name }}…</span><el-button size="small" @click="cancelTest">取消检测</el-button></div>
    <el-alert v-if="testResult" :title="`${testResult.name} · ${testResult.time}`" :type="testResult.ok ? 'success' : 'error'" closable show-icon class="management-error" @close="testResult = null">
      <p>{{ testResult.message }}</p>
      <p v-if="testResult.details.length">{{ testResult.details.join('；') }}</p>
      <p v-if="testResult.stage || testResult.errorCode">阶段：{{ testResult.stage || '—' }}<span v-if="testResult.errorCode"> · {{ testResult.errorCode }}</span></p>
    </el-alert>
    <div class="panel resource-panel">
      <div class="resource-panel-heading">
        <div><h3 class="resource-panel-title">代理节点 <span v-if="hasLoaded" class="table-count">{{ filtered.length }} / {{ proxies.length }}</span></h3><p class="resource-panel-caption">维护网络出口、验证连通性并管理账号绑定</p></div>
        <el-button size="small" :disabled="loading || !filtered.length || busy" @click="downloadResourceCSV('proxies', filtered)">导出筛选结果 CSV</el-button>
      </div>
      <div class="table-toolbar filter-bar">
        <el-input v-model="search" placeholder="搜索名称 / 主机" clearable :prefix-icon="'Search'" aria-label="搜索代理" style="width: 240px; max-width: 100%" />
        <el-select v-model="typeFilter" aria-label="代理类型筛选" placeholder="全部类型" clearable style="width: 140px">
          <el-option label="HTTP" value="http" />
          <el-option label="SOCKS5" value="socks5" />
        </el-select>
        <el-select v-model="statusFilter" aria-label="代理状态筛选" placeholder="全部状态" clearable style="width: 130px">
          <el-option label="启用" value="enabled" /><el-option label="停用" value="disabled" />
        </el-select>
        <el-select v-model="checkFilter" aria-label="代理检测筛选" placeholder="本页检测结果" clearable><el-option label="检测通过" value="passed" /><el-option label="检测未通过" value="failed" /><el-option label="尚未检测" value="unchecked" /></el-select>
        <el-select v-model="sortOrder" aria-label="代理排序" placeholder="默认排序" clearable><el-option label="名称 A → Z" value="name:asc" /><el-option label="绑定账号最多" value="accounts:desc" /></el-select>
        <el-button v-if="search || typeFilter || statusFilter || checkFilter" @click="resetFilters">清除筛选</el-button>
        <div class="spacer" />
      </div>
      <div v-if="selectedRows.length" class="resource-selection" aria-live="polite">
        <span>本页已选 {{ selectedRows.length }} 项</span>
        <el-button size="small" :disabled="busy" @click="openBulk(true)">批量启用</el-button>
        <el-button size="small" type="warning" plain :disabled="busy" @click="openBulk(false)">批量停用</el-button>
        <el-button size="small" :disabled="busy" @click="clearSelection">取消选择</el-button>
      </div>
      <el-table class="resource-desktop-table" ref="table" :data="paginated" stripe row-key="id" :empty-text="proxies.length ? '没有符合筛选条件的代理，请调整或清除筛选' : '暂无代理，点击「添加代理」开始'" @selection-change="(rows) => selected = rows.map((row) => row.id)">
        <el-table-column type="selection" width="46" :selectable="() => !busy" />
        <el-table-column label="名称" min-width="150"><template #default="{ row }"><div class="resource-identity"><span class="resource-identity-mark is-amber" aria-hidden="true"><ConsoleIcon name="globe" :size="15" /></span><div class="resource-identity-copy"><button class="resource-name" :disabled="busy" @click="showDetail(row)">{{ row.name }}</button><span class="resource-id-label" :title="String(row.id)"># {{ row.id }}</span></div></div></template></el-table-column>
        <el-table-column label="类型" width="90" align="center">
          <template #default="{ row }">
            <span class="resource-protocol">{{ String(row.type || '').toUpperCase() }}</span>
          </template>
        </el-table-column>
        <el-table-column label="地址" min-width="170">
          <template #default="{ row }"><span class="mono">{{ proxyAddress(row) }}</span></template>
        </el-table-column>
        <el-table-column label="认证" width="90" align="center">
          <template #default="{ row }">
            <span class="proxy-auth-state"><el-icon v-if="row.username || row.has_password"><Lock /></el-icon>{{ row.username || row.has_password ? '已配置' : '无认证' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="110" align="center">
          <template #default="{ row }">
            <div class="resource-switch-cell"><el-switch :model-value="row.enabled" :disabled="busy" :loading="pending === row.id" :aria-label="`${row.name} 启用状态`" @change="(value) => toggle(row, value)" /><span class="resource-state" :class="{ 'is-enabled': row.enabled }">{{ row.enabled ? '已启用' : '已停用' }}</span></div>
          </template>
        </el-table-column>
        <el-table-column label="绑定账号" width="100" align="center"><template #default="{ row }"><el-button link type="primary" :disabled="busy" class="resource-association-count" :aria-label="`查看 ${row.name} 的绑定账号`" @click="router.push({ name: 'accounts', query: { proxy_id: row.id } })">{{ row.accounts || 0 }}</el-button></template></el-table-column>
        <el-table-column label="本页检测" min-width="170"><template #default="{ row }"><span v-if="testing === row.id" class="resource-row-sub">正在检测…</span><template v-else-if="testResults[row.id]"><span class="resource-state" :class="testResults[row.id].ok ? 'is-enabled' : 'is-error'">{{ testResults[row.id].ok ? testResults[row.id].latency : '检测未通过' }}</span><span class="resource-row-sub">{{ testResults[row.id].time }}</span></template><span v-else class="resource-row-sub">尚未检测</span></template></el-table-column>
        <el-table-column label="操作" fixed="right" width="180" align="right">
          <template #default="{ row }">
            <el-button link type="primary" size="small" :disabled="busy" :loading="testing === row.id" @click="testProxy(row)">测试</el-button>
            <el-button link type="primary" size="small" :disabled="busy" @click="openEdit(row)">编辑</el-button>
            <el-button link type="danger" size="small" :disabled="busy" :loading="pending === row.id" @click="remove(row, $event)">删除</el-button>
          </template>
        </el-table-column>
        <template #empty><ResourceEmptyState :loading="loading" :error="error" @retry="load" :filtered="!!proxies.length" kind="代理" hint="配置 HTTP 或 SOCKS5 网络出口，测试后关联账号。" action="添加第一个代理" :disabled="busy" @create="openCreate" @reset="resetFilters" /></template>
      </el-table>
      <ResourceMobileList :items="paginated" selectable :selected="selected" :disabled="busy" @select="updateSelection" @detail="showDetail">
        <template #status="{ row }"><span class="resource-state" :class="{ 'is-enabled': row.enabled }">{{ row.enabled ? '已启用' : '已停用' }}</span></template>
        <template #default="{ row }">
          <span class="resource-protocol">{{ String(row.type || '').toUpperCase() }}</span> <code class="mono">{{ proxyAddress(row) }}</code>
          <dl><div><dt>连接认证</dt><dd>{{ row.username || row.has_password ? '已配置认证' : '无需认证' }}</dd></div><div><dt>绑定账号</dt><dd><el-button link type="primary" :disabled="busy" class="resource-association-count" :aria-label="`查看 ${row.name} 的绑定账号`" @click="router.push({ name: 'accounts', query: { proxy_id: row.id } })">{{ row.accounts || 0 }} 个账号</el-button></dd></div></dl>
          <div class="proxy-card-test"><span v-if="testing === row.id">正在检测…</span><template v-else-if="testResults[row.id]"><span class="resource-state" :class="testResults[row.id].ok ? 'is-enabled' : 'is-error'">{{ testResults[row.id].ok ? `${testResults[row.id].ip} · ${testResults[row.id].latency}` : '检测未通过' }}</span><span class="resource-row-sub">{{ testResults[row.id].time }}</span></template><span v-else>尚未检测 · 可测试连通性与出口 IP</span></div>
        </template>
        <template #actions="{ row }"><el-switch :model-value="row.enabled" :disabled="busy" :loading="pending === row.id" :aria-label="`${row.name} 启用状态`" @change="(value) => toggle(row, value)" /><span class="spacer" /><el-button size="small" :disabled="busy" :loading="testing === row.id" @click="testProxy(row)">测试</el-button><el-button size="small" :disabled="busy" @click="openEdit(row)">编辑</el-button><el-button size="small" text type="danger" :disabled="busy" :loading="pending === row.id" @click="remove(row, $event)">删除</el-button></template>
        <template #empty><ResourceEmptyState :loading="loading" :error="error" @retry="load" :filtered="!!proxies.length" kind="代理" hint="配置 HTTP 或 SOCKS5 网络出口，测试后关联账号。" action="添加第一个代理" :disabled="busy" @create="openCreate" @reset="resetFilters" /></template>
      </ResourceMobileList>
      <div class="resource-table-footer">
        <div class="resource-page-size"><span>每页显示</span><el-select v-model="pageSize" aria-label="代理每页条数" size="small"><el-option v-for="size in [10, 20, 50, 100]" :key="size" :label="String(size)" :value="size" /></el-select><span>条 · 共 {{ filtered.length }} 条</span></div>
        <el-pagination v-model:current-page="page" :page-size="pageSize" :total="filtered.length" :pager-count="5" layout="prev, pager, next" small />
      </div>
      <p class="resource-panel-note">CSV 不包含代理地址、认证用户名或密码。点击代理名称可查看连接配置和账号关联。</p>
    </div>

    <ResourceBulkStatusDialog ref="bulkDialog" resource-label="代理" :resources="proxies" :update="api.proxyUpdate" :reload="load" @visibility="(value) => bulkVisible = value" @result="applyBulkResult" />
    <ResourceDetailsDialog v-if="detail" v-model="detailVisible" :title="detail.name" kind="代理" :fields="[{ label: '协议类型', value: String(detail.type || '').toUpperCase() }, { label: '代理地址', value: proxyAddress(detail), mono: true }, { label: '连接认证', value: detail.username || detail.has_password ? '已配置认证' : '无需认证' }, { label: '绑定账号', value: String(detail.accounts || 0) }]">
      <template #status><el-tag :type="detail.enabled ? 'success' : 'info'" size="small">{{ detail.enabled ? '已启用' : '已停用' }}</el-tag></template>
      <div v-if="testResults[detail.id]" class="proxy-detail-test"><strong :class="testResults[detail.id].ok ? 'is-passed' : 'is-failed'">{{ testResults[detail.id].ok ? '本次检测通过' : '本次检测未通过' }}</strong><p>{{ testResults[detail.id].message }}</p><p v-if="testResults[detail.id].details.length">{{ testResults[detail.id].details.join('；') }}</p><span class="resource-row-sub">检测时间：{{ testResults[detail.id].time }}</span></div>
      <p v-else>本页尚未检测此代理。测试可查看连通性、出口 IP 和延迟。</p>
      <el-button link type="primary" @click="detailVisible = false; router.push({ name: 'accounts', query: { proxy_id: detail.id } })">查看绑定账号</el-button>
      <template #actions><el-button v-if="testing === detail.id" @click="cancelTest">取消检测</el-button><el-button v-else :disabled="busy" @click="testProxy(detail)">测试连通性</el-button><el-button type="primary" :disabled="busy" @click="detailVisible = false; openEdit(detail)">编辑代理</el-button></template>
    </ResourceDetailsDialog>
    <el-dialog v-model="dialogVisible" :title="editing ? '编辑代理' : '添加代理'" width="min(560px, calc(100vw - 24px))" class="detail-dialog resource-form-dialog" align-center append-to-body :close-on-click-modal="false" :before-close="closeEditor" @opened="nameInput?.focus()" :close-on-press-escape="!pending" :show-close="!pending" @closed="form.password = ''">
      <p class="dialog-intro">添加网络出口后，可先测试连通性，再从账号管理中绑定使用。</p>
      <el-alert v-if="error" :title="error" type="error" :closable="false" class="management-error" />
      <el-form ref="formRef" :model="form" label-position="top" @submit.prevent="save" :disabled="busy" scroll-to-error>
        <div class="dialog-section-title">节点信息</div>
        <el-form-item label="名称" prop="name" :rules="[{ required: true, whitespace: true, message: '请输入名称', trigger: 'blur' }]">
          <el-input ref="nameInput" aria-label="名称" v-model="form.name" maxlength="60" placeholder="例如：US-01" />
        </el-form-item>
        <el-form-item label="类型" required>
          <el-radio-group v-model="form.type">
            <el-radio-button value="http">HTTP</el-radio-button>
            <el-radio-button value="socks5">SOCKS5</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item label="主机" prop="host" required :rules="[{ validator: validateHost, trigger: 'blur' }]">
          <el-input aria-label="主机" v-model="form.host" placeholder="代理服务器 IP 或域名" />
        </el-form-item>
        <el-form-item label="端口" prop="port" required :rules="[{ validator: validatePort, trigger: 'blur' }]">
          <el-input-number aria-label="端口" v-model="form.port" :min="1" :max="65535" :step="1" :precision="0" controls-position="right" placeholder="例如：8080" style="width: 100%" />
        </el-form-item>
        <div class="dialog-section-title">连接认证 <span class="dialog-hint">选填</span></div>
        <el-form-item label="用户名">
          <el-input aria-label="用户名" v-model="form.username" placeholder="可选" />
        </el-form-item>
        <el-form-item label="密码">
          <el-input aria-label="密码" v-model="form.password" type="password" show-password :disabled="clearPassword" autocomplete="new-password" :placeholder="editing ? '留空保持原密码' : '可选'" />
        </el-form-item>
        <el-form-item v-if="editing" label="清除密码"><el-checkbox v-model="clearPassword">明确清除已保存的密码</el-checkbox></el-form-item>
      </el-form>
      <template #footer>
        <el-button :disabled="busy" @click="closeEditor">取消</el-button>
        <el-button type="primary" :loading="pending === 'save'" :disabled="busy" @click="save">保存</el-button>
      </template>
    </el-dialog>
  </ResourcePageLayout>
</template>

<style scoped>
.resource-selection { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; padding: 0 18px 14px; }
.resource-selection .el-button + .el-button { margin-left: 0; }
.management-error { margin-bottom: 16px; overflow-wrap: anywhere; }
.proxy-testing-notice { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 12px 16px; margin-bottom: 16px; border: 1px solid var(--card-border); border-radius: 8px; background: var(--primary-soft); color: var(--text-2); }
.proxy-auth-state { display: inline-flex; gap: 4px; align-items: center; font-size: 12px; color: var(--text-3); }
.proxy-card-test { margin-top: 10px; padding-top: 8px; border-top: 1px dashed var(--card-border); color: var(--text-3); }
.proxy-detail-test { padding: 12px; border: 1px solid var(--card-border); border-radius: 8px; background: var(--el-fill-color-light); }
.proxy-detail-test p { margin: 8px 0; overflow-wrap: anywhere; }
.proxy-detail-test .is-passed { color: var(--success); }
.proxy-detail-test .is-failed { color: var(--danger); }
:deep(.el-table .cell) { overflow-wrap: anywhere; }
</style>
