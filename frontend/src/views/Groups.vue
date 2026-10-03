<script setup>
import { formatTime as formatTimestamp } from '../utils/format'
import ConsoleIcon from '../components/ConsoleIcon.vue'
import { computed, nextTick, onMounted, ref, toRef, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '../api'
import PageHeader from '../components/PageHeader.vue'
import ResourcePageLayout from '../components/ResourcePageLayout.vue'
import ResourcePageSummary from '../components/ResourcePageSummary.vue'
import ResourceDetailsDialog from '../components/ResourceDetailsDialog.vue'
import ResourceEmptyState from '../components/ResourceEmptyState.vue'
import ResourceMobileList from '../components/ResourceMobileList.vue'
import { downloadResourceCSV, sortResources } from '../utils/resourceManagement'
import { useResourceFormDialog } from '../utils/resourceFormDialog'
import { managementFilters, useManagementRead } from '../utils/management'

const { loading, error, read, cancel } = useManagementRead()
const hasLoaded = ref(false)
const groups = ref([])
const page = ref(1)
const pageSize = toRef(managementFilters.groups, 'pageSize', 20)
const sortOrder = toRef(managementFilters.groups, 'sort', '')
const formRef = ref()
const nameInput = ref()
const validating = ref(false)
const detailID = ref('')
const detail = computed(() => groups.value.find((row) => row.id === detailID.value))
const detailVisible = ref(false)
function showDetail(row) { detailID.value = row.id; detailVisible.value = true }
const router = useRouter()
const search = toRef(managementFilters.groups, 'search')
const dialogVisible = ref(false)
const editing = ref(null)
const pending = ref('')
const form = ref({ name: '', description: '' })
const { capture: captureDraft, close: closeEditor } = useResourceFormDialog({
  readDraft: () => (form.value), isBusy: () => !!pending.value || validating.value, hide: () => { dialogVisible.value = false },
})
const filtered = computed(() => {
  const kw = search.value.trim().toLowerCase()
  return groups.value.filter((g) => !kw || String(g.name || '').toLowerCase().includes(kw) || String(g.description || '').toLowerCase().includes(kw))
})
const sorted = computed(() => sortResources(filtered.value, sortOrder.value))
const paginated = computed(() => sorted.value.slice((page.value - 1) * pageSize.value, page.value * pageSize.value))
watch([filtered, pageSize], () => { page.value = Math.min(page.value, Math.max(1, Math.ceil(filtered.value.length / pageSize.value))) })
watch([search, pageSize, sortOrder], () => { page.value = 1 })
function load() {
  return read((signal) => api.groups({ signal }), (data) => { groups.value = data.groups || []; hasLoaded.value = true })
}
const summary = computed(() => [
  { label: '账号分组', value: groups.value.length, hint: '按用途划分资源边界' },
  { label: '账号关联', value: groups.value.reduce((sum, row) => sum + (Number(row.accounts) || 0), 0), hint: '一个账号可关联多个分组' },
  { label: '密钥关联', value: groups.value.reduce((sum, row) => sum + (Number(row.keys) || 0), 0), hint: '一个密钥可访问多个分组' },
])
function openCreate() {
  formRef.value?.clearValidate()
  editing.value = null
  form.value = { name: '', description: '' }
  error.value = ''
  captureDraft()
  dialogVisible.value = true
}
function openEdit(row) {
  formRef.value?.clearValidate()
  editing.value = row
  form.value = { name: row.name, description: row.description || '' }
  error.value = ''
  captureDraft()
  dialogVisible.value = true
}
async function save() {
  if (pending.value || validating.value) return
  validating.value = true
  let valid = false
  try { valid = await formRef.value?.validate().catch(() => false) } finally { validating.value = false }
  if (!valid) return
  cancel()
  pending.value = 'save'
  error.value = ''
  try {
    const payload = { name: form.value.name.trim(), description: form.value.description }
    if (editing.value) await api.groupUpdate(editing.value.id, { ...payload, version: editing.value.version })
    else await api.groupCreate(payload)
    ElMessage.success(editing.value ? '分组已更新' : '分组已创建')
    dialogVisible.value = false
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
  if (pending.value) return
  const trigger = event?.currentTarget || document.activeElement
  pending.value = row.id
  try {
    try {
      await ElMessageBox.confirm(
        `确定删除分组「${row.name}」吗？${Number(row.accounts) || 0} 个账号和 ${Number(row.keys) || 0} 个 API KEY 将解除该分组。`,
        '删除确认', { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger' },
      )
    } catch { return }
    cancel()
    error.value = ''
    await api.groupDelete(row.id)
    ElMessage.success('分组已删除')
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
function formatTime(ts) {
  if (!Number.isFinite(Number(ts)) || !ts) return '—'
  return formatTimestamp(ts)
}
function showKeys(row) {
  managementFilters.keys.search = ''
  managementFilters.keys.status = ''
  managementFilters.keys.group = row.id
  router.push({ name: 'keys', query: { group_id: row.id } })
}
onMounted(load)
</script>

<template>
  <ResourcePageLayout v-loading="loading">
    <PageHeader :title="$route.meta.title" :subtitle="$route.meta.subtitle">
      <el-button :disabled="!!pending" type="primary" :icon="'Plus'" @click="openCreate">创建分组</el-button>
      <el-button aria-label="刷新分组" :disabled="!!pending" :icon="'Refresh'" circle @click="load" />
    </PageHeader>

    <ResourcePageSummary v-if="hasLoaded" :items="summary" />

    <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon class="management-error" />
    <div class="panel resource-panel">
      <div class="resource-panel-heading">
        <div><h3 class="resource-panel-title">账号分组 <span v-if="hasLoaded" class="table-count">{{ filtered.length }} / {{ groups.length }}</span></h3><p class="resource-panel-caption">按使用场景组织账号，为密钥分配可用资源</p></div>
        <el-button size="small" :disabled="loading || !filtered.length || !!pending" @click="downloadResourceCSV('groups', filtered)">导出筛选结果 CSV</el-button>
      </div>
      <div class="table-toolbar filter-bar">
        <el-input v-model="search" placeholder="搜索名称 / 描述" clearable :prefix-icon="'Search'" aria-label="搜索分组" style="width: 260px; max-width: 100%" />
        <el-select v-model="sortOrder" aria-label="分组排序" placeholder="默认排序" clearable><el-option label="名称 A → Z" value="name:asc" /><el-option label="最近创建" value="created_at:desc" /><el-option label="账号数量最多" value="accounts:desc" /><el-option label="密钥数量最多" value="keys:desc" /></el-select>
        <el-button v-if="search" @click="search = ''">清除筛选</el-button>
        <div class="spacer" />
      </div>
      <el-table class="resource-desktop-table" :data="paginated" stripe :empty-text="groups.length ? '没有符合筛选条件的分组，请调整或清除筛选' : '暂无分组，点击「创建分组」开始'">
        <el-table-column label="分组名称" min-width="170"><template #default="{ row }"><div class="resource-identity"><span class="resource-identity-mark is-purple" aria-hidden="true"><ConsoleIcon name="groups" :size="15" /></span><div class="resource-identity-copy"><button class="resource-name" :disabled="!!pending" @click="showDetail(row)">{{ row.name }}</button><span class="resource-id-label" :title="String(row.id)"># {{ row.id }}</span></div></div></template></el-table-column>
        <el-table-column label="描述" min-width="200" show-overflow-tooltip><template #default="{ row }"><span :class="{ 'resource-row-sub': !row.description }">{{ row.description || '暂无描述' }}</span></template></el-table-column>
        <el-table-column label="账号数" width="90" align="center"><template #default="{ row }"><el-button link type="primary" :disabled="!!pending" class="resource-association-count" :aria-label="`查看 ${row.name} 的账号`" @click="router.push({ name: 'accounts', query: { group_id: row.id } })">{{ row.accounts || 0 }}</el-button></template></el-table-column>
        <el-table-column label="KEY 数" width="90" align="center"><template #default="{ row }"><el-button link type="primary" :disabled="!!pending" class="resource-association-count is-purple" :aria-label="`查看 ${row.name} 的密钥`" @click="showKeys(row)">{{ row.keys || 0 }}</el-button></template></el-table-column>
        <el-table-column label="创建时间" width="170">
          <template #default="{ row }">{{ formatTime(row.created_at) }}</template>
        </el-table-column>
        <el-table-column label="操作" fixed="right" width="160" align="right">
          <template #default="{ row }">
            <el-button link type="primary" size="small" :disabled="!!pending" @click="openEdit(row)">编辑</el-button>
            <el-button link type="danger" size="small" :disabled="!!pending" :loading="pending === row.id" @click="remove(row, $event)">删除</el-button>
          </template>
        </el-table-column>
        <template #empty><ResourceEmptyState :loading="loading" :error="error" @retry="load" :filtered="!!groups.length" kind="分组" hint="按用途创建分组，再关联账号与 API KEY。" action="创建第一个分组" :disabled="!!pending" @create="openCreate" @reset="search = ''" /></template>
      </el-table>
      <ResourceMobileList :items="paginated" :disabled="!!pending" @detail="showDetail">
        <template #default="{ row }">
          <p class="group-description">{{ row.description || '暂无描述' }}</p>
          <dl><div><dt>关联账号</dt><dd><el-button link type="primary" :disabled="!!pending" class="resource-association-count" :aria-label="`查看 ${row.name} 的账号`" @click="router.push({ name: 'accounts', query: { group_id: row.id } })">{{ row.accounts || 0 }} 个账号</el-button></dd></div><div><dt>关联密钥</dt><dd><el-button link type="primary" :disabled="!!pending" class="resource-association-count is-purple" :aria-label="`查看 ${row.name} 的密钥`" @click="showKeys(row)">{{ row.keys || 0 }} 个 KEY</el-button></dd></div></dl>
          <span class="resource-row-sub">创建于 {{ formatTime(row.created_at) }}</span>
        </template>
        <template #actions="{ row }"><el-button size="small" :disabled="!!pending" @click="openEdit(row)">编辑</el-button><el-button size="small" text type="danger" :disabled="!!pending" :loading="pending === row.id" @click="remove(row, $event)">删除</el-button></template>
        <template #empty><ResourceEmptyState :loading="loading" :error="error" @retry="load" :filtered="!!groups.length" kind="分组" hint="按用途创建分组，再关联账号与 API KEY。" action="创建第一个分组" :disabled="!!pending" @create="openCreate" @reset="search = ''" /></template>
      </ResourceMobileList>
      <div class="resource-table-footer">
        <div class="resource-page-size"><span>每页显示</span><el-select v-model="pageSize" aria-label="分组每页条数" size="small"><el-option v-for="size in [10, 20, 50, 100]" :key="size" :label="String(size)" :value="size" /></el-select><span>条 · 共 {{ filtered.length }} 条</span></div>
        <el-pagination v-model:current-page="page" :page-size="pageSize" :total="filtered.length" :pager-count="5" layout="prev, pager, next" small />
      </div>
      <p class="resource-panel-note">点击分组名称查看详情，点击账号数或 KEY 数进入对应的资源列表。</p>
    </div>

    <ResourceDetailsDialog v-if="detail" v-model="detailVisible" :title="detail.name" kind="分组" :fields="[{ label: '分组描述', value: detail.description || '暂无描述' }, { label: '创建时间', value: formatTime(detail.created_at) }]" :metrics="[{ label: '关联账号', value: detail.accounts || 0 }, { label: '关联密钥', value: detail.keys || 0 }]">
      <p>从关联资源列表继续维护账号或密钥。删除分组会解除关联，不会删除账号和密钥本身。</p>
      <el-button link type="primary" @click="detailVisible = false; router.push({ name: 'accounts', query: { group_id: detail.id } })">查看关联账号</el-button>
      <el-button link type="primary" @click="detailVisible = false; showKeys(detail)">查看关联密钥</el-button>
      <template #actions><el-button type="primary" :disabled="!!pending" @click="detailVisible = false; openEdit(detail)">编辑分组</el-button></template>
    </ResourceDetailsDialog>
    <el-dialog v-model="dialogVisible" :title="editing ? '编辑分组' : '创建分组'" width="min(500px, calc(100vw - 24px))" class="detail-dialog resource-form-dialog" align-center append-to-body :close-on-click-modal="false" :before-close="closeEditor" @opened="nameInput?.focus()" :close-on-press-escape="!pending" :show-close="!pending">
      <p class="dialog-intro">按用途整理账号，让 API KEY 通过分组使用对应的账号。</p>
      <el-alert v-if="error" :title="error" type="error" :closable="false" class="management-error" />
      <el-form ref="formRef" :model="form" label-position="top" @submit.prevent="save" :disabled="!!pending" scroll-to-error>
        <div class="dialog-section-title">分组信息</div>
        <el-form-item label="名称" prop="name" :rules="[{ required: true, whitespace: true, message: '请输入名称', trigger: 'blur' }]">
          <el-input ref="nameInput" aria-label="名称" v-model="form.name" maxlength="60" placeholder="例如：Plus 账号" />
        </el-form-item>
        <el-form-item label="描述">
          <el-input aria-label="描述" v-model="form.description" type="textarea" :rows="3" placeholder="可选，说明该分组的用途" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button :disabled="!!pending" @click="closeEditor">取消</el-button>
        <el-button type="primary" :loading="pending === 'save'" :disabled="!!pending" @click="save">保存</el-button>
      </template>
    </el-dialog>
  </ResourcePageLayout>
</template>

<style scoped>
.management-error { margin-bottom: 16px; overflow-wrap: anywhere; }
.group-description { margin: 0; color: var(--text-2); }
:deep(.el-table .cell) { overflow-wrap: anywhere; }
</style>
