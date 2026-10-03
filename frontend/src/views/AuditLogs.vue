<script>
const cursorHistory = new Map()
</script>
<script setup>
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { api } from '../api'
import { useManagementRead } from '../utils/management'
import { auditActions, auditQuery, auditRequestParams, auditRangeError, auditResultLabel, validAuditPage, auditExport, downloadJSON } from '../utils/observability'
import { formatTime } from '../utils/format'
import { logLocalDateInput, logTimestampInput } from '../utils/logQueryState'
import PageHeader from '../components/PageHeader.vue'
import ConsoleIcon from '../components/ConsoleIcon.vue'

const route = useRoute()
const router = useRouter()
const { loading, error, read, cancel } = useManagementRead()
const query = ref(auditQuery(route.query))
const result = ref(null)
const current = ref(false)
const previous = ref(cursorHistory.get(route.fullPath) || [])
const detail = ref(null)
const detailOpen = ref(false)
const appliedQuery = ref(auditQuery(route.query))
const activeFilters = computed(() => [
  appliedQuery.value.action && { key: 'action', label: '操作', value: auditActions[appliedQuery.value.action] || appliedQuery.value.action },
  appliedQuery.value.entity_id && { key: 'entity_id', label: '对象', value: appliedQuery.value.entity_id },
  (appliedQuery.value.from || appliedQuery.value.to) && { key: 'time', label: '时间', value: `${appliedQuery.value.from ? formatTime(Number(appliedQuery.value.from)) : '不限开始'} → ${appliedQuery.value.to ? formatTime(Number(appliedQuery.value.to)) : '不限结束'}` },
].filter(Boolean))
function clearFilter(key) {
  if (key === 'time') { query.value.from = ''; query.value.to = '' } else query.value[key] = ''
  search()
}
function recent(seconds) { const now = Math.floor(Date.now() / 1000); query.value.from = seconds ? String(now - seconds) : ''; query.value.to = seconds ? String(now) : ''; search() }
function restoreQuery() { query.value = auditQuery(route.query) }
async function copyObject() {
  try { await navigator.clipboard.writeText(detail.value.entity_id); ElMessage.success('对象 ID 已复制') }
  catch { ElMessage.error('无法访问剪贴板，请选中对象 ID 手动复制') }
}
function queryObject() { query.value.entity_id = detail.value.entity_id; detailOpen.value = false; search() }

let lastTrigger
const dirty = computed(() => JSON.stringify(auditRequestParams(query.value)) !== JSON.stringify(auditRequestParams(auditQuery(route.query))))
const canPage = computed(() => current.value && !loading.value && !dirty.value && result.value?.supported)
async function load() {
  cancel()
  current.value = false
  const invalid = auditRangeError(query.value)
  if (invalid) { error.value = invalid; return }
  const snapshot = { ...query.value }
  await read(signal => api.auditLogs(auditRequestParams(snapshot), { signal, silent: true }), data => {
    if (!validAuditPage(data)) throw new Error('服务端返回了不完整的操作记录，请重试')
    result.value = data
    appliedQuery.value = snapshot
    current.value = true
  })
}
async function navigate(value, history = []) {
  cancel()
  current.value = false
  error.value = auditRangeError(value)
  query.value = value
  if (error.value) return
  const next = { path: '/audit-logs', query: auditRequestParams(value) }
  const path = router.resolve(next).fullPath
  previous.value = history
  cursorHistory.set(path, [...history])
  if (cursorHistory.size > 50) cursorHistory.delete(cursorHistory.keys().next().value)
  if (path === route.fullPath) await load()
  else await router.replace(next)
}
function search() { return navigate({ ...query.value, cursor: '' }) }
function reset() { return navigate(auditQuery()) }
function nextPage() {
  if (!canPage.value || !result.value.has_more) return
  navigate({ ...query.value, cursor: result.value.next_cursor }, [...previous.value, query.value.cursor])
}
function previousPage() {
  if (!canPage.value || !previous.value.length) return
  navigate({ ...query.value, cursor: previous.value.at(-1) }, previous.value.slice(0, -1))
}
function firstPage() { navigate({ ...query.value, cursor: '' }) }
function showDetail(item, event) {
  detail.value = item
  lastTrigger = event?.currentTarget || document.activeElement
  detailOpen.value = true
}
function restoreFocus() { nextTick(() => lastTrigger?.isConnected && lastTrigger.focus()) }
function exportPage() {
  if (!canPage.value || !result.value.items.length) return
  try { downloadJSON(auditExport(result.value.items), 'codex-agent-audit-summary.json') }
  catch { ElMessage.error('导出失败，请重试') }
}
watch(() => route.fullPath, () => {
  if (route.path !== '/audit-logs') return
  query.value = auditQuery(route.query)
  previous.value = cursorHistory.get(route.fullPath) || []
  detailOpen.value = false
  load()
}, { immediate: true })
onBeforeUnmount(() => { detail.value = null })
</script>
<template>
  <div class="audit-page">
    <PageHeader :title="$route.meta.title" :subtitle="$route.meta.subtitle">
      <el-button :disabled="!canPage || !result?.items.length" @click="exportPage">导出本页摘要</el-button>
      <el-button :loading="loading" :disabled="dirty" aria-label="刷新操作记录" @click="load">刷新</el-button>
    </PageHeader>
    <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon class="management-error" />
    <div class="panel audit-filter-panel">
