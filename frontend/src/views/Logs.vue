<script>
// The URL preserves the current query/cursor; this small in-memory map also
// preserves backward paging when switching away and back through the tabs.
const cursorHistory = new Map()
</script>

<script setup>
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { Refresh, Search, RefreshLeft, Download, Star, Document, Bottom, Top, Coin, Clock, Cpu, Key, User, Setting, Warning, Delete } from '@element-plus/icons-vue'
import { logManagement, downloadManagedLogs } from '../api/logManagement'
import { createLatestRequest } from '../utils/latestRequest'
import { formatDate } from '../utils/format'
import { createSanitizedLogExport } from '../utils/logExport'
import { readPreference, writePreference, removePreference } from '../utils/preferences'
import PageHeader from '../components/PageHeader.vue'
import { logRangeError, logNavigationAllowed, logExportAllowed, validLogPage, logTimeParts, logLatency, logTokenCount, logLocalDateInput, logTimestampInput } from '../utils/logQueryState'
import { managedFilterKeys, managedQueryKeys, managedFilterLabels, managedFilterValueLimit, managedExactFilterError, emptyManagedFilters, readManagedFilters, managedSignature, buildManagedQuery, managedQuerySnapshot, validUsageStats, presentManagedRecord, logSortOptions, logErrorOptions, logRequestTypeOptions } from '../utils/logManagementState'
import LogUsageAnalytics from '../components/LogUsageAnalytics.vue'
import LogCleanupDialog from '../components/LogCleanupDialog.vue'
import LogResourceLookup from '../components/LogResourceLookup.vue'
import LogRelatedResource from '../components/LogRelatedResource.vue'
import LogDataCoverage from '../components/LogDataCoverage.vue'
import LogErrorReviewDialog from '../components/LogErrorReviewDialog.vue'
import LogErrorReviewPanel from '../components/LogErrorReviewPanel.vue'
import LogIpGeo from '../components/LogIpGeo.vue'
import LogIpGeoBatch from '../components/LogIpGeoBatch.vue'
import LogObservationDetails from '../components/LogObservationDetails.vue'
import { observedPathOptions, observationFilterFields, observationBooleanFields, observationBooleanOptions, observationSummary, observationAttemptLabel, truncationLabel, reasoningRoute } from '../utils/logObservationState'
import { logStepOptions, validLogTimezone } from '../utils/logManagementState'
import { getLogFailure } from '../utils/logFailure'
import LogSeverityBadge from '../components/LogSeverityBadge.vue'
import LogModelConsistency from '../components/LogModelConsistency.vue'
import { logSeverityOptions } from '../utils/logSeverity'
import LogIngressErrorsDialog from '../components/LogIngressErrorsDialog.vue'
import { clientRequestIdentity, imageTokenFields, imageTokenObservation } from '../utils/logPassiveObservation'
import { logMetricNumber, logCacheRatio, logPercent } from '../utils/logMetrics'
import { logExactTokenCount } from '../utils/logTokenDisplay'

const route = useRoute()
const router = useRouter()
const keys = managedQueryKeys
const defaults = emptyManagedFilters
const readFilters = readManagedFilters
const sizeFrom = (query) => [20, 50, 100, 200].includes(Number(query.page_size)) ? Number(query.page_size) : 20
const cursorFrom = (query) => typeof query.cursor === 'string' ? query.cursor : ''
const pageFrom = (query) => Math.min(100000, Math.max(1, Number.parseInt(query.page, 10) || 1))
const filters = ref(readFilters(route.query))
const appliedFilters = ref(readFilters(route.query))
const appliedPageSize = ref(sizeFrom(route.query))
const loadedPath = ref('')
const loadedPage = ref(1)
const advancedVisible = ref(managedFilterKeys.some(key => !['status', 'account', 'key_name', 'model'].includes(key) && route.query[key]))
const pageSize = ref(sizeFrom(route.query))
const page = ref(pageFrom(route.query))
const cursor = ref(cursorFrom(route.query))
const loading = ref(false)
const loadError = ref('')
const items = ref([])
const total = ref(null)
const totalKind = ref('')
const hasMore = ref(false)
const nextCursor = ref('')
const cursorSupported = ref(true)
const previous = ref(cursorHistory.get(route.fullPath) || [])
const detail = ref(null)
const detailVisible = ref(false)
const detailLoading = ref(false)
const detailError = ref('')
const detailSource = ref('snapshot')
const loadedDetailScope = ref({})
const detailScope = ref({})
const detailFallback = ref(null)
const detailExpired = ref(false)
const detailLatest = createLatestRequest()
let detailRequest = 0
const errorExpanded = ref(false)
const exporting = ref(false)
const exportVisible = ref(false)
const cleanupVisible = ref(false)
const errorReviewVisible = ref(false)
const ingressVisible = ref(false)
const columnsVisible = ref(false)
const columnOptions = [['identity', '账号 / KEY'], ['model', '模型与服务层'], ['tokens', 'Token 用量'], ['latency', '耗时'], ['ids', '请求 / 响应 ID'], ['ip', '客户端 IP'], ['ua', 'User-Agent'], ['groups', '请求时分组'], ['observations', '链路观测']]
const defaultColumns = ['identity', 'model', 'tokens', 'latency']
const validColumns = value => Array.isArray(value) && value.length <= columnOptions.length && new Set(value).size === value.length && value.every(key => columnOptions.some(([allowed]) => allowed === key))
const visibleColumns = ref(readPreference('codex.logs.columns', [...defaultColumns], validColumns))
const draftColumns = ref([...visibleColumns.value])
function editColumns() { draftColumns.value = [...visibleColumns.value]; columnsVisible.value = true }
function toggleColumn(key, selected) { draftColumns.value = selected ? [...new Set([...draftColumns.value, key])] : draftColumns.value.filter(value => value !== key) }
function applyColumns() { visibleColumns.value = [...draftColumns.value]; writePreference('codex.logs.columns', visibleColumns.value); columnsVisible.value = false }
const exportFormat = ref('csv')
const exportIdentities = ref(false)
const lookupVisible = ref(false)
const lookupField = ref('account')
const relatedVisible = ref(false)
const relatedResource = ref(null)
function openRelated(kind, id, name = '') { relatedResource.value = { kind, id: id || '', name }; relatedVisible.value = true }
function manageRelated(kind) { detailVisible.value = false; router.push({ path: kind === 'key' ? '/keys' : '/groups' }) }
function relatedFilter(key, value) { detailVisible.value = false; filterDimension(key, value) }
function openLookup(field) { lookupField.value = field; lookupVisible.value = true }
function selectResource(row) {
  const key = { account: 'account_id', key: 'key_id', model: 'model', group: 'group_id' }[row.field]
  if (!key) return
  filters.value[key] = row.value
  if (row.field === 'account') filters.value.account = ''
  if (row.field === 'key') filters.value.key_name = ''
  search()
}
const displayTimezone = computed(() => appliedFilters.value.timezone || 'Asia/Shanghai')
const rowTime = time => logTimeParts(time)
const rowFullTime = time => { const parts = rowTime(time); return `${parts.date} ${parts.time} (Asia/Shanghai)` }
const fullExportLoading = ref(false)
const fullExportError = ref('')
const fullExportProgress = ref(null)
let exportController
const statistics = ref(null)
const statsLoading = ref(false)
const statsError = ref('')
const statsSignature = ref('')
const statsRequestedSignature = ref('')
const statsLatest = createLatestRequest()
const querySnapshot = ref(null)
const scopedParams = computed(() => ({ ...appliedSnapshot.value.params, ...(querySnapshot.value?.signature === appliedSnapshot.value.signature ? { as_of: querySnapshot.value.as_of } : {}) }))
let statsRequest = 0
const saveVisible = ref(false)
const filterName = ref('')
const savedChoice = ref('')
const preferenceKey = 'codex.logs.saved-filters'
const validateSaved = (value) => Array.isArray(value) && value.length <= 20 && value.every((item) =>
  item && typeof item.name === 'string' && item.name.length > 0 && item.name.length <= 40 &&
  item.filters && typeof item.filters === 'object' && !Array.isArray(item.filters) &&
  Object.keys(item.filters).every((key) => keys.includes(key)) &&
  Object.entries(item.filters).every(([key, entry]) => typeof entry === 'string' && entry.length <= managedFilterValueLimit(key)) && !managedExactFilterError(item.filters))
const savedFilters = ref(readPreference(preferenceKey, [], validateSaved))
const density = ref(readPreference('codex.logs.density', 'comfortable', value => ['comfortable', 'compact'].includes(value)))
const appliedTimePreset = computed(() => {
  if (!filters.value.from && !filters.value.to) return 0
  const span = Number(filters.value.to) - Number(filters.value.from)
  return [3600, 86400, 604800].includes(span) ? span : -1
})
const hasFilters = computed(() => managedFilterKeys.some(key => appliedFilters.value[key] !== ''))
const draftDirty = computed(() => managedSignature(filters.value, pageSize.value) !== managedSignature(appliedFilters.value, appliedPageSize.value))
const appliedSnapshot = computed(() => managedQuerySnapshot(appliedFilters.value, appliedPageSize.value))
const statsCurrent = computed(() => !draftDirty.value && !statsLoading.value && !statsError.value && statsSignature.value === appliedSnapshot.value.signature)
const queryState = computed(() => ({ loading: loading.value, dirty: draftDirty.value, error: loadError.value, currentResult: loadedPath.value === route.fullPath }))
const canNavigate = computed(() => logNavigationAllowed(queryState.value))
const canExport = computed(() => logExportAllowed(queryState.value, items.value))
const pageStats = computed(() => items.value.reduce((value, row) => {
  if (row.status === 'ok') value.ok++
  if (row.status === 'error' || row.status === 'aborted') value.failed++
  return value
}, { ok: 0, failed: 0 }))
const activeFilterChips = computed(() => {
  const chips = managedFilterKeys.filter(key => !['from', 'to'].includes(key) && appliedFilters.value[key]).map(key => ({ key, label: managedFilterLabels[key], value: key === 'status' ? ({ ok: '成功', error: '失败', aborted: '中断', failed: '失败与中断' })[appliedFilters.value[key]] || appliedFilters.value[key] : key === 'error_category' ? logErrorOptions.find(([value]) => value === appliedFilters.value[key])?.[1] || appliedFilters.value[key] : key === 'stream' ? appliedFilters.value[key] === 'true' ? '流式' : '非流式' : appliedFilters.value[key] }))
  if (appliedFilters.value.from || appliedFilters.value.to) chips.unshift({ key: 'time', label: '时间', value: `${appliedFilters.value.from ? rowFullTime(Number(appliedFilters.value.from)) : '不限开始'} → ${appliedFilters.value.to ? rowFullTime(Number(appliedFilters.value.to)) : '不限结束'}` })
  return chips
})
const latest = createLatestRequest()
let requestID = 0
let lastTrigger
const canGoBack = computed(() => previous.value.length > 0 || (!cursorSupported.value && page.value > 1))
const emptyTitle = computed(() => loading.value ? '正在加载请求记录' : loadError.value ? '记录暂时无法加载' : hasFilters.value ? '没有匹配的请求' : '还没有请求记录')
const emptyDescription = computed(() => loading.value ? '结果更新后会显示在这里。' : loadError.value ? '请检查错误信息并重新加载。' : hasFilters.value ? '可以调整筛选条件，或重置查询。' : '通过网关发起请求后，可在这里查看用量与执行结果。')
const statusLabel = (value) => ({ ok: '成功', error: '失败', aborted: '中断' })[value] || '未知'
const queryScope = computed(() => {
  if (loading.value && items.value.length) return `正在加载新结果，暂时显示上次的 ${items.value.length} 条记录`
  if (loading.value) return '正在加载请求记录…'
  if (draftDirty.value) return `筛选已修改，尚未查询；当前显示上次查询的 ${items.value.length} 条记录`
  if (loadError.value && items.value.length) return `显示上次成功加载的第 ${loadedPage.value} 页（${items.value.length} 条），当前查询尚未完成`
  if (loadError.value) return '查询失败，尚未取得可用的请求记录'
  if (totalKind.value === 'not_computed' || total.value === null) return `当前页 ${items.value.length} 条；未计算全量总数`
  if (totalKind.value === 'exact') return `匹配 ${total.value} 条；当前页 ${items.value.length} 条`
  return `当前查询范围匹配 ${total.value} 条；旧接口可能仅扫描最近 10 万行`
})

