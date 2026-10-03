<script setup>
import { formatTime as formatTimestamp, formatNumber } from '../utils/format'
import ConsoleIcon from '../components/ConsoleIcon.vue'
import { computed, nextTick, onBeforeUnmount, onMounted, ref, toRef, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '../api'
import PageHeader from '../components/PageHeader.vue'
import ResourcePageLayout from '../components/ResourcePageLayout.vue'
import ResourcePageSummary from '../components/ResourcePageSummary.vue'
import ResourceDetailsDialog from '../components/ResourceDetailsDialog.vue'
import ResourceEmptyState from '../components/ResourceEmptyState.vue'
import ResourceMobileList from '../components/ResourceMobileList.vue'
import CopyTextDialog from '../components/CopyTextDialog.vue'
import ResourceBulkStatusDialog from '../components/ResourceBulkStatusDialog.vue'
import { downloadResourceCSV, indexResources, sortResources } from '../utils/resourceManagement'
import { useResourceFormDialog } from '../utils/resourceFormDialog'
import { managementFilters, useManagementRead } from '../utils/management'

const { loading, error, read, cancel } = useManagementRead()
const hasLoaded = ref(false)
const keys = ref([])
const page = ref(1)
const pageSize = toRef(managementFilters.keys, 'pageSize', 20)
const sortOrder = toRef(managementFilters.keys, 'sort', '')
const formRef = ref()
const nameInput = ref()
const validating = ref(false)
const detailID = ref('')
const detail = computed(() => keys.value.find((row) => row.id === detailID.value))
const detailVisible = ref(false)
function showDetail(row) { detailID.value = row.id; detailVisible.value = true }
const route = useRoute()
const router = useRouter()
const table = ref()
const selected = ref([])
const bulkDialog = ref()
const bulkVisible = ref(false)
const busy = computed(() => !!pending.value || bulkVisible.value)
const groups = ref([])
const search = toRef(managementFilters.keys, 'search')
const groupFilter = toRef(managementFilters.keys, 'group')
const statusFilter = toRef(managementFilters.keys, 'status', '')
watch(() => route.query.group_id, (id) => { if (route.name === 'keys' && typeof id === 'string') { groupFilter.value = id; search.value = '' } }, { immediate: true })
watch(groupFilter, (value) => {
  if (route.name === 'keys' && route.query.group_id !== undefined && route.query.group_id !== value) router.replace({ query: { ...route.query, group_id: value || undefined } })
})
const dialogVisible = ref(false)
const editing = ref(null)
const form = ref({ name: '', groups: [], enabled: true })
const pending = ref('')
const createdKey = ref('')
const createdVisible = ref(false)
const copying = ref(false)
const copyVisible = ref(false)
const copyText = ref('')
const { capture: captureDraft, close: closeEditor } = useResourceFormDialog({
  readDraft: () => (form.value), isBusy: () => busy.value || validating.value, hide: () => { dialogVisible.value = false },
})
const filtered = computed(() => {
  const kw = search.value.trim().toLowerCase()
  return keys.value.filter((k) => (!kw || String(k.name || '').toLowerCase().includes(kw) || String(k.key_masked || '').toLowerCase().includes(kw)) && (!groupFilter.value || (k.groups || []).includes(groupFilter.value)) && (!statusFilter.value || k.enabled === (statusFilter.value === 'enabled')))
})
const sorted = computed(() => sortResources(filtered.value, sortOrder.value))
const paginated = computed(() => sorted.value.slice((page.value - 1) * pageSize.value, page.value * pageSize.value))
watch([filtered, pageSize], () => { page.value = Math.min(page.value, Math.max(1, Math.ceil(filtered.value.length / pageSize.value))) })
watch([search, groupFilter, statusFilter, pageSize, sortOrder], () => { page.value = 1 })
function clearSelection() { selected.value = []; table.value?.clearSelection() }
function updateSelection(ids) {
  selected.value = ids
  for (const row of paginated.value) table.value?.toggleRowSelection(row, ids.includes(row.id))
}
watch([page, pageSize, sortOrder], clearSelection)
const selectedIDs = computed(() => new Set(selected.value))
const selectedRows = computed(() => filtered.value.filter((row) => selectedIDs.value.has(row.id)))
watch([search, groupFilter, statusFilter], clearSelection)
function resetFilters() { search.value = ''; groupFilter.value = ''; statusFilter.value = '' }
function openBulk(enabled) {
  if (busy.value || !selectedRows.value.length) return
  cancel()
  bulkDialog.value.open(selectedRows.value, enabled)
}
function applyBulkResult(result) {
  const row = keys.value.find((item) => item.id === result.id)
  if (row) { row.enabled = result.enabled; row.version = result.version }
}
async function load() {
  let loaded = false
  await read((signal) => Promise.all([api.keys({ signal }), api.groups({ signal })]), ([keyData, groupData]) => {
    keys.value = (keyData.keys || []).map(({ key, ...row }) => row)
    groups.value = groupData.groups || []
    loaded = true
    hasLoaded.value = true
  })
  return loaded
}
const summary = computed(() => [
  { label: '全部密钥', value: keys.value.length, hint: '应用独立凭据', action: '', active: !statusFilter.value },
  { label: '已启用', value: keys.value.filter((row) => row.enabled).length, hint: '允许应用发起请求', action: 'enabled', active: statusFilter.value === 'enabled' },
  { label: '已停用', value: keys.value.filter((row) => !row.enabled).length, hint: '已暂停访问权限', action: 'disabled', active: statusFilter.value === 'disabled' },
])
function openCreate() {
  formRef.value?.clearValidate()
  editing.value = null
  form.value = { name: '', groups: groups.value.some((group) => group.id === groupFilter.value) ? [groupFilter.value] : [], enabled: true }
  error.value = ''
  captureDraft()
  dialogVisible.value = true
}
function openEdit(row) {
  formRef.value?.clearValidate()
  editing.value = row
  form.value = { name: row.name, groups: [...(row.groups || [])], enabled: row.enabled }
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
  cancel()
  pending.value = 'save'
  error.value = ''
  try {
    if (editing.value) {
      await api.keyUpdate(editing.value.id, { name: form.value.name.trim(), groups: form.value.groups, enabled: form.value.enabled, version: editing.value.version })
      ElMessage.success('密钥已更新')
    } else {
      const created = await api.keyCreate({ name: form.value.name.trim(), groups: form.value.groups })
      createdKey.value = typeof created.key === 'string' ? created.key : ''
      if (!createdKey.value) {
        error.value = '服务端未返回完整 key，请刷新列表核对结果后再操作。'
        dialogVisible.value = false
        return
      }
      createdVisible.value = true
      ElMessage.success('密钥已创建')
    }
    dialogVisible.value = false
    await load()
  } catch (cause) {
    if (cause?.status === 409) {
      dialogVisible.value = false
      await load()
      error.value = '密钥已被修改，列表已刷新。请重新打开编辑核对最新内容。'
    } else error.value = cause?.message || '保存失败，请重试'
  } finally { pending.value = '' }
}
async function remove(row, event) {
  if (busy.value) return
  const trigger = event?.currentTarget || document.activeElement
  pending.value = row.id
  try {
    try {
      await ElMessageBox.confirm(`确定删除密钥「${row.name}」吗？使用该 key 的调用将立即失效。`, '删除确认', {
        type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger',
      })
    } catch { return }
    cancel()
    error.value = ''
    await api.keyDelete(row.id)
    ElMessage.success('密钥已删除')
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
    const result = await api.keyUpdate(row.id, { enabled: value, version: row.version })
    row.enabled = value
    if (result.version !== undefined) row.version = result.version
    else await load()
    ElMessage.success(value ? '已启用' : '已停用')
  } catch (cause) {
    if (cause?.status === 409) {
      await load()
      error.value = '密钥已被修改，列表已刷新。请核对最新状态后重新操作。'
    } else error.value = cause?.message || '状态更新失败，请重试'
  } finally { pending.value = '' }
}
async function copyKey() {
  if (copying.value || !createdKey.value) return
  copying.value = true
  try {
    await navigator.clipboard.writeText(createdKey.value)
    ElMessage.success('完整 key 已复制到剪贴板')
  } catch {
    copyText.value = createdKey.value
    copyVisible.value = true
  } finally { copying.value = false }
}
function formatTime(ts) {
  if (!ts || !Number.isFinite(Number(ts))) return '—'
  return formatTimestamp(ts)
}
function lastUsed(row) {
  if (!row.last_used_at || !Number.isFinite(Number(row.last_used_at))) return Number(row.requests) === 0 ? '从未使用' : '未记录'
  return formatTime(row.last_used_at)
}

const groupIndex = computed(() => indexResources(groups.value))
function groupName(id) { return groupIndex.value.get(id)?.name || id }
onMounted(load)
onBeforeUnmount(() => { createdKey.value = ''; copyText.value = '' })
</script>

<template>
  <ResourcePageLayout v-loading="loading">
    <PageHeader :title="$route.meta.title" :subtitle="$route.meta.subtitle">
      <el-button :disabled="busy" type="primary" :icon="'Plus'" @click="openCreate">创建 KEY</el-button>
      <el-button aria-label="刷新密钥列表" :disabled="busy" :icon="'Refresh'" circle @click="load" />
    </PageHeader>

    <ResourcePageSummary v-if="hasLoaded" :items="summary" :disabled="busy" @select="(status) => statusFilter = status" />

    <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon class="management-error" />
    <div class="panel resource-panel">
      <div class="resource-panel-heading">
        <div><h3 class="resource-panel-title">访问密钥 <span v-if="hasLoaded" class="table-count">{{ filtered.length }} / {{ keys.length }}</span></h3><p class="resource-panel-caption">管理应用访问范围、启用状态与调用用量</p></div>
        <el-button size="small" :disabled="loading || !filtered.length || busy" @click="downloadResourceCSV('keys', filtered, groups)">导出筛选结果 CSV</el-button>
      </div>
      <div class="table-toolbar filter-bar">
        <el-input v-model="search" placeholder="搜索名称 / 密钥标识" clearable :prefix-icon="'Search'" aria-label="搜索密钥" style="width: 240px; max-width: 100%" />
        <el-select v-model="groupFilter" aria-label="密钥分组筛选" placeholder="全部分组" clearable filterable style="width: 160px">
          <el-option v-for="g in groups" :key="g.id" :label="g.name" :value="g.id" />
        </el-select>
        <el-select v-model="statusFilter" aria-label="密钥状态筛选" placeholder="全部状态" clearable style="width: 130px">
          <el-option label="启用" value="enabled" /><el-option label="停用" value="disabled" />
        </el-select>
        <el-select v-model="sortOrder" aria-label="密钥排序" placeholder="默认排序" clearable><el-option label="名称 A → Z" value="name:asc" /><el-option label="最近创建" value="created_at:desc" /><el-option label="最近使用" value="last_used_at:desc" /><el-option label="请求数最多" value="requests:desc" /><el-option label="Token 最多" value="tokens:desc" /><el-option label="失败数最多" value="errors:desc" /></el-select>
        <el-button v-if="search || groupFilter || statusFilter" @click="resetFilters">清除筛选</el-button>
        <div class="spacer" />
      </div>
      <div v-if="selectedRows.length" class="resource-selection" aria-live="polite">
        <span>本页已选 {{ selectedRows.length }} 项</span>
        <el-button size="small" :disabled="busy" @click="openBulk(true)">批量启用</el-button>
        <el-button size="small" type="warning" plain :disabled="busy" @click="openBulk(false)">批量停用</el-button>
        <el-button size="small" :disabled="busy" @click="clearSelection">取消选择</el-button>
      </div>
      <el-table class="resource-desktop-table" ref="table" :data="paginated" stripe row-key="id" :empty-text="keys.length ? '没有符合筛选条件的密钥，请调整或清除筛选' : '暂无密钥，点击「创建 KEY」开始'" @selection-change="(rows) => selected = rows.map((row) => row.id)">
        <el-table-column type="selection" width="46" :selectable="() => !busy" />
        <el-table-column label="名称 / Key" min-width="210"><template #default="{ row }"><div class="resource-identity"><span class="resource-identity-mark is-blue" aria-hidden="true"><ConsoleIcon name="key" :size="15" /></span><div class="resource-identity-copy"><button class="resource-name" :disabled="busy" @click="showDetail(row)">{{ row.name }}</button><span class="resource-row-sub mono">{{ row.key_masked }}</span></div></div></template></el-table-column>
        <el-table-column label="分组" min-width="160">
          <template #default="{ row }">
            <div class="tag-list">
              <el-tag v-for="id in row.groups" :key="id" size="small">{{ groupName(id) }}</el-tag>
              <span v-if="!row.groups?.length" style="color: var(--text-3); font-size: 12px">无分组</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="110" align="center">
          <template #default="{ row }">
            <div class="resource-switch-cell"><el-switch :model-value="row.enabled" :disabled="busy" :loading="pending === row.id" :aria-label="`${row.name} 启用状态`" @change="(value) => toggle(row, value)" /><span class="resource-state" :class="{ 'is-enabled': row.enabled }">{{ row.enabled ? '已启用' : '已停用' }}</span></div>
          </template>
        </el-table-column>
        <el-table-column prop="requests" label="请求数" width="90" align="right">
          <template #default="{ row }">{{ formatNumber(row.requests) }}</template>
        </el-table-column>
        <el-table-column prop="tokens" label="Token" width="100" align="right">
          <template #default="{ row }">{{ formatNumber(row.tokens) }}</template>
        </el-table-column>
        <el-table-column prop="errors" label="失败" width="70" align="right">
          <template #default="{ row }">
            <span :style="{ color: row.errors > 0 ? 'var(--danger)' : 'var(--text-3)' }">{{ formatNumber(row.errors) }}</span>
          </template>
        </el-table-column>
        <el-table-column label="最后使用" width="170">
          <template #default="{ row }">{{ lastUsed(row) }}</template>
        </el-table-column>
        <el-table-column label="操作" fixed="right" width="130" align="right">
          <template #default="{ row }">
            <el-button link type="primary" size="small" :disabled="busy" @click="openEdit(row)">编辑</el-button>
            <el-button link type="danger" size="small" :disabled="busy" :loading="pending === row.id" @click="remove(row, $event)">删除</el-button>
          </template>
        </el-table-column>
        <template #empty><ResourceEmptyState :loading="loading" :error="error" @retry="load" :filtered="!!keys.length" kind="密钥" hint="为应用创建独立凭据，并选择可访问的账号分组。" action="创建第一个 KEY" :disabled="busy" @create="openCreate" @reset="resetFilters" /></template>
      </el-table>
      <ResourceMobileList :items="paginated" selectable :selected="selected" :disabled="busy" @select="updateSelection" @detail="showDetail">
        <template #status="{ row }"><span class="resource-state" :class="{ 'is-enabled': row.enabled }">{{ row.enabled ? '已启用' : '已停用' }}</span></template>
        <template #default="{ row }">
          <code class="resource-inline-code mono">{{ row.key_masked }}</code>
          <div class="tag-list key-card-groups"><el-tag v-for="id in row.groups" :key="id" size="small">{{ groupName(id) }}</el-tag><span v-if="!row.groups?.length" class="resource-row-sub">未分配分组</span></div>
          <dl><div><dt>请求 / 失败</dt><dd>{{ formatNumber(row.requests) }} / <span :style="{ color: row.errors > 0 ? 'var(--danger)' : 'inherit' }">{{ formatNumber(row.errors) }}</span></dd></div><div><dt>Token</dt><dd>{{ formatNumber(row.tokens) }}</dd></div></dl>
          <span class="resource-row-sub">最后使用：{{ lastUsed(row) }}</span>
        </template>
        <template #actions="{ row }"><el-switch :model-value="row.enabled" :disabled="busy" :loading="pending === row.id" :aria-label="`${row.name} 启用状态`" @change="(value) => toggle(row, value)" /><span class="spacer" /><el-button size="small" :disabled="busy" @click="openEdit(row)">编辑</el-button><el-button size="small" text type="danger" :disabled="busy" :loading="pending === row.id" @click="remove(row, $event)">删除</el-button></template>
        <template #empty><ResourceEmptyState :loading="loading" :error="error" @retry="load" :filtered="!!keys.length" kind="密钥" hint="为应用创建独立凭据，并选择可访问的账号分组。" action="创建第一个 KEY" :disabled="busy" @create="openCreate" @reset="resetFilters" /></template>
      </ResourceMobileList>
      <div class="resource-table-footer">
        <div class="resource-page-size"><span>每页显示</span><el-select v-model="pageSize" aria-label="密钥每页条数" size="small"><el-option v-for="size in [10, 20, 50, 100]" :key="size" :label="String(size)" :value="size" /></el-select><span>条 · 共 {{ filtered.length }} 条</span></div>
        <el-pagination v-model:current-page="page" :page-size="pageSize" :total="filtered.length" :pager-count="5" layout="prev, pager, next" small />
      </div>
      <p class="resource-panel-note">CSV 仅包含管理元数据，不包含密钥及凭据片段。完整密钥只在创建时显示一次。</p>
    </div>

    <ResourceDetailsDialog v-if="detail" v-model="detailVisible" :title="detail.name" kind="密钥" :fields="[{ label: '密钥标识', value: detail.key_masked, mono: true }, { label: '创建时间', value: formatTime(detail.created_at) }, { label: '最后使用', value: lastUsed(detail) }, { label: '访问分组', value: detail.groups?.map(groupName).join('、') || '未分配分组' }]" :metrics="[{ label: '请求', value: formatNumber(detail.requests) }, { label: 'Token', value: formatNumber(detail.tokens) }, { label: '失败', value: formatNumber(detail.errors) }]">
      <template #status><el-tag :type="detail.enabled ? 'success' : 'info'" size="small">{{ detail.enabled ? '已启用' : '已停用' }}</el-tag></template>
      <p>完整密钥仅在创建时显示一次。需要更换凭据时，请创建新密钥并更新应用后，再停用旧密钥。</p>
      <template #actions><el-button type="primary" :disabled="busy" @click="detailVisible = false; openEdit(detail)">编辑密钥</el-button></template>
    </ResourceDetailsDialog>
    <el-dialog v-model="dialogVisible" :title="editing ? '编辑密钥' : '创建密钥'" width="min(560px, calc(100vw - 24px))" class="detail-dialog resource-form-dialog" align-center append-to-body :close-on-click-modal="false" :before-close="closeEditor" @opened="nameInput?.focus()" :close-on-press-escape="!pending" :show-close="!pending">
      <p class="dialog-intro">每个应用使用独立密钥，便于单独管理访问范围、查看用量和暂停访问。</p>
      <el-alert v-if="error" :title="error" type="error" :closable="false" class="management-error" />
      <el-form ref="formRef" :model="form" label-position="top" @submit.prevent="save" :disabled="busy" scroll-to-error>
        <div class="dialog-section-title">基本信息</div>
        <el-form-item label="名称" prop="name" :rules="[{ required: true, whitespace: true, message: '请输入名称', trigger: 'blur' }]">
          <el-input ref="nameInput" aria-label="名称" v-model="form.name" maxlength="60" placeholder="例如：对外服务 A" />
        </el-form-item>
        <div class="dialog-section-title">访问范围</div>
        <el-form-item label="分组">
          <el-select aria-label="访问分组" v-model="form.groups" multiple filterable style="width: 100%" placeholder="选择该 key 可调用的分组">
            <el-option v-for="g in groups" :key="g.id" :label="g.name" :value="g.id" />
          </el-select>
          <p class="key-scope-hint">未分配分组的密钥无法调用账号。可先创建密钥，稍后再分配。</p>
          <p v-if="!groups.length" class="key-scope-hint">当前还没有分组。<el-button link type="primary" @click="closeEditor().then((closed) => closed && router.push({ name: 'groups' }))">前往创建分组</el-button></p>
        </el-form-item>
        <el-form-item v-if="editing" label="启用">
          <el-switch v-model="form.enabled" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button :disabled="busy" @click="closeEditor">取消</el-button>
        <el-button type="primary" :loading="pending === 'save'" :disabled="busy" @click="save">保存</el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="createdVisible" title="密钥创建成功" class="dialog-secret detail-dialog" width="min(560px, calc(100vw - 24px))" align-center append-to-body :close-on-click-modal="false" :close-on-press-escape="false" :show-close="false" @closed="createdKey = ''">
      <el-alert type="warning" :closable="false" show-icon title="完整 key 仅显示这一次，请立即保存！" style="margin-bottom: 12px" />
      <el-input :model-value="createdKey" type="textarea" :rows="4" readonly aria-label="新创建的完整密钥" autocomplete="off" />
      <template #footer>
        <el-button type="primary" :loading="copying" :disabled="!createdKey" @click="copyKey">复制完整 key</el-button>
        <el-button @click="createdVisible = false">我已保存</el-button>
      </template>
    </el-dialog>
    <ResourceBulkStatusDialog ref="bulkDialog" resource-label="密钥" :resources="keys" :update="api.keyUpdate" :reload="load" @visibility="(value) => bulkVisible = value" @result="applyBulkResult" />
    <CopyTextDialog v-model="copyVisible" :text="copyText" @update:model-value="(visible) => { if (!visible) copyText = '' }" />
  </ResourcePageLayout>
</template>

<style scoped>
.resource-selection { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; padding: 0 18px 14px; }
.resource-selection .el-button + .el-button { margin-left: 0; }
.management-error { margin-bottom: 16px; overflow-wrap: anywhere; }
.key-card-groups { margin-top: 8px; }
.key-scope-hint { margin: 7px 0 0; color: var(--text-3); font-size: 12px; line-height: 1.6; }
:deep(.el-table .cell) { overflow-wrap: anywhere; }
</style>