<div class="audit-shortcuts" role="group" aria-label="操作记录时间范围"><span>快捷时间</span><el-button link @click="recent(3600)">近 1 小时</el-button><el-button link @click="recent(86400)">近 24 小时</el-button><el-button link @click="recent(604800)">近 7 天</el-button><el-button link @click="recent(0)">全部时间</el-button></div>
      <form class="audit-filters" @submit.prevent="search">
        <el-select v-model="query.action" filterable allow-create default-first-option clearable aria-label="操作类型" placeholder="全部操作类型">
          <el-option v-for="(label, action) in auditActions" :key="action" :label="label" :value="action" />
        </el-select>
        <el-input v-model="query.entity_id" maxlength="256" clearable aria-label="操作对象 ID" placeholder="操作对象 ID（精确）" />
        <el-date-picker :model-value="logLocalDateInput(query.from)" @update:model-value="query.from = logTimestampInput($event)" type="datetime" value-format="YYYY-MM-DDTHH:mm" placeholder="开始时间（含）" aria-label="操作开始时间" />
        <el-date-picker :model-value="logLocalDateInput(query.to)" @update:model-value="query.to = logTimestampInput($event)" type="datetime" value-format="YYYY-MM-DDTHH:mm" placeholder="结束时间（不含）" aria-label="操作结束时间" />
        <el-button type="primary" native-type="submit" :loading="loading">查询</el-button>
        <el-button :disabled="loading" @click="reset">重置</el-button>
      </form>
      <p class="audit-hint">时间按上海时区（UTC+08:00）显示和输入。查询已保存的操作记录。<router-link to="/logs">模型请求与用量请查看使用记录</router-link>。导出摘要仅包含时间、操作类型和结果。</p>
<div v-if="activeFilters.length" class="audit-applied"><span>已应用</span><el-tag v-for="filter in activeFilters" :key="filter.key" closable :title="filter.value" @close="clearFilter(filter.key)">{{ filter.label }}：{{ filter.value }}</el-tag></div><p v-if="dirty" class="audit-draft" role="status">筛选条件已修改，请点击查询。<el-button link type="primary" @click="restoreQuery">撤销筛选修改</el-button></p>
    </div>
    <div class="panel audit-results" v-loading="loading">
      <el-alert v-if="result && !current && !loading" title="当前查询未成功，下方仍为上次结果。请重新查询。" type="warning" :closable="false" show-icon />
      <el-empty v-if="result?.supported === false" description="当前使用文件存储，不提供持久化操作记录">
        <router-link to="/diagnostics">查看运行诊断</router-link>
      </el-empty>
      <template v-else>
        <el-table :data="result?.items || []" row-key="id" empty-text="当前条件下没有操作记录" class="audit-table">
          <el-table-column label="时间" min-width="165"><template #default="{ row }"><span class="audit-timestamp"><ConsoleIcon name="activity" :size="12" />{{ formatTime(row.created_at) }}</span></template></el-table-column>
          <el-table-column label="操作" min-width="150"><template #default="{ row }"><span class="ui-chip ui-chip--purple"><ConsoleIcon name="audit" :size="12" />{{ auditActions[row.action] || row.action }}</span></template></el-table-column>
          <el-table-column prop="entity_id" label="操作对象 ID" min-width="210" show-overflow-tooltip />
          <el-table-column prop="actor" label="操作者" min-width="100" show-overflow-tooltip><template #default="{ row }"><span class="ui-chip ui-chip--blue"><ConsoleIcon name="shield" :size="11" />{{ row.actor || '—' }}</span></template></el-table-column>
          <el-table-column label="结果" min-width="100"><template #default="{ row }"><el-tag :type="['ok', 'success'].includes(row.result) ? 'success' : ['failed', 'error'].includes(row.result) ? 'danger' : 'info'">{{ auditResultLabel(row.result) }}</el-tag></template></el-table-column>
          <el-table-column label="详情" width="90" fixed="right"><template #default="{ row }"><el-button link type="primary" @click="event => showDetail(row, event)">详情</el-button></template></el-table-column>
        </el-table>
        <div class="audit-mobile-list" aria-label="操作记录列表">
          <el-empty v-if="!result?.items.length" description="当前条件下没有操作记录" />
          <article v-for="item in result?.items || []" :key="item.id" class="audit-mobile-record">
            <div><strong>{{ auditActions[item.action] || item.action }}</strong><el-tag :type="['ok', 'success'].includes(item.result) ? 'success' : 'info'" size="small">{{ auditResultLabel(item.result) }}</el-tag></div>
            <time>{{ formatTime(item.created_at) }}</time>
            <p>{{ item.entity_id || '未指定对象' }}</p>
            <el-button link type="primary" @click="event => showDetail(item, event)">查看详情</el-button>
          </article>
        </div>
        <div class="audit-pagination">
          <span>当前页 {{ result?.items.length || 0 }} 条{{ result?.has_more ? '，还有更多记录' : '' }}</span>
          <el-select v-model="query.limit" aria-label="操作记录每页条数" :disabled="loading" @change="search"><el-option v-for="size in [20, 50, 100, 200]" :key="size" :label="`${size} 条 / 页`" :value="size" /></el-select>
          <el-button :disabled="!canPage || !query.cursor" @click="firstPage">首页</el-button>
          <el-button :disabled="!canPage || !previous.length" @click="previousPage">上一页</el-button>
          <el-button :disabled="!canPage || !result?.has_more" @click="nextPage">下一页</el-button>
        </div>
      </template>
    </div>
    <el-dialog v-model="detailOpen" title="操作详情" width="min(680px, calc(100vw - 32px))" class="detail-dialog" align-center append-to-body @closed="restoreFocus">
      <dl v-if="detail" class="audit-detail">
        <dt>记录 ID</dt><dd>{{ detail.id }}</dd>
        <dt>时间</dt><dd>{{ formatTime(detail.created_at) }}</dd>
        <dt>操作类型</dt><dd>{{ auditActions[detail.action] || detail.action }}（{{ detail.action }}）</dd>
        <dt>操作对象 ID</dt><dd>{{ detail.entity_id || '—' }}</dd>
        <dt>操作者</dt><dd>{{ detail.actor || '—' }}</dd>
        <dt>结果</dt><dd>{{ auditResultLabel(detail.result) }}</dd>
      </dl>
      <template #footer><el-button v-if="detail?.entity_id" @click="copyObject">复制对象 ID</el-button><el-button v-if="detail?.entity_id" @click="queryObject">查看此对象的操作</el-button><el-button @click="detailOpen = false">关闭</el-button></template>
    </el-dialog>
  </div>