function buildQuery() { return buildManagedQuery(appliedFilters.value, appliedPageSize.value, cursor.value, page.value) }
function validateRange() {
  const exactError = managedExactFilterError(filters.value)
  if (exactError) { loadError.value = exactError; return false }
  if (!validLogTimezone(filters.value.timezone)) { loadError.value = '请输入有效的 IANA 时区，例如 Asia/Shanghai。'; return false }
  if (filters.value.http_code && !/^[1-5]\d{2}$/.test(filters.value.http_code)) { loadError.value = 'HTTP 状态须为 100–599 的三位数字。'; return false }
  const message = logRangeError(filters.value)
  if (message) { loadError.value = message; return false }
  return true
}
const friendlyFailure = getLogFailure
function requestMode(row) {
  const inbound = logRequestTypeOptions.find(([value]) => value === row.request_type)?.[1] || (row.stream === true ? '流式' : row.stream === false ? '非流式' : '模式未知')
  const attempts = Array.isArray(row.attempts) ? row.attempts : []
  const websocket = attempts.findLastIndex(attempt => attempt?.phase === 'websocket')
  if (websocket < 0) return inbound
  if (attempts.slice(websocket + 1).some(attempt => attempt?.phase === 'request')) return `${inbound} · WS 不可用，已回退 HTTP`
  return `${inbound} · ${attempts[websocket]?.observed_http_status_code === 101 ? '上游 WebSocket' : '上游 WS 握手未完成'}`
}
function setDate(field, event) { filters.value[field] = logTimestampInput(event.target.value) }
function setStatus(value) { filters.value.status = value; search() }
function clearFilter(key) { if (key === 'time') { filters.value.from = ''; filters.value.to = '' } else filters.value[key] = ''; search() }
function discardFilterDraft() { filters.value = { ...appliedFilters.value }; pageSize.value = appliedPageSize.value; savedChoice.value = '' }
function filterDimension(key, value) { filters.value[key] = value; search() }
function refresh() {
  if (draftDirty.value) { search(); return }
  querySnapshot.value = null; statsRequestedSignature.value = ''; cursor.value = ''; previous.value = []; page.value = 1
  updateQuery()
}
async function loadStatistics() {
  const snapshot = appliedSnapshot.value
  if (!querySnapshot.value || querySnapshot.value.signature !== snapshot.signature || querySnapshot.value.expires_at <= Math.floor(Date.now() / 1000)) { load(true); return }
  const params = { ...snapshot.params, as_of: querySnapshot.value.as_of }
  const id = ++statsRequest
  statsRequestedSignature.value = snapshot.signature
  statsLoading.value = true
  statsError.value = ''
  try {
    await statsLatest.run(signal => logManagement.stats(params, { signal }), data => {
      if (!validUsageStats(data)) throw new Error('统计响应格式无效，请重试')
      statistics.value = data
      statsSignature.value = snapshot.signature
    })
  } catch (error) { if (id === statsRequest) statsError.value = error.status === 409 ? '查询快照已过期，请刷新日志重新查询。' : error.message || '统计加载失败' }
  finally { if (id === statsRequest) statsLoading.value = false }
}
async function load(force = false) {
  if (!querySnapshot.value && cursor.value) { refresh(); return }
  if (force && cursor.value) { refresh(); return }
  const rangeError = managedExactFilterError(appliedFilters.value) || logRangeError(appliedFilters.value)
  if (rangeError) { loadError.value = rangeError; ++requestID; latest.cancel(); loading.value = false; return }
  const id = ++requestID
  loading.value = true
  loadError.value = ''
  const params = buildQuery()
  const snapshot = appliedSnapshot.value
  const requestedPath = route.fullPath
  let scopeReady = false
  let requestedDetailScope
  // New APIs paginate by cursor. Only the original endpoint receives page.
  if (cursorSupported.value) delete params.page
  try {
    await latest.run(async signal => {
      const current = querySnapshot.value
      if (current?.signature === snapshot.signature && current.expires_at <= Math.floor(Date.now() / 1000) && params.cursor) throw Object.assign(new Error('查询快照已过期，请刷新日志重新查询。'), { status: 409 })
      const renew = force || !current || current.signature !== snapshot.signature || current.expires_at <= Math.floor(Date.now() / 1000)
      if (renew) {
        ++statsRequest; statsLatest.cancel(); statsLoading.value = true
        const result = await logManagement.snapshot(snapshot.params, { signal })
        if (signal.aborted) throw new DOMException('Query aborted', 'AbortError')
        if (!result?.as_of || typeof result.as_of !== 'string' || !Number.isFinite(result.expires_at) || result.expires_at <= Math.floor(Date.now() / 1000)) throw new Error('查询快照无效，请刷新重试')
        querySnapshot.value = { ...result, signature: snapshot.signature }
      }
      scopeReady = true
      if (renew || statsRequestedSignature.value !== snapshot.signature) loadStatistics()
      requestedDetailScope = Object.freeze({ ...snapshot.params, as_of: querySnapshot.value.as_of })
      return logManagement.list({ ...params, as_of: requestedDetailScope.as_of }, { signal })
    }, (data) => {
      if (!validLogPage(data)) throw new Error('日志响应格式无效，未更新当前结果。请重试。')
      loadedPath.value = requestedPath
      loadedPage.value = page.value
      items.value = data.items.map(presentManagedRecord)
      loadedDetailScope.value = requestedDetailScope
      total.value = Number.isFinite(data.total) && data.total >= 0 ? data.total : null
      totalKind.value = typeof data.total_kind === 'string' ? data.total_kind : ''
      cursorSupported.value = typeof data.has_more === 'boolean' || 'next_cursor' in data
      nextCursor.value = typeof data.next_cursor === 'string' ? data.next_cursor : ''
      hasMore.value = cursorSupported.value ? data.has_more === true && Boolean(nextCursor.value) : total.value !== null && page.value * appliedPageSize.value < total.value
    })
  } catch (error) {
    if (id === requestID) { loadError.value = error.status === 409 ? '查询快照已过期，请刷新日志重新查询。' : error.message || '日志加载失败，请重试'; if (!scopeReady) { statsLoading.value = false; statsError.value = loadError.value } }
  } finally {
    if (id === requestID) loading.value = false
  }
}
async function updateQuery() {
  ++requestID
  latest.cancel()
  loading.value = true
  const query = buildQuery()
  const target = router.resolve({ path: '/logs', query }).fullPath
  cursorHistory.set(target, [...previous.value])
  if (cursorHistory.size > 50) cursorHistory.delete(cursorHistory.keys().next().value)
  if (target === route.fullPath) await load()
  else await router.replace({ path: '/logs', query })
}
function search() {
  if (!validateRange()) { ++requestID; latest.cancel(); loading.value = false; return }
  appliedFilters.value = readFilters(filters.value)
  appliedPageSize.value = pageSize.value
  cursor.value = ''
  previous.value = []
  page.value = 1
  updateQuery()
}
function reset() {
  filters.value = defaults()
  savedChoice.value = ''
  search()
}
function setTimePreset(seconds) {
  const now = Math.floor(Date.now() / 1000)
  filters.value.from = seconds ? String(now - seconds) : ''
  filters.value.to = seconds ? String(now) : ''
  savedChoice.value = ''
  search()
}
function firstPage() { if (!canNavigate.value) return; cursor.value = ''; previous.value = []; page.value = 1; updateQuery() }
function nextPage() {
  if (!canNavigate.value || !hasMore.value) return
  previous.value.push(cursor.value)
  cursor.value = nextCursor.value
  page.value++
  updateQuery()
}
function previousPage() {
  if (!canNavigate.value || !canGoBack.value) return
  cursor.value = previous.value.pop() || ''
  page.value = Math.max(1, page.value - 1)
  updateQuery()
}
function openDetail(row, event, scope = loadedDetailScope.value) {
  lastTrigger = event?.currentTarget || document.activeElement
  detail.value = row
  detailFallback.value = row
  detailScope.value = Object.freeze({ ...scope })
  detailExpired.value = false
  errorExpanded.value = false
  detailVisible.value = true
  detailError.value = ''
  detailSource.value = 'snapshot'
  loadDetail(row)
}
function openReviewDetail(row) { openDetail(row, undefined, {}) }
async function loadDetail(row = detail.value) {
  const id = ++detailRequest
  detailLatest.cancel()
  const identity = row?.event_id || row?.id
  detailLoading.value = false
  if (!identity) return
  detailLoading.value = true
  detailError.value = ''
  try {
    await detailLatest.run(signal => logManagement.detail(identity, { params: detailScope.value, signal }), data => {
      if (!data?.record || typeof data.record !== 'object' || Array.isArray(data.record)) throw new Error('详情响应格式无效')
      detail.value = presentManagedRecord(data.record)
      detailSource.value = 'server'
    })
  } catch (error) { if (id === detailRequest) { detailExpired.value = error.status === 409; detail.value = detailFallback.value; detailSource.value = 'snapshot'; detailError.value = detailExpired.value ? '详情查询快照已过期，请关闭详情并刷新日志后重新打开。' : error.message || '详情加载失败，保留列表快照' } }
  finally { if (id === detailRequest) detailLoading.value = false }
}
function restoreFocus() { nextTick(() => lastTrigger?.isConnected && lastTrigger.focus()) }
function viewAccount(row) {
  router.push({ path: '/accounts', query: row.account_id ? { account_id: row.account_id } : { search: row.account } })
}
function viewKey(row) {
  if (!row.key_id && !row.key_name) return
  filters.value.key_id = row.key_id || ''
  filters.value.key_name = row.key_id ? '' : row.key_name
  detailVisible.value = false
  search()
}
async function copyIdentifier(value) {
  try { await navigator.clipboard.writeText(String(value)); ElMessage.success('已复制标识') }
  catch { ElMessage.error('无法访问剪贴板，请选中标识后手动复制') }
}
function saveDownload(blob, filename) {
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = filename
  document.body.append(anchor)
  anchor.click()
  anchor.remove()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
async function exportAll() {
  if (fullExportLoading.value || !canNavigate.value) return
  const snapshot = appliedSnapshot.value
  exportController = new AbortController()
  fullExportLoading.value = true
  fullExportError.value = ''
  fullExportProgress.value = null
  try {
    const result = await downloadManagedLogs({ ...snapshot.params, as_of: querySnapshot.value?.as_of }, { format: exportFormat.value, redacted: !exportIdentities.value, signal: exportController.signal, onProgress: progress => { fullExportProgress.value = progress } })
    saveDownload(result.blob, result.filename)
    ElMessage.success(`已导出全部匹配的 ${result.count} 条留存记录`)
    exportVisible.value = false
  } catch (error) { if (error?.name !== 'AbortError') fullExportError.value = error.message || '导出失败，请重试'; else fullExportError.value = '导出已取消，未生成文件。' }
  finally { fullExportLoading.value = false; exportController = undefined }
}
function cancelFullExport() { if (fullExportLoading.value) exportController?.abort(); else exportVisible.value = false }
const exportProgressText = computed(() => {
  if (!fullExportLoading.value) return ''
  const progress = fullExportProgress.value
  if (!progress) return '正在服务端准备全部匹配记录，可取消…'
  return `正在接收 ${logTokenCount(progress.count)} 条记录 · ${(progress.received / 1024).toFixed(1)} KB${progress.total ? ` / ${(progress.total / 1024).toFixed(1)} KB` : ''}`
})
async function exportPage() {
  if (exporting.value || !canExport.value) return
  exporting.value = true
  let url
  try {
    const content = createSanitizedLogExport(items.value)
    url = URL.createObjectURL(new Blob([content], { type: 'application/x-ndjson;charset=utf-8' }))
    const anchor = document.createElement('a')
    anchor.href = url
    anchor.download = `logs-redacted-page-${page.value}-${formatDate(Date.now() / 1000)}.jsonl`
    document.body.append(anchor)
    anchor.click()
    anchor.remove()
    ElMessage.success(`已导出当前页 ${items.value.length} 条脱敏记录`)
  } catch (error) {
    ElMessage.error(error.message || '日志导出失败')
  } finally {
    if (url) setTimeout(() => URL.revokeObjectURL(url), 1000)
    exporting.value = false
  }
}
function saveFilter() {
  const name = filterName.value.trim()
  if (!name) { ElMessage.warning('请填写筛选名称'); return }
  const exactError = managedExactFilterError(filters.value)
  if (exactError) { ElMessage.warning(exactError); return }
  const entry = { name, filters: readFilters(filters.value) }
  const next = [entry, ...savedFilters.value.filter((item) => item.name !== name)].slice(0, 20)
  writePreference(preferenceKey, next)
  const verified = readPreference(preferenceKey, null, validateSaved)
  if (!verified || JSON.stringify(verified) !== JSON.stringify(next)) { ElMessage.error('浏览器无法保存筛选，请检查存储权限'); return }
  savedFilters.value = verified
  savedChoice.value = name
  saveVisible.value = false
  ElMessage.success('已保存常用筛选')
}
function useSaved(name) {
  const entry = savedFilters.value.find((item) => item.name === name)
  if (entry) { filters.value = readFilters(entry.filters); search() }
}
function clearSaved() {
  removePreference(preferenceKey)
  if (readPreference(preferenceKey, [], validateSaved).length) { ElMessage.error('无法清除保存的筛选'); return }
  savedFilters.value = []
  savedChoice.value = ''
}
watch(() => route.fullPath, () => {
  if (route.path !== '/logs') return
  filters.value = readFilters(route.query)
  appliedFilters.value = readFilters(route.query)
  pageSize.value = sizeFrom(route.query)
  appliedPageSize.value = sizeFrom(route.query)
  page.value = pageFrom(route.query)
  cursor.value = cursorFrom(route.query)
  previous.value = cursorHistory.get(route.fullPath) || []
  load()
})
watch(density, value => writePreference('codex.logs.density', value))
watch(detailVisible, value => { if (!value) { ++detailRequest; detailLatest.cancel(); detailLoading.value = false } })
onMounted(load)
onUnmounted(() => { ++requestID; ++statsRequest; ++detailRequest; latest.cancel(); statsLatest.cancel(); detailLatest.cancel(); exportController?.abort() })
</script>


<template>
  <div class="logs-page">
    <PageHeader :title="$route.meta.title" subtitle="追踪每一次请求的状态、用量与执行结果">
      <el-button type="primary" :icon="Download" :disabled="!canNavigate" @click="fullExportError = ''; exportIdentities = false; exportVisible = true">导出全部筛选</el-button>
      <el-button :icon="Refresh" circle :loading="loading" aria-label="刷新日志" @click="refresh" />
    </PageHeader>

    <div class="log-workspace">
      <section class="log-query-panel" aria-label="日志查询条件">
      <div class="log-toolbar-top">
        <div class="log-status-tabs" role="group" aria-label="日志状态">
          <button v-for="item in [{ value: '', label: '全部请求' }, { value: 'ok', label: '成功' }, { value: 'error', label: '失败' }, { value: 'aborted', label: '中断' }]" :key="item.value" type="button" :class="{ active: filters.status === item.value }" :aria-pressed="filters.status === item.value" @click="setStatus(item.value)"><span v-if="item.value" class="status-dot" :class="'status-' + item.value" />{{ item.label }}</button>
        </div>
        <div class="time-presets" role="group" aria-label="日志时间预设">
          <button v-for="preset in [{ label: '全部时间', value: 0 }, { label: '近 1 小时', value: 3600 }, { label: '近 24 小时', value: 86400 }, { label: '近 7 天', value: 604800 }]" :key="preset.value" type="button" :class="{ active: appliedTimePreset === preset.value }" :aria-pressed="appliedTimePreset === preset.value" @click="setTimePreset(preset.value)">{{ preset.label }}</button>
        </div>
      </div>

      <form class="log-query" @submit.prevent="search">
        <div class="log-primary-filters">
          <div class="log-filter-field"><span class="field-label-action">账号<button type="button" @click.prevent="openLookup('account')">查找账号</button></span><el-input v-model="filters.account" placeholder="账号名称，精确匹配" aria-label="账号名称" :prefix-icon="Search" clearable @keydown.enter.prevent="search" @clear="search" /></div>
          <div class="log-filter-field"><span class="field-label-action">API KEY<button type="button" @click.prevent="openLookup('key')">查找 KEY</button></span><el-input v-model="filters.key_name" placeholder="KEY 名称，精确匹配" aria-label="KEY 名称" clearable @keydown.enter.prevent="search" @clear="search" /></div>
          <div class="log-filter-field"><span class="field-label-action">模型<button type="button" @click.prevent="openLookup('model')">查找模型</button></span><el-input v-model="filters.model" placeholder="模型名称，精确匹配" aria-label="模型名称" clearable @keydown.enter.prevent="search" @clear="search" /></div>
          <div class="log-query-actions">
            <el-button type="primary" :loading="loading" native-type="submit">查询</el-button>
            <el-button :icon="RefreshLeft" @click="reset">重置</el-button>
            <button type="button" class="advanced-toggle" :aria-expanded="advancedVisible" aria-controls="log-advanced-filters" @click="advancedVisible = !advancedVisible">高级筛选 <span aria-hidden="true">{{ advancedVisible ? '−' : '+' }}</span></button>
          </div>
        </div>
        <div v-show="advancedVisible" id="log-advanced-filters" class="log-advanced-filters">
          <label class="date-field"><span>开始时间 <small>包含</small></span><input type="datetime-local" :value="logLocalDateInput(filters.from)" aria-label="开始时间" @input="setDate('from', $event)" /></label>
          <label class="date-field"><span>结束时间 <small>不包含</small></span><input type="datetime-local" :value="logLocalDateInput(filters.to)" aria-label="结束时间" @input="setDate('to', $event)" /></label>
          <div v-for="field in [['account_id', '账号 ID'], ['key_id', 'KEY ID'], ['group_id', '分组 ID'], ['request_id', '记录 ID'], ['client_request_id', '客户端关联 ID'], ['ip', '客户端 IP']]" :key="field[0]" class="log-filter-field"><span class="field-label-action">{{ field[1] }}<button v-if="field[0] === 'group_id'" type="button" @click.prevent="openLookup('group')">查找分组</button></span><el-input v-model="filters[field[0]]" :aria-label="field[1]" :placeholder="field[0] === 'request_id' ? '上游请求 ID、事件 ID 或记录 ID' : field[0] === 'client_request_id' ? '入站 X-Client-Request-ID，精确匹配' : '精确匹配'" clearable @keydown.enter.prevent="search" /></div>
          <label class="log-filter-field"><span>端点</span><el-select v-model="filters.endpoint" aria-label="请求端点" clearable placeholder="全部端点" filterable allow-create><el-option v-for="endpoint in ['responses', 'responses/compact', 'compact', 'models']" :key="endpoint" :value="endpoint" :label="endpoint" /></el-select></label>
          <label class="log-filter-field"><span>HTTP 状态</span><el-input v-model="filters.http_code" aria-label="HTTP 状态" placeholder="例如 200、429" maxlength="3" clearable @keydown.enter.prevent="search" /></label>
          <label class="log-filter-field"><span>执行状态</span><el-select v-model="filters.status" aria-label="执行状态" clearable placeholder="全部状态"><el-option label="成功" value="ok" /><el-option label="失败" value="error" /><el-option label="中断" value="aborted" /><el-option label="全部失败与中断" value="failed" /></el-select></label>
          <label class="log-filter-field"><span>响应模式</span><el-select v-model="filters.stream" aria-label="响应模式" clearable placeholder="全部模式"><el-option label="流式" value="true" /><el-option label="非流式" value="false" /></el-select></label>
          <label class="log-filter-field"><span>错误类别</span><el-select v-model="filters.error_category" aria-label="错误类别" clearable filterable placeholder="全部错误类别"><el-option v-for="[value, label] in logErrorOptions" :key="value" :label="label" :value="value" /></el-select></label>
          <label class="log-filter-field"><span>请求类型</span><el-select v-model="filters.request_type" aria-label="请求类型" clearable placeholder="全部请求类型"><el-option v-for="[value, label] in logRequestTypeOptions" :key="value" :label="label" :value="value" /></el-select></label>
          <label v-for="field in [['upstream_model', '上游模型'], ['service_tier', '服务层'], ['reasoning_effort', '推理强度']]" :key="field[0]" class="log-filter-field"><span>{{ field[1] }}</span><el-input v-model="filters[field[0]]" :aria-label="field[1]" :placeholder="field[0] === 'service_tier' ? '例如 priority、default' : field[0] === 'reasoning_effort' ? '例如 high、xhigh' : '实际观测到的模型'" clearable @keydown.enter.prevent="search" /></label>
          <label class="log-filter-field"><span>模型观测差异</span><el-select v-model="filters.model_mismatch" aria-label="模型观测差异" clearable placeholder="不限（含未知）"><el-option label="观测模型不同" value="true" /><el-option label="观测模型相同" value="false" /></el-select></label>
          <label v-for="[key,label] in observationFilterFields" :key="key" class="log-filter-field"><span>{{ label }}</span><el-input v-if="key.endsWith('_model')" v-model="filters[key]" :aria-label="label" placeholder="完整模型名称，精确匹配" clearable @keydown.enter.prevent="search" /><el-select v-else v-model="filters[key]" :aria-label="label" clearable placeholder="标准化路径，不含域名或查询参数"><el-option v-for="path in observedPathOptions" :key="path" :label="path" :value="path" /></el-select></label>
          <label v-for="[key,label] in observationBooleanFields" :key="key" class="log-filter-field"><span>{{ label }}</span><el-select v-model="filters[key]" :aria-label="label" clearable placeholder="不限（含未知）"><el-option :label="observationBooleanOptions(key)[0]" value="true" /><el-option :label="observationBooleanOptions(key)[1]" value="false" /></el-select></label>
          <label class="log-filter-field"><span>原生压缩</span><el-select v-model="filters.native_compaction" aria-label="原生压缩" clearable placeholder="不限（含未知）"><el-option label="已观测到原生压缩" value="true" /><el-option label="已观测为未触发" value="false" /></el-select></label>
          <label class="log-filter-field"><span>错误级别</span><el-select v-model="filters.error_severity" aria-label="错误级别" clearable placeholder="全部级别"><el-option v-for="[value,label] in logSeverityOptions" :key="value" :value="value" :label="label" /></el-select></label>
          <label class="log-filter-field"><span>错误阶段</span><el-input v-model="filters.error_phase" aria-label="错误阶段" placeholder="例如 auth、upstream" clearable @keydown.enter.prevent="search" /></label>
          <label class="log-filter-field"><span>统计粒度</span><el-select v-model="filters.step" aria-label="统计粒度"><el-option v-for="[value, label] in logStepOptions" :key="value" :value="value" :label="label" /></el-select></label>
          <label class="log-filter-field"><span>统计分桶时区</span><el-input v-model="filters.timezone" aria-label="统计时区" placeholder="Asia/Shanghai（上海时间）" clearable @keydown.enter.prevent="search" /></label>
          <p class="advanced-scope-note">账号 ID 优先于账号名称；请求 / 记录 ID 查询对应标识。模型差异只比较已观测到的完整模型对，未知不算一致；“模型观测差异”保留请求与兼容上游模型的原口径。截断“未截断”只含明确 false。模型、路径、名称、分组和 IP 均按完整值匹配。</p>
          <div class="advanced-help"><span>日期输入使用上海时间（UTC+08:00）；日 / 周 / 月按指定统计时区分桶</span><el-button :icon="Star" size="small" @click="filterName = ''; saveVisible = true">保存筛选</el-button></div>
          <div v-if="savedFilters.length" class="saved-filters"><el-select v-model="savedChoice" placeholder="使用常用筛选" aria-label="使用常用筛选" clearable @change="useSaved"><el-option v-for="item in savedFilters" :key="item.name" :label="item.name" :value="item.name" /></el-select><el-button link @click="clearSaved">清除已保存筛选</el-button></div>
        </div>
        <div v-if="activeFilterChips.length || draftDirty" class="query-notices">
          <span v-if="activeFilterChips.length" class="filter-chips-label">已应用</span>
          <el-tag v-for="chip in activeFilterChips" :key="chip.key" closable size="small" :title="chip.value" :aria-label="chip.label + '：' + chip.value" @close="clearFilter(chip.key)">{{ chip.label }}：{{ chip.value }}</el-tag>
          <span v-if="draftDirty" class="draft-notice" role="status">筛选已修改，点击「查询」应用；分页和导出将在结果更新后恢复。</span><el-button v-if="draftDirty" link type="primary" @click="discardFilterDraft">撤销筛选修改</el-button>
        </div>
      </form>
      </section>

      <section class="log-analytics-panel" aria-label="请求用量分析"><LogUsageAnalytics :snapshot="querySnapshot" :timezone="displayTimezone" :stats="statistics" :loading="statsLoading" :error="statsError" :current="statsCurrent" @refresh="loadStatistics" @filter="filterDimension" /></section>

      <section class="log-results-panel" aria-label="请求记录">
      <div class="log-management-toolbar"><div class="log-records-title"><span class="records-title-icon"><el-icon><Document /></el-icon></span><h2>请求记录</h2><div class="query-scope" role="status">{{ queryScope }}</div></div><div class="log-management-actions"><el-button link :icon="Download" :loading="exporting" :disabled="!canExport" @click="exportPage">导出当前页（脱敏）</el-button><span class="tool-divider" /><el-button link :icon="Warning" @click="errorReviewVisible = true">错误处理</el-button><el-button link @click="ingressVisible = true">准入错误</el-button><el-button link :icon="Delete" @click="cleanupVisible = true">清理明细</el-button></div></div>
      <div class="log-list-controls">
      <div class="log-sort-toolbar">
        <label><span>排序</span><el-select v-model="filters.sort_by" aria-label="日志排序字段" @change="search"><el-option v-for="[value, label] in logSortOptions" :key="value" :value="value" :label="label" /></el-select></label>
        <el-select v-model="filters.sort_order" aria-label="日志排序方向" @change="search"><el-option label="从高到低 / 最新优先" value="desc" /><el-option label="从低到高 / 最早优先" value="asc" /></el-select>
        <label class="exact-total-option"><input type="checkbox" :checked="filters.exact_total === 'true'" @change="filters.exact_total = $event.target.checked ? 'true' : 'false'; search()" /><span>计算精确总数</span></label>
      </div>

      <div class="log-results-heading">
        <div class="log-result-options">
          <span v-if="!loading && !loadError && !draftDirty && items.length" class="page-outcomes">本页 <span class="outcome-ok">{{ pageStats.ok }} 成功</span><span v-if="pageStats.failed" class="outcome-error"> · {{ pageStats.failed }} 失败/中断</span></span>
          <LogIpGeoBatch :ips="items.map(item => item.ip)" :disabled="loading || draftDirty || !!loadError" />
          <el-button link :icon="Setting" class="columns-button" @click="editColumns">显示列</el-button><div class="table-density" role="group" aria-label="日志表格密度"><button type="button" :class="{ active: density === 'comfortable' }" :aria-pressed="density === 'comfortable'" @click="density = 'comfortable'">舒适</button><button type="button" :class="{ active: density === 'compact' }" :aria-pressed="density === 'compact'" @click="density = 'compact'">紧凑</button></div>
        </div>
      </div>
      </div>
      <div v-if="loadError" class="log-load-error"><el-alert :title="loadError" type="error" show-icon :closable="false" /><el-button size="small" @click="refresh">重新加载</el-button></div>

      <div v-loading="loading" class="log-records" :class="{ 'has-stale-results': loadError || draftDirty }" :aria-busy="loading">
        <el-table :data="items" :size="density === 'compact' ? 'small' : 'default'" :class="['log-table', 'density-' + density]" empty-text="当前条件没有日志记录">
          <template #empty><div class="logs-empty"><el-icon><Document /></el-icon><strong>{{ emptyTitle }}</strong><p>{{ emptyDescription }}</p><el-button v-if="!loading && hasFilters && !loadError" size="small" @click="reset">重置查询条件</el-button></div></template>
          <el-table-column label="请求时间（上海）" width="148">
            <template #default="{ row }"><div class="request-time" :title="rowFullTime(row.time)"><span>{{ rowTime(row.time).date }}</span><strong>{{ rowTime(row.time).time }}</strong></div><div class="cell-secondary request-endpoint" :title="`${row.endpoint || '—'} · ${requestMode(row)}`"><span>{{ row.endpoint || '—' }}</span><span class="mode-mark">{{ requestMode(row) }}</span></div></template>
          </el-table-column>
          <el-table-column v-if="visibleColumns.includes('identity')" label="账号 / KEY" min-width="165">
            <template #default="{ row }"><div class="identity-primary"><span class="identity-avatar" aria-hidden="true"><el-icon><User /></el-icon></span><el-button v-if="row.account || row.account_id" link type="primary" class="identity-link" :title="row.account || row.account_id" @click="viewAccount(row)">{{ row.account || row.account_id }}</el-button><span v-else class="cell-muted">未记录账号</span></div><div class="cell-secondary identity-key" :title="row.key_name"><el-icon aria-hidden="true"><Key /></el-icon><span class="field-prefix">KEY</span>{{ row.key_name || '—' }}</div></template>
          </el-table-column>
          <el-table-column v-if="visibleColumns.includes('model')" label="模型" min-width="160"><template #default="{ row }"><span class="model-name" :title="`请求模型：${row.model || '未记录'}`"><el-icon aria-hidden="true"><Cpu /></el-icon><span>{{ row.model || '—' }}</span></span><div class="model-chip-row"><LogModelConsistency :record="row" /><span v-if="row.returned_model || row.upstream_model" class="observed-model" :title="`${row.returned_model ? '返回模型' : '上游声明模型'}：${row.returned_model || row.upstream_model} · 来源：${row.upstream_model_source || '未记录'}`"><span aria-hidden="true">↳</span> {{ row.returned_model ? '返回' : '上游声明' }} {{ row.returned_model || row.upstream_model }}</span><span v-if="row.service_tier" class="model-metadata" :title="`服务层：${row.service_tier} · ${row.service_tier_source === 'response' ? '响应已报告' : row.service_tier_source === 'request' ? '来自请求，未确认响应值' : '来源未记录'}`">{{ row.service_tier_source === 'response' ? '响应' : row.service_tier_source === 'request' ? '请求' : '记录' }} {{ row.service_tier }}</span><span v-if="reasoningRoute(row).label" class="model-metadata reasoning-chip" :title="reasoningRoute(row).title">{{ reasoningRoute(row).label }}</span></div></template></el-table-column>
          <el-table-column v-if="visibleColumns.includes('tokens')" label="Token 用量" min-width="315"><template #header><div class="token-column-heading"><span>Token 用量</span><span class="token-column-legend"><i class="legend-input" />输入<i class="legend-output" />输出<i class="legend-cache" />读取<i class="legend-write" />创建</span></div></template><template #default="{ row }"><div class="token-pills"><div class="token-line token-input" :title="`输入 Token：${logExactTokenCount(row.input_tokens)}`" :aria-label="`输入 Token：${logExactTokenCount(row.input_tokens)}`"><el-icon aria-hidden="true"><Bottom /></el-icon><strong>{{ logTokenCount(row.input_tokens) }}</strong></div><div class="token-line token-output" :title="`输出 Token：${logExactTokenCount(row.output_tokens)}`" :aria-label="`输出 Token：${logExactTokenCount(row.output_tokens)}`"><el-icon aria-hidden="true"><Top /></el-icon><strong>{{ logTokenCount(row.output_tokens) }}</strong></div><div class="token-line token-cache" :title="`缓存读取 Token：${logExactTokenCount(row.cached_tokens)}${logCacheRatio(row) !== null ? ' · 读取命中率 ' + logPercent(logCacheRatio(row)) : ' · 读取命中率未知'}`" :aria-label="`缓存读取 Token：${logExactTokenCount(row.cached_tokens)}`"><el-icon aria-hidden="true"><Coin /></el-icon><strong>{{ logTokenCount(row.cached_tokens) }}</strong></div><div class="token-line token-write" :title="`缓存创建 Token：${logExactTokenCount(row.cache_write_tokens)}`" :aria-label="`缓存创建 Token：${logExactTokenCount(row.cache_write_tokens)}`"><span class="token-kind">建</span><strong>{{ logTokenCount(row.cache_write_tokens) }}</strong></div></div><div v-if="logCacheRatio(row) !== null" class="cache-ratio"><span class="cache-ratio-track"><i :style="{ width: `${Math.min(100, logCacheRatio(row) * 100)}%` }" /></span>读取命中率 {{ logPercent(logCacheRatio(row)) }}</div></template></el-table-column>
          <el-table-column v-if="visibleColumns.includes('latency')" label="耗时" width="108"><template #default="{ row }"><span class="latency-value" :title="`总耗时：${logLatency(row.latency_ms)}`"><el-icon aria-hidden="true"><Clock /></el-icon>{{ logLatency(row.latency_ms) }}</span><span v-if="logMetricNumber(row.ttft_ms) !== null" class="ttft-value">首字 {{ logLatency(row.ttft_ms) }}</span></template></el-table-column>
          <el-table-column v-if="visibleColumns.includes('ids')" label="请求 / 响应 ID" min-width="225"><template #default="{ row }"><div class="optional-field"><span>客户端关联</span><code>{{ clientRequestIdentity(row).value || '—' }}</code><span>上游请求</span><code>{{ row.request_id || '—' }}</code><span>响应</span><code>{{ row.response_id || '—' }}</code><span>记录</span><code>{{ row.event_id || row.id || '—' }}</code></div></template></el-table-column>
          <el-table-column v-if="visibleColumns.includes('ip')" label="客户端 IP" min-width="140"><template #default="{ row }"><span class="optional-text">{{ row.ip || '—' }}</span></template></el-table-column>
          <el-table-column v-if="visibleColumns.includes('ua')" label="User-Agent" min-width="230"><template #default="{ row }"><span class="optional-text" :title="row.user_agent">{{ row.user_agent || '—' }}</span></template></el-table-column>
          <el-table-column v-if="visibleColumns.includes('groups')" label="请求时分组" min-width="150"><template #default="{ row }"><div v-if="row.groups?.length" class="optional-groups"><el-button v-for="id in row.groups" :key="id" link type="primary" @click="openRelated('group', id)">{{ id }}</el-button></div><span v-else class="cell-muted">未记录</span></template></el-table-column>
          <el-table-column v-if="visibleColumns.includes('observations')" label="链路观测" min-width="230"><template #default="{ row }"><div class="observation-cell"><span>发送 <code :title="row.sent_model">{{ row.sent_model || '未记录' }}</code></span><span>返回 <code :title="row.returned_model">{{ row.returned_model || '未记录' }}</code></span><span>入站 <code>{{ observationSummary(row).inbound || '未记录' }}</code></span><span>上游 <code>{{ observationSummary(row).upstream || '未记录' }}</code></span><small>{{ observationAttemptLabel(row) }}{{ row.attempts_truncated === true ? ' · 尝试已截断' : '' }}</small><small v-if="row.error_truncated === true" class="observation-inline-warning">错误文本已截断</small></div></template></el-table-column>
          <el-table-column label="执行结果" min-width="184"><template #default="{ row }"><div class="result-status"><span class="status-label" :class="'status-' + row.status"><span class="status-dot" />{{ statusLabel(row.status) }}</span><span class="http-code" :class="{ 'http-code-ok': row.http_code >= 200 && row.http_code < 300 }">HTTP {{ row.http_code ?? '—' }}</span><LogSeverityBadge :record="row" /></div><div v-if="friendlyFailure(row)" class="error-summary" :title="`${friendlyFailure(row).label}：${row.error || '未记录原始错误，请查看详情。'}`"><span class="error-category">{{ friendlyFailure(row).label }}</span><span class="error-excerpt">{{ row.error || '未记录原始错误，请查看详情。' }}</span></div></template></el-table-column>
          <el-table-column label="" fixed="right" width="70" align="center"><template #default="{ row }"><el-button link type="primary" :aria-label="'查看 ' + rowFullTime(row.time) + ' 的日志详情'" @click="openDetail(row, $event)">详情</el-button></template></el-table-column>
        </el-table>

        <div class="mobile-log-list" :class="'density-' + density">
          <article v-for="(row, index) in items" :key="`${row.time}-${index}`" class="mobile-log-card">
            <div class="mobile-log-top"><time>{{ rowTime(row.time).date }} <strong>{{ rowTime(row.time).time }}</strong></time><span class="status-label" :class="'status-' + row.status"><span class="status-dot" />{{ statusLabel(row.status) }}</span></div>
            <div v-if="visibleColumns.includes('model') || visibleColumns.includes('latency')" class="mobile-log-model"><span v-if="visibleColumns.includes('model')" class="model-name" :title="row.model || '未记录模型'"><el-icon aria-hidden="true"><Cpu /></el-icon><span>{{ row.model || '未记录模型' }}</span></span><span v-if="visibleColumns.includes('latency')" class="latency-value" :title="`总耗时：${logLatency(row.latency_ms)}`"><el-icon aria-hidden="true"><Clock /></el-icon>{{ logLatency(row.latency_ms) }}</span></div>
            <div v-if="visibleColumns.includes('model')" class="mobile-metadata"><LogModelConsistency :record="row" /><span v-if="row.returned_model || row.upstream_model">{{ row.returned_model ? '返回' : '上游声明' }} {{ row.returned_model || row.upstream_model }}</span><span v-if="row.service_tier" class="service-tier-metadata" :title="`服务层：${row.service_tier} · ${row.service_tier_source === 'response' ? '响应已报告' : row.service_tier_source === 'request' ? '来自请求，未确认响应值' : '来源未记录'}`">{{ row.service_tier_source === 'response' ? '响应' : row.service_tier_source === 'request' ? '请求' : '记录' }} {{ row.service_tier }}</span><span v-if="reasoningRoute(row).label" :title="reasoningRoute(row).title">{{ reasoningRoute(row).label }}</span><span v-if="logMetricNumber(row.ttft_ms) !== null">首字 {{ logLatency(row.ttft_ms) }}</span></div><div v-if="visibleColumns.includes('identity')" class="mobile-log-identity"><el-button v-if="row.account || row.account_id" link type="primary" class="identity-link" @click="viewAccount(row)">{{ row.account || row.account_id }}</el-button><span v-else class="cell-muted">未记录账号</span><span class="identity-key"><span class="field-prefix">KEY</span>{{ row.key_name || '—' }}</span></div>
            <div v-if="visibleColumns.includes('tokens')" class="mobile-log-tokens token-pills"><span class="token-line token-input" :title="`输入 Token：${logExactTokenCount(row.input_tokens)}`"><el-icon aria-hidden="true"><Bottom /></el-icon><span class="token-kind">输入</span><b>{{ logTokenCount(row.input_tokens) }}</b></span><span class="token-line token-output" :title="`输出 Token：${logExactTokenCount(row.output_tokens)}`"><el-icon aria-hidden="true"><Top /></el-icon><span class="token-kind">输出</span><b>{{ logTokenCount(row.output_tokens) }}</b></span><span class="token-line token-cache" :title="`缓存读取 Token：${logExactTokenCount(row.cached_tokens)}${logCacheRatio(row) !== null ? ' · 读取命中率 ' + logPercent(logCacheRatio(row)) : ''}`"><el-icon aria-hidden="true"><Coin /></el-icon><span class="token-kind">读取</span><b>{{ logTokenCount(row.cached_tokens) }}</b></span><span class="token-line token-write" :title="`缓存创建 Token：${logExactTokenCount(row.cache_write_tokens)}`"><span class="token-kind">创建</span><b>{{ logTokenCount(row.cache_write_tokens) }}</b></span></div>
            <div v-if="visibleColumns.includes('observations')" class="mobile-log-observations observation-cell"><span>发送模型 <code>{{ row.sent_model || '未记录' }}</code></span><span>返回模型 <code>{{ row.returned_model || '未记录' }}</code></span><span>入站路径 <code>{{ observationSummary(row).inbound || '未记录' }}</code></span><span>上游路径 <code>{{ observationSummary(row).upstream || '未记录' }}</code></span><small>{{ observationAttemptLabel(row) }}{{ row.attempts_truncated === true ? ' · 尝试已截断' : '' }}</small><small v-if="row.error_truncated === true" class="observation-inline-warning">错误文本已截断</small></div>
            <dl v-if="['ids', 'ip', 'ua', 'groups'].some(key => visibleColumns.includes(key))" class="mobile-optional-fields"><template v-if="visibleColumns.includes('ids')"><dt>请求 ID</dt><dd>{{ row.request_id || '—' }}</dd><dt>响应 ID</dt><dd>{{ row.response_id || '—' }}</dd><dt>记录 ID</dt><dd>{{ row.event_id || row.id || '—' }}</dd></template><template v-if="visibleColumns.includes('ip')"><dt>客户端 IP</dt><dd>{{ row.ip || '—' }}</dd></template><template v-if="visibleColumns.includes('ua')"><dt>User-Agent</dt><dd>{{ row.user_agent || '—' }}</dd></template><template v-if="visibleColumns.includes('groups')"><dt>请求时分组</dt><dd><span v-if="!row.groups?.length">未记录</span><el-button v-for="id in row.groups || []" :key="id" link type="primary" @click="openRelated('group', id)">{{ id }}</el-button></dd></template></dl>
            <div v-if="friendlyFailure(row)" class="mobile-log-error"><div class="mobile-error-heading"><span class="error-category">{{ friendlyFailure(row).label }}</span><LogSeverityBadge :record="row" /></div><p>{{ row.error || '未记录原始错误，请查看详情。' }}</p></div>
            <dl v-if="visibleColumns.includes('ids')" class="mobile-optional-fields"><dt>客户端关联 ID</dt><dd>{{ clientRequestIdentity(row).value || '未记录' }}</dd></dl>
            <div class="mobile-log-bottom"><span>{{ row.endpoint || '—' }} · {{ requestMode(row) }} <span class="http-code">HTTP {{ row.http_code ?? '—' }}</span></span><el-button link type="primary" :aria-label="'查看 ' + rowFullTime(row.time) + ' 的日志详情'" @click="openDetail(row, $event)">详情</el-button></div>
          </article>
          <div v-if="!items.length" class="logs-empty"><el-icon><Document /></el-icon><strong>{{ emptyTitle }}</strong><p>{{ emptyDescription }}</p><el-button v-if="!loading && hasFilters && !loadError" size="small" @click="reset">重置查询条件</el-button></div>
        </div>
      </div>

      <div class="log-pagination">
        <el-select v-model="pageSize" aria-label="每页日志条数" :disabled="loading" @change="search"><el-option v-for="size in [20, 50, 100, 200]" :key="size" :value="size" :label="size + ' 条 / 页'" /></el-select>
        <div class="log-page-controls"><span>第 {{ page }} 页</span><el-button :disabled="!canNavigate || page === 1" @click="firstPage">首页</el-button><el-button :disabled="!canNavigate || !canGoBack" @click="previousPage">上一页</el-button><el-button :disabled="!canNavigate || !hasMore" @click="nextPage">下一页</el-button></div>
      </div>
      </section>
    </div>
    <p class="log-export-note">全量统计和导出覆盖全部匹配的留存明细。当前页脱敏导出仅保留时间、状态及用量，不含身份、模型和原始错误。</p>
    <LogDataCoverage />

    <LogErrorReviewDialog v-model="errorReviewVisible" :filters="appliedSnapshot.params" :timezone="displayTimezone" @detail="openReviewDetail" />
    <LogIngressErrorsDialog v-model="ingressVisible" :query="appliedSnapshot.params" :timezone="displayTimezone" />
    <LogCleanupDialog v-model="cleanupVisible" :query="scopedParams" @changed="load(true)" />
    <el-dialog v-model="detailVisible" title="日志详情" width="min(900px, calc(100vw - 32px))" class="detail-dialog log-detail-dialog" align-center append-to-body @closed="restoreFocus">
      <template v-if="detail">
        <div class="detail-status"><div><span class="status-label" :class="'status-' + detail.status"><span class="status-dot" />{{ statusLabel(detail.status) }}</span><span class="http-code">HTTP {{ detail.http_code ?? '—' }}</span><LogSeverityBadge :record="detail" /></div><strong>{{ detail.model || '未记录模型' }}</strong><LogModelConsistency :record="detail" /><p>{{ rowFullTime(detail.time) }} · {{ detail.endpoint || '—' }} · {{ requestMode(detail) }}</p></div>
        <section v-if="detail.status === 'error' || detail.status === 'aborted'" class="detail-severity"><h3>管理错误级别</h3><LogSeverityBadge :record="detail" explain /></section>
        <p class="detail-note" role="status">{{ detailLoading ? '正在读取服务端完整详情，先显示列表快照…' : detailSource === 'server' ? detailScope.as_of ? '服务端留存记录详情 · 与列表共享固定记录快照' : '服务端留存记录详情 · 独立错误处理视图读取当前留存记录，不使用主列表快照' : '当前页返回的请求快照' }}</p>
        <div v-if="detailError" class="detail-load-error"><el-alert :title="detailError + '；以下保留列表快照。'" type="error" :closable="false" /><el-button v-if="!detailExpired" size="small" @click="loadDetail()">重试详情</el-button><el-button v-else size="small" @click="detailVisible = false; refresh()">关闭并刷新日志</el-button></div>
        <section v-if="friendlyFailure(detail)" class="detail-failure"><h3>{{ friendlyFailure(detail).label }}</h3><p>{{ friendlyFailure(detail).explanation }}</p></section>
        <section class="detail-section"><h3>请求信息</h3><dl class="log-detail"><dt>账号</dt><dd><el-button v-if="detail.account || detail.account_id" link type="primary" class="wrap-link" @click="viewAccount(detail)">{{ detail.account || detail.account_id }}</el-button><span v-else>—</span></dd><template v-if="detail.account_id"><dt>账号 ID</dt><dd class="detail-mono">{{ detail.account_id }}</dd></template><template v-if="detail.account_name_snapshot && detail.account_name_snapshot !== detail.account"><dt>请求时账号</dt><dd>{{ detail.account_name_snapshot }}</dd></template><dt>KEY 名称</dt><dd><el-button v-if="detail.key_name || detail.key_id" link type="primary" class="wrap-link" title="按稳定 ID 查看当前 KEY" aria-label="查看 KEY 关联资源" @click="openRelated('key', detail.key_id, detail.key_name)">{{ detail.key_name || detail.key_id }}</el-button><span v-else>—</span></dd><template v-if="detail.key_id"><dt>KEY ID</dt><dd class="detail-mono">{{ detail.key_id }}</dd></template><dt>分组</dt><dd><span v-if="!detail.groups?.length">—</span><el-button v-for="id in detail.groups || []" :key="id" link type="primary" class="wrap-link" :aria-label="'查看分组 ' + id" @click="openRelated('group', id)">{{ id }}</el-button></dd><dt>请求模型</dt><dd>{{ detail.model || '—' }}</dd><dt>上游模型</dt><dd>{{ detail.upstream_model || '未记录' }}</dd><dt>请求服务层</dt><dd>{{ detail.requested_service_tier || '未记录' }}</dd><dt>服务层记录值</dt><dd>{{ detail.service_tier || '未记录' }}<small v-if="detail.service_tier" class="detail-note"> · {{ detail.service_tier_source === 'response' ? '响应已报告' : detail.service_tier_source === 'request' ? '来自请求，未确认响应值' : '来源未记录' }}</small></dd><dt>推理档位</dt><dd class="reasoning-route" :title="reasoningRoute(detail).title">{{ reasoningRoute(detail).path }}<small class="detail-note"> · 请求 → 发送 → 响应</small></dd><dt>推理强度记录值</dt><dd>{{ detail.reasoning_effort || '未记录' }}<small v-if="detail.reasoning_effort" class="detail-note"> · {{ detail.reasoning_effort_source === 'response' ? '响应已报告' : detail.reasoning_effort_source === 'request' ? '来自请求，未确认响应值' : '来源未记录' }}</small></dd><dt>请求类型</dt><dd>{{ requestMode(detail) }}</dd><dt>原生压缩</dt><dd>{{ typeof detail.native_compaction === 'boolean' ? detail.native_compaction ? '已触发' : '未触发' : '未记录' }}</dd><dt>端点</dt><dd>{{ detail.endpoint || '—' }}</dd><template v-if="detail.event_id || detail.id"><dt>记录 ID</dt><dd class="detail-mono identifier-value">{{ detail.event_id || detail.id }}<el-button link type="primary" size="small" aria-label="复制记录 ID" @click="copyIdentifier(detail.event_id || detail.id)">复制</el-button></dd></template></dl></section>
        <section class="detail-section client-correlation"><h3>客户端关联</h3><dl class="log-detail"><dt>客户端关联 ID</dt><dd class="detail-mono identifier-value">{{ clientRequestIdentity(detail).value || '未记录' }}<el-button v-if="clientRequestIdentity(detail).value" link type="primary" size="small" aria-label="复制客户端关联 ID" @click="copyIdentifier(clientRequestIdentity(detail).value)">复制</el-button></dd><dt>来源</dt><dd>{{ clientRequestIdentity(detail).source }}</dd><dt>关联范围</dt><dd>{{ clientRequestIdentity(detail).scope === 'connection' ? 'WebSocket 连接' : clientRequestIdentity(detail).scope === 'request' ? 'HTTP 请求' : '未记录' }}</dd></dl><p class="detail-note">{{ clientRequestIdentity(detail).explanation }} 未传入或未合法采集时保持空缺，不补生成标识。</p></section>
        <section class="detail-section"><h3>上游关联</h3><dl class="log-detail"><dt>上游请求 ID</dt><dd class="detail-mono identifier-value">{{ detail.request_id || '未记录' }}<el-button v-if="detail.request_id" link type="primary" size="small" aria-label="复制上游请求 ID" @click="copyIdentifier(detail.request_id)">复制</el-button></dd><dt>响应 ID</dt><dd class="detail-mono identifier-value">{{ detail.response_id || '未记录' }}<el-button v-if="detail.response_id" link type="primary" size="small" aria-label="复制响应 ID" @click="copyIdentifier(detail.response_id)">复制</el-button></dd></dl><p class="detail-note">上游未返回或历史未采集的标识保持空缺，不用本地记录 ID 替代。</p></section>
        <LogObservationDetails :record="detail" />
        <section class="detail-section"><h3>用量与耗时</h3><dl class="log-detail"><dt>输入 Token</dt><dd :title="logExactTokenCount(detail.input_tokens)">{{ logTokenCount(detail.input_tokens) }}</dd><dt>输出 Token</dt><dd :title="logExactTokenCount(detail.output_tokens)">{{ logTokenCount(detail.output_tokens) }}</dd><dt>缓存读取</dt><dd :title="logExactTokenCount(detail.cached_tokens)">{{ logTokenCount(detail.cached_tokens) }}</dd><dt>缓存创建</dt><dd :title="logExactTokenCount(detail.cache_write_tokens)">{{ logTokenCount(detail.cache_write_tokens) }}</dd><dt>推理 Token</dt><dd :title="logExactTokenCount(detail.reasoning_tokens)">{{ logTokenCount(detail.reasoning_tokens) }}</dd><dt>上游总 Token</dt><dd :title="logExactTokenCount(detail.usage_total_tokens)">{{ logTokenCount(detail.usage_total_tokens) }}</dd><dt>读取命中率</dt><dd>{{ logPercent(logCacheRatio(detail)) }}<span class="detail-inline-note">读取 / 输入 Token · 不含创建</span></dd><dt>响应头等待</dt><dd>{{ logLatency(detail.upstream_header_ms) }}</dd><dt>总耗时</dt><dd>{{ logLatency(detail.latency_ms) }}</dd><dt>首字延迟</dt><dd :class="{ 'cell-muted': logMetricNumber(detail.ttft_ms) === null }">{{ logMetricNumber(detail.ttft_ms) !== null ? logLatency(detail.ttft_ms) : '未记录' }}</dd></dl><p class="detail-note">“—”表示未记录或无法计算。读取与创建分别展示，读取命中率不包含创建；缓存属于输入、推理属于输出，均不重复计入总 Token；总耗时与首字延迟分别计量。</p></section>
        <section class="detail-section image-token-details"><h3>图片 Token 观测</h3><dl class="log-detail"><template v-for="[field,source,label] in imageTokenFields" :key="field"><dt>{{ label }}</dt><dd>{{ imageTokenObservation(detail,field,source).precisionExceeded ? '超出前端精确显示范围' : logTokenCount(imageTokenObservation(detail,field,source).value) }}<small class="image-token-source">{{ imageTokenObservation(detail,field,source).source || '来源未记录' }}</small><p class="detail-note">{{ imageTokenObservation(detail,field,source).explanation }}</p></dd></template></dl></section>
        <section class="detail-section"><h3>连接信息</h3><dl class="log-detail"><dt>代理</dt><dd>{{ detail.proxy_name || '—' }}</dd><dt>IP</dt><dd>{{ detail.ip || '—' }}<LogIpGeo v-if="detailVisible && detail.ip" :ip="detail.ip" /></dd><dt>User-Agent</dt><dd class="detail-mono">{{ detail.user_agent || '未记录' }}</dd></dl></section>
        <section class="detail-section"><details class="observation-details"><summary>采集来源与协议元数据</summary><dl class="log-detail"><template v-for="[key, label] in [['upstream_model_source', '模型来源'], ['service_tier_source', '服务层来源'], ['reasoning_effort_source', '推理强度来源'], ['first_output_kind', '首个输出事件'], ['terminal_status', '响应终态'], ['incomplete_reason', '未完成原因'], ['error_source', '错误来源'], ['error_phase', '错误阶段']]" :key="key"><dt>{{ label }}</dt><dd class="detail-mono">{{ detail[key] || '未记录' }}</dd></template><dt>Rollout 单位</dt><dd>{{ logTokenCount(detail.rollout_budget_units) }}</dd><dt>上游用量数值</dt><dd>{{ detail.usage_amount || '未记录' }}</dd></dl><p class="detail-note">Rollout 单位和上游用量数值由上游定义，按其原始单位展示。缺失的来源或响应字段保持未知。</p></details></section>
<section class="detail-section"><h3>已保存错误</h3><p v-if="detail.error" class="detail-note" :class="{ 'observation-inline-warning': detail.error_truncated === true }">{{ truncationLabel(detail.error_truncated) }}{{ detail.error_truncated === true ? '，仅展示已保存片段。' : detail.error_truncated === false ? '；此标记仅针对记录时的诊断内容。' : '，不能保证正文完整。' }}</p><pre id="log-raw-error" class="detail-error" :class="{ 'has-error': detail.error, 'error-collapsed': detail.error?.length > 600 && !errorExpanded }">{{ detail.error || '无错误信息' }}</pre><el-button v-if="detail.error?.length > 600" link type="primary" class="error-expand" :aria-expanded="errorExpanded" aria-controls="log-raw-error" @click="errorExpanded = !errorExpanded">{{ errorExpanded ? '收起错误内容' : detail.error_truncated === true ? '展开已保存错误' : '展开完整错误' }}</el-button></section>
      </template>
      <LogErrorReviewPanel v-if="detail" :record="detail" :active="detailVisible" />
      <template #footer><el-button @click="detailVisible = false">关闭详情</el-button></template>
    </el-dialog>
    <LogRelatedResource v-model="relatedVisible" :resource="relatedResource" @filter="relatedFilter" @manage="manageRelated" />
    <LogResourceLookup v-model="lookupVisible" :field="lookupField" :params="scopedParams" @select="selectResource" />
    <el-dialog v-model="columnsVisible" title="使用记录显示列" width="min(420px, calc(100vw - 24px))"><p class="detail-note">请求时间、执行结果及详情始终显示。以下设置同时应用到桌面表格和手机卡片。</p><div class="column-choices"><label v-for="[value, label] in columnOptions" :key="value"><input type="checkbox" :checked="draftColumns.includes(value)" @change="toggleColumn(value, $event.target.checked)" /><span>{{ label }}</span></label></div><el-button link @click="draftColumns = [...defaultColumns]">恢复默认显示</el-button><template #footer><el-button @click="columnsVisible = false">取消</el-button><el-button type="primary" @click="applyColumns">应用显示设置</el-button></template></el-dialog>
    <el-dialog v-model="exportVisible" title="导出全部筛选结果" width="min(500px, calc(100vw - 24px))" :close-on-click-modal="!fullExportLoading" :close-on-press-escape="!fullExportLoading" :show-close="!fullExportLoading">
      <p class="export-scope-description">导出当前已应用筛选下的全部留存明细，包含其他分页的记录{{ statsCurrent && statistics ? '（当前统计 ' + logTokenCount(statistics.total_requests) + ' 条）' : '' }}。</p>
      <p class="detail-note">默认脱敏导出：保留匿名标识、模型、用量和错误类别，移除账号 / KEY 名称、代理、IP、UA 与原始错误。所有导出均不包含凭据。</p>
      <label class="export-identity-choice"><input type="checkbox" :checked="exportIdentities" :disabled="fullExportLoading" @change="exportIdentities = $event.target.checked" /><span>包含身份管理字段（名称、IP、UA 与脱敏错误）</span></label>
      <label class="export-format"><span>文件格式</span><el-select v-model="exportFormat" aria-label="导出文件格式" :disabled="fullExportLoading"><el-option label="CSV · 表格文件" value="csv" /><el-option label="JSONL · 逐行记录" value="jsonl" /></el-select></label>
      <p v-if="exportProgressText" class="export-progress" role="status">{{ exportProgressText }}</p>
      <el-alert v-if="fullExportError" :title="fullExportError" type="error" :closable="false" />
      <template #footer><el-button @click="cancelFullExport">{{ fullExportLoading ? '取消导出' : '取消' }}</el-button><el-button type="primary" :loading="fullExportLoading" :disabled="!canNavigate" @click="exportAll">导出全部记录</el-button></template>
    </el-dialog>
    <el-dialog v-model="saveVisible" title="保存常用筛选" width="min(420px, calc(100vw - 24px))">
      <p class="dialog-intro">保存当前筛选内容，下次可从高级筛选中直接使用。</p>
      <el-form label-position="top" @submit.prevent="saveFilter"><el-form-item label="筛选名称"><el-input v-model="filterName" maxlength="40" show-word-limit placeholder="例如：最近失败请求" aria-label="筛选名称" @keyup.enter="saveFilter" /></el-form-item></el-form>
      <p class="detail-note">最多保存 20 组；同名筛选将更新。时间范围按当前值保存。</p>
      <template #footer><el-button @click="saveVisible = false">取消</el-button><el-button type="primary" @click="saveFilter">保存</el-button></template>
    </el-dialog>
  </div>
</template>

<style scoped>
.image-token-source{display:block;margin-top:4px;font-size:11px;color:var(--text-3);overflow-wrap:anywhere}.image-token-details .detail-note{margin:5px 0 0}
.result-status,.detail-status>div { flex-wrap:wrap; }.mobile-error-heading { display:flex;align-items:center;flex-wrap:wrap;gap:7px; }.detail-severity { margin:15px 0; }.detail-severity h3 { font-size:12px;font-weight:500;color:var(--text-2);margin:0 0 8px; }
.observation-cell { display:grid;gap:4px;font-size:12px;line-height:1.7;min-width:0;color:var(--text-3); }.observation-cell>span { display:grid;grid-template-columns:auto minmax(0,1fr);gap:7px; }.observation-cell code { color:var(--text-2);font-family:inherit;overflow-wrap:anywhere; }.observation-cell small { font-size:11px; }.observation-inline-warning { color:var(--warning,#a66c11); }.mobile-log-observations { margin:12px 0;padding:10px 0;border-top:1px solid var(--card-border);border-bottom:1px solid var(--card-border); }.mobile-log-observations small { font-size:12px; }
.optional-field { display: grid; grid-template-columns: 30px minmax(0, 1fr); gap: 5px 8px; font-size: 11px; color: var(--text-3); }.optional-field code { color: var(--text-2); font-size: 12px; overflow-wrap: anywhere; }.optional-text { color: var(--text-2); font-size: 12px; overflow-wrap: anywhere; }.optional-groups { display: grid; justify-items: start; }.optional-groups .el-button { white-space: normal; overflow-wrap: anywhere; height: auto; line-height: 1.6; margin: 0; font-size: 12px; }.mobile-optional-fields { display: grid; grid-template-columns: 82px minmax(0, 1fr); gap: 7px 9px; border-top: 1px solid var(--card-border); padding-top: 10px; margin: 10px 0; font-size: 12px; }.mobile-optional-fields dt { color: var(--text-3); }.mobile-optional-fields dd { margin: 0; color: var(--text-2); overflow-wrap: anywhere; }.mobile-optional-fields .el-button { white-space: normal; height: auto; line-height: 1.6; font-size: 12px; }
.field-label-action { display: flex; justify-content: space-between; align-items: center; gap: 8px; }.field-label-action button { border: 0; background: transparent; color: var(--primary, #409eff); padding: 0; font: inherit; font-size: 11px; cursor: pointer; }.export-identity-choice { display: flex; align-items: flex-start; gap: 8px; font-size: 12px; line-height: 1.7; margin: 15px 0; color: var(--text-2); }.export-identity-choice input { margin-top: 4px; accent-color: var(--primary, #409eff); }
.logs-page { min-width: 0; container: log-layout / inline-size; --log-primary: var(--primary, #409eff); --log-primary-soft: var(--primary-soft, #ecf5ff); }
.log-workspace { padding: 0; overflow: hidden; }
.log-toolbar-top { display: flex; justify-content: space-between; align-items: center; gap: 16px; padding: 0 20px; border-bottom: 1px solid var(--card-border, #e4eaf3); }
.log-status-tabs { display: flex; align-self: stretch; gap: 22px; flex-shrink: 0; }
.log-status-tabs button { display: flex; align-items: center; gap: 6px; min-height: 52px; padding: 0 1px; margin-bottom: -1px; border: 0; border-bottom: 2px solid transparent; color: var(--text-2); background: transparent; font: inherit; font-size: 13px; cursor: pointer; }
.log-status-tabs button.active { color: var(--log-primary); border-bottom-color: var(--log-primary); font-weight: 500; }
.status-dot { display: inline-block; width: 6px; height: 6px; border-radius: 50%; background: currentColor; flex-shrink: 0; }
.status-ok, .outcome-ok { color: var(--success, #529b2e); }
.status-error, .outcome-error { color: var(--danger, #d9363e); }
.status-aborted { color: var(--text-3, #607088); }
.time-presets { display: flex; gap: 3px; padding: 3px 0; }
.time-presets button, .table-density button { border: 0; border-radius: 4px; padding: 5px 9px; background: transparent; color: var(--text-3); font: inherit; font-size: 11px; white-space: nowrap; cursor: pointer; }
.time-presets button.active { color: var(--log-primary); background: var(--log-primary-soft); }
.log-query { padding: 18px 20px; border-bottom: 1px solid var(--card-border, #e4eaf3); }
.log-primary-filters { display: grid; grid-template-columns: minmax(150px, 1.1fr) minmax(130px, 1fr) minmax(130px, 1fr) auto; align-items: end; gap: 10px; }
.log-primary-filters > * { min-width: 0; }
.log-filter-field { display: flex; flex-direction: column; gap: 7px; }
.log-filter-field > span { color: var(--text-2); font-size: 12px; font-weight: 500; }
.log-query-actions { display: flex; align-items: center; gap: 8px; }
.log-query-actions .el-button + .el-button, .log-pagination .el-button + .el-button { margin-left: 0; }
.advanced-toggle { display: inline-flex; gap: 7px; align-items: center; white-space: nowrap; font: inherit; font-size: 12px; color: var(--text-2); background: none; border: 0; padding: 7px 2px 7px 7px; cursor: pointer; }
.advanced-toggle:hover { color: var(--log-primary); }
.log-advanced-filters { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr) minmax(155px, .9fr); align-items: end; gap: 14px; padding-top: 16px; margin-top: 16px; border-top: 1px dashed var(--card-border, #e4eaf3); }
.date-field { display: flex; flex-direction: column; gap: 7px; min-width: 0; }
.date-field > span { font-size: 12px; color: var(--text-2); }
.date-field small { color: var(--text-3); font-size: 11px; margin-left: 4px; }
.date-field input { min-width: 0; width: 100%; box-sizing: border-box; height: 32px; padding: 0 9px; border: 1px solid var(--el-border-color, #dcdfe6); border-radius: 4px; color: var(--text-1); background: var(--card-bg); font: inherit; font-size: 12px; }
.date-field input:focus { outline: 1px solid var(--log-primary); border-color: var(--log-primary); }
.advanced-help { display: flex; flex-wrap: wrap; align-items: center; gap: 9px; }
.advanced-help > span { font-size: 11px; color: var(--text-3); }
.saved-filters { display: flex; align-items: center; flex-wrap: wrap; gap: 10px; grid-column: 1 / -1; }
.saved-filters .el-select { width: 230px; max-width: 100%; }
.query-notices { display: flex; align-items: center; flex-wrap: wrap; gap: 10px; margin-top: 12px; }
.query-notices .el-tag { max-width: min(100%, 290px); height: 25px; }
.query-notices .el-tag :deep(.el-tag__content) { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; min-width: 0; }
.filter-chips-label { color: var(--text-3); font-size: 11px; }
.draft-notice { font-size: 11px; line-height: 1.5; color: var(--warning, #b88230); }
.log-metrics-strip { display: grid; grid-template-columns: minmax(135px, .8fr) repeat(3, minmax(0, 1fr)); align-items: center; gap: 20px; padding: 17px 20px; border-bottom: 1px solid var(--card-border, #e4eaf3); background: var(--el-fill-color-lighter, #fafbfd); }
.metrics-scope { display: flex; flex-direction: column; gap: 5px; }
.metrics-scope strong { font-size: 12px; font-weight: 500; color: var(--text-2); }
.metrics-scope > span { font-size: 11px; color: var(--text-3); }
.log-metric { display: grid; grid-template-columns: auto 1fr; align-items: baseline; column-gap: 8px; row-gap: 4px; border-left: 1px solid var(--card-border); padding-left: 20px; min-width: 0; }
.log-metric > span { grid-column: 1 / -1; color: var(--text-3); font-size: 11px; }
.log-metric > strong { font-size: 19px; font-weight: 600; line-height: 1.4; font-variant-numeric: tabular-nums; color: var(--text-1); white-space: nowrap; }
.log-metric > strong.metric-cache { color: var(--log-primary); }
.log-metric > small { color: var(--text-3); font-size: 10px; line-height: 1.5; }
.metric-help { width: 14px; height: 14px; margin-left: 3px; padding: 0; border: 1px solid var(--text-3); border-radius: 50%; font-family: inherit; font-size: 10px; line-height: 12px; color: var(--text-3); background: transparent; cursor: help; vertical-align: middle; }
.metrics-stale { opacity: .72; }
.log-results-heading { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 12px 20px; min-height: 47px; box-sizing: border-box; }
.query-scope { font-size: 12px; line-height: 1.6; color: var(--text-3); }
.log-result-options { display: flex; align-items: center; gap: 14px; flex-shrink: 0; }
.page-outcomes { color: var(--text-3); font-size: 11px; }
.table-density { display: flex; border: 1px solid var(--card-border, #e4eaf3); border-radius: 4px; padding: 1px; }
.table-density button { font-size: 11px; padding: 3px 7px; }
.table-density button.active { color: var(--log-primary); background: var(--log-primary-soft); }
.log-load-error { display: flex; align-items: center; gap: 12px; padding: 0 20px 12px; }
.log-load-error .el-alert { min-width: 0; }
.log-records { min-height: 120px; }
.log-table { width: 100%; font-size: 13px; }
.log-table :deep(.el-table__header-wrapper th) { background: var(--el-fill-color-light); font-size: 12px; font-weight: 500; height: 40px; color: var(--text-2); }
.log-table :deep(.el-table__row td) { vertical-align: middle; }
.log-table.density-comfortable :deep(.el-table__row td) { padding-top: 14px; padding-bottom: 14px; }
.log-table.density-compact :deep(.el-table__row td) { padding-top: 7px; padding-bottom: 7px; }
.log-table :deep(.cell) { padding: 0 14px; line-height: 1.55; }
.request-time { display: flex; flex-direction: column; gap: 1px; font-variant-numeric: tabular-nums; }
.request-time > span { font-size: 11px; color: var(--text-3); }
.request-time strong { font-weight: 500; font-size: 13px; color: var(--text-1); letter-spacing: .02em; }
.cell-secondary { font-size: 11px; color: var(--text-3); margin-top: 5px; }
.request-endpoint { display: flex; gap: 7px; align-items: center; flex-wrap: wrap; }
.mode-mark { color: var(--text-3); font-size: 11px; padding-left: 7px; border-left: 1px solid var(--card-border); }
.identity-link { display: block; max-width: 100%; padding: 0; height: auto; font-size: 13px; text-align: left; }
.identity-link :deep(span) { display: block; overflow: hidden; white-space: nowrap; text-overflow: ellipsis; }
.identity-key { display: block; overflow: hidden; white-space: nowrap; text-overflow: ellipsis; color: var(--text-3); font-size: 11px; }
.field-prefix { font-size: 11px; margin-right: 6px; color: var(--text-3); }
.cell-muted { color: var(--text-3); font-size: 11px; }
.model-name { display: -webkit-box; -webkit-box-orient: vertical; -webkit-line-clamp: 2; overflow: hidden; overflow-wrap: anywhere; color: var(--text-1); font-size: 13px; font-weight: 500; line-height: 1.5; }
.token-line { display: grid; grid-template-columns: 26px minmax(0, 1fr); column-gap: 12px; line-height: 1.55; font-variant-numeric: tabular-nums; }
.token-line > span { color: var(--text-3); font-size: 11px; text-align: left; }
.token-line strong { font-weight: 400; font-size: 12px; color: var(--text-1); }
.token-cache strong { color: var(--text-3); }
.cache-ratio { color: var(--log-primary); font-size: 10px; font-variant-numeric: tabular-nums; margin-top: 3px; }
.latency-value { font-variant-numeric: tabular-nums; white-space: nowrap; color: var(--text-2); font-size: 12px; }
.result-status, .status-label { display: flex; align-items: center; gap: 7px; }
.result-status { flex-wrap: wrap; gap: 10px; }
.status-label { font-size: 12px; font-weight: 500; }
.http-code { color: var(--text-3); font-size: 12px; font-variant-numeric: tabular-nums; font-weight: 400; white-space: nowrap; }
.error-summary { margin-top: 6px; font-size: 11px; line-height: 1.5; }
.error-category { color: var(--danger); font-size: 12px; }
.error-excerpt { display: -webkit-box; -webkit-box-orient: vertical; -webkit-line-clamp: 2; overflow: hidden; overflow-wrap: anywhere; color: var(--text-3); font-size: 12px; margin-top: 2px; }
.log-table.density-compact .error-excerpt { -webkit-line-clamp: 1; }
.log-pagination { display: flex; justify-content: space-between; align-items: center; flex-wrap: wrap; gap: 10px; padding: 15px 20px; }
.log-pagination > .el-select { width: 122px; }
.log-page-controls { display: flex; align-items: center; gap: 8px; }
.log-page-controls > span { color: var(--text-3); font-size: 12px; margin-right: 6px; }
.log-page-controls .el-button { font-size: 12px; }
.log-export-note { margin: 12px 1px 0; color: var(--text-3); font-size: 11px; line-height: 1.7; }
.logs-empty { display: grid; justify-items: center; gap: 10px; padding: 45px 18px; line-height: 1.7; }
.logs-empty > .el-icon { color: var(--text-3); font-size: 23px; margin-bottom: 2px; }
.logs-empty strong { color: var(--text-2); font-size: 13px; font-weight: 500; }
.logs-empty p { color: var(--text-3); font-size: 12px; margin: 0; white-space: normal; text-align: center; }
.mobile-log-list { display: none; }
.detail-status { padding-bottom: 17px; border-bottom: 1px solid var(--card-border); }
.detail-status > div { display: flex; align-items: center; gap: 12px; }
.detail-status > strong { display: block; margin-top: 14px; font-size: 19px; font-weight: 500; overflow-wrap: anywhere; color: var(--text-1); }
.detail-status p { font-size: 11px; color: var(--text-3); margin: 7px 0 0; }
.detail-note { font-size: 11px; color: var(--text-3); line-height: 1.6; }
.detail-failure { padding: 14px 16px; margin: 18px 0; background: var(--el-fill-color-light); border-left: 2px solid var(--danger); border-radius: 3px; }
.detail-failure h3 { margin: 0 0 6px; font-size: 12px; font-weight: 500; color: var(--danger); }
.detail-failure p { margin: 0; font-size: 12px; line-height: 1.7; color: var(--text-2); }
.detail-section { padding: 18px 0; border-bottom: 1px solid var(--card-border); }
.detail-section:last-child { border-bottom: 0; }
.detail-section h3 { margin: 0 0 15px; font-size: 12px; font-weight: 500; color: var(--text-1); }
.log-detail-dialog .detail-status { padding: 4px 0 18px; }
@media (min-width: 768px) { .log-detail-dialog .log-detail { grid-template-columns: 100px minmax(0,1fr) 100px minmax(0,1fr); column-gap: 18px; } .log-detail-dialog .client-correlation .log-detail { grid-template-columns: 110px minmax(0,1fr); } }
.log-detail { display: grid; grid-template-columns: 92px minmax(0, 1fr); gap: 12px; font-size: 12px; line-height: 1.7; margin: 0; }
.log-detail dt { color: var(--text-3); }
.log-detail dd { margin: 0; overflow-wrap: anywhere; white-space: pre-wrap; }
.detail-mono { font-family: ui-monospace, SFMono-Regular, Consolas, monospace; font-size: 11px; }
.detail-inline-note { font-size: 11px; color: var(--text-3); margin-left: 10px; }
.wrap-link { white-space: normal; height: auto; overflow-wrap: anywhere; text-align: left; font-size: 12px; }
.detail-error { margin: 0; padding: 13px; background: var(--el-fill-color-light); border: 1px solid var(--card-border); border-radius: 4px; white-space: pre-wrap; overflow-wrap: anywhere; word-break: break-word; font: 11px/1.8 ui-monospace, SFMono-Regular, Consolas, monospace; color: var(--text-3); }
.detail-error.has-error { color: var(--text-2); }
.detail-error.error-collapsed { max-height: 160px; overflow: auto; }
.error-expand { margin-top: 9px; font-size: 12px; }
.logs-page button:focus-visible { outline: 2px solid var(--log-primary); outline-offset: 3px; }
@media (max-width: 1150px) { .log-toolbar-top { align-items: flex-start; flex-wrap: wrap; gap: 0; } .time-presets { padding: 11px 0; } .log-primary-filters { grid-template-columns: repeat(3, minmax(0, 1fr)); } .log-query-actions { grid-column: 1 / -1; justify-content: flex-end; } .log-result-options { gap: 9px; } }
@container log-layout (max-width: 900px) {
  .log-metrics-strip { grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 10px; }
  .metrics-scope { grid-column: 1 / -1; flex-direction: row; justify-content: space-between; }
  .log-metric { grid-template-columns: minmax(0, 1fr); row-gap: 3px; }
  .log-metric:nth-child(2) { border-left: 0; padding-left: 0; }
  .log-metric > small { white-space: nowrap; }
}
@container log-layout (max-width: 1000px) {
  .log-toolbar-top { padding: 0 14px; } .log-status-tabs { gap: 21px; width: 100%; } .log-status-tabs button { min-height: 45px; font-size: 12px; } .time-presets { width: 100%; padding: 9px 0; } .time-presets button { flex: 1; padding: 5px 7px; font-size: 11px; }
  .log-query { padding: 14px; } .log-primary-filters { grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 10px 8px; } .log-primary-filters > .log-filter-field:first-child { grid-column: 1 / -1; } .log-query-actions { justify-content: flex-start; margin-top: 4px; } .log-query-actions .el-button { min-width: 65px; } .advanced-toggle { margin-left: auto; }
  .log-metrics-strip { grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 10px; padding: 12px 14px; } .metrics-scope { grid-column: 1 / -1; flex-direction: row; justify-content: space-between; } .log-metric { padding-left: 10px; grid-template-columns: minmax(0, 1fr); row-gap: 3px; } .log-metric:nth-child(2) { border-left: 0; padding-left: 0; } .log-metric > strong { font-size: 16px; } .log-metric > small { font-size: 10px; } .log-metric > span { font-size: 10px; }
  .log-advanced-filters { grid-template-columns: minmax(0, 1fr); gap: 12px; } .advanced-help { justify-content: space-between; } .date-field input { height: 35px; } .saved-filters { gap: 8px; }
  .log-results-heading { padding: 10px 14px; align-items: flex-start; flex-wrap: wrap; gap: 7px; } .log-result-options { width: 100%; justify-content: space-between; } .query-scope { font-size: 12px; } .table-density { margin-left: auto; }
  .log-table { display: none; } .mobile-log-list { display: block; } .mobile-log-card { padding: 16px 14px; border-bottom: 1px solid var(--card-border); } .mobile-log-card:first-child { border-top: 1px solid var(--card-border); }
  .mobile-log-top { display: flex; justify-content: space-between; align-items: center; gap: 8px; } .mobile-log-top time { font-size: 12px; color: var(--text-3); font-variant-numeric: tabular-nums; } .mobile-log-top time strong { color: var(--text-2); font-weight: 400; margin-left: 3px; }
  .mobile-log-model { display: flex; align-items: flex-start; justify-content: space-between; gap: 12px; margin: 13px 0 8px; } .mobile-log-model .model-name { font-size: 13px; } .mobile-log-model .latency-value { flex-shrink: 0; font-size: 12px; }
  .mobile-log-identity { display: grid; grid-template-columns: 1fr; gap: 4px; } .mobile-log-identity .identity-link { font-size: 13px; }
  .mobile-log-tokens { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 8px; padding: 10px 0; margin-top: 8px; border-top: 1px solid var(--el-border-color-lighter); color: var(--text-3); font-size: 11px; } .mobile-log-tokens > span { display: flex; flex-wrap: wrap; align-items: baseline; gap: 5px; } .mobile-log-tokens b { font-weight: 400; color: var(--text-1); font-size: 12px; font-variant-numeric: tabular-nums; overflow-wrap: anywhere; }
  .mobile-log-tokens small { color: var(--log-primary); font-size: 10px; }
  .mobile-log-error { border-left: 2px solid var(--danger); padding: 3px 0 3px 9px; margin: 1px 0 10px; } .mobile-log-error p { display: -webkit-box; -webkit-box-orient: vertical; -webkit-line-clamp: 2; overflow: hidden; overflow-wrap: anywhere; margin: 4px 0 0; color: var(--text-3); font-size: 12px; line-height: 1.6; }
  .mobile-log-bottom { display: flex; align-items: center; justify-content: space-between; gap: 8px; } .mobile-log-bottom > span { font-size: 11px; color: var(--text-3); line-height: 1.6; } .mobile-log-bottom .http-code { margin-left: 7px; } .mobile-log-bottom .el-button { height: auto; font-size: 11px; }
  .mobile-log-list.density-compact .mobile-log-card { padding-top: 11px; padding-bottom: 11px; } .mobile-log-list.density-compact .mobile-log-error p { -webkit-line-clamp: 1; }
  .log-pagination { padding: 14px; gap: 12px; } .log-page-controls { gap: 5px; flex-wrap: wrap; } .log-page-controls .el-button { padding: 7px 9px; font-size: 12px; } .log-page-controls > span { margin-right: 3px; font-size: 12px; } .log-pagination > .el-select { width: 118px; } .log-load-error { padding: 0 14px 12px; align-items: flex-start; } .log-load-error .el-button { padding: 7px 8px; }
  .log-export-note { font-size: 11px; }
}
@media (max-width: 700px) { .log-detail { grid-template-columns: 76px minmax(0, 1fr); gap: 11px; } }
@container log-layout (min-width: 650px) and (max-width: 1000px) {
  .log-toolbar-top { flex-wrap: nowrap; align-items: center; gap: 16px; }
  .log-status-tabs { width: auto; flex-shrink: 0; }
  .time-presets { width: auto; }
  .time-presets button { flex: initial; }
  .log-primary-filters { grid-template-columns: repeat(3, minmax(0, 1fr)); }
  .log-primary-filters > .log-filter-field:first-child { grid-column: auto; }
  .log-query-actions { justify-content: flex-end; }
  .advanced-toggle { margin-left: 0; }
  .log-results-heading { flex-wrap: nowrap; align-items: center; }
  .log-result-options { width: auto; gap: 12px; }
}
@media (max-width: 360px) { .log-status-tabs { justify-content: space-between; gap: 10px; } .log-pagination { justify-content: center; } .log-page-controls { justify-content: center; width: 100%; } .log-primary-filters { grid-template-columns: minmax(0, 1fr); } .log-primary-filters > .log-filter-field:first-child { grid-column: auto; } .log-metrics-strip { column-gap: 6px; } .log-metric { padding-left: 7px; } .log-metric > strong { font-size: 14px; } }
.observation-details summary { font-size: 12px; color: var(--text-2); cursor: pointer; }.observation-details[open] summary { margin-bottom: 15px; }
.column-choices { display: grid; gap: 14px; margin: 20px 0; }.column-choices label { display: flex; align-items: center; gap: 8px; font-size: 13px; color: var(--text-2); }.column-choices input { accent-color: var(--log-primary); }.columns-button { font-size: 11px; }
.mobile-metadata { display: flex; align-items: baseline; flex-wrap: wrap; gap: 4px 10px; color: var(--text-3); font-size: 11px; line-height: 1.5; margin: -2px 0 9px; overflow-wrap: anywhere; }
.observed-model { display: block; margin-top: 4px; color: var(--text-3); font-size: 11px; line-height: 1.5; overflow-wrap: anywhere; }.model-metadata { display: block; margin-top: 3px; color: var(--text-3); font-size: 11px; }.ttft-value { display: block; margin-top: 5px; font-size: 11px; font-variant-numeric: tabular-nums; color: var(--text-3); }.identifier-value .el-button { margin-left: 8px; }
.advanced-scope-note { grid-column: 1 / -1; margin: 0; font-size: 11px; line-height: 1.7; color: var(--text-3); }
.log-sort-toolbar { display: flex; align-items: center; flex-wrap: wrap; gap: 9px; padding: 13px 20px 0; }
.log-sort-toolbar > label:first-child { display: flex; align-items: center; gap: 9px; color: var(--text-3); font-size: 12px; }
.log-sort-toolbar > label:first-child .el-select { width: 118px; }.log-sort-toolbar > .el-select { width: 172px; }
.exact-total-option { display: flex; align-items: center; gap: 5px; color: var(--text-3); font-size: 12px; margin-left: auto; cursor: pointer; }.exact-total-option input { accent-color: var(--log-primary); }
.detail-load-error { display: flex; align-items: flex-start; gap: 8px; margin: 12px 0; }.detail-load-error .el-alert { min-width: 0; }.detail-load-error .el-button { flex-shrink: 0; }
.export-scope-description { font-size: 13px; line-height: 1.8; color: var(--text-2); margin: 0 0 8px; }.export-format { display: flex; gap: 14px; align-items: center; margin: 20px 0; font-size: 12px; color: var(--text-2); }.export-format .el-select { flex: 1; }.export-progress { font-size: 12px; line-height: 1.8; color: var(--text-3); }
@media(max-width:700px) { .log-sort-toolbar { padding: 13px 14px 0; }.log-sort-toolbar > .el-select { flex: 1; min-width: 140px; }.exact-total-option { margin-left: 0; }.log-advanced-filters { grid-template-columns: repeat(2, minmax(0, 1fr)); }.date-field, .advanced-help, .saved-filters { grid-column: 1 / -1; }.log-filter-field .el-select { width: 100%; } }
@media(max-width:360px) { .log-advanced-filters { grid-template-columns: minmax(0, 1fr); }.log-sort-toolbar > label:first-child { flex: 1; }.log-sort-toolbar > label:first-child .el-select { flex: 1; } }
/* Keep the query, usage overview and records visually distinct. */
.log-workspace { display: flex; flex-direction: column; gap: 22px; border: 0; overflow: visible; padding: 0; margin-top: 26px; background: transparent; box-shadow: none; }
.log-query-panel,.log-analytics-panel,.log-results-panel { min-width: 0; border: 1px solid var(--card-border); border-radius: 14px; background: var(--card-bg); box-shadow: 0 2px 5px rgb(17 24 39 / 2%); overflow: hidden; }
.log-toolbar-top { padding: 0 24px; gap: 24px; }
.log-status-tabs { gap: 28px; }
.log-status-tabs button { min-height: 60px; font-size: 12px; }
.log-status-tabs button.active { font-weight: 600; }
.time-presets { padding: 4px; border: 1px solid var(--card-border); border-radius: 8px; background: var(--bg); }
.time-presets button { padding: 6px 10px; border-radius: 5px; font-size: 10px; }
.time-presets button.active { background: var(--card-bg); color: var(--text-1); box-shadow: 0 1px 4px rgb(17 24 39 / 7%); }
.log-query { padding: 24px; border-bottom: 0; }
.log-primary-filters { gap: 16px; }
.log-filter-field { gap: 9px; }
.log-filter-field > span { font-size: 11px; color: var(--text-2); }
.field-label-action button { font-size: 10px; opacity: .85; }
.log-query-actions .el-button { font-size: 12px; }
.advanced-toggle { font-size: 11px; padding-left: 3px; }
.log-advanced-filters { background: color-mix(in srgb,var(--bg) 60%,var(--card-bg)); border: 1px solid var(--card-border); border-radius: 10px; padding: 20px; margin-top: 22px; gap: 18px 16px; }
.date-field > span { font-size: 11px; }
.date-field input { height: 36px; border-radius: 7px; }
.advanced-scope-note { padding: 13px; background: var(--card-bg); border: 1px solid var(--card-border); border-radius: 7px; font-size: 10px; }
.query-notices { margin-top: 17px; gap: 8px; }
.query-notices .el-tag { border-radius: 5px; font-size: 10px; }
.log-analytics-panel :deep(.usage-analytics) { border: 0; }
.log-management-toolbar { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 16px; padding: 23px 24px 20px; }
.section-kicker { display: block; color: var(--text-3); font-size: 8px; letter-spacing: 1.5px; font-weight: 550; margin-bottom: 7px; }
.log-records-title h2 { color: var(--text-1); font-size: 15px; font-weight: 600; letter-spacing: -.3px; margin: 0; }
.log-management-actions { display: flex; flex-wrap: wrap; align-items: center; gap: 16px; }
.log-management-actions .el-button { margin-left: 0; color: var(--text-2); font-size: 11px; font-weight: 400; }
.log-management-actions .el-button:hover { color: var(--primary); }
.tool-divider { width: 1px; height: 13px; background: var(--card-border); }
.log-sort-toolbar { padding: 14px 24px; background: color-mix(in srgb,var(--bg) 50%,var(--card-bg)); border-top: 1px solid var(--card-border); border-bottom: 1px solid var(--card-border); gap: 10px; }
.log-sort-toolbar > label:first-child { font-size: 11px; }
.exact-total-option { font-size: 11px; }
.log-results-heading { padding: 17px 24px; }
.query-scope { font-size: 11px; }
.table-density { padding: 3px; border-radius: 6px; background: var(--bg); }
.table-density button { font-size: 10px; padding: 4px 8px; border-radius: 4px; }
.table-density button.active { background: var(--card-bg); color: var(--text-1); box-shadow: 0 1px 4px rgb(17 24 39 / 7%); }
.log-result-options .el-button { font-size: 11px; }
.log-table :deep(.el-table__header-wrapper th) { background: color-mix(in srgb,var(--bg) 66%,var(--card-bg)); height: 32px; color: var(--text-3); font-size: 11px; }
.log-table :deep(.cell) { padding: 0 10px; line-height: 1.4; }
.log-table.density-comfortable :deep(.el-table__row td) { padding-top: 6px; padding-bottom: 6px; }
.log-table.density-compact :deep(.el-table__row td) { padding-top: 3px; padding-bottom: 3px; }
.request-time > span { font-size: 10px; }.request-time strong { font-size: 12px; font-weight: 550; }
.identity-link { font-size: 12px; font-weight: 550; color: var(--text-1); }
.identity-link:hover { color: var(--primary); }
.identity-key { font-size: 10px; margin-top: 4px; }
.model-name { font-size: 12px; }
.status-label { font-size: 11px; }.http-code { font-size: 10px; }.latency-value { font-size: 11px; }
.token-line { column-gap: 9px; line-height: 1.4; }.token-line > span { font-size: 11px; }.token-line strong { font-size: 12px; }
.log-pagination { padding: 18px 24px; border-top: 1px solid var(--card-border); }
.log-export-note { margin: 17px 2px 0; font-size: 10px; }
.log-load-error { padding: 0 24px 16px; }
.detail-status { padding: 20px; border: 1px solid var(--card-border); background: var(--bg); border-radius: 12px; }
.detail-status > strong { font-size: 21px; margin-top: 15px; letter-spacing: -.5px; }
.detail-status p { line-height: 1.8; }
.detail-section { padding: 23px 0; }
.detail-section h3 { font-weight: 600; font-size: 12px; margin-bottom: 18px; }
.log-detail { gap: 14px; }
.detail-failure { border-radius: 8px; border: 1px solid color-mix(in srgb,var(--danger) 18%,var(--card-border)); background: color-mix(in srgb,var(--danger) 4%,var(--card-bg)); }
.detail-error { border-radius: 8px; padding: 16px; }
.observation-details { padding: 15px; border: 1px solid var(--card-border); border-radius: 8px; background: var(--bg); }
.observation-details summary { font-size: 11px; }
@media (max-width: 1150px) { .log-toolbar-top { padding: 15px 24px; gap: 12px; align-items: center; }.log-status-tabs button { min-height: 38px; margin-bottom: 0; }.log-query-actions { justify-content: flex-end; }.time-presets { width: auto; }.log-management-actions { gap: 13px; } }
@container log-layout (max-width: 1000px) { .log-toolbar-top { padding: 16px; gap: 12px; }.log-status-tabs { gap: 24px; }.time-presets { padding: 4px; }.time-presets button { padding: 6px 9px; }.log-query { padding: 20px 16px; }.log-primary-filters { gap: 14px 10px; }.log-advanced-filters { padding: 16px; gap: 16px; }.log-management-toolbar { padding: 20px 16px 17px; gap: 17px; }.log-management-actions { gap: 13px; }.log-sort-toolbar { padding: 12px 16px; }.log-results-heading { padding: 15px 16px; }.log-result-options { flex-wrap: wrap; gap: 10px; }.mobile-log-card { margin: 0 15px 12px; border: 1px solid var(--card-border); border-radius: 10px; padding: 18px 16px; background: var(--card-bg); }.mobile-log-card:first-child { border-top: 1px solid var(--card-border); }.mobile-log-tokens { padding: 11px 12px; background: var(--bg); border: 1px solid var(--card-border); border-radius: 7px; margin: 13px 0; }.mobile-log-model { margin-top: 15px; }.log-pagination { padding: 16px; }.log-workspace { gap: 18px; }.log-load-error { padding: 0 16px 15px; } }
/* Compact operational records: semantic accents keep dense rows easy to scan. */
.log-workspace { gap: 14px; margin-top: 17px; }
.log-query-panel,.log-analytics-panel,.log-results-panel { border-radius: 11px; }
.log-toolbar-top { padding: 0 19px; gap: 20px; }
.log-status-tabs { gap: 24px; }
.log-status-tabs button { min-height: 43px; font-size: 11px; }
.log-status-tabs button.active::before { content: ''; width: 5px; height: 5px; background: var(--primary); border-radius: 2px; }
.log-status-tabs button.active:has(.status-dot)::before { display: none; }
.time-presets { border-radius: 6px; padding: 2px; }
.time-presets button { padding: 4px 8px; }
.log-query { padding: 14px 19px 16px; }
.log-primary-filters { gap: 12px; }
.log-filter-field { gap: 6px; }
.log-query-actions { gap: 7px; }
.log-query-actions .el-button { min-height: 32px; }
.log-advanced-filters { padding: 16px; gap: 13px; margin-top: 14px; }
.query-notices { margin-top: 11px; }
.log-management-toolbar { padding: 13px 18px; min-height: 57px; gap: 10px; }
.log-records-title { display: flex; align-items: center; flex-wrap: wrap; gap: 9px; min-width: 0; }
.log-records-title h2 { font-size: 13px; white-space: nowrap; }
.records-title-icon { display: inline-grid; place-items: center; width: 27px; height: 27px; border-radius: 7px; color: var(--badge-blue,var(--primary)); background: var(--badge-blue-soft,var(--primary-soft)); border: 1px solid color-mix(in srgb,var(--badge-blue,var(--primary)) 13%,transparent); font-size: 14px; }
.log-records-title .query-scope { font-size: 10px; padding-left: 9px; border-left: 1px solid var(--card-border); line-height: 1.6; }
.log-management-actions { gap: 9px; }
.log-management-actions .el-button { font-size: 10px; }
.log-management-actions :deep(.el-button > .el-icon) { font-size: 13px; }
.tool-divider { height: 12px; }
.log-list-controls { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 8px 16px; padding: 9px 18px; border-top: 1px solid var(--card-border); border-bottom: 1px solid var(--card-border); background: color-mix(in srgb,var(--bg) 55%,var(--card-bg)); }
.log-sort-toolbar { background: none; border: 0; padding: 0; gap: 7px; }
.log-sort-toolbar > label:first-child { font-size: 10px; gap: 7px; }
.log-sort-toolbar > label:first-child .el-select { width: 108px; }
.log-sort-toolbar > .el-select { width: 148px; }
.exact-total-option { gap: 4px; font-size: 10px; margin-left: 3px; }
.exact-total-option input { width: 12px; height: 12px; margin: 0; }
.log-results-heading { padding: 0; min-height: 0; }
.log-result-options { gap: 10px; }
.page-outcomes { font-size: 10px; }
.log-result-options :deep(.el-button),.columns-button { font-size: 10px; }
.table-density { padding: 2px; border-radius: 5px; }
.table-density button { font-size: 9px; padding: 4px 7px; }
.log-table :deep(.el-table__header-wrapper th) { height: 32px; font-size: 11px; }
.log-table :deep(.cell) { padding: 0 11px; line-height: 1.45; }
.log-table.density-comfortable :deep(.el-table__row td) { padding-top: 5px; padding-bottom: 5px; height: 48px; box-sizing: border-box; }
.log-table.density-compact :deep(.el-table__row td) { padding-top: 3px; padding-bottom: 3px; height: 40px; box-sizing: border-box; }
.request-time { flex-direction: row; align-items: baseline; flex-wrap: nowrap; gap: 5px; white-space: nowrap; }
.request-time > span { font-size: 9px; letter-spacing: -.25px; }
.request-time strong { font-size: 11px; font-weight: 550; }
.request-endpoint { gap: 4px; margin-top: 6px; font-size: 9px; flex-wrap: nowrap; overflow: hidden; }
.request-endpoint > span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.request-endpoint > span:first-child { color: var(--text-2); padding: 2px 4px; border-radius: 4px; background: var(--bg); border: 1px solid var(--card-border); }
.mode-mark { padding-left: 5px; font-size: 9px; }
.identity-primary { display: flex; align-items: center; gap: 6px; min-width: 0; }
.identity-avatar { width: 22px; height: 22px; flex: 0 0 22px; display: inline-grid; place-items: center; color: var(--badge-blue,var(--primary)); background: var(--badge-blue-soft,var(--primary-soft)); border-radius: 6px; font-size: 12px; }
.identity-primary .identity-link { min-width: 0; max-width: calc(100% - 28px); font-size: 11px; font-weight: 500; padding: 0; }
.identity-primary .identity-link :deep(span) { min-width: 0; }
.identity-key { display: flex; align-items: center; gap: 4px; padding-left: 28px; margin-top: 4px; font-size: 9px; }
.identity-key > .el-icon { font-size: 10px; color: var(--text-3); flex-shrink: 0; }
.identity-key .field-prefix { font-size: 8px; margin-right: 0; }
.model-name { display: inline-flex; align-items: center; gap: 5px; max-width: 100%; min-height: 24px; padding: 3px 6px; border-radius: 5px; background: var(--badge-purple-soft,var(--primary-soft)); color: var(--badge-purple,var(--primary)); font-size: 10px; font-weight: 550; line-height: 1.5; vertical-align: middle; box-sizing: border-box; }
.model-name > .el-icon { font-size: 12px; flex-shrink: 0; }
.model-name > span { overflow: hidden; white-space: nowrap; text-overflow: ellipsis; min-width: 0; }
.model-chip-row { display: flex; align-items: center; flex-wrap: wrap; gap: 4px; max-width: 100%; margin-top: 4px; }
.model-chip-row:empty { display: none; }
.observed-model { display: block; margin-top: 0; min-width: 0; font-size: 10px; line-height: 17px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: var(--text-3); }
.model-metadata { display: inline-block; padding: 0 4px; border-radius: 3px; border: 1px solid color-mix(in srgb,var(--badge-blue,var(--primary)) 12%,transparent); background: var(--badge-blue-soft,var(--primary-soft)); color: var(--badge-blue,var(--primary)); font-size: 8px; line-height: 15px; margin-top: 0; white-space: nowrap; }
.reasoning-chip { background: var(--badge-purple-soft,var(--primary-soft)); border-color: color-mix(in srgb,var(--badge-purple,var(--primary)) 12%,transparent); color: var(--badge-purple,var(--primary)); }
.token-column-heading { display: flex; align-items: center; gap: 8px; flex-wrap: nowrap; }
.token-column-legend { display: flex; align-items: center; gap: 3px; color: var(--text-3); font-size: 8px; font-weight: 400; white-space: nowrap; }
.token-column-legend i { display: inline-block; width: 4px; height: 4px; border-radius: 1px; margin-left: 3px; }
.legend-input { background: var(--badge-blue,var(--primary)); }.legend-output { background: var(--badge-purple,var(--primary)); }.legend-cache { background: var(--badge-amber,var(--warning)); }.legend-write { background: var(--badge-green,var(--success)); }
.token-pills { display: flex; align-items: center; flex-wrap: nowrap; gap: 5px; min-width: 0; overflow-x: auto; scrollbar-width: thin; padding-bottom: 1px; }
.token-pills .token-line { display: inline-flex; flex: 0 0 auto; align-items: center; gap: 4px; min-width: 37px; max-width: none; padding: 4px 6px; border-radius: 5px; line-height: 1.25; font-size: 10px; white-space: nowrap; font-variant-numeric: tabular-nums; box-sizing: border-box; }
.token-pills .token-line > .el-icon { flex-shrink: 0; font-size: 11px; }
.token-pills .token-line > strong,.token-pills .token-line > b { color: inherit; font-size: 10px; font-weight: 550; line-height: 1.25; letter-spacing: -.1px; }
.token-input { color: var(--badge-blue,var(--primary)); background: var(--badge-blue-soft,var(--primary-soft)); }
.token-output { color: var(--badge-purple,var(--primary)); background: var(--badge-purple-soft,var(--primary-soft)); }
.token-cache { color: var(--badge-amber,var(--warning)); background: var(--badge-amber-soft,var(--bg)); }
.token-write { color: var(--badge-green,var(--success)); background: var(--badge-green-soft,var(--bg)); }
.cache-ratio { display: flex; align-items: center; gap: 5px; margin-top: 5px; font-size: 8px; color: var(--text-3); }
.cache-ratio-track { width: 33px; height: 3px; background: var(--badge-amber-soft,var(--bg)); border-radius: 3px; overflow: hidden; }
.cache-ratio-track i { height: 100%; display: block; background: var(--badge-amber,var(--warning)); border-radius: inherit; }
.latency-value { display: inline-flex; align-items: center; gap: 4px; padding: 3px 6px; min-height: 24px; color: var(--badge-green,var(--success)); background: var(--badge-green-soft,var(--primary-soft)); border-radius: 5px; font-size: 10px; font-weight: 500; box-sizing: border-box; }
.latency-value > .el-icon { font-size: 11px; flex-shrink: 0; }
.ttft-value { font-size: 9px; margin-top: 4px; padding-left: 2px; }
.result-status { gap: 4px; flex-wrap: nowrap; }
.status-label { display: inline-flex; align-items: center; gap: 4px; padding: 3px 6px; border-radius: 5px; min-height: 22px; box-sizing: border-box; font-size: 10px; white-space: nowrap; }
.status-label.status-ok { color: var(--badge-green,var(--success)); background: var(--badge-green-soft,var(--primary-soft)); }
.status-label.status-error { color: var(--danger); background: color-mix(in srgb,var(--danger) 9%,var(--card-bg)); }
.status-label.status-aborted { color: var(--text-3); background: var(--bg); }
.status-dot { width: 4px; height: 4px; }
.http-code { display: inline-block; padding: 2px 4px; border: 1px solid var(--card-border); border-radius: 4px; font-size: 8px; color: var(--text-3); line-height: 1.5; background: var(--card-bg); }
.http-code-ok { color: var(--badge-green,var(--success)); border-color: color-mix(in srgb,var(--badge-green,var(--success)) 20%,var(--card-border)); }
.result-status :deep(.severity-badge) { font-size: 9px; padding: 2px 4px; white-space: nowrap; }
.error-summary { display: flex; align-items: center; gap: 5px; max-width: 100%; margin-top: 5px; line-height: 1.4; overflow: hidden; }
.error-category { font-size: 9px; flex-shrink: 0; }
.error-excerpt { display: block; margin-top: 0; min-width: 0; font-size: 9px; overflow: hidden; white-space: nowrap; text-overflow: ellipsis; }
.log-table :deep(.el-table__fixed-right .el-button),.log-table :deep(.el-table__body .el-button.is-link) { font-size: 11px; }
.log-pagination { padding: 11px 18px; }
.log-export-note { margin-top: 10px; font-size: 9px; }
.log-load-error { padding: 12px 18px; }
@container log-layout (max-width: 1100px) { .log-list-controls { align-items: flex-start; gap: 8px; }.log-sort-toolbar { flex: 1 1 100%; }.log-results-heading { flex: 1 1 100%; }.log-result-options { width: 100%; justify-content: flex-end; }.page-outcomes { margin-right: auto; }.log-records-title .query-scope { font-size: 9px; }.log-management-actions { gap: 7px; } }
@container log-layout (max-width: 1000px) { .log-workspace { gap: 13px; }.log-toolbar-top { padding: 9px 14px; gap: 9px; }.log-status-tabs { gap: 20px; }.log-status-tabs button { min-height: 30px; }.time-presets { padding: 2px; }.time-presets button { padding: 4px 7px; }.log-query { padding: 12px 14px 14px; }.log-primary-filters { gap: 10px; }.log-query-actions { margin-top: 0; }.log-management-toolbar { padding: 12px 14px; gap: 8px; min-height: 50px; }.log-list-controls { padding: 8px 14px; gap: 7px; }.log-sort-toolbar { padding: 0; }.log-results-heading { padding: 0; }.mobile-log-card { margin: 0 11px 8px; padding: 13px 12px; border-radius: 8px; }.mobile-log-card:first-child { margin-top: 10px; }.mobile-log-top time,.mobile-log-top time strong { font-size: 10px; }.mobile-log-model { gap: 8px; margin: 10px 0 7px; }.mobile-log-model .model-name { font-size: 10px; }.mobile-log-model .latency-value { font-size: 10px; }.mobile-metadata { margin: 0 0 8px; gap: 3px 7px; font-size: 9px; }.mobile-log-identity { grid-template-columns: minmax(0,1fr) minmax(0,1fr); gap: 8px; align-items: center; }.mobile-log-identity .identity-link { font-size: 11px; }.mobile-log-identity .identity-key { display: block; padding-left: 0; margin: 0; font-size: 9px; text-align: right; }.mobile-log-tokens.token-pills { display: flex; justify-content: flex-start; gap: 6px; padding: 0 0 2px; margin: 10px 0 9px; border: 0; background: transparent; border-radius: 0; overflow-x: auto; }.mobile-log-tokens.token-pills > .token-line { flex: 1 0 auto; justify-content: center; padding: 5px 6px; gap: 4px; }.token-pills .token-kind { color: inherit; font-size: 8px; line-height: 1.25; }.mobile-log-error { padding: 2px 0 2px 7px; margin-bottom: 8px; }.mobile-log-error p { font-size: 10px; -webkit-line-clamp: 1; margin-top: 3px; }.mobile-log-bottom > span { font-size: 9px; }.mobile-log-bottom .el-button { font-size: 10px; }.mobile-log-observations { margin: 9px 0; padding: 8px 0; }.mobile-log-observations,.mobile-log-observations small { font-size: 10px; }.mobile-optional-fields { font-size: 10px; }.log-pagination { padding: 11px 14px; } }
@media (max-width: 600px) { .log-workspace { margin-top: 13px; }.log-records-title { gap: 7px; }.records-title-icon { width: 24px; height: 24px; font-size: 12px; }.log-records-title h2 { font-size: 12px; }.log-records-title .query-scope { padding-left: 7px; }.log-management-actions { justify-content: space-between; gap: 5px; }.log-management-actions .el-button { font-size: 9px; }.log-sort-toolbar > label:first-child .el-select { width: 90px; }.log-sort-toolbar > .el-select { width: 130px; min-width: 125px; }.exact-total-option { font-size: 9px; }.log-result-options { gap: 6px; }.log-result-options :deep(.el-button) { font-size: 9px; }.page-outcomes { font-size: 9px; }.table-density button { font-size: 8px; padding: 4px 5px; }.token-pills .token-line > b { font-size: 10px; }.model-chip-row { overflow-x: auto; }.log-advanced-filters { padding: 12px; }.log-status-tabs { gap: 18px; } }
</style>