</template>
<style scoped>
.audit-shortcuts{display:flex;align-items:center;gap:14px;flex-wrap:wrap;margin-bottom:16px}.audit-shortcuts>span,.audit-applied>span{font-size:11px;color:var(--text-3)}.audit-shortcuts .el-button{margin:0}.audit-applied{display:flex;align-items:center;gap:8px;flex-wrap:wrap;margin-top:12px}.audit-applied .el-tag{height:auto;max-width:100%;white-space:normal;overflow-wrap:anywhere;padding-block:4px}.audit-draft .el-button{margin-left:8px}
.audit-timestamp{display:inline-flex;align-items:center;gap:5px;color:var(--text-2);font-size:10px;font-variant-numeric:tabular-nums}
.audit-filter-panel{padding:20px;margin-bottom:20px}
.audit-filters{display:flex;flex-wrap:wrap;gap:12px;align-items:center}
.audit-filters>.el-select,.audit-filters>.el-input{width:210px;flex:1 1 190px}
.audit-filters :deep(.el-date-editor){width:210px;flex:1 1 190px}
.audit-hint{color:var(--el-text-color-secondary);font-size:13px;line-height:1.6;margin:16px 0 0}
.audit-draft{font-size:13px;color:var(--el-color-warning);margin:8px 0 0}
.audit-results{padding:20px;min-width:0}
.audit-pagination{display:flex;align-items:center;justify-content:flex-end;gap:10px;flex-wrap:wrap;padding-top:20px}
.audit-pagination>span{margin-right:auto;color:var(--el-text-color-secondary);font-size:13px}
.audit-pagination>.el-select{width:132px}
.audit-pagination>.el-button{margin-left:0}
.audit-detail{display:grid;grid-template-columns:110px minmax(0,1fr);gap:16px;margin:0}
.audit-detail dt{color:var(--el-text-color-secondary)}.audit-detail dd{margin:0;overflow-wrap:anywhere;white-space:pre-wrap}
.audit-mobile-list{display:none}.audit-mobile-record{padding:14px 0}.audit-mobile-record+.audit-mobile-record{border-top:1px solid var(--card-border)}.audit-mobile-record>div{display:flex;align-items:center;justify-content:space-between;gap:12px}.audit-mobile-record strong{font-size:13px;font-weight:500;overflow-wrap:anywhere}.audit-mobile-record time{display:block;font-size:11px;color:var(--text-3);margin-top:7px}.audit-mobile-record p{font-family:Consolas,monospace;font-size:12px;overflow-wrap:anywhere;color:var(--text-2);margin:10px 0 5px}
@media(max-width:767px){.audit-filter-panel,.audit-results{padding:14px}.audit-pagination>span{flex-basis:100%}.audit-filters>.el-input,.audit-filters>.el-select,.audit-filters :deep(.el-date-editor){min-width:0;max-width:100%;width:100%;flex-basis:100%}.audit-filters>.el-button{margin:0;flex:1}.audit-detail{grid-template-columns:1fr;gap:8px}.audit-detail dd{margin-bottom:8px}.audit-table{display:none}.audit-mobile-list{display:block}}
</style>
