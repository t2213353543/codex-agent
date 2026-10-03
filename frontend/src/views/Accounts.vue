<script setup>
import { computed, nextTick, onMounted, onUnmounted, reactive, ref, toRef, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { Check, Document, Download, Refresh, Search, Setting, Upload, UploadFilled, User, ArrowDown, ArrowUp, Close } from '@element-plus/icons-vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api } from '../api'
import { indexResources } from '../utils/resourceManagement'
import { createLatestRequest } from '../utils/latestRequest'
import { formatDuration, formatNumber, formatPercent, formatTime, SHANGHAI_TIMEZONE, shanghaiDayStart, parseShanghaiDateInput } from '../utils/format'
import { accountColumnLabels, lastUsedScopeLabel, defaultAccountColumns, accountEditForm, accountEditPatch, defaultImportConfiguration, validateAccountConfiguration } from '../utils/accountFields'
import { readPreference, writePreference } from '../utils/preferences'
import { importReport, maxImportBytes, parseImportDocuments, redactImportMessage, importRequestSource } from '../utils/importAccounts'
import { externalLeasePreviews, externalLeaseCommitReceipt, externalLeaseSummary, canPrepareDuplicateLease } from '../utils/externalImportLeases'
import PageHeader from '../components/PageHeader.vue'
import ConsoleIcon from '../components/ConsoleIcon.vue'
import AccountListEmpty from '../components/AccountListEmpty.vue'
import AccountBulkDialog from '../components/AccountBulkDialog.vue'
import AccountExportDialog from '../components/AccountExportDialog.vue'
import AccountReauthorizeDialog from '../components/AccountReauthorizeDialog.vue'
import AccountBundleImportDialog from '../components/AccountBundleImportDialog.vue'
import AccountOAuthDialog from '../components/AccountOAuthDialog.vue'
import AccountUpstreamDialog from '../components/AccountUpstreamDialog.vue'
import AccountUsageWindows from '../components/AccountUsageWindows.vue'
import AccountGroupList from '../components/AccountGroupList.vue'
import { officialUsageWindows, readOfficialAccountUsage } from '../utils/accountOfficialUsage'
import AccountSchedulesDialog from '../components/AccountSchedulesDialog.vue'
import AccountLeaseDialog from '../components/AccountLeaseDialog.vue'
import AccountLeaseRestorePanel from '../components/AccountLeaseRestorePanel.vue'
import AccountStatsDialog from '../components/AccountStatsDialog.vue'
import AccountCrsDialog from '../components/AccountCrsDialog.vue'
import AccountBatchUpstreamDialog from '../components/AccountBatchUpstreamDialog.vue'
import AccountBatchReauthorizeDialog from '../components/AccountBatchReauthorizeDialog.vue'
import AccountIdentityPanel from '../components/AccountIdentityPanel.vue'
import AccountRuntimePanel from '../components/AccountRuntimePanel.vue'
import { accountManagement } from '../api/accountManagement'
import { accountDateFilterError, accountSortFields, accountStatusLabel, accountStatusLabels as statusNames, compactAccountFilters, localAccountDate, normalizeAccountFilters, validateAccountDateRange, validateAccountSelection } from '../utils/accountManagement'

const accountViewport = window.matchMedia('(max-width: 767px)')
const narrowTable = ref(accountViewport.matches)
const pinnedViewport = window.matchMedia('(min-width: 1024px)')
const pinnedColumns = ref(pinnedViewport.matches)
function syncPinnedViewport(event) { pinnedColumns.value = event.matches }
function syncAccountViewport(event) { narrowTable.value = event.matches }
const route = useRoute()
const router = useRouter()
const loading = ref(false)
const listPending = ref(false)
const loadError = ref('')
const accounts = ref([])
const accountsTable = ref(null)
const groups = ref([])
const proxies = ref([])
const hasMore = ref(false)
const total = ref(0)
let syncingSelection = false
const listRequest = createLatestRequest()
let loadSequence = 0
let filterTimer
let syncingRoute = false
let active = true
const defaultColumns = defaultAccountColumns
const columnLabels = accountColumnLabels
const columnKeys = Object.keys(columnLabels)
const validColumns = (value) => Array.isArray(value) && value.length > 0 && new Set(value).size === value.length && value.every((key) => columnKeys.includes(key)) && value.includes('name') && value.includes('actions')
const storedColumns = readPreference('accountColumns', [...defaultColumns], validColumns)
// Upgrade the previous six-column default once; keep genuine user choices.
const reducedDefault = ['name', 'status', 'expires_at', 'groups', 'proxies', 'actions']
const visibleColumns = ref(storedColumns.length === reducedDefault.length && reducedDefault.every((key, i) => storedColumns[i] === key) ? [...defaultColumns] : storedColumns)
const columnDialog = ref(false)
const filters = reactive(normalizeAccountFilters(route.query))
const invalidDateFilter = ref(accountDateFilterError(route.query))
watch(visibleColumns, (value) => writePreference('accountColumns', value), { deep: true })
const colVisible = (key) => visibleColumns.value.includes(key) || ['id', 'status', 'last_used_at'].includes(key) && filters.sort_by === key
const officialUsage = ref({}), officialUsageLoading = ref(false), officialPending = ref(new Set())
let officialController, officialSequence = 0, officialSignature = '', officialNextRead = 0, officialTimer, officialVersionRetryAt = 0
watch(() => colVisible('quota') || narrowTable.value, () => loadOfficialUsage())
async function loadOfficialUsage(force = false) {
  const visible = colVisible('quota') || narrowTable.value
  const signature = accounts.value.map(row => `${row.id}:${row.version}`).join('|')
  if (visible && signature === officialSignature && !force && (officialUsageLoading.value || Date.now() < officialNextRead)) return
  officialController?.abort(); const sequence = ++officialSequence
  officialUsageLoading.value = false; officialPending.value = new Set()
  if (!visible || !accounts.value.length) { officialSignature = ''; return }
  officialSignature = signature; officialNextRead = Date.now() + 300000
  officialController = new AbortController(); officialUsageLoading.value = true
  officialPending.value = new Set(accounts.value.map(row => row.id))
  officialUsage.value = Object.fromEntries(accounts.value.map(row => [row.id, officialUsage.value[row.id]?.version === row.version ? officialUsage.value[row.id] : { version: row.version }]))
  const versions = new Map(accounts.value.map(row => [row.id, row.version]))
  let versionConflict = false
  try {
    await readOfficialAccountUsage(accounts.value, { send: accountManagement.upstreamBatch, signal: officialController.signal, force, onResult: (id, result) => {
      if (sequence !== officialSequence) return
      if (result.code === 'version_conflict') versionConflict = true
      officialUsage.value = { ...officialUsage.value, [id]: { ...result, version: versions.get(id) } }
      officialPending.value = new Set([...officialPending.value].filter(value => value !== id))
      const resetTimes = officialUsageWindows(result.snapshot).map(window => window.reset_at * 1000).filter(time => time > Date.now())
      const expires = result.error ? Date.now() + 30000 : (result.snapshot?.checked_at || Date.now() / 1000) * 1000 + 300000
      officialNextRead = Math.min(officialNextRead, Math.max(Date.now() + 1000, expires), ...resetTimes)
    } })
  } finally { if (sequence === officialSequence) officialUsageLoading.value = false }
  // Background credential rotation changes account versions even with the list
  // on manual refresh. Recover its metadata instead of retrying the old version.
  // Bound recovery so continuous edits cannot cause a tight reload loop.
  if (sequence === officialSequence && versionConflict && Date.now() >= officialVersionRetryAt) {
    officialVersionRetryAt = Date.now() + 30000
    await load({ background: true })
  }
}
function updateOfficialUsage(snapshot) {
  if (snapshot?.kind !== 'usage') return
  const row = accounts.value.find(row => row.id === snapshot.id)
  if (row && snapshot.version === row.version) officialUsage.value = { ...officialUsage.value, [row.id]: { version: row.version, snapshot, error: snapshot.ok ? '' : snapshot.error?.message || '官方用量查询失败' } }
}
const lastUsed = ref({}), lastUsedScope = ref(''), lastUsedLoading = ref(false), lastUsedError = ref('')
let lastUsedController, lastUsedSequence = 0
watch(() => colVisible('last_used_at'), loadLastUsed)
async function loadLastUsed() {
  lastUsedController?.abort(); const sequence = ++lastUsedSequence
  lastUsed.value = {}; lastUsedScope.value = ''; lastUsedError.value = ''; lastUsedLoading.value = false
  if (!colVisible('last_used_at') || !accounts.value.length) return
  lastUsedController = new AbortController(); lastUsedLoading.value = true
  const signal = lastUsedController.signal, ids = accounts.value.map(row => row.id), result = {}
  let scope = ''
  try {
    for (let offset = 0; offset < ids.length; offset += 100) {
      const batch = ids.slice(offset, offset + 100), data = await accountManagement.lastUsed(batch, { signal })
      if (!['durable_usage_events', 'retained_request_details'].includes(data.scope) || scope && scope !== data.scope || !Array.isArray(data.items) || data.items.length !== batch.length || new Set(data.items.map(row => row.id)).size !== batch.length || data.items.some(row => !batch.includes(row.id) || row.at !== null && (!Number.isSafeInteger(row.at) || row.at <= 0))) throw new Error('最近业务请求时间返回不完整')
      scope = data.scope
      for (const row of data.items) result[row.id] = row.at
      if (sequence !== lastUsedSequence) return
    }
    if (sequence === lastUsedSequence) { lastUsed.value = result; lastUsedScope.value = scope }
  } catch (caught) { if (sequence === lastUsedSequence && caught.name !== 'AbortError') lastUsedError.value = caught.message || '最近业务请求时间读取失败' }
  finally { if (sequence === lastUsedSequence) lastUsedLoading.value = false }
}
const lastUsedText = id => lastUsed.value[id] ? formatTime(lastUsed.value[id]) : Object.hasOwn(lastUsed.value, id) ? '此范围无匹配记录' : lastUsedLoading.value ? '读取中' : '未取得'


const search = toRef(filters, 'search')
const statusFilter = toRef(filters, 'status')
const statusShortcuts = [{ value: '', label: '全部状态' }, { value: 'ok', label: '正常' }, { value: 'error', label: '异常' }, { value: 'expired', label: '已过期' }, { value: 'unverified', label: '未验证' }, { value: 'disabled', label: '已停用' }]
function applyStatusShortcut(value) { statusFilter.value = value; savedFilter.value = '' }
const groupFilter = toRef(filters, 'group_id')
const proxyFilter = toRef(filters, 'proxy_id')
const advancedFilters = ref(Object.keys(compactAccountFilters(filters)).some((key) => !['search', 'status', 'sort_by', 'sort_order'].includes(key)))
const activeFilterCount = computed(() => Object.entries(filters).filter(([key, value]) => value !== '' && !key.startsWith('sort_')).length)
const filterChips = computed(() => [
  search.value && { key: 'search', text: `搜索：${search.value}` },
  statusFilter.value && { key: 'status', text: `状态：${statusNames[statusFilter.value]}` },
  groupFilter.value && { key: 'group_id', text: `分组：${groupName(groupFilter.value)}` },
  proxyFilter.value && { key: 'proxy_id', text: `代理：${proxyName(proxyFilter.value)}` },
  filters.tag && { key: 'tag', text: `标签：${filters.tag}` },
  filters.privacy && { key: 'privacy', text: filters.privacy === 'disabled' ? '隐私：已确认关闭训练' : '隐私：状态未知' },
  filters.enabled && { key: 'enabled', text: filters.enabled === 'true' ? '已启用' : '已停用' },
  filters.has_refresh_token && { key: 'has_refresh_token', text: filters.has_refresh_token === 'true' ? '含刷新令牌' : '无刷新令牌' },
  filters.ungrouped && { key: 'ungrouped', text: '未分组' },
  filters.no_proxy && { key: 'no_proxy', text: '直连 / 无代理' },
  filters.expires_after && { key: 'expires_after', text: `到期晚于 ${formatTime(Date.parse(filters.expires_after) / 1000)}` },
  filters.expires_before && { key: 'expires_before', text: `到期早于 ${formatTime(Date.parse(filters.expires_before) / 1000)}` },
].filter(Boolean))
function removeFilterChip(key) { filters[key] = ''; savedFilter.value = '' }
function clearFilters() {
  syncingRoute = true
  Object.assign(filters, normalizeAccountFilters())
  syncingRoute = false
  savedFilter.value = ''
  resetFilteredPage()
}
function accountLogs(account) { router.push({ path: '/logs', query: { account_id: account.id } }) }
function setExpiryFilter(key, value) { const seconds = parseShanghaiDateInput(value); filters[key] = Number.isFinite(seconds) ? new Date(seconds * 1000).toISOString() : '' }
const pageSize = ref([10, 20, 50, 100, 200, 500].includes(Number(route.query.page_size || route.query.limit)) ? Number(route.query.page_size || route.query.limit) : 20)
const page = computed(() => Math.max(1, Math.floor(Number(route.query.page) || 1)))
const pageJump = ref(page.value)
const totalPages = computed(() => Math.max(1, Math.ceil(total.value / pageSize.value)))
const displayAccounts = computed(() => accounts.value)
const pageOverview = computed(() => ({
  ready: accounts.value.filter(account => account.enabled && account.status === 'ok').length,
  active: accounts.value.reduce((sum, account) => sum + (Number(account.in_flight) || 0), 0),
  attention: accounts.value.filter(account => ['error', 'expired', 'unverified'].includes(account.status)).length,
}))
const showTodayUsage = ref(readPreference('accountsTodayUsage', false, value => typeof value === 'boolean'))
const todayUsage = ref({}), todayLoading = ref(false), todayError = ref(''), todayCheckedAt = ref(0)
let todayController, todaySequence = 0
const usageTimezone = SHANGHAI_TIMEZONE
watch(showTodayUsage, value => { writePreference('accountsTodayUsage', value); loadTodayUsage() })
async function loadTodayUsage() {
  todayController?.abort(); const sequence = ++todaySequence
  todayUsage.value = {}; todayError.value = ''; todayLoading.value = false
  if (!showTodayUsage.value || !accounts.value.length) return
  todayController = new AbortController(); todayLoading.value = true
  const signal = todayController.signal, ids = accounts.value.map(row => row.id)
  const now = Math.floor(Date.now() / 1000), from = shanghaiDayStart(now), to = Math.max(from + 1, now)
  const result = {}
  try {
    for (let offset = 0; offset < ids.length; offset += 100) {
      const batch = ids.slice(offset, offset + 100)
      const data = await accountManagement.usageBatch({ ids: batch, from, to }, { signal })
      if (data.scope !== 'retained_request_details' || !Array.isArray(data.items) || data.items.length !== batch.length || new Set(data.items.map(row => row.id)).size !== batch.length || data.items.some(row => !batch.includes(row.id) || !Number.isFinite(row.total_requests))) throw new Error('今日统计返回不完整，请重新查询')
      for (const row of data.items) result[row.id] = row
      if (sequence !== todaySequence) return
    }
    if (sequence === todaySequence) { todayUsage.value = result; todayCheckedAt.value = to }
  } catch (caught) { if (sequence === todaySequence && caught.name !== 'AbortError') todayError.value = caught.message || '今日统计读取失败' }
  finally { if (sequence === todaySequence) todayLoading.value = false }
}
const filterError = computed(() => invalidDateFilter.value || validateAccountDateRange(filters))
function filterQuery() { return { ...compactAccountFilters(filters), page_size: String(pageSize.value) } }
function clearInvalidDateFilter() { invalidDateFilter.value = ''; resetFilteredPage() }
function resetFilteredPage() {
  listRequest.cancel()
  clearTimeout(filterTimer)
  clearSelection()
  router.replace({ query: filterQuery() }).then((failure) => { if (failure) load() })
}
watch([filters, pageSize], () => {
  if (syncingRoute) return
  listRequest.cancel()
  clearSelection()
  clearTimeout(filterTimer)
  filterTimer = setTimeout(resetFilteredPage, 180)
}, { deep: true, flush: 'sync' })
watch(() => route.query, (query) => {
  clearTimeout(filterTimer)
  invalidDateFilter.value = accountDateFilterError(query)
  const next = normalizeAccountFilters(query)
  if (JSON.stringify(compactAccountFilters(next)) !== JSON.stringify(compactAccountFilters(filters))) clearSelection()
  syncingRoute = true
  Object.assign(filters, next)
  pageSize.value = [10, 20, 50, 100, 200, 500].includes(Number(query.page_size || query.limit)) ? Number(query.page_size || query.limit) : 20
  pageJump.value = page.value
  syncingRoute = false
  load()
})
function changePage(direction) { navigatePage(page.value + direction) }
function navigatePage(number) {
  if (loading.value || filterError.value) return
  const target = Math.min(totalPages.value, Math.max(1, Math.floor(Number(number) || 1)))
  router.replace({ query: { ...filterQuery(), page: target > 1 ? String(target) : undefined } })
}
const validFilters = (value) => Array.isArray(value) && value.length <= 20 && value.every((filter) => filter && typeof filter.name === 'string' && filter.name.length <= 60 && (filter.filters && typeof filter.filters === 'object' || typeof filter.search === 'string'))
const savedFilters = ref(readPreference('accountFilters', [], validFilters))
const savedFilter = ref('')
async function saveFilter() {
  try {
    const { value } = await ElMessageBox.prompt('为当前筛选条件命名', '保存常用筛选', { inputValidator: (value) => Boolean(value?.trim()) && value.trim().length <= 60 || '请输入 1–60 个字符', confirmButtonText: '保存', cancelButtonText: '取消' })
    const filter = { name: value.trim(), filters: { ...filters } }
    savedFilters.value = [...savedFilters.value.filter((item) => item.name !== filter.name), filter].slice(-20)
    writePreference('accountFilters', savedFilters.value)
    savedFilter.value = filter.name
  } catch (error) { if (error !== 'cancel' && error !== 'close') ElMessage.error('未能保存筛选，请重试') }
}
function applyFilter(name) {
  const filter = savedFilters.value.find((item) => item.name === name)
  if (!filter) return
  syncingRoute = true
  invalidDateFilter.value = accountDateFilterError(filter.filters || filter)
  Object.assign(filters, normalizeAccountFilters(filter.filters || filter))
  syncingRoute = false
  advancedFilters.value = true
  if (invalidDateFilter.value) { loadError.value = invalidDateFilter.value; return }
  resetFilteredPage()
}
function removeFilter() {
  savedFilters.value = savedFilters.value.filter((item) => item.name !== savedFilter.value)
  writePreference('accountFilters', savedFilters.value)
  savedFilter.value = ''
}
async function load({ background = false } = {}) {
  if (!active) return
  if (filterError.value) { loadError.value = filterError.value; loading.value = false; listPending.value = false; return }
  const sequence = ++loadSequence
  listPending.value = true
  if (!background) loading.value = true
  if (!background) loadError.value = ''
  const params = { ...compactAccountFilters(filters), page: page.value, page_size: pageSize.value }
  try {
    await listRequest.run((signal) => Promise.all([accountManagement.list(params, { signal }), api.groups({ signal }), api.proxies({ signal })]), ([accountData, groupData, proxyData]) => {
      if (!Array.isArray(accountData.accounts) || !Number.isSafeInteger(accountData.total) || accountData.total < 0 || accountData.total_kind !== 'exact') throw new Error('服务器未返回完整账号分页信息')
      syncingSelection = true
      accounts.value = accountData.accounts
      hasMore.value = accountData.has_more === true
      total.value = accountData.total
      groups.value = groupData.groups || []
      proxies.value = proxyData.proxies || []
      loadError.value = ''
      refreshedAt.value = Date.now()
      syncTableSelection()
      loadTodayUsage()
      loadLastUsed()
      loadOfficialUsage()
    })
  } catch (error) { if (error.name !== 'AbortError') loadError.value = error.message || '账号加载失败，请重试' }
  finally { if (sequence === loadSequence) { loading.value = false; listPending.value = false } }
}
const groupIndex = computed(() => indexResources(groups.value))
const proxyIndex = computed(() => indexResources(proxies.value))
const groupName = (id) => groupIndex.value.get(id)?.name || id
const proxyName = (id) => proxyIndex.value.get(id)?.name || id
const statusType = (row) => ({ ok: 'success', error: 'danger', disabled: 'info', expired: 'warning' }[row.status] || 'info')
const statusLabel = (row) => accountStatusLabel(row.status)
const credentialLabel = (row) => ({ unverified: '尚未验证', ok: '验证成功', valid: '验证成功', refreshed: '已刷新', error: '维护失败', failed: '维护失败', expired: '已过期' }[row.credential_status] || row.credential_status || '尚未验证')
const concurrencyProgress = (row) => typeof row.in_flight === 'number' && typeof row.concurrency === 'number' && row.concurrency > 0 ? Math.min(100, Math.max(0, row.in_flight / row.concurrency * 100)) : null
const credentialTone = (row) => ['ok', 'valid', 'refreshed'].includes(row.credential_status) ? 'green' : ['error', 'failed', 'expired'].includes(row.credential_status) ? 'amber' : 'neutral'
const usageWindow = (row) => row.window_start ? `自 ${formatTime(row.window_start)} 起` : `最近 ${formatNumber(row.window_days)} 天`
function expiryLabel(row) {
  const remaining = Number(row.expires_at) * 1000 - Date.now()
  if (!row.expires_at) return ''
  return remaining <= 0 ? '令牌已到期' : remaining < 3 * 86400000 ? '3 天内到期' : ''
}

const detailsVisible = ref(false)
const details = ref(null)
const detailLoading = ref(false)
const detailError = ref('')
const detailRequest = createLatestRequest()
let detailSequence = 0
const chartContainer = ref(null)
const chartLoading = ref(false)
const chartMessage = ref('')
const recentItems = ref([])
const chartRequest = createLatestRequest()
let chart
let chartObserver
let chartSequence = 0
function disposeChart() {
  chartSequence++
  chartRequest.cancel()
  chartObserver?.disconnect()
  chartObserver = undefined
  chart?.dispose()
  chart = undefined
  chartLoading.value = false
}
function cssVar(name, fallback) { return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || fallback }
async function drawChart() {
  if (!detailsVisible.value || !chartContainer.value || !recentItems.value.length) return
  const sequence = chartSequence
  let init
  try { ({ init } = await import('../utils/echarts')) }
  catch { if (sequence === chartSequence && detailsVisible.value) chartMessage.value = '图表加载失败，请重新打开详情'; return }
  // Closing or switching details during the module download must not recreate a chart.
  if (sequence !== chartSequence || !active || !detailsVisible.value || !chartContainer.value || !recentItems.value.length) return
  if (!chart) {
    chart = init(chartContainer.value)
    chartObserver = new ResizeObserver(() => chart?.resize())
    chartObserver.observe(chartContainer.value)
  }
  const items = recentItems.value
  chart.setOption({
    tooltip: { trigger: 'item', renderMode: 'richText', confine: true, formatter: (params) => {
      const row = items[params.dataIndex]
      if (!row) return ''
      return [formatTime(row.time), `状态：${row.status === 'ok' ? '成功' : '失败'}`, `模型：${row.model || '—'}`, `Token：入 ${formatNumber(row.input_tokens)} / 出 ${formatNumber(row.output_tokens)} / 读取 ${formatNumber(row.cached_tokens_known === true || row.cached_tokens_known !== false && row.cached_tokens > 0 ? row.cached_tokens : null)} / 创建 ${formatNumber(row.cache_write_tokens)}`, `耗时：${formatDuration(row.latency_ms)}`, row.error && `错误：${row.error}`, row.proxy_name && `代理：${row.proxy_name}`, row.ip && `IP：${row.ip}`].filter(Boolean).join('\n')
    } },
    grid: { left: 12, right: 12, top: 12, bottom: 12 },
    xAxis: { type: 'value', show: false, min: 0, max: Math.max(items.length - 1, 1) },
    yAxis: { type: 'value', show: false, min: 0, max: 1 },
    series: [{ type: 'scatter', symbolSize: 10, data: items.map((row, index) => ({ value: [index, 0.5], itemStyle: { color: row.status === 'ok' ? cssVar('--success', '#16a34a') : cssVar('--danger', '#dc2626') } })) }],
  })
}
async function loadChart() {
  if (!details.value?.id || !detailsVisible.value) return
  disposeChart()
  const sequence = ++chartSequence
  chartLoading.value = true
  chartMessage.value = ''
  recentItems.value = []
  try {
    await chartRequest.run((signal) => api.accountRequests(details.value.id, 80, { signal }), async (data) => {
      recentItems.value = (data.items || []).slice().sort((a, b) => Number(a.time) - Number(b.time))
      chartMessage.value = recentItems.value.length ? '' : '该账号暂无请求记录'
      await nextTick()
      await drawChart()
    })
  } catch (error) { chartMessage.value = error.message || '请求记录加载失败' }
  finally { if (sequence === chartSequence) chartLoading.value = false }
}
async function loadDetails(id) {
  if (!active) return
  const sequence = ++detailSequence
  detailLoading.value = true
  detailError.value = ''
  try { await detailRequest.run((signal) => api.accountDetail(id, { signal }), (data) => { details.value = data.account }) }
  catch (error) { detailError.value = error.message || '详情加载失败' }
  finally { if (sequence === detailSequence) detailLoading.value = false }
}
function scrollDetailSection(section) { document.getElementById(`account-detail-${section}`)?.scrollIntoView({ behavior: 'smooth', block: 'start' }) }
function openDetails(row) { details.value = row; detailsVisible.value = true; loadDetails(row.id) }
watch(detailsVisible, (visible) => {
  if (!visible) {
    detailRequest.cancel(); disposeChart()
    if (route.query.account_id) router.replace({ query: { ...route.query, account_id: undefined } })
  }
})
watch(() => route.query.account_id, (id) => {
  if (typeof id === 'string' && id && (!detailsVisible.value || details.value?.id !== id)) openDetails(accounts.value.find((account) => account.id === id) || { id, name: '正在加载…' })
}, { immediate: true })

const saving = ref(false)
const busyIDs = ref(new Set())
const actionError = ref('')
const editVisible = ref(false)
const editing = ref(null)
const editForm = ref(accountEditForm())
const mutationBusy = computed(() => saving.value || importing.value || importManagementBusy.value || bulkWorking.value || batchReauthorizeWorking.value || batchUpstreamWorking.value || bundleWorking.value || reauthorizeWorking.value || oauthWorking.value || leaseWorking.value || crsWorking.value || busyIDs.value.size > 0)
function markBusy(id, busy) { const next = new Set(busyIDs.value); if (busy) next.add(id); else next.delete(id); busyIDs.value = next }
function openEdit(row) {
  editing.value = row
  editForm.value = accountEditForm(row)
  actionError.value = ''
  editVisible.value = true
}
async function closeEdit(done) {
  if (saving.value) return
  if (editing.value && JSON.stringify(editForm.value) !== JSON.stringify(accountEditForm(editing.value))) {
    try { await ElMessageBox.confirm('尚有未保存的账号修改，关闭后将丢弃这些修改。', '放弃修改？', { type: 'warning', confirmButtonText: '放弃修改', cancelButtonText: '继续编辑', closeOnClickModal: false }) }
    catch { return }
  }
  if (typeof done === 'function') done()
  else editVisible.value = false
}
async function saveEdit() {
  if (mutationBusy.value) return
  actionError.value = validateAccountConfiguration(editForm.value, true)
  if (actionError.value) return
  saving.value = true
  actionError.value = ''
  try {
    await api.accountUpdate(editing.value.id, accountEditPatch(editing.value, editForm.value))
    editVisible.value = false
    ElMessage.success('账号已更新')
    await load()
    if (detailsVisible.value && details.value?.id === editing.value.id) await loadDetails(editing.value.id)
  } catch (error) {
    actionError.value = error.status === 409 ? '账号已被其他操作修改，已重新读取最新版本。请核对后重新编辑。' : error.message || '账号更新失败'
    if (error.status === 409) {
      try { const data = await api.accountDetail(editing.value.id); editing.value = data.account; editForm.value = accountEditForm(data.account) }
      catch (reloadError) { actionError.value += ` 重新读取失败：${reloadError.message}` }
    }
  } finally { saving.value = false }
}
async function remove(row) {
  if (mutationBusy.value) return
  markBusy(row.id, true)
  actionError.value = ''
  try {
    await ElMessageBox.confirm(`确定删除账号「${row.name}」吗？历史日志将保留。`, '删除确认', { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消', confirmButtonClass: 'el-button--danger' })
    await api.accountDelete(row.id, row.version)
    ElMessage.success('账号已删除')
    if (details.value?.id === row.id) detailsVisible.value = false
    await load()
  } catch (error) { if (error !== 'cancel' && error !== 'close') { actionError.value = error.status === 409 ? '账号版本已变化，请刷新后核对再删除' : error.message || '账号删除失败'; if (error.status === 409) await load() } }
  finally { markBusy(row.id, false) }
}
async function refreshToken(row) {
  if (mutationBusy.value) return
  markBusy(row.id, true)
  actionError.value = ''
  try {
    const result = await api.accountRefresh(row.id)
    ElMessage.success(`令牌已刷新，到期时间 ${formatTime(result.expires_at)}`)
    await load()
    if (detailsVisible.value && details.value?.id === row.id) await loadDetails(row.id)
  } catch (error) { actionError.value = error.message || '令牌刷新失败' }
  finally { markBusy(row.id, false) }
}

const selection = ref([])
const selectionKind = ref('manual')
const selectingAll = ref(false)
const selectionRequest = createLatestRequest()
let selectionSequence = 0
const mobileSelected = (row) => selection.value.some((item) => item.id === row.id)
const allMobileSelected = computed(() => accounts.value.length > 0 && accounts.value.every(mobileSelected))
const pageSelectedCount = computed(() => accounts.value.filter(mobileSelected).length)
async function syncTableSelection() {
  syncingSelection = true
  await nextTick()
  accountsTable.value?.clearSelection()
  for (const row of accounts.value) if (mobileSelected(row)) accountsTable.value?.toggleRowSelection(row, true)
  await nextTick()
  syncingSelection = false
}
function clearSelection() {
  selectionRequest.cancel()
  selectionSequence++
  selectingAll.value = false
  selection.value = []
  selectionKind.value = 'manual'
  syncTableSelection()
}
function onTableSelection(rows) {
  if (syncingSelection || loading.value || mutationBusy.value) return
  const currentIDs = new Set(accounts.value.map((row) => row.id))
  const existing = new Map(selection.value.map((item) => [item.id, item]))
  selection.value = [...selection.value.filter((item) => !currentIDs.has(item.id)), ...rows.map((row) => existing.get(row.id) || { ...row })]
  selectionKind.value = 'manual'
}
function selectMobileAccount(row, checked) {
  if (mutationBusy.value || loading.value) return
  selection.value = checked ? [...selection.value.filter((item) => item.id !== row.id), { ...row }] : selection.value.filter((item) => item.id !== row.id)
  selectionKind.value = 'manual'
}
function selectMobilePage(checked) {
  if (mutationBusy.value || loading.value) return
  const ids = new Set(accounts.value.map((row) => row.id))
  const original = new Map(selection.value.map((row) => [row.id, row]))
  selection.value = [...selection.value.filter((row) => !ids.has(row.id)), ...(checked ? accounts.value.map((row) => original.get(row.id) || { ...row }) : [])]
  selectionKind.value = 'manual'
  syncTableSelection()
}
async function selectAllResults() {
  if (loading.value || mutationBusy.value || selectingAll.value || filterError.value) return
  const sequence = ++selectionSequence
  const snapshot = compactAccountFilters(filters)
  const signature = JSON.stringify(snapshot)
  selectingAll.value = true
  actionError.value = ''
  try {
    await selectionRequest.run((signal) => accountManagement.selection(snapshot, { signal }), (data) => {
      if (signature !== JSON.stringify(compactAccountFilters(filters))) return
      const items = validateAccountSelection(data)
      const rows = new Map(accounts.value.map((row) => [row.id, row]))
      selection.value = items.map((item) => ({ ...item, name: rows.get(item.id)?.name }))
      selectionKind.value = 'all'
      syncTableSelection()
    })
  } catch (error) { if (error.name !== 'AbortError') actionError.value = error.message || '全部选择失败，请重试' }
  finally { if (sequence === selectionSequence) selectingAll.value = false }
}
watch(narrowTable, syncTableSelection)
function mobileAction(command, row) { if (command === 'refresh') refreshToken(row); else if (command === 'delete') remove(row); else if (command === 'reauthorize') openReauthorize(row); else if (command === 'oauth') openOAuth(row); else if (['usage', 'models', 'test'].includes(command)) openUpstream(row, command); else if (command === 'schedules') openSchedules(row); else if (command === 'lease') openLease(row); else if (command === 'stats') openStats(row); else openDetails(row) }
const bulkVisible = ref(false)
const bulkAction = ref('edit')
const bulkWorking = ref(false)
const bulkTargets = ref([])
const batchUpstreamVisible = ref(false), batchUpstreamWorking = ref(false), batchUpstreamKind = ref('usage')
const batchReauthorizeVisible = ref(false), batchReauthorizeWorking = ref(false)
const exportVisible = ref(false)
const bundleVisible = ref(false)
const bundleWorking = ref(false)
const reauthorizeVisible = ref(false)
const reauthorizeWorking = ref(false)
const reauthorizeAccount = ref(null)
const oauthVisible = ref(false)
const oauthWorking = ref(false)
const oauthAccount = ref(null)
function openOAuth(row = null) { oauthAccount.value = row ? { ...row } : null; oauthVisible.value = true }
const upstreamVisible = ref(false)
const upstreamAccount = ref(null)
const upstreamTab = ref('usage')
function openUpstream(row, tab = 'usage') { upstreamAccount.value = { ...row }; upstreamTab.value = tab; upstreamVisible.value = true }
const schedulesVisible = ref(false), schedulesAccount = ref(null)
function openSchedules(row) { schedulesAccount.value = { ...row }; schedulesVisible.value = true }
const leaseVisible = ref(false), leaseAccount = ref(null), leaseWorking = ref(false)
function openLease(row) { leaseAccount.value = { ...row }; leaseVisible.value = true }
const statsVisible = ref(false), statsAccount = ref(null)
function openStats(row) { statsAccount.value = { ...row }; statsVisible.value = true }
const crsVisible = ref(false), crsWorking = ref(false)
const refreshedAt = ref(0)
const autoRefreshSeconds = ref(readPreference('accountsAutoRefresh', 0, value => [0, 5, 10, 15, 30].includes(value)))
const documentHidden = ref(document.hidden)
function syncVisibility() { documentHidden.value = document.hidden }
const refreshPaused = computed(() => documentHidden.value || listPending.value || mutationBusy.value || selection.value.length > 0 || selectingAll.value || detailsVisible.value || editVisible.value || importVisible.value || bulkVisible.value || batchReauthorizeVisible.value || batchUpstreamVisible.value || exportVisible.value || bundleVisible.value || reauthorizeVisible.value || oauthVisible.value || upstreamVisible.value || schedulesVisible.value || leaseVisible.value || statsVisible.value || crsVisible.value || columnDialog.value)
let refreshTimer
function configureRefresh() {
  clearInterval(refreshTimer)
  if (autoRefreshSeconds.value) refreshTimer = setInterval(() => {
    if (!active || refreshPaused.value || document.activeElement?.matches('input, textarea, [role="combobox"], [contenteditable="true"]')) return
    load({ background: true })
  }, autoRefreshSeconds.value * 1000)
}
watch(autoRefreshSeconds, value => { writePreference('accountsAutoRefresh', value); configureRefresh() })
function openBulk(action = 'edit') { bulkTargets.value = selection.value.map((item) => ({ ...item })); if (action === 'credentials') batchReauthorizeVisible.value = true; else if (['usage', 'models'].includes(action)) { batchUpstreamKind.value = action; batchUpstreamVisible.value = true } else { bulkAction.value = action; bulkVisible.value = true } }
function openReauthorize(row) { reauthorizeAccount.value = { ...row }; reauthorizeVisible.value = true }
async function afterAccountsChanged() { clearSelection(); await load(); if (detailsVisible.value && details.value?.id) await loadDetails(details.value.id) }
const importVisible = ref(false)
const importText = ref('')
const importSourceMode = ref('files')
const importFiles = ref([])
const importFileInput = ref(null)
const importDragDepth = ref(0)
const importReading = ref(false)
let importReadSequence = 0
const importGroups = ref([])
const importConfig = ref(defaultImportConfiguration())
const importExternalLeases = ref(false)
const importLeaseReceipts = ref([])
const importLeaseBusyIDs = ref(new Set())
const importLeaseBusy = computed(() => importLeaseBusyIDs.value.size > 0)
const importManagementBusy = computed(() => importLeaseBusy.value)
const importWorking = computed(() => importing.value || importManagementBusy.value)
function setImportLeaseBusy(key, value) { const ids = new Set(importLeaseBusyIDs.value); if (value) ids.add(key); else ids.delete(key); importLeaseBusyIDs.value = ids }
const importDefaults = () => ({ ...importConfig.value, groups: [...importGroups.value], proxies: [...importConfig.value.proxies] })
const importRows = ref([])
const importResults = ref([])
const importError = ref('')
const importPreviewing = ref(false)
const importing = ref(false)
const importSubmitted = ref(false)
const importReady = ref(false)
const previewRequest = createLatestRequest()
let previewSequence = 0
const importCounts = computed(() => Object.fromEntries(['new', 'duplicate', 'replace', 'failed'].map((kind) => [kind, importRows.value.filter((row) => row.kind === kind).length])))
const pendingImport = computed(() => importRows.value.filter((row) => row.kind !== 'failed' && (['add', 'replace'].includes(row.action) || row.action === 'lease' && canPrepareDuplicateLease(row, importExternalLeases.value))))
const importHasSource = computed(() => importSourceMode.value === 'files' ? importFiles.value.length > 0 : Boolean(importText.value.trim()))
const importHasExternalBindings = computed(() => importRows.value.some((row) => row.externalBindings))
const importExternalLeaseRows = computed(() => importRows.value.filter(row => row.externalLease))
const importProgress = computed(() => pendingImport.value.length ? Math.round(importResults.value.length / pendingImport.value.length * 100) : 0)
const importResultCounts = computed(() => ({ success: importResults.value.filter((row) => row.status === 'success').length, failed: importResults.value.filter((row) => row.status === 'failed').length + importCounts.value.failed, skipped: importRows.value.filter((row) => row.kind !== 'failed' && row.action === 'skip').length + importResults.value.filter((row) => row.status === 'skipped').length }))
const importWarningLabel = (warning) => ({ bundle_proxies_not_imported: '导出文件中的代理未自动创建，请选择本地代理', external_proxy_replaced: '已使用所选本地代理替代原代理', external_bindings_not_imported: '原系统的分组和代理绑定未直接沿用', sub2api_settings_not_imported: '原系统的扩展配置未自动导入', account_expiry_not_imported: '原账号管理到期日未用作登录凭据到期日', model_mapping_not_imported: '原系统的模型映射未导入', external_lease_pending_confirmation: '识别到外部管理租期；账号导入后须单独预览并确认应用', external_lease_not_imported: '未选择识别外部租期，账号导入不应用原租期', external_cost_not_imported: '已忽略原系统的金额配置' }[warning] || warning)
const classificationLabel = (kind) => ({ new: '新增', duplicate: '重复', replace: '替换候选', failed: '失败' }[kind] || kind)
watch([importText, importSourceMode, importFiles, importGroups, importConfig, importExternalLeases], () => {
  previewSequence++
  previewRequest.cancel()
  importRows.value = []; importResults.value = []; importLeaseReceipts.value = []; importSubmitted.value = false; importReady.value = false; importError.value = ''; importPreviewing.value = false
}, { deep: true })
async function readImportFiles(files) {
  if (importWorking.value) return
  const selected = Array.from(files || [])
  if (!selected.length) return
  if (selected.length > 100) { ElMessage.warning('每次最多选择 100 个文件，请分批导入'); return }
  if (selected.some((file) => !/\.(json|jsonl|ndjson|txt)$/i.test(file.name))) { ElMessage.warning('请选择 JSON、JSONL 或 TXT 文件'); return }
  if (selected.reduce((total, file) => total + file.size, 0) > maxImportBytes) { ElMessage.warning('文件总大小不能超过 32 MB，请分批导入'); return }
  const sequence = ++importReadSequence
  importReading.value = true
  try {
    const documents = await Promise.all(selected.map(async (file) => ({ name: file.name, size: file.size, text: await file.text() })))
    if (sequence !== importReadSequence || !importVisible.value) return
    importFiles.value = documents
    importSourceMode.value = 'files'
  }
  catch { importError.value = '无法读取文件，请重试或粘贴文本' }
  finally { if (sequence === importReadSequence) importReading.value = false }
}
function readImportFile(event) {
  readImportFiles(event.target.files)
  event.target.value = ''
}
function dropImportFiles(event) {
  importDragDepth.value = 0
  readImportFiles(event.dataTransfer?.files)
}
async function previewImport() {
  importError.value = validateAccountConfiguration(importConfig.value)
  if (importError.value) return
  if (importWorking.value || importPreviewing.value || importReading.value) return
  if (!importHasSource.value) { ElMessage.warning('请粘贴账号内容或选择文件'); return }
  const documents = importSourceMode.value === 'files' ? importFiles.value : [{ name: '', text: importText.value }]
  if (documents.reduce((total, doc) => total + new TextEncoder().encode(doc.text).length, 0) > maxImportBytes) { ElMessage.warning('内容不能超过 32 MB，请分批导入'); return }
  const localRows = parseImportDocuments(documents)
  const validRows = localRows.filter((row) => row.kind !== 'failed')
  importError.value = ''
  importSubmitted.value = false
  importReady.value = false
  importResults.value = []
  importLeaseReceipts.value = []
  if (!validRows.length) {
    importRows.value = localRows.map((row, index) => ({ ...row, sourceIndex: index, kind: row.kind || 'new', action: 'skip', message: row.message || '请先修正格式错误，再生成服务端预览' }))
    importError.value = '没有可导入的账号，请检查下方标注的文件和行号'
    return
  }
  const sequence = ++previewSequence
  const source = JSON.stringify(validRows.map(row => importRequestSource(row)))
  importPreviewing.value = true
  try {
    await previewRequest.run((signal) => api.accountImportPreview({ raw: source, replace: false, defaults: importDefaults(), include_external_leases: importExternalLeases.value }, { signal }), (data) => {
      if (!Array.isArray(data.results) || data.results.length !== validRows.length || data.dry_run !== true || importExternalLeases.value && data.results.some((result, index) => result.index !== index + 1)) throw new Error('服务器预览结果不完整或行号不匹配，已阻止导入')
      const leaseByIndex = externalLeasePreviews(data, validRows.length, importExternalLeases.value)
      const bySource = new Map(data.results.map((result, index) => [validRows[index].sourceIndex, result]))
      const leaseBySource = new Map(validRows.map((row, index) => [row.sourceIndex, leaseByIndex.get(index + 1)]))
      importRows.value = localRows.map((row) => {
        if (row.kind === 'failed') return row
        const result = bySource.get(row.sourceIndex)
        const kind = result.code === 'name_conflict' ? 'replace' : ({ added: 'new', duplicate: 'duplicate', replaced: 'replace', failed: 'failed' }[result.status] || 'failed')
        const externalLease = leaseBySource.get(row.sourceIndex)
        return { ...row, name: result.name || row.name || '—', id: result.id, externalLease, kind, action: kind === 'new' ? 'add' : 'skip', locked: kind === 'duplicate' && !externalLease, warnings: (result.warnings || []).map((warning) => redactImportMessage(importWarningLabel(warning), localRows)), message: kind === 'replace' ? '同名账号已存在；请选择是否明确替换凭据' : kind === 'duplicate' ? externalLease ? '凭据已存在，默认跳过；可明确保留账号，仅准备租期确认' : '服务端识别到重复凭据，将跳过' : kind === 'new' ? '服务端预览：新增' : result.code === 'invalid_external_lease' ? '外部管理租期无效，此行账号不会导入；请修正原文件或取消外部租期识别后重新预览' : result.code === 'proxy_mapping_required' ? '原账号绑定了代理，请在高级配置中选择本地代理' : result.code === 'unsupported_platform' ? '此网关仅支持 OpenAI / Codex 账号' : result.code === 'unsupported_type' ? '此网关仅支持 OAuth 登录账号' : redactImportMessage(result.error || result.code || '导入失败', localRows) }
      })
      importReady.value = true
    })
  } catch (error) { importRows.value = []; importError.value = redactImportMessage(error.message || '导入预览失败，请重试', localRows) }
  finally { if (sequence === previewSequence) importPreviewing.value = false }
}
async function doImport() {
  if (mutationBusy.value || importPreviewing.value || importSubmitted.value || !importReady.value || !pendingImport.value.length) return
  importing.value = true
  importSubmitted.value = true
  importError.value = ''
  try {
    // Replacement is opt-in for each row. New rows always submit replace:false,
    // so a name created after the preview cannot be silently overwritten.
    for (const row of [...pendingImport.value]) {
      if (!active) break
      try {
        const response = await api.accountImport({ raw: JSON.stringify(importRequestSource(row, row.name)), replace: row.action === 'replace', defaults: importDefaults(), include_external_leases: importExternalLeases.value })
        const result = response.results?.[0]
        if (!result || !['added', 'replaced', 'duplicate', 'failed'].includes(result.status)) throw new Error('服务端未返回明确结果，请刷新列表核对')
        importResults.value.push({ sourceIndex: row.sourceIndex, line: row.line, name: row.name, status: ['added', 'replaced'].includes(result.status) ? 'success' : result.status === 'duplicate' ? 'skipped' : 'failed', message: result.status === 'added' ? '已新增' : result.status === 'replaced' ? '凭据已替换' : result.status === 'duplicate' ? row.action === 'lease' ? '原账号和凭据已保留；管理配置请在各自独立回执中核对' : '重复凭据，已跳过' : redactImportMessage(result.error || result.code || '导入失败', importRows.value) })
        if (importExternalLeases.value && (row.externalLease || response.external_leases?.length)) {
          try {
            const receipt = externalLeaseCommitReceipt(response, row.externalLease, result.id)
            if (receipt) importLeaseReceipts.value.push({ key: row.sourceIndex, line: row.line, sourceFile: row.sourceFile, name: row.name, receipt })
          } catch (cause) {
            importLeaseReceipts.value.push({ key: row.sourceIndex, line: row.line, sourceFile: row.sourceFile, name: row.name, error: redactImportMessage(cause.message || '租期回执无法确认，请核对账号。', importRows.value) })
          }
        }
      } catch (error) { importResults.value.push({ sourceIndex: row.sourceIndex, line: row.line, name: row.name, status: 'failed', message: redactImportMessage(error.message || '请求失败', importRows.value) }) }
    }
    const { success, failed, skipped } = importResultCounts.value
    ElMessage[failed ? 'warning' : 'success'](`导入完成：成功 ${success} 项，失败 ${failed} 项，跳过 ${skipped} 项`)
    await load()
  } finally { importing.value = false }
}
function downloadImport(failuresOnly = false) {
  let report = importReport(importRows.value, importResults.value)
  if (failuresOnly) report = report.filter((row) => row.result === 'failed')
  const link = document.createElement('a')
  const url = URL.createObjectURL(new Blob([JSON.stringify(report, null, 2)], { type: 'application/json;charset=utf-8' }))
  link.href = url; link.download = failuresOnly ? 'account-import-failures.json' : 'account-import-report.json'; link.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
function closeImport() { previewRequest.cancel(); importReadSequence++; importReading.value = false; importFiles.value = []; importSourceMode.value = 'files'; importDragDepth.value = 0; importText.value = ''; importRows.value = []; importResults.value = []; importLeaseReceipts.value = []; importExternalLeases.value = false; importLeaseBusyIDs.value = new Set(); importGroups.value = []; importConfig.value = defaultImportConfiguration() }
onMounted(() => { load(); configureRefresh(); officialTimer = setInterval(() => { if (!document.hidden) loadOfficialUsage() }, 30000); document.addEventListener('visibilitychange', syncVisibility); window.addEventListener('theme-change', drawChart); accountViewport.addEventListener('change', syncAccountViewport); pinnedViewport.addEventListener('change', syncPinnedViewport) })
onUnmounted(() => { active = false; officialSequence++; officialController?.abort(); clearInterval(officialTimer); lastUsedSequence++; lastUsedController?.abort(); clearTimeout(filterTimer); clearInterval(refreshTimer); todaySequence++; todayController?.abort(); document.removeEventListener('visibilitychange', syncVisibility); listRequest.cancel(); selectionRequest.cancel(); previewRequest.cancel(); detailRequest.cancel(); disposeChart(); window.removeEventListener('theme-change', drawChart); accountViewport.removeEventListener('change', syncAccountViewport); pinnedViewport.removeEventListener('change', syncPinnedViewport) })
</script>

<template>
  <div class="accounts-page" :aria-busy="loading">
    <PageHeader :title="$route.meta.title" subtitle="管理 ChatGPT 账号、登录凭据与 PowerPoint 调用资源">
      <el-button type="primary" :disabled="mutationBusy" @click="openOAuth()">添加账号</el-button>
      <el-button :icon="Upload" :disabled="mutationBusy" @click="importVisible = true">导入账号</el-button>
      <el-button :icon="Refresh" circle aria-label="刷新账号列表" :loading="loading" @click="load" />
    </PageHeader>
    <el-alert v-if="loadError" type="error" :title="loadError" :closable="false" show-icon class="feedback"><el-button v-if="invalidDateFilter" link type="primary" @click="clearInvalidDateFilter">清除无效日期并查询</el-button></el-alert>
    <el-alert v-if="lastUsedError && colVisible('last_used_at')" :title="lastUsedError" type="warning" :closable="false" class="feedback"><el-button link type="primary" @click="loadLastUsed">重试最近业务请求</el-button></el-alert>
    <el-alert v-if="actionError && !editVisible" type="error" :title="actionError" :closable="false" show-icon class="feedback" />
    <div class="account-overview" aria-label="账号概览" :class="{ 'overview-stale': loading || loadError }">
      <div class="account-overview-card"><span>筛选结果</span><strong>{{ loading || loadError ? '—' : formatNumber(total) }}</strong><small>{{ loadError ? '上次查询结果' : '全部匹配账号' }}</small><span class="overview-mark"><el-icon><User /></el-icon></span></div>
      <div class="account-overview-card"><span>本页就绪</span><strong>{{ loading || loadError ? '—' : formatNumber(pageOverview.ready) }}</strong><small><i class="overview-dot" />已启用 · 状态正常</small><span class="overview-mark positive"><el-icon><Check /></el-icon></span></div>
      <div class="account-overview-card"><span>本页实时并发</span><strong>{{ loading || loadError ? '—' : formatNumber(pageOverview.active) }}</strong><small>当前正在处理的请求</small><span class="overview-mark"><el-icon><Refresh /></el-icon></span></div>
      <div class="account-overview-card" :class="{ attention: pageOverview.attention }"><span>本页待关注</span><strong>{{ loading || loadError ? '—' : formatNumber(pageOverview.attention) }}</strong><small>异常、到期或未验证</small><span class="overview-mark"><el-icon><Setting /></el-icon></span></div>
    </div>
    <div class="panel accounts-panel">
      <div class="account-directory-heading"><div><h3>账号列表 <span class="directory-count">{{ loading && !accounts.length ? '加载中' : loadError ? '上次结果' : `共 ${formatNumber(total)} 个` }}</span></h3><span class="directory-sort">全结果按{{ accountSortFields[filters.sort_by] }}{{ filters.sort_order === 'asc' ? '升序' : '降序' }}</span></div><div class="directory-tools"><el-dropdown trigger="click" @command="command => command === 'export' ? exportVisible = true : command === 'crs' ? crsVisible = true : bundleVisible = true"><el-button :disabled="mutationBusy">迁移备份<el-icon><ArrowDown /></el-icon></el-button><template #dropdown><el-dropdown-menu><el-dropdown-item command="export">导出账号</el-dropdown-item><el-dropdown-item command="restore">恢复凭据备份</el-dropdown-item><el-dropdown-item command="crs">从 CRS 同步</el-dropdown-item></el-dropdown-menu></template></el-dropdown><el-button :icon="Setting" @click="columnDialog = true">显示列</el-button></div></div>
      <div class="account-status-shortcuts" role="group" aria-label="快速筛选账号状态"><button v-for="item in statusShortcuts" :key="item.value" type="button" :aria-pressed="statusFilter === item.value" @click="applyStatusShortcut(item.value)">{{ item.label }}</button></div>
      <div class="table-toolbar account-filters">
        <el-input v-model="search" aria-label="搜索账号" placeholder="搜索名称、备注、标签或 ID" clearable :prefix-icon="Search" class="name-filter" />
        <el-select v-model="groupFilter" filterable aria-label="常用分组筛选" placeholder="全部分组" clearable class="select-filter" :disabled="filters.ungrouped === 'true'"><el-option v-for="group in groups" :key="group.id" :label="group.name" :value="group.id" /></el-select>
        <el-select v-if="savedFilters.length" v-model="savedFilter" aria-label="常用筛选" placeholder="常用筛选" clearable class="select-filter" @change="applyFilter"><el-option v-for="filter in savedFilters" :key="filter.name" :label="filter.name" :value="filter.name" /></el-select>
        <el-button :aria-expanded="advancedFilters" aria-controls="account-advanced-filters" @click="advancedFilters = !advancedFilters">筛选与排序<el-icon class="filter-chevron"><ArrowUp v-if="advancedFilters" /><ArrowDown v-else /></el-icon></el-button>
        <el-button v-if="activeFilterCount" link @click="clearFilters">清除筛选（{{ activeFilterCount }}）</el-button>
      </div>
      <div class="account-view-options"><div class="view-option-label"><span class="view-option-dot" />列表偏好<span class="view-option-hint">按你的工作习惯查看资源</span></div><div class="account-view-controls"><el-checkbox v-model="showTodayUsage">今日用量</el-checkbox><el-select v-model="autoRefreshSeconds" aria-label="账号自动刷新" class="auto-refresh-select"><el-option label="手动刷新" :value="0" /><el-option v-for="seconds in [5, 10, 15, 30]" :key="seconds" :label="`${seconds} 秒自动刷新`" :value="seconds" /></el-select><span v-if="autoRefreshSeconds" class="auto-refresh-status">{{ refreshPaused ? '交互中暂停' : '自动刷新已启用' }}</span></div>
      </div>
      <div v-if="showTodayUsage" class="today-usage-note"><span>{{ todayLoading ? '正在统计今日留存明细…' : todayError ? todayError : `今日留存请求 · ${usageTimezone} · 统计至 ${formatTime(todayCheckedAt)}` }}；已清理的请求不在统计内。</span><el-button v-if="todayError" link type="primary" @click="loadTodayUsage">重试今日统计</el-button></div>
      <div v-show="advancedFilters" id="account-advanced-filters" class="account-advanced-filters">
        <label><span>所属分组</span><el-select v-model="groupFilter" aria-label="分组筛选" placeholder="全部分组" clearable :disabled="filters.ungrouped === 'true'"><el-option v-for="group in groups" :key="group.id" :label="group.name" :value="group.id" /></el-select></label>
        <label><span>绑定代理</span><el-select v-model="proxyFilter" aria-label="代理筛选" placeholder="全部代理" clearable :disabled="filters.no_proxy === 'true'"><el-option v-for="proxy in proxies" :key="proxy.id" :label="proxy.name" :value="proxy.id" /></el-select></label>
        <label><span>启用状态</span><el-select v-model="filters.enabled" aria-label="启用状态筛选" placeholder="全部" clearable><el-option label="已启用" value="true" /><el-option label="已停用" value="false" /></el-select></label>
        <label><span>刷新令牌</span><el-select v-model="filters.has_refresh_token" aria-label="刷新令牌筛选" placeholder="全部" clearable><el-option label="含刷新令牌" value="true" /><el-option label="无刷新令牌" value="false" /></el-select></label>
        <label><span>训练隐私</span><el-select v-model="filters.privacy" aria-label="训练隐私筛选" placeholder="全部" clearable><el-option label="已确认关闭训练" value="disabled" /><el-option label="隐私状态未知" value="unknown" /></el-select></label>
        <label><span>标签</span><el-input v-model="filters.tag" aria-label="账号标签筛选" placeholder="精确标签" clearable /></label>
        <label><span>令牌到期晚于</span><input type="datetime-local" :value="localAccountDate(filters.expires_after)" aria-label="令牌到期开始时间" @input="setExpiryFilter('expires_after', $event.target.value)" /></label>
        <label><span>令牌到期早于</span><input type="datetime-local" :value="localAccountDate(filters.expires_before)" aria-label="令牌到期结束时间" @input="setExpiryFilter('expires_before', $event.target.value)" /></label>
        <label><span>全结果排序</span><el-select v-model="filters.sort_by" aria-label="账号排序字段"><el-option v-for="(label, key) in accountSortFields" :key="key" :label="label" :value="key" /></el-select></label>
        <label><span>排序方向</span><el-select v-model="filters.sort_order" aria-label="账号排序方向"><el-option label="降序" value="desc" /><el-option label="升序" value="asc" /></el-select></label>
        <div class="filter-flags"><el-checkbox :model-value="filters.ungrouped === 'true'" @change="value => filters.ungrouped = value ? 'true' : ''">仅未分组</el-checkbox><el-checkbox :model-value="filters.no_proxy === 'true'" @change="value => filters.no_proxy = value ? 'true' : ''">仅直连</el-checkbox></div>
        <div class="filter-save-actions"><el-button :disabled="Boolean(filterError)" @click="saveFilter">保存筛选</el-button><el-button v-if="savedFilter" link type="danger" @click="removeFilter">移除此筛选</el-button></div>
      </div>
      <div v-if="filterChips.length" class="account-filter-chips" aria-label="已应用筛选"><span class="filter-chip-label">已筛选</span><button v-for="chip in filterChips" :key="chip.key" type="button" class="filter-chip" :title="chip.text" :aria-label="`移除筛选 ${chip.text}`" @click="removeFilterChip(chip.key)"><span>{{ chip.text }}</span><el-icon><Close /></el-icon></button></div>
      <div class="account-summary" :class="{ 'has-selection': selection.length }">
        <div class="selection-tools"><el-checkbox v-if="accounts.length" :model-value="allMobileSelected" :indeterminate="pageSelectedCount > 0 && !allMobileSelected" :disabled="mutationBusy || loading || selectingAll" aria-label="选择本页全部账号" @change="selectMobilePage">本页</el-checkbox><span aria-live="polite">{{ loading ? '加载中…' : loadError ? '保留上次结果' : selection.length ? `已选择 ${selection.length} 个${selectionKind === 'all' ? '（全部结果）' : ''}` : `本页 ${accounts.length} 个` }}</span><el-button link type="primary" :loading="selectingAll" :disabled="!total || loading || mutationBusy || Boolean(loadError)" @click="selectAllResults">选择全部 {{ formatNumber(total) }} 个</el-button><el-button v-if="selection.length" link :disabled="mutationBusy" @click="clearSelection">取消选择</el-button></div>
        <div class="selection-actions"><el-button :disabled="!selection.length || mutationBusy || loading" @click="openBulk('edit')">批量编辑</el-button><el-dropdown trigger="click" @command="openBulk"><el-button :disabled="!selection.length || mutationBusy || loading">更多操作<el-icon><ArrowDown /></el-icon></el-button><template #dropdown><el-dropdown-menu><el-dropdown-item command="enable">批量启用</el-dropdown-item><el-dropdown-item command="disable">批量停用</el-dropdown-item><el-dropdown-item command="refresh">批量刷新令牌</el-dropdown-item><el-dropdown-item command="usage">批量读取上游额度</el-dropdown-item><el-dropdown-item command="models">批量读取模型目录</el-dropdown-item><el-dropdown-item command="credentials">按 ID 批量替换凭据</el-dropdown-item><el-dropdown-item command="bind">追加绑定代理</el-dropdown-item><el-dropdown-item command="tags">替换标签</el-dropdown-item><el-dropdown-item command="delete" divided>批量删除</el-dropdown-item></el-dropdown-menu></template></el-dropdown></div>
      </div>
      <div v-if="!narrowTable" class="table-scroll">
        <el-table ref="accountsTable" v-loading="loading" :data="displayAccounts" size="small" row-key="id" @selection-change="onTableSelection">
          <template #empty><AccountListEmpty :loading="loading" :error="loadError" :filtered="Boolean(activeFilterCount)" :busy="mutationBusy" @retry="load" @clear="clearFilters" @import="importVisible = true" /></template>
          <el-table-column type="selection" width="42" :selectable="() => !mutationBusy && !loading" />
          <el-table-column v-if="colVisible('name')" prop="name" label="账号名称" min-width="224" :fixed="pinnedColumns ? 'left' : false"><template #default="{ row }"><div class="account-identity-cell"><span class="account-identity-icon" aria-hidden="true"><ConsoleIcon name="accounts" :size="15" /></span><div class="account-identity-copy"><el-button link type="primary" class="account-name" :title="row.name" :aria-label="`查看账号 ${row.name} 详情`" @click="openDetails(row)">{{ row.name }}</el-button><div class="account-identity-badges"><span class="account-mini-pill" :class="`is-${credentialTone(row)}`" :title="credentialLabel(row)">{{ credentialLabel(row) }}</span><span class="account-token-hint" :title="row.has_refresh_token ? '含刷新令牌' : '无刷新令牌'"><ConsoleIcon :name="row.has_refresh_token ? 'refresh' : 'key'" :size="10" />{{ row.has_refresh_token ? '含刷新令牌' : '无刷新令牌' }}</span><span v-if="!row.enabled" class="account-mini-pill is-neutral">已停用</span></div></div></div></template></el-table-column>
          <el-table-column v-if="colVisible('status')" label="状态" width="112"><template #default="{ row }"><el-tag :type="statusType(row)" size="small">{{ statusLabel(row) }}</el-tag><div v-if="row.last_error" class="account-row-error" :title="row.last_error">{{ row.last_error }}</div></template></el-table-column>
          <el-table-column v-if="colVisible('concurrency')" label="并发" width="105" align="center"><template #header><el-tooltip content="当前正在处理的请求数 / 配置的并发容量" placement="top"><span>并发</span></el-tooltip></template><template #default="{ row }"><div class="account-concurrency"><span class="account-metric-badge" :class="{ 'is-full': concurrencyProgress(row) === 100 }"><ConsoleIcon name="activity" :size="11" /><strong>{{ formatNumber(row.in_flight) }}</strong><span>/ {{ formatNumber(row.concurrency) }}</span></span><progress v-if="concurrencyProgress(row) !== null" :value="concurrencyProgress(row)" max="100" aria-label="账号并发占用比例" class="account-concurrency-progress" /></div></template></el-table-column>
          <el-table-column v-if="colVisible('usage')" label="请求 / 成功 / 失败" width="153"><template #default="{ row }"><div class="account-usage-line"><span class="account-usage-total">{{ formatNumber(row.usage?.requests) }}</span><span class="account-usage-success" title="成功"><ConsoleIcon name="check" :size="10" />{{ formatNumber(row.usage?.ok) }}</span><span class="account-usage-errors" :class="{ 'has-errors': row.usage?.errors > 0 }" title="失败"><ConsoleIcon name="close" :size="10" />{{ formatNumber(row.usage?.errors) }}</span></div><div class="account-token-line"><span>Token</span><strong>{{ formatNumber(row.usage?.tokens) }}</strong></div></template></el-table-column>
          <el-table-column v-if="colVisible('priority')" label="优先级" width="68" align="center"><template #default="{ row }"><span class="account-number-chip is-purple">{{ formatNumber(row.priority) }}</span></template></el-table-column>
          <el-table-column v-if="colVisible('weight')" label="权重" width="60" align="center"><template #default="{ row }"><span class="account-number-chip is-blue">{{ formatNumber(row.weight) }}</span></template></el-table-column>
          <el-table-column v-if="colVisible('expires_at')" label="令牌到期" width="170"><template #default="{ row }"><span class="account-time-label"><ConsoleIcon name="activity" :size="11" />{{ formatTime(row.expires_at) }}</span><div v-if="expiryLabel(row)" class="expiry-warning"><span class="account-mini-pill is-amber">{{ expiryLabel(row) }}</span></div></template></el-table-column>
          <el-table-column v-if="colVisible('groups')" label="分组" min-width="125"><template #default="{ row }"><AccountGroupList :names="(row.groups || []).map(groupName)" /></template></el-table-column>
          <el-table-column v-if="colVisible('proxies')" label="代理" min-width="125"><template #default="{ row }"><div class="tag-list account-resource-tags is-proxies"><el-tag v-for="id in row.proxies" :key="id" size="small" type="warning"><ConsoleIcon name="globe" :size="10" />{{ proxyName(id) }}</el-tag><span v-if="!row.proxies?.length" class="muted">直连</span></div></template></el-table-column>
          <el-table-column v-if="colVisible('quota')" label="用量窗口" width="330"><template #header><span>官方用量窗口</span><el-button link type="primary" size="small" :loading="officialUsageLoading" aria-label="刷新本页官方用量" @click="loadOfficialUsage(true)">刷新</el-button></template><template #default="{ row }"><AccountUsageWindows :snapshot="officialUsage[row.id]?.snapshot" :error="officialUsage[row.id]?.error" :loading="officialPending.has(row.id)" @open="openUpstream(row, 'usage')" /></template></el-table-column>
          <el-table-column v-if="showTodayUsage" label="今日留存请求" width="175"><template #default="{ row }"><template v-if="todayUsage[row.id]"><el-button link type="primary" @click="openStats(row)">{{ formatNumber(todayUsage[row.id].total_requests) }} 次</el-button><div class="muted">Token {{ formatNumber(todayUsage[row.id].total_tokens) }}</div></template><span v-else class="muted">{{ todayLoading ? '读取中' : '未取得' }}</span></template></el-table-column>
          <el-table-column v-if="colVisible('last_used_at')" label="最近业务请求" width="195"><template #default="{ row }"><span class="account-time-label">{{ lastUsedText(row.id) }}</span><div class="muted account-time-scope">{{ lastUsedScopeLabel(lastUsedScope) }}</div></template></el-table-column>
          <el-table-column v-if="colVisible('tokens')" label="输入 / 输出 / 缓存读取" width="220"><template #default="{ row }">{{ formatNumber(row.usage?.input) }} / {{ formatNumber(row.usage?.output) }} / {{ formatNumber(row.usage?.cached) }}<div class="muted">历史累计 · <el-button link type="primary" @click="openStats(row)">查看缓存读写统计</el-button></div></template></el-table-column>
          <el-table-column v-if="colVisible('latency')" label="平均耗时" width="105" align="right"><template #default="{ row }">{{ row.usage?.requests > 0 ? formatDuration(row.usage.avg_latency_ms, { empty: '暂无样本' }) : '暂无样本' }}</template></el-table-column>
          <el-table-column v-if="colVisible('credential')" label="凭据状态" min-width="135"><template #default="{ row }"><span class="account-mini-pill" :class="`is-${credentialTone(row)}`">{{ credentialLabel(row) }}</span><div class="muted">{{ row.has_refresh_token ? '含刷新令牌' : '无刷新令牌' }}</div></template></el-table-column>
          <el-table-column v-if="colVisible('notes')" prop="notes" label="备注" min-width="200" show-overflow-tooltip />
          <el-table-column v-if="colVisible('tags')" label="标签" min-width="140"><template #default="{ row }"><div class="tag-list"><el-tag v-for="tag in row.tags" :key="tag" size="small" type="info">{{ tag }}</el-tag><span v-if="!row.tags?.length" class="muted">无标签</span></div></template></el-table-column>
          <el-table-column v-for="key in ['last_checked_at', 'created_at', 'updated_at'].filter(colVisible)" :key="key" :label="columnLabels[key]" width="175"><template #default="{ row }">{{ formatTime(row[key]) }}</template></el-table-column>
          <el-table-column v-if="colVisible('id')" label="账号 ID / 版本" min-width="220"><template #default="{ row }"><span class="plain-text">{{ row.id }}</span><div class="muted">版本 {{ formatNumber(row.version) }}</div></template></el-table-column>
          <el-table-column v-if="colVisible('last_error')" label="最近错误" min-width="200"><template #default="{ row }"><span class="error-text">{{ row.last_error || '—' }}</span></template></el-table-column>
          <el-table-column v-if="colVisible('actions')" label="操作" width="166" :fixed="pinnedColumns ? 'right' : false"><template #default="{ row }"><div class="row-actions"><el-button link type="primary" :disabled="mutationBusy" @click="openEdit(row)">编辑</el-button><el-button link type="success" :loading="busyIDs.has(row.id)" :disabled="mutationBusy || !row.has_refresh_token" @click="refreshToken(row)">刷新令牌</el-button><el-dropdown trigger="click" @command="mobileAction($event, row)"><el-button link :disabled="mutationBusy" :aria-label="`账号 ${row.name} 的更多操作`">更多<el-icon><ArrowDown /></el-icon></el-button><template #dropdown><el-dropdown-menu><el-dropdown-item command="details">查看详情</el-dropdown-item><el-dropdown-item command="usage">上游额度</el-dropdown-item><el-dropdown-item command="models">模型目录</el-dropdown-item><el-dropdown-item command="test">连通测试</el-dropdown-item><el-dropdown-item command="schedules">定时检查</el-dropdown-item><el-dropdown-item command="stats">账号统计</el-dropdown-item><el-dropdown-item command="lease">账号到期管理</el-dropdown-item><el-dropdown-item command="oauth">OAuth 重新授权</el-dropdown-item><el-dropdown-item command="reauthorize">替换凭据 JSON</el-dropdown-item><el-dropdown-item command="delete" divided>删除账号</el-dropdown-item></el-dropdown-menu></template></el-dropdown></div></template></el-table-column>
        </el-table>
      </div>
      <div v-else class="mobile-account-list" :aria-busy="loading">
        <AccountListEmpty v-if="!accounts.length" :loading="loading" :error="loadError" :filtered="Boolean(activeFilterCount)" :busy="mutationBusy" @retry="load" @clear="clearFilters" @import="importVisible = true" />
        <article v-for="row in displayAccounts" :key="row.id" class="mobile-account" :class="{ selected: mobileSelected(row) }">
          <div class="mobile-account-heading"><el-checkbox :model-value="mobileSelected(row)" :aria-label="`选择账号 ${row.name}`" :disabled="mutationBusy || loading" @change="selectMobileAccount(row, $event)" /><div class="mobile-account-identity"><div class="account-identity-cell"><span class="account-identity-icon" aria-hidden="true"><ConsoleIcon name="accounts" :size="15" /></span><div class="account-identity-copy"><el-button link type="primary" class="account-name" :title="row.name" :aria-label="`查看账号 ${row.name} 详情`" @click="openDetails(row)">{{ row.name }}</el-button><div class="account-identity-badges"><span class="account-mini-pill" :class="`is-${credentialTone(row)}`" :title="credentialLabel(row)">{{ credentialLabel(row) }}</span><span class="account-token-hint" :title="row.has_refresh_token ? '含刷新令牌' : '无刷新令牌'"><ConsoleIcon :name="row.has_refresh_token ? 'refresh' : 'key'" :size="10" />{{ row.has_refresh_token ? '含刷新令牌' : '无刷新令牌' }}</span><span v-if="!row.enabled" class="account-mini-pill is-neutral">已停用</span></div></div></div></div></div>
          <div class="mobile-account-status"><el-tag :type="statusType(row)" size="small">{{ statusLabel(row) }}</el-tag><span v-if="expiryLabel(row)" class="expiry-warning">{{ expiryLabel(row) }}</span></div>
          <p v-if="colVisible('id')" class="today-usage-mobile plain-text">账号 ID：{{ row.id }} · 版本 {{ formatNumber(row.version) }}</p>
          <div v-if="row.last_error" class="account-row-error" :title="row.last_error">{{ row.last_error }}</div>
          <dl class="mobile-account-metrics"><div><dt>本地窗口请求</dt><dd>{{ formatNumber(row.usage?.requests) }}<small v-if="row.usage?.errors" class="error-text"> / {{ formatNumber(row.usage.errors) }} 失败</small></dd></div><div><dt>本地 Token · {{ row.window_days }} 天</dt><dd>{{ formatNumber(row.quota_used) }}<small> / {{ row.quota > 0 ? formatNumber(row.quota) : '不限额度' }}</small></dd></div><div><dt>并发占用</dt><dd>{{ formatNumber(row.in_flight) }}<small> / {{ formatNumber(row.concurrency) }}</small></dd></div><div><dt>缓存读写</dt><dd><el-button link type="primary" @click="openStats(row)">查看已观测统计</el-button></dd></div></dl>
          <p v-if="colVisible('last_used_at')" class="today-usage-mobile">最近业务请求：{{ lastUsedText(row.id) }}<br /><span class="muted">{{ lastUsedScopeLabel(lastUsedScope) }}</span></p>
          <p v-if="showTodayUsage" class="today-usage-mobile">今日留存请求：<template v-if="todayUsage[row.id]">{{ formatNumber(todayUsage[row.id].total_requests) }} 次 · {{ formatNumber(todayUsage[row.id].total_tokens) }} Token</template><template v-else>{{ todayLoading ? '读取中' : '未取得' }}</template></p>
          <AccountUsageWindows :snapshot="officialUsage[row.id]?.snapshot" :error="officialUsage[row.id]?.error" :loading="officialPending.has(row.id)" @open="openUpstream(row, 'usage')" />
          <div class="mobile-account-resources"><span>分组</span><AccountGroupList :names="(row.groups || []).map(groupName)" /><span>代理</span><div class="tag-list account-resource-tags is-proxies"><el-tag v-for="id in row.proxies" :key="id" size="small"><ConsoleIcon name="globe" :size="10" />{{ proxyName(id) }}</el-tag><span v-if="!row.proxies?.length" class="muted">直连</span></div></div>
          <div class="mobile-account-actions"><el-button size="small" :disabled="mutationBusy" @click="openEdit(row)">编辑</el-button><el-button size="small" @click="accountLogs(row)">使用记录</el-button><el-dropdown trigger="click" @command="mobileAction($event, row)"><el-button size="small" :aria-label="`账号 ${row.name} 的更多操作`">更多<el-icon><ArrowDown /></el-icon></el-button><template #dropdown><el-dropdown-menu><el-dropdown-item command="details">查看完整详情</el-dropdown-item><el-dropdown-item command="usage">上游额度</el-dropdown-item><el-dropdown-item command="models">模型目录</el-dropdown-item><el-dropdown-item command="test">连通测试</el-dropdown-item><el-dropdown-item command="schedules">定时检查</el-dropdown-item><el-dropdown-item command="stats">账号统计</el-dropdown-item><el-dropdown-item command="lease">账号到期管理</el-dropdown-item><el-dropdown-item command="oauth" :disabled="mutationBusy">OAuth 重新授权</el-dropdown-item><el-dropdown-item command="reauthorize" :disabled="mutationBusy">替换凭据 JSON</el-dropdown-item><el-dropdown-item command="refresh" :disabled="mutationBusy || !row.has_refresh_token">刷新令牌{{ row.has_refresh_token ? '' : '（无刷新令牌）' }}</el-dropdown-item><el-dropdown-item command="delete" divided :disabled="mutationBusy" class="mobile-delete-action">删除账号</el-dropdown-item></el-dropdown-menu></template></el-dropdown></div>
        </article>
      </div>
      <div class="account-pagination"><span class="muted">{{ loadError ? '分页信息暂不可用' : `共 ${formatNumber(total)} 个 · 第 ${page} / ${totalPages} 页` }}</span><el-select v-model="pageSize" aria-label="每页账号数" class="page-size"><el-option v-for="size in [10, 20, 50, 100, 200, 500]" :key="size" :label="`${size} / 页`" :value="size" /></el-select><div class="page-jump"><label for="account-page-number">跳至</label><input id="account-page-number" :value="pageJump" @input="pageJump = $event.target.value" type="number" :min="1" :max="totalPages" aria-label="账号页码" :disabled="loading || Boolean(loadError)" @keydown.enter.prevent="navigatePage(pageJump)" /><el-button :disabled="loading || Boolean(loadError)" @click="navigatePage(pageJump)">跳转</el-button></div><div class="pagination-buttons"><el-button :disabled="loading || Boolean(loadError) || page <= 1" @click="changePage(-1)">上一页</el-button><el-button :disabled="loading || Boolean(loadError) || !hasMore" @click="changePage(1)">下一页</el-button></div></div>
    </div>

    <el-dialog v-model="columnDialog" title="显示 / 隐藏列" width="min(560px, calc(100vw - 32px))" align-center append-to-body destroy-on-close class="detail-dialog"><p class="muted">{{ narrowTable ? '这里设置桌面表格显示的列。手机端点击账号名称或“更多 → 查看完整详情”，可查看全部字段。' : '默认保留主要配置和用量。更多元数据可按需显示，表格支持横向滚动。' }}</p><el-checkbox-group v-model="visibleColumns" class="column-options"><el-checkbox v-for="key in columnKeys" :key="key" :value="key" :disabled="key === 'name' || key === 'actions'">{{ columnLabels[key] }}</el-checkbox></el-checkbox-group><template #footer><el-button @click="visibleColumns = [...defaultColumns]">恢复默认</el-button><el-button type="primary" @click="columnDialog = false">完成</el-button></template></el-dialog>

    <el-dialog v-model="detailsVisible" title="账号详情" width="min(880px, calc(100vw - 32px))" class="detail-dialog account-detail-dialog" @opened="loadChart" @closed="disposeChart" align-center append-to-body destroy-on-close>
      <div v-if="details" v-loading="detailLoading">
        <div class="account-detail-hero"><div class="detail-avatar"><el-icon :size="24"><User /></el-icon></div><div><h3 class="detail-name">{{ details.name }}</h3><el-tag size="small" :type="statusType(details)">{{ statusLabel(details) }}</el-tag><span class="detail-enabled">{{ details.enabled ? '已启用' : '已停用' }}</span></div></div>
        <div class="detail-quick-actions"><el-button :disabled="mutationBusy || detailLoading || Boolean(detailError)" @click="openOAuth(details)">OAuth 重新授权</el-button><el-button :disabled="mutationBusy || detailLoading || Boolean(detailError)" @click="openReauthorize(details)">重新授权</el-button><el-button :disabled="!details.id" @click="openUpstream(details)">用量与测试</el-button><el-button :disabled="!details.id" @click="openStats(details)">账号统计</el-button><el-button :disabled="!details.id" @click="openSchedules(details)">定时检查</el-button><el-button :disabled="!details.id" @click="openLease(details)">账号到期管理</el-button><el-button :disabled="!details.id" @click="accountLogs(details)">查看使用记录</el-button></div>
                <el-alert v-if="detailError" :title="detailError" type="error" :closable="false" class="feedback" />
        <nav class="detail-section-links" aria-label="账号详情区块"><button v-for="section in [{ id: 'configuration', label: '基本信息' }, { id: 'identity', label: '登录身份' }, { id: 'runtime', label: '运行状态' }, { id: 'requests', label: '最近请求' }]" :key="section.id" type="button" @click="scrollDetailSection(section.id)">{{ section.label }}</button></nav>
        <h4 id="account-detail-configuration" class="detail-section-heading">基本信息</h4>
        <dl class="account-detail-fields">
          <div><dt>账号 ID / 版本</dt><dd><span class="plain-text">{{ details.id }} / {{ formatNumber(details.version) }}</span></dd></div>
          <div><dt>状态</dt><dd><el-tag :type="statusType(details)">{{ statusLabel(details) }}</el-tag></dd></div>
          <div><dt>启用状态</dt><dd>{{ details.enabled ? '已启用' : '已停用' }}</dd></div>
          <div><dt>并发占用 / 容量</dt><dd>{{ formatNumber(details.in_flight) }} / {{ formatNumber(details.concurrency) }}</dd></div>
          <div><dt>优先级 / 权重</dt><dd>{{ formatNumber(details.priority) }} / {{ formatNumber(details.weight) }}</dd></div>
          <div><dt>备注</dt><dd><span class="plain-text">{{ details.notes || '无备注' }}</span></dd></div>
          <div><dt>标签</dt><dd><div class="tag-list"><el-tag v-for="tag in details.tags" :key="tag">{{ tag }}</el-tag><span v-if="!details.tags?.length">无标签</span></div></dd></div>
          <div><dt>令牌到期</dt><dd>{{ formatTime(details.expires_at) }} <span class="expiry-warning">{{ expiryLabel(details) }}</span></dd></div>
          <div><dt>最近验证</dt><dd>{{ formatTime(details.last_checked_at) }}</dd></div>
          <div><dt>凭据状态</dt><dd>{{ credentialLabel(details) }} · {{ details.has_refresh_token ? '含刷新令牌' : '无刷新令牌' }}</dd></div>
          <div><dt>创建 / 更新</dt><dd>{{ formatTime(details.created_at) }} / {{ formatTime(details.updated_at) }}</dd></div>
          <div><dt>最近失败原因</dt><dd><span :class="details.last_error ? 'error-text' : 'muted'">{{ details.last_error || '无记录' }}</span></dd></div>
          <div><dt>分组 / 代理</dt><dd>{{ details.groups?.map(groupName).join('、') || '未分组' }} / {{ details.proxies?.map(proxyName).join('、') || '直连' }}</dd></div>
          <div><dt>本地 Token / 额度</dt><dd>{{ formatNumber(details.quota_used) }} / {{ details.quota === 0 ? '不限额度' : formatNumber(details.quota) }}<span v-if="details.quota > 0"> · {{ formatNumber(details.quota_percent) }}%</span></dd></div>
          <div><dt>本地统计窗口</dt><dd>{{ formatNumber(details.window_days) }} 天 · {{ usageWindow(details) }}</dd></div>
          <div><dt>请求 / 成功 / 失败</dt><dd>{{ formatNumber(details.usage?.requests) }} / {{ formatNumber(details.usage?.ok) }} / {{ formatNumber(details.usage?.errors) }}</dd></div>
          <div><dt>Token / 平均耗时</dt><dd>{{ formatNumber(details.usage?.tokens) }} / {{ details.usage?.requests > 0 ? formatDuration(details.usage.avg_latency_ms, { empty: '暂无样本' }) : '暂无样本' }}</dd></div>
          <div><dt>历史输入 / 输出 / 缓存读取</dt><dd>{{ formatNumber(details.usage?.input) }} / {{ formatNumber(details.usage?.output) }} / {{ formatNumber(details.usage?.cached) }}</dd></div>
          <div><dt>缓存读写与命中率</dt><dd><el-button link type="primary" @click="openStats(details)">查看已观测统计</el-button></dd></div>
        </dl>
        <p class="muted">请求与 Token 用量按上面的账号窗口统计，未作为跨全部历史的累计值。</p>
        <section id="account-detail-identity"><AccountIdentityPanel v-if="detailsVisible && details.id" :account="details" /></section>
        <section id="account-detail-runtime"><AccountRuntimePanel v-if="detailsVisible && details.id" :account="details" /></section>
        <div id="account-detail-requests" class="detail-toolbar"><h4>最近请求（最多 80 条）</h4><el-button :disabled="mutationBusy || !details.has_refresh_token" :loading="busyIDs.has(details.id)" @click="refreshToken(details)">刷新令牌</el-button></div>
        <p v-if="busyIDs.has(details.id)" role="status">正在等待服务器完成令牌刷新…</p>
        <p class="muted">令牌刷新完成后会显示结果；账号状态以最近验证和实际请求记录为准。</p>
        <div v-loading="chartLoading" class="chart-shell"><div ref="chartContainer" class="account-chart" /><p v-if="chartMessage" class="chart-message">{{ chartMessage }}</p></div>
        <div class="recent-records"><details v-for="(item, index) in [...recentItems].reverse()" :key="item.id || index" class="request-record"><summary>{{ formatTime(item.time) }} · {{ item.status === 'ok' ? '成功' : '失败' }} · {{ item.model || '未知模型' }}</summary><p>输入 {{ formatNumber(item.input_tokens) }} / 输出 {{ formatNumber(item.output_tokens) }} / 读取 {{ formatNumber(item.cached_tokens_known === true || item.cached_tokens_known !== false && item.cached_tokens > 0 ? item.cached_tokens : null) }} / 创建 {{ formatNumber(item.cache_write_tokens) }} · {{ formatDuration(item.latency_ms) }}</p><p v-if="item.error" class="error-text">{{ item.error }}</p><p v-if="item.proxy_name || item.ip">代理：{{ item.proxy_name || '—' }} · IP：{{ item.ip || '—' }}</p></details></div>
      </div>
      <template #footer><el-button @click="detailsVisible = false">关闭</el-button><el-button type="primary" :disabled="!details || mutationBusy || detailLoading || Boolean(detailError)" @click="openEdit(details)">编辑账号</el-button></template>
    </el-dialog>

    <el-dialog v-model="editVisible" :title="`编辑账号：${editing?.name || ''}`" width="min(560px, calc(100vw - 32px))" :close-on-click-modal="false" :before-close="closeEdit" :close-on-press-escape="!saving" :show-close="!saving" align-center append-to-body destroy-on-close class="detail-dialog">
      <el-alert v-if="actionError" :title="actionError" type="error" :closable="false" class="feedback" />
      <p class="dialog-intro">维护账号信息与资源配置，保存后生效。</p>
      <el-form label-width="100px" :disabled="saving" class="account-edit-form">
        <h4 class="form-section-title">基本信息</h4>
        <el-form-item label="账号名称"><el-input v-model="editForm.name" aria-label="编辑账号名称" /></el-form-item>
        <el-form-item label="启用"><el-switch v-model="editForm.enabled" aria-label="账号启用状态" /></el-form-item>
        <h4 class="form-section-title">容量与调度</h4>
        <el-form-item label="并发容量"><el-input-number v-model="editForm.concurrency" :min="1" :max="1000" :precision="0" controls-position="right" aria-label="账号并发容量" /><span class="field-hint">同时处理的最大请求数</span></el-form-item>
        <el-form-item label="优先级"><el-input-number v-model="editForm.priority" :precision="0" controls-position="right" aria-label="账号优先级" /><span class="field-hint">越大越优先被选中</span></el-form-item>
        <el-form-item label="权重"><el-input-number v-model="editForm.weight" :min="1" :max="1000" :precision="0" controls-position="right" aria-label="账号权重" /><span class="field-hint">同优先级内的分配权重</span></el-form-item>
        <h4 class="form-section-title">备注与资源归属</h4>
        <el-form-item label="备注"><el-input v-model="editForm.notes" type="textarea" :rows="3" aria-label="账号备注" /></el-form-item>
        <el-form-item label="标签"><el-select v-model="editForm.tags" multiple filterable allow-create default-first-option :reserve-keyword="false" class="full-width" placeholder="输入标签后按回车" aria-label="账号标签"><el-option v-for="tag in editForm.tags" :key="tag" :label="tag" :value="tag" /></el-select></el-form-item>
        <el-form-item label="分组"><el-select v-model="editForm.groups" multiple filterable class="full-width" aria-label="账号分组"><el-option v-for="group in groups" :key="group.id" :label="group.name" :value="group.id" /></el-select></el-form-item>
        <el-form-item label="绑定代理"><el-select v-model="editForm.proxies" multiple filterable class="full-width" placeholder="留空表示直连" aria-label="绑定代理"><el-option v-for="proxy in proxies" :key="proxy.id" :label="proxy.name" :value="proxy.id" /></el-select></el-form-item>
      </el-form>
      <template #footer><el-button :disabled="saving" @click="closeEdit">取消</el-button><el-button type="primary" :loading="saving" @click="saveEdit">保存</el-button></template>
    </el-dialog>

    <AccountBulkDialog v-model="bulkVisible" :targets="bulkTargets" :groups="groups" :proxies="proxies" :initial-action="bulkAction" @busy="bulkWorking = $event" @updated="afterAccountsChanged" />
    <AccountExportDialog v-model="exportVisible" :selection="selection" :filters="filters" :total="total" />
    <AccountReauthorizeDialog v-model="reauthorizeVisible" :account="reauthorizeAccount" @busy="reauthorizeWorking = $event" @updated="afterAccountsChanged" />
    <AccountBundleImportDialog v-model="bundleVisible" :groups="groups" :proxies="proxies" @busy="bundleWorking = $event" @updated="afterAccountsChanged" />
    <AccountOAuthDialog v-model="oauthVisible" :account="oauthAccount" :groups="groups" :proxies="proxies" @busy="oauthWorking = $event" @updated="afterAccountsChanged" />
    <AccountUpstreamDialog v-model="upstreamVisible" :account="upstreamAccount" :initial-tab="upstreamTab" @snapshot="updateOfficialUsage" />
    <AccountSchedulesDialog v-model="schedulesVisible" :account="schedulesAccount" />
    <AccountLeaseDialog v-model="leaseVisible" :account="leaseAccount" @busy="leaseWorking = $event" @updated="afterAccountsChanged" />
    <AccountStatsDialog v-model="statsVisible" :account="statsAccount" />
    <AccountBatchUpstreamDialog v-model="batchUpstreamVisible" :items="bulkTargets" :kind="batchUpstreamKind" @busy="batchUpstreamWorking = $event" />
    <AccountBatchReauthorizeDialog v-model="batchReauthorizeVisible" :targets="bulkTargets" @busy="batchReauthorizeWorking = $event" @updated="afterAccountsChanged" />
    <AccountCrsDialog v-model="crsVisible" :proxies="proxies" @busy="crsWorking = $event" @updated="afterAccountsChanged" />

    <el-dialog v-model="importVisible" title="导入账号 · 先预览再提交" class="detail-dialog account-import-dialog" width="min(920px, calc(100vw - 32px))" :close-on-click-modal="false" :close-on-press-escape="!importWorking" :show-close="!importWorking" @closed="closeImport" align-center append-to-body destroy-on-close>
      <p class="dialog-intro import-intro">直接导入 Sub2API 导出文件或 Codex 登录凭据，先核对账号，再选择导入。</p>
      <ol class="import-steps" aria-label="导入进度">
        <li :class="{ current: !importReady && !importSubmitted, complete: importReady || importSubmitted }"><span><el-icon v-if="importReady || importSubmitted"><Check /></el-icon><template v-else>1</template></span>添加账号</li>
        <li :class="{ current: importReady && !importSubmitted, complete: importSubmitted }"><span><el-icon v-if="importSubmitted"><Check /></el-icon><template v-else>2</template></span>核对预览</li>
        <li :class="{ current: importSubmitted }"><span>3</span>导入结果</li>
      </ol>
      <div class="import-source-switch" role="group" aria-label="账号导入来源">
        <button type="button" :class="{ selected: importSourceMode === 'files' }" :aria-pressed="importSourceMode === 'files'" :disabled="importWorking" @click="importSourceMode = 'files'"><el-icon><Upload /></el-icon>上传文件</button>
        <button type="button" :class="{ selected: importSourceMode === 'paste' }" :aria-pressed="importSourceMode === 'paste'" :disabled="importWorking" @click="importSourceMode = 'paste'"><el-icon><Document /></el-icon>粘贴内容</button>
      </div>
      <div v-if="importSourceMode === 'files'" class="import-file-source">
        <input ref="importFileInput" class="import-file-input" type="file" accept=".json,.jsonl,.ndjson,.txt" multiple :disabled="importWorking || importReading" aria-label="选择账号导入文件" @change="readImportFile" />
        <button type="button" class="import-dropzone" :class="{ dragging: importDragDepth > 0 }" :disabled="importWorking || importReading" @click="importFileInput?.click()" @dragenter.prevent="importDragDepth++" @dragleave.prevent="importDragDepth = Math.max(0, importDragDepth - 1)" @dragover.prevent @drop.prevent="dropImportFiles">
          <span class="import-upload-icon"><el-icon><UploadFilled /></el-icon></span>
          <strong>{{ importReading ? '正在读取文件…' : importFiles.length ? '重新选择或拖入文件' : '点击选择文件，或拖放到这里' }}</strong>
          <span>支持多个 JSON、JSONL、TXT 文件，总大小不超过 32 MB</span>
        </button>
        <div v-if="importFiles.length" class="import-file-list" aria-label="已选择的账号文件">
          <div v-for="(file, index) in importFiles" :key="`${file.name}-${index}`" class="import-file-row"><el-icon><Document /></el-icon><span :title="file.name">{{ file.name }}</span><small>{{ file.size < 1024 ? `${file.size} B` : `${(file.size / 1024).toFixed(1)} KB` }}</small><el-button link :disabled="importWorking" :aria-label="`移除文件 ${file.name}`" @click="importFiles = importFiles.filter((_, position) => position !== index)">移除</el-button></div>
        </div>
      </div>
      <div v-else class="import-paste-source">
        <el-input v-model="importText" :disabled="importWorking" aria-label="账号导入内容" type="textarea" :rows="7" placeholder="粘贴 Sub2API 导出内容、auth.json、JSON 数组，或每行一个 token。\n也支持：名称----access_token" spellcheck="false" autocomplete="off" />
      </div>
      <div class="import-format-hints"><span>支持格式</span><span>Sub2API</span><span>auth.json</span><span>JSON / JSONL</span><span>Token / 名称----Token</span></div>
      <section class="import-external-leases"><el-checkbox v-model="importExternalLeases" :disabled="importWorking || importSubmitted">识别 Sub2API 外部管理租期，导入账号后单独确认</el-checkbox><p class="muted">默认不应用外部租期。仅识别完整 OpenAI OAuth 账号外层的 expires_at / auto_pause_on_expired，登录凭据中的 expires_at 仍是令牌到期时间。勾选只生成租期预览和独立恢复回执，不直接写入租期。</p></section>
      <details class="import-advanced">
        <summary><span class="import-advanced-title"><el-icon><Setting /></el-icon>高级配置</span><span>分组、代理与调度参数</span></summary>
        <p class="dialog-hint">优先保留文件中的并发、优先级、权重等配置；以下默认值只补充缺失项。替换凭据时保留已有账号配置。</p>
        <el-form class="import-defaults" label-position="top" :disabled="importWorking">
        <el-row :gutter="16">
          <el-col :xs="24" :sm="12"><el-form-item label="并发容量"><el-input-number v-model="importConfig.concurrency" :min="1" :max="1000" :precision="0" controls-position="right" aria-label="导入默认并发容量" /></el-form-item></el-col>
          <el-col :xs="24" :sm="12"><el-form-item label="权重"><el-input-number v-model="importConfig.weight" :min="1" :max="1000" :precision="0" controls-position="right" aria-label="导入默认权重" /></el-form-item></el-col>
          <el-col :xs="24" :sm="12"><el-form-item label="优先级"><el-input-number v-model="importConfig.priority" :precision="0" controls-position="right" aria-label="导入默认优先级" /></el-form-item></el-col>
        </el-row>
        <el-form-item label="新增账号分组"><el-select v-model="importGroups" multiple class="full-width" aria-label="新增账号默认分组"><el-option v-for="group in groups" :key="group.id" :label="group.name" :value="group.id" /></el-select></el-form-item>
        <el-form-item label="绑定代理"><el-select v-model="importConfig.proxies" multiple class="full-width" aria-label="新增账号默认代理" placeholder="留空表示直连"><el-option v-for="proxy in proxies" :key="proxy.id" :label="proxy.name" :value="proxy.id" /></el-select></el-form-item>
        <el-checkbox v-model="importConfig.override_imported">使用以上配置覆盖文件中已有的配置</el-checkbox>
        <p class="muted">跨系统的分组和代理需要在这里重新选择；修改配置后请重新生成预览。</p>
      </el-form>
      </details>
      <el-alert v-if="importHasExternalBindings" type="warning" :closable="false" show-icon title="文件包含原系统的代理或分组。导出文件中的代理不会自动创建，原绑定不会直接沿用；请在高级配置中选择对应的本地代理和分组。" class="feedback" />
      <el-alert v-if="importError" :title="importError" type="error" :closable="false" class="feedback" />
      <template v-if="importRows.length">
        <div class="import-preview-heading"><h3>{{ importSubmitted ? '本次导入' : '账号预览' }}</h3><span>共 {{ importRows.length }} 项</span></div>
        <div class="preview-counts" role="status"><el-tag type="success">新增 {{ importCounts.new }}</el-tag><el-tag type="info">重复 {{ importCounts.duplicate }}</el-tag><el-tag type="warning">替换 {{ importCounts.replace }}</el-tag><el-tag type="danger">失败 {{ importCounts.failed }}</el-tag></div>
        <p class="dialog-hint">重复凭据自动跳过；同名账号需逐项选择替换。格式错误的项目不会导入，其余有效项目可继续。</p>
<div class="table-scroll preview-table"><el-table :data="importRows" max-height="330"><el-table-column prop="line" label="行号" width="66" /><el-table-column prop="name" label="账号名称" min-width="160"><template #default="{ row }"><span>{{ row.name || '自动命名' }}</span><div v-if="row.sourceFile" class="muted">{{ row.sourceFile }}</div></template></el-table-column><el-table-column label="分类" width="95"><template #default="{ row }"><el-tag size="small" :type="row.kind === 'new' ? 'success' : row.kind === 'failed' ? 'danger' : row.kind === 'replace' ? 'warning' : 'info'">{{ classificationLabel(row.kind) }}</el-tag></template></el-table-column><el-table-column label="预览说明" min-width="230"><template #default="{ row }">{{ row.message }}<div v-for="(warning, index) in row.warnings" :key="index" class="import-warning">{{ warning }}</div></template></el-table-column><el-table-column label="明确选择" width="180"><template #default="{ row }"><el-select v-model="row.action" :aria-label="`第 ${row.line} 行导入处理方式`" :disabled="!importReady || importWorking || importSubmitted || row.kind === 'failed' || row.locked"><el-option label="跳过" value="skip" /><el-option v-if="row.kind === 'new'" label="新增" value="add" /><el-option v-if="row.kind === 'replace'" label="替换已有凭据" value="replace" /><el-option v-if="canPrepareDuplicateLease(row, importExternalLeases)" label="保留账号，仅准备租期确认" value="lease" /></el-select></template></el-table-column></el-table></div>
        <section v-if="importExternalLeases && importExternalLeaseRows.length" class="external-lease-preview" aria-label="外部租期预览">
          <h4>外部管理租期 · {{ importExternalLeaseRows.length }} 项</h4><p class="muted">来源为 Sub2API 账号外层管理字段；以下为服务器解析结果。只有选中导入并取得明确账号回执后，才可另外确认应用租期。</p>
          <article v-for="row in importExternalLeaseRows" :key="row.sourceIndex"><strong>{{ row.name }} · {{ row.sourceFile || '粘贴内容' }} · 第 {{ row.line }} 行</strong><p>租期：{{ row.externalLease.expires_at ? formatTime(row.externalLease.expires_at) : '不限期' }} · 自动停用：{{ row.externalLease.auto_disable ? '是' : '否' }} · 延期自动恢复：关闭</p><p class="muted">{{ row.externalLease.auto_disable_source === 'sub2api_default' ? '原数据未明确自动停用设置，按 Sub2API 默认开启。' : '自动停用设置来自原文件的明确值。' }} {{ externalLeaseSummary(row.externalLease) }}。</p><span class="muted">{{ row.action === 'skip' ? '此账号当前选择跳过，租期不会纳入恢复。' : '账号导入不会自动应用此租期。' }}</span></article>
        </section>
        <div v-if="importing" class="import-progress" role="status"><span>正在提交 {{ importResults.length }} / {{ pendingImport.length }}</span><progress :value="importProgress" max="100" class="import-progress-bar" aria-label="账号导入进度" /></div>
        <el-alert v-if="importSubmitted && !importing" :type="importResultCounts.failed ? 'warning' : 'success'" :closable="false" show-icon :title="`导入完成：成功 ${importResultCounts.success} 项，失败 ${importResultCounts.failed} 项，跳过 ${importResultCounts.skipped} 项`" class="import-result-summary" />
        <div v-if="importResults.length" class="table-scroll result-table"><el-table :data="importResults" max-height="250"><el-table-column prop="line" label="行号" width="66" /><el-table-column prop="name" label="账号" min-width="180" /><el-table-column label="结果" width="75"><template #default="{ row }"><el-tag :type="row.status === 'success' ? 'success' : row.status === 'skipped' ? 'info' : 'danger'">{{ row.status === 'success' ? '成功' : row.status === 'skipped' ? '跳过' : '失败' }}</el-tag></template></el-table-column><el-table-column prop="message" label="说明" min-width="200" /></el-table></div>
        <div class="download-actions"><el-button :icon="Download" :disabled="importWorking" @click="downloadImport(false)">下载脱敏结果</el-button><el-button :disabled="importWorking" @click="downloadImport(true)">下载失败项</el-button></div>
      </template>
      <section v-for="entry in importLeaseReceipts" :key="entry.key" class="external-lease-result" :aria-label="'外部租期回执 ' + entry.name"><h4>{{ entry.name }} · {{ entry.sourceFile || '粘贴内容' }} · 第 {{ entry.line }} 行</h4><p class="muted">Sub2API 外层管理租期。此回执只对应本次单行账号请求，索引按服务端回执保留；不会与其他文件回执合并。</p><el-alert v-if="entry.error" :title="entry.error" type="error" :closable="false" /><AccountLeaseRestorePanel v-if="entry.receipt" :receipt="entry.receipt" :disabled="importWorking" @busy="setImportLeaseBusy(entry.key, $event)" @updated="load" /></section>
      <template #footer><div class="import-footer"><span class="import-footer-hint">{{ importReady ? `已选择 ${pendingImport.length} 项，可逐项调整` : '预览后确认，账号才会保存' }}</span><div class="import-footer-actions"><el-button :disabled="importWorking" @click="importVisible = false">关闭</el-button><el-button :loading="importPreviewing || importReading" :disabled="importWorking || !importHasSource" @click="previewImport">生成预览</el-button><el-button type="primary" :loading="importing" :disabled="importWorking || !importReady || !pendingImport.length || importPreviewing || importSubmitted" @click="doImport">确认导入 {{ pendingImport.length }} 项</el-button></div></div></template>
    </el-dialog>
  </div>
</template>

<style scoped>
.import-external-leases { margin: 14px 0; padding-top: 12px; border-top: 1px solid var(--card-border); }.import-external-leases .el-checkbox { height: auto; white-space: normal; align-items: flex-start; }.import-external-leases :deep(.el-checkbox__label) { white-space: normal; line-height: 1.7; }.external-lease-preview, .external-lease-result { margin-top: 18px; padding-top: 14px; border-top: 1px solid var(--card-border); font-size: 12px; line-height: 1.8; min-width: 0; }.external-lease-preview h4, .external-lease-result h4 { font-size: 13px; font-weight: 500; color: var(--text-1); margin: 0 0 10px; overflow-wrap: anywhere; }.external-lease-preview article { padding: 11px 0; border-top: 1px solid var(--card-border); overflow-wrap: anywhere; }.external-lease-preview strong { font-weight: 500; }.external-lease-preview p { margin: 5px 0; }.external-lease-result :deep(.lease-restore) { margin-top: 12px; }
.auto-refresh-select { width: 160px; margin-left: auto; }
.today-usage-note { display: flex; flex-wrap: wrap; gap: 8px; padding: 0 20px 12px; color: var(--text-3); font-size: 12px; line-height: 1.7; }
.today-usage-mobile { font-size: 12px; line-height: 1.7; color: var(--text-3); margin: 0 0 12px; }
.auto-refresh-status { font-size: 12px; color: var(--text-3); }
.directory-sort { color: var(--text-3); font-size: 12px; }
.account-directory-heading h3 .directory-count { margin-left: 10px; font-weight: 400; }
.selection-tools, .selection-actions { display: flex; flex-wrap: wrap; align-items: center; gap: 8px 12px; }
.selection-tools .el-checkbox { margin: 0; }
.selection-tools .el-button + .el-button, .selection-actions .el-button + .el-button { margin-left: 0; }
.page-jump { display: flex; align-items: center; gap: 8px; color: var(--text-3); font-size: 12px; }
.page-jump input, .account-advanced-filters input[type="datetime-local"] { box-sizing: border-box; min-width: 0; height: 32px; padding: 0 9px; color: var(--text-1); background: var(--el-fill-color-blank); border: 1px solid var(--el-border-color); border-radius: 4px; font: inherit; }
.page-jump input { width: 60px; }
.account-advanced-filters input[type="datetime-local"] { width: 100%; color-scheme: light dark; }
.filter-flags { display: flex; flex-wrap: wrap; gap: 6px 18px; }
.filter-flags .el-checkbox { margin: 0; }
.quota-progress, .import-progress-bar { display: block; width: 100%; height: 6px; appearance: none; border: 0; border-radius: 4px; overflow: hidden; background: var(--el-fill-color); accent-color: var(--el-color-primary); }
progress::-webkit-progress-bar { background: var(--el-fill-color); }
progress::-webkit-progress-value { background: var(--el-color-primary); }
.account-detail-fields { margin: 0; border: 1px solid var(--card-border); font-size: 12px; line-height: 1.7; }
.account-detail-fields > div { display: grid; grid-template-columns: 140px minmax(0, 1fr); border-bottom: 1px solid var(--card-border); }
.account-detail-fields > div:last-child { border-bottom: 0; }
.account-detail-fields dt { padding: 10px; color: var(--text-3); background: var(--el-fill-color-light); }
.account-detail-fields dd { margin: 0; padding: 10px; overflow-wrap: anywhere; min-width: 0; }
.account-overview { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); margin-bottom: 16px; border: 1px solid var(--card-border); border-radius: 10px; background: var(--card-bg); overflow: hidden; }
.account-overview-card { display: grid; grid-template-columns: 1fr auto; align-items: baseline; gap: 3px 8px; padding: 12px 18px; }
.account-overview-card + .account-overview-card { border-left: 1px solid var(--card-border); }
.account-overview-card span { color: var(--text-2); font-size: 12px; }
.account-overview-card strong { grid-column: 2; grid-row: 1 / 3; align-self: center; color: var(--text-1); font-size: 23px; line-height: 1.3; font-weight: 600; font-variant-numeric: tabular-nums; }
.account-overview-card.attention strong { color: var(--warning); }
.account-overview-card small { color: var(--text-3); font-size: 11px; }
.account-directory-heading { display: flex; align-items: center; justify-content: space-between; gap: 14px; flex-wrap: wrap; padding: 15px 18px 12px; }
.account-directory-heading h3 { margin: 0; font-size: 14px; font-weight: 600; }
.directory-count { font-size: 11px; color: var(--text-3); }
.account-filters { margin-bottom: 0; padding: 0 18px 14px; }
.account-filter-chips { display: flex; flex-wrap: wrap; align-items: center; gap: 7px; padding: 0 18px 12px; }
.filter-chip-label { font-size: 11px; color: var(--text-3); }
.filter-chip { display: inline-flex; align-items: center; gap: 6px; max-width: 100%; min-height: 28px; padding: 3px 8px; border: 1px solid var(--el-color-primary-light-7); border-radius: 6px; color: var(--accent-blue); background: var(--accent-blue-soft); font: inherit; font-size: 12px; cursor: pointer; }
.filter-chip > span { max-width: 280px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.filter-chip .el-icon { flex-shrink: 0; }
.filter-chip:focus-visible { outline: 2px solid var(--accent-blue); outline-offset: 2px; }
.identity-separator { margin: 0 6px; color: var(--el-border-color); }
.account-row-error { margin-top: 5px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 11px; line-height: 1.5; color: var(--danger); }
.mobile-account { padding: 15px 14px; border-top: 1px solid var(--card-border); transition: background .15s; }
.mobile-account.selected { background: var(--accent-blue-soft); }
.mobile-account-heading { display: flex; align-items: flex-start; gap: 10px; }
.mobile-account-heading > .el-checkbox { height: 28px; margin-right: 0; }
.mobile-account-identity { flex: 1; min-width: 0; }
.mobile-account-identity .account-name { font-size: 14px; font-weight: 600; }
.mobile-account-status { display: flex; align-items: center; gap: 10px; margin-top: 10px; }
.mobile-account-metrics { display: grid; grid-template-columns: 1fr 1.25fr; gap: 10px 16px; margin: 14px 0; }
.mobile-account-metrics > div { min-width: 0; }
.mobile-account-metrics dt { margin-bottom: 3px; font-size: 11px; color: var(--text-3); }
.mobile-account-metrics dd { margin: 0; color: var(--text-1); font-size: 14px; font-weight: 500; font-variant-numeric: tabular-nums; overflow-wrap: anywhere; }
.mobile-account-metrics small { color: var(--text-3); font-size: 11px; font-weight: 400; }
.mobile-account-metrics small.error-text { color: var(--danger); }
.mobile-account-resources { display: grid; grid-template-columns: auto 1fr; gap: 5px 8px; font-size: 11px; line-height: 1.6; }
.mobile-account-resources span:nth-child(odd) { color: var(--text-3); }
.mobile-account-resources span:nth-child(even) { overflow-wrap: anywhere; color: var(--text-2); }
.mobile-account-actions { display: flex; align-items: center; gap: 8px; margin-top: 13px; }
.mobile-account-actions .el-button + .el-button { margin-left: 0; }
.mobile-account-actions .el-dropdown { margin-left: auto; }
.mobile-delete-action { color: var(--danger); }
.filter-chevron { margin-left: 6px; }
.account-advanced-filters { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 14px; align-items: end; padding: 15px 20px; border-top: 1px solid var(--card-border); background: var(--el-fill-color-lighter); }
.account-advanced-filters > label { display: flex; flex-direction: column; gap: 7px; font-size: 12px; color: var(--text-3); }
.account-advanced-filters .el-select { width: 100%; }
.filter-save-actions { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.account-summary.has-selection { color: var(--accent-blue); background: var(--accent-blue-soft); }
.account-detail-hero { display: flex; align-items: flex-start; gap: 14px; margin-bottom: 18px; }
.account-detail-hero > div:last-child { min-width: 0; }
.detail-avatar { width: 46px; height: 46px; display: grid; place-items: center; flex: 0 0 46px; color: var(--accent-blue); background: var(--accent-blue-soft); border-radius: 12px; }
.account-detail-hero .detail-name { margin: 0 0 9px; font-size: 18px; }
.detail-enabled { margin-left: 10px; color: var(--text-3); font-size: 12px; }
.detail-quick-actions { display: flex; flex-wrap: wrap; gap: 8px; margin-bottom: 20px; }
.detail-quick-actions .el-button + .el-button { margin-left: 0; }
.form-section-title { margin: 24px 0 16px; padding-bottom: 10px; border-bottom: 1px solid var(--card-border); font-size: 12px; font-weight: 600; color: var(--text-2); }
.form-section-title:first-child { margin-top: 8px; }

.accounts-page, .accounts-panel, .table-scroll { min-width: 0; max-width: 100%; }
.accounts-panel { padding: 0; overflow: hidden; }
.table-scroll { overflow-x: auto; }
.feedback { margin-bottom: 14px; }
.account-filters { flex-wrap: wrap; gap: 10px; }
.name-filter { width: 230px; }
.select-filter { width: 170px; }
.account-summary, .account-pagination, .detail-toolbar { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
.account-summary { padding: 10px 20px; border-top: 1px solid var(--card-border); color: var(--text-3); font-size: 12px; }
.account-pagination { padding: 16px; border-top: 1px solid var(--card-border); }
.page-size { width: 110px; }
.pagination-buttons { display: flex; }
.account-name { display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical; overflow: hidden; white-space: normal; text-align: left; line-height: 1.5; height: auto; overflow-wrap: anywhere; }
.account-name :deep(span) { display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical; overflow: hidden; max-height: 3em; line-height: 1.5; overflow-wrap: anywhere; white-space: normal; }
.row-actions { display: flex; gap: 8px; align-items: center; }
.row-actions .el-button + .el-button { margin-left: 0; }
.muted { color: var(--text-3); font-size: 12px; line-height: 1.6; }
.expiry-warning { color: var(--warning); font-size: 12px; line-height: 1.5; }
.error-text { color: var(--danger); white-space: pre-wrap; overflow-wrap: anywhere; }
.plain-text { white-space: pre-wrap; overflow-wrap: anywhere; }
.quota-caption { margin-bottom: 5px; font-variant-numeric: tabular-nums; }
.field-hint { margin-left: 12px; color: var(--text-3); font-size: 12px; line-height: 1.5; }
.account-edit-form :deep(.el-form-item__content) { gap: 6px 0; }
.account-edit-form :deep(.el-input-number), .import-defaults :deep(.el-input-number) { width: 180px; max-width: 100%; }
.column-options { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 4px; }
.detail-name { font-size: 17px; line-height: 1.5; overflow-wrap: anywhere; margin: 0 0 18px; }
.full-width { width: 100%; }
.chart-shell { min-height: 100px; position: relative; }
.account-chart { width: 100%; height: 100px; }
.chart-message { position: absolute; top: 25%; left: 12px; color: var(--text-3); }
.request-record { border-top: 1px solid var(--card-border); padding: 12px 0; font-size: 12px; line-height: 1.7; overflow-wrap: anywhere; }
.request-record summary { cursor: pointer; color: var(--text-2); }
.request-record p { margin: 6px 0; }
.bulk-field, .result-table, .import-defaults { margin-top: 16px; }
.file-picker { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; margin: 12px 0; }
.file-picker input { max-width: 100%; min-width: 0; }
.preview-counts, .download-actions { display: flex; gap: 8px; flex-wrap: wrap; margin: 14px 0; }
.preview-table :deep(.cell), .result-table :deep(.cell), :deep(.el-descriptions__content) { overflow-wrap: anywhere; white-space: normal; }
.tag-list :deep(.el-tag) { height: auto; white-space: normal; overflow-wrap: anywhere; }
.import-intro { margin-top: 0; }
.import-steps { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin: 0 0 24px; padding: 15px 20px; list-style: none; border: 1px solid var(--card-border); border-radius: 12px; background: var(--el-fill-color-light); }
.import-steps li { display: flex; align-items: center; gap: 8px; color: var(--text-3); font-size: 13px; white-space: nowrap; }
.import-steps li > span { display: grid; place-items: center; width: 25px; height: 25px; border: 1px solid var(--card-border); border-radius: 50%; font-size: 12px; background: var(--card-bg); }
.import-steps li.current { color: var(--text-1); font-weight: 600; }
.import-steps li.current > span { background: var(--el-color-primary); border-color: var(--el-color-primary); color: white; }
.import-steps li.complete > span { background: var(--el-color-primary-light-9); border-color: var(--el-color-primary-light-7); color: var(--el-color-primary); }
.import-source-switch { display: inline-flex; gap: 4px; padding: 4px; margin-bottom: 14px; border: 1px solid var(--card-border); border-radius: 10px; background: var(--el-fill-color-light); }
.import-source-switch button { display: inline-flex; align-items: center; justify-content: center; gap: 8px; min-height: 34px; padding: 6px 18px; border: 1px solid transparent; border-radius: 7px; color: var(--text-2); font: inherit; font-size: 13px; background: transparent; cursor: pointer; }
.import-source-switch button.selected { background: var(--card-bg); color: var(--el-color-primary); border-color: var(--card-border); box-shadow: 0 1px 3px rgb(0 0 0 / 5%); font-weight: 600; }
.import-file-input { position: absolute; width: 1px; height: 1px; padding: 0; overflow: hidden; clip: rect(0, 0, 0, 0); white-space: nowrap; border: 0; }
.import-dropzone { display: flex; width: 100%; min-height: 178px; padding: 24px 16px; flex-direction: column; align-items: center; justify-content: center; gap: 9px; border: 1px dashed var(--el-color-primary-light-5); border-radius: 12px; background: color-mix(in srgb, var(--el-color-primary) 3%, var(--card-bg)); color: var(--text-1); font: inherit; text-align: center; cursor: pointer; transition: border-color .18s, background .18s; }
.import-dropzone:hover, .import-dropzone.dragging { border-color: var(--el-color-primary); background: var(--el-color-primary-light-9); }
.import-dropzone:disabled, .import-source-switch button:disabled { cursor: not-allowed; opacity: .6; }
.import-dropzone strong { font-size: 14px; font-weight: 600; }
.import-dropzone > span:last-child { color: var(--text-3); font-size: 12px; line-height: 1.5; }
.import-upload-icon { display: grid; place-items: center; width: 43px; height: 43px; margin-bottom: 3px; border-radius: 12px; background: var(--el-color-primary-light-9); color: var(--el-color-primary); font-size: 25px; }
.import-file-list { display: grid; gap: 6px; max-height: 150px; overflow-y: auto; margin-top: 10px; }
.import-file-row { display: flex; align-items: center; gap: 10px; min-width: 0; padding: 9px 12px; border: 1px solid var(--card-border); border-radius: 8px; font-size: 12px; }
.import-file-row > .el-icon { color: var(--el-color-primary); flex-shrink: 0; }
.import-file-row > span { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.import-file-row small { color: var(--text-3); white-space: nowrap; }
.import-paste-source :deep(textarea) { font-family: ui-monospace, SFMono-Regular, Consolas, monospace; font-size: 12px; line-height: 1.75; padding: 14px 16px; }
.import-format-hints { display: flex; align-items: center; flex-wrap: wrap; gap: 6px; margin: 12px 0 20px; font-size: 11px; color: var(--text-3); }
.import-format-hints span:not(:first-child) { padding: 3px 7px; border: 1px solid var(--card-border); border-radius: 5px; color: var(--text-2); background: var(--el-fill-color-light); }
.import-advanced { border: 1px solid var(--card-border); border-radius: 10px; padding: 0 16px; margin-bottom: 20px; }
.import-advanced summary { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 8px; padding: 15px 0; cursor: pointer; color: var(--text-3); font-size: 12px; list-style: none; }
.import-advanced summary::-webkit-details-marker { display: none; }
.import-advanced summary::after { content: '+'; font-size: 18px; line-height: 1; }
.import-advanced[open] summary::after { content: '−'; }
.import-advanced-title { display: inline-flex; align-items: center; gap: 8px; margin-right: auto; color: var(--text-1); font-size: 13px; font-weight: 600; }
.import-advanced .import-defaults { margin-top: 16px; }
.import-defaults :deep(.el-input-number) { width: 100%; }
.import-defaults :deep(.el-checkbox) { white-space: normal; height: auto; margin: 0 0 8px; }
.import-defaults :deep(.el-checkbox__label) { white-space: normal; line-height: 1.5; }
.import-preview-heading { display: flex; justify-content: space-between; align-items: center; gap: 12px; margin-top: 24px; }
.import-preview-heading h3 { margin: 0; color: var(--text-1); font-size: 14px; }
.import-preview-heading > span { font-size: 12px; color: var(--text-3); }
.import-warning { color: var(--text-3); font-size: 11px; line-height: 1.6; margin-top: 4px; }
.preview-table, .result-table { border: 1px solid var(--card-border); border-radius: 10px; }
.import-progress { display: grid; gap: 8px; margin-top: 16px; font-size: 12px; color: var(--text-2); }
.import-result-summary { margin-top: 16px; }
.import-footer { display: flex; width: 100%; align-items: center; justify-content: space-between; gap: 16px; flex-wrap: wrap; }
.import-footer-hint { color: var(--text-3); font-size: 12px; }
.import-footer-actions { display: flex; flex-wrap: wrap; gap: 8px; margin-left: auto; }
.import-footer-actions .el-button + .el-button { margin-left: 0; }
@media (max-width: 1100px) { .account-advanced-filters { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (max-width: 767px) {
  .directory-sort { display: none; }
  .selection-tools { gap: 4px 8px; }
  .selection-actions { width: 100%; justify-content: flex-end; }
  .account-detail-fields > div { grid-template-columns: 105px minmax(0, 1fr); }
  .account-overview { grid-template-columns: repeat(2, minmax(0, 1fr)); margin-bottom: 12px; }
  .account-overview-card { padding: 10px 12px; }
  .account-overview-card:nth-child(3) { border-left: 0; }
  .account-overview-card:nth-child(n + 3) { border-top: 1px solid var(--card-border); }
  .account-overview-card strong { font-size: 21px; }
  .account-directory-heading { padding: 14px 12px 12px; }
  .directory-count { font-size: 10px; }
  .account-filters { padding: 0 12px 12px; }
  .account-filter-chips { padding: 0 12px 12px; }
  .filter-chip > span { max-width: 205px; }
  .account-advanced-filters { padding: 12px; gap: 12px; }
  .filter-save-actions { grid-column: 1 / -1; }
  .account-summary { gap: 6px 10px; }
  .account-summary > span { flex: 1; text-align: right; font-size: 11px; }
  .account-summary > .el-button { width: 100%; margin-left: 0; }
  .account-pagination > .muted { width: 100%; }
}
@media (max-width: 600px) {
  .account-advanced-filters { grid-template-columns: 1fr; }
  .name-filter { width: 100%; }
  .select-filter { width: calc(50% - 5px); }
  .account-summary, .account-pagination { padding-left: 12px; padding-right: 12px; }
  .account-pagination { justify-content: center; }
  .column-options { grid-template-columns: 1fr; }
  .download-actions .el-button { margin-left: 0; }
  .import-steps { gap: 6px; padding: 12px 9px; }
  .import-steps li { gap: 5px; font-size: 11px; }
  .import-steps li > span { width: 21px; height: 21px; }
  .import-advanced summary > span:nth-child(2) { display: none; }
  .import-footer { gap: 10px; }
  .import-footer-hint { width: 100%; text-align: left; }
  .import-footer-actions { display: grid; grid-template-columns: 1fr 1fr; width: 100%; justify-content: flex-end; }
  .import-footer-actions .el-button { padding: 8px 10px; }
  .import-footer-actions .el-button:last-child { grid-column: 1 / -1; width: 100%; }
}
/* Resource workspace: the same hierarchy in Linen and Orbit. */
.account-overview { border: 0; border-radius: 0; gap: 16px; margin: 26px 0 24px; background: transparent; overflow: visible; }
.account-overview-card { position: relative; display: flex; flex-direction: column; align-items: flex-start; gap: 9px; min-height: 132px; padding: 22px 24px; border: 1px solid var(--card-border); border-radius: 13px; background: var(--card-bg); box-shadow: 0 2px 5px rgb(17 24 39 / 2%); }
.account-overview-card + .account-overview-card { border-left: 1px solid var(--card-border); }
.account-overview-card > span:first-child { padding-right: 30px; font-size: 11px; color: var(--text-3); }
.account-overview-card > strong { align-self: flex-start; color: var(--text-1); font-size: 29px; font-weight: 600; letter-spacing: -.8px; line-height: 1.25; }
.account-overview-card > small { display: flex; align-items: center; gap: 6px; font-size: 10px; line-height: 1.5; color: var(--text-3); }
.account-overview-card .overview-mark { position: absolute; top: 21px; right: 20px; width: 32px; height: 32px; display: grid; place-items: center; border: 1px solid var(--card-border); border-radius: 9px; background: var(--bg); color: var(--text-3); font-size: 15px; }
.account-overview-card .overview-mark.positive { color: var(--success); background: color-mix(in srgb,var(--success) 7%,var(--card-bg)); border-color: color-mix(in srgb,var(--success) 14%,var(--card-border)); }
.account-overview-card.attention > strong { color: var(--warning); }
.account-overview-card.attention .overview-mark { color: var(--warning); background: color-mix(in srgb,var(--warning) 7%,var(--card-bg)); }
.overview-dot { width: 5px; height: 5px; border-radius: 50%; background: var(--success); }
.overview-stale { opacity: .62; }
.accounts-panel { border-radius: 14px; box-shadow: 0 2px 5px rgb(17 24 39 / 2%); }
.account-directory-heading { padding: 23px 24px 20px; gap: 16px; }
.account-directory-heading h3 { font-size: 15px; font-weight: 600; letter-spacing: -.3px; margin-bottom: 5px; }
.account-directory-heading h3 .directory-count { display: inline-block; margin-left: 8px; padding: 3px 7px; border: 1px solid var(--card-border); border-radius: 5px; font-size: 10px; line-height: 1; vertical-align: 2px; }
.directory-sort { font-size: 10px; color: var(--text-3); }
.directory-tools { display: flex; align-items: center; gap: 8px; }
.directory-tools .el-button { font-size: 11px; }
.directory-tools .el-button + .el-button { margin-left: 0; }
.directory-tools :deep(.el-icon) { margin-left: 3px; }
.account-filters { padding: 0 24px 20px; gap: 10px; }
.account-filters .name-filter { width: min(330px,40%); flex-grow: 1; }
.account-filters .select-filter { width: 160px; }
.account-filters .el-button { margin-left: 0; font-size: 12px; }
.account-view-options { display: flex; align-items: center; justify-content: space-between; flex-wrap: wrap; gap: 12px; padding: 11px 24px; border-top: 1px solid var(--card-border); background: color-mix(in srgb,var(--bg) 50%,var(--card-bg)); }
.view-option-label { display: flex; align-items: center; gap: 8px; font-size: 11px; color: var(--text-2); }
.view-option-dot { width: 5px; height: 5px; border-radius: 50%; background: var(--text-3); opacity: .65; }
.view-option-hint { color: var(--text-3); font-size: 10px; padding-left: 4px; }
.account-view-controls { display: flex; align-items: center; flex-wrap: wrap; gap: 14px; }
.account-view-controls .el-checkbox { margin-right: 0; height: 28px; }
.account-view-controls :deep(.el-checkbox__label) { font-size: 11px; }
.account-view-controls .auto-refresh-select { width: 146px; margin-left: 0; }
.auto-refresh-status { font-size: 10px; }
.account-advanced-filters { padding: 22px 24px; gap: 18px 16px; background: color-mix(in srgb,var(--bg) 52%,var(--card-bg)); }
.account-advanced-filters > label { font-size: 11px; gap: 9px; }
.account-advanced-filters input[type="datetime-local"] { border-radius: 7px; height: 36px; }
.account-filter-chips { padding: 14px 24px; border-top: 1px solid var(--card-border); }
.filter-chip { font-size: 11px; color: var(--primary); background: var(--primary-soft); border-color: color-mix(in srgb,var(--primary) 16%,var(--card-border)); }
.account-summary { padding: 13px 24px; min-height: 54px; font-size: 11px; background: var(--card-bg); }
.account-summary.has-selection { color: var(--primary); background: var(--primary-soft); }
.selection-tools .el-button { font-size: 11px; }
.selection-tools :deep(.el-checkbox__label) { font-size: 11px; }
.selection-actions { gap: 8px; }
.selection-actions .el-button { font-size: 11px; min-height: 30px; height: 30px; padding: 7px 11px; }
.table-scroll :deep(.el-table__header-wrapper th) { height: 42px; font-size: 11px; font-weight: 500; color: var(--text-3); background: color-mix(in srgb,var(--bg) 66%,var(--card-bg)); }
.table-scroll :deep(.el-table__body td) { padding-top: 17px; padding-bottom: 17px; }
.accounts-page .table-scroll :deep(.el-table tbody tr:hover > td.el-table__cell),
.accounts-page .table-scroll :deep(.el-table tbody tr.hover-row > td.el-table__cell) { background: var(--el-table-tr-bg-color, var(--card-bg)) !important; }
.accounts-page .table-scroll :deep(.el-table tbody td.el-table__cell) { transition: none; }
.table-scroll :deep(.el-table .cell) { padding-right: 13px; padding-left: 13px; line-height: 1.65; font-size: 12px; }
.table-scroll :deep(.el-table .el-tag) { border-radius: 5px; font-size: 10px; padding: 2px 7px; min-height: 22px; }
.account-name { color: var(--text-1); font-size: 12px; font-weight: 600; }
.account-name:hover { color: var(--primary); }
.account-name + .muted { font-size: 10px; margin-top: 5px; }
.row-actions { gap: 10px; }
.row-actions .el-button { font-size: 11px; }
.row-actions :deep(.el-icon) { font-size: 11px; }
.account-pagination { padding: 18px 24px; }
.account-pagination .muted,.page-jump { font-size: 11px; }
.today-usage-note { padding: 12px 24px; margin: 0; background: color-mix(in srgb,var(--primary) 3%,var(--card-bg)); border-top: 1px solid var(--card-border); font-size: 11px; }
.account-detail-hero { padding-bottom: 21px; border-bottom: 1px solid var(--card-border); margin-bottom: 20px; }
.detail-avatar { border: 1px solid color-mix(in srgb,var(--primary) 14%,var(--card-border)); background: var(--primary-soft); color: var(--primary); }
.detail-quick-actions { padding: 12px; border-radius: 9px; background: var(--bg); border: 1px solid var(--card-border); }
.detail-quick-actions .el-button { font-size: 11px; }
.account-detail-fields { border-radius: 9px; overflow: hidden; }
.account-detail-fields dt { font-size: 11px; padding: 13px; background: color-mix(in srgb,var(--bg) 75%,var(--card-bg)); }
.account-detail-fields dd { padding: 13px; }
.request-record { border: 1px solid var(--card-border); border-radius: 8px; padding: 13px 15px; margin-bottom: 8px; background: var(--card-bg); }
.request-record summary { font-size: 11px; line-height: 1.8; }
.request-record[open] { background: var(--bg); }
@media (max-width: 1100px) { .account-overview { gap: 12px; }.account-overview-card { padding: 20px 18px; }.account-overview-card .overview-mark { right: 14px; width: 29px; height: 29px; }.view-option-hint { display: none; } }
@media (max-width: 850px) { .account-overview { grid-template-columns: repeat(2,minmax(0,1fr)); }.account-overview-card:nth-child(3) { border-left: 1px solid var(--card-border); }.account-overview-card:nth-child(n+3) { border-top: 1px solid var(--card-border); }.account-overview-card { min-height: 122px; } }
@media (max-width: 420px) { .account-view-options { align-items: flex-start; flex-direction: column; }.account-view-controls { width: 100%; justify-content: space-between; }.account-directory-heading { align-items: flex-start; }.directory-tools { flex-wrap: wrap; justify-content: flex-end; }.account-directory-heading h3 .directory-count { display: block; width: fit-content; margin: 6px 0 0; } }
</style>

<style scoped>
.accounts-page--dense .account-overview { display:grid; grid-template-columns:repeat(4,minmax(0,1fr)); gap:0; margin-bottom:12px; border:1px solid var(--card-border); border-radius:8px; background:var(--card-bg); overflow:hidden; }
.accounts-page--dense .account-overview-card { position:relative; display:grid; grid-template-columns:minmax(0,1fr) auto; grid-template-rows:auto auto; align-content:center; gap:4px 9px; min-height:76px; padding:11px 15px 11px 51px; border:0; border-radius:0; box-shadow:none; background:transparent; }
.accounts-page--dense .account-overview-card + .account-overview-card { border-left:1px solid var(--card-border); }
.accounts-page--dense .account-overview-card > span:first-child { grid-column:1; grid-row:1; font-size:11px; color:var(--text-2); }
.accounts-page--dense .account-overview-card strong { grid-column:2; grid-row:1 / 3; font-size:23px; line-height:1.3; letter-spacing:-.5px; }
.accounts-page--dense .account-overview-card small { grid-column:1; grid-row:2; font-size:9px; line-height:1.5; color:var(--text-3); white-space:normal; }
.accounts-page--dense .account-overview-card .overview-mark { position:absolute; left:13px; top:50%; right:auto; transform:translateY(-50%); display:grid; place-items:center; width:28px; height:28px; border-radius:6px; color:var(--badge-blue,var(--primary)); background:var(--badge-blue-soft,var(--primary-soft)); }
.accounts-page--dense .account-overview-card .overview-mark.positive { color:var(--badge-green,var(--success)); background:var(--badge-green-soft,var(--primary-soft)); }
.accounts-page--dense .account-overview-card:nth-child(3) .overview-mark { color:var(--badge-purple,var(--primary)); background:var(--badge-purple-soft,var(--primary-soft)); }
.accounts-page--dense .account-overview-card:nth-child(4) .overview-mark { color:var(--badge-amber,var(--warning)); background:var(--badge-amber-soft,var(--primary-soft)); }
.accounts-page--dense .account-overview-card .overview-mark .el-icon { font-size:14px; }
.accounts-page--dense .account-directory-heading { padding:12px 15px 9px; gap:9px; }
.accounts-page--dense .account-directory-heading h3 { font-size:12px; color:var(--text-1); }
.accounts-page--dense .directory-sort { font-size:9px; margin-top:3px; }
.accounts-page--dense .directory-count { font-size:10px; padding:1px 5px; }
.accounts-page--dense .directory-tools { gap:6px; }
.accounts-page--dense .account-filters { gap:7px; padding:0 15px 10px; }
.accounts-page--dense .account-view-options { padding:7px 15px; gap:8px; }
.accounts-page--dense .account-view-controls { gap:10px; }
.accounts-page--dense .account-advanced-filters { padding:13px 15px; gap:11px; }
.accounts-page--dense .account-filter-chips { padding:8px 15px; gap:5px; }
.accounts-page--dense .account-summary { padding:7px 15px; min-height:39px; gap:8px; }
.accounts-page--dense .selection-actions .el-button { height:26px; min-height:26px; padding:4px 9px; font-size:10px; }
.accounts-page--dense .table-scroll :deep(.el-table__header-wrapper th) { height:35px; padding:5px 0; font-size:10px; }
.accounts-page--dense .table-scroll :deep(.el-table__body td) { padding-top:6px; padding-bottom:6px; height:58px; }
.accounts-page--dense .table-scroll :deep(.el-table .cell) { padding-left:11px; padding-right:11px; font-size:11px; line-height:1.5; }
.accounts-page--dense .table-scroll :deep(.el-table .el-tag) { min-height:19px; height:19px; padding:1px 6px; font-size:9px; border-radius:4px; }
.account-identity-cell { display:flex; gap:8px; align-items:center; min-width:0; }
.account-identity-icon { display:grid; place-items:center; flex:0 0 29px; width:29px; height:29px; border-radius:7px; color:var(--badge-blue,var(--primary)); background:var(--badge-blue-soft,var(--primary-soft)); border:1px solid color-mix(in srgb,var(--badge-blue,var(--primary)) 12%,transparent); }
.account-identity-copy { min-width:0; flex:1; }
.accounts-page--dense .account-name { display:block; max-width:100%; line-height:1.4; font-size:11px; font-weight:600; }
.accounts-page--dense .account-name :deep(span) { display:block; white-space:nowrap; overflow:hidden; text-overflow:ellipsis; line-height:1.5; }
.account-identity-badges { display:flex; align-items:center; gap:4px; flex-wrap:wrap; margin-top:4px; line-height:1; }
.account-mini-pill { display:inline-flex; align-items:center; width:fit-content; gap:3px; border-radius:4px; padding:3px 5px; font-size:9px; line-height:1.2; font-weight:500; white-space:nowrap; color:var(--text-3); background:var(--el-fill-color-light); }
.account-mini-pill.is-green { color:var(--badge-green,var(--success)); background:var(--badge-green-soft,var(--primary-soft)); }
.account-mini-pill.is-amber { color:var(--badge-amber,var(--warning)); background:var(--badge-amber-soft,var(--primary-soft)); }
.account-token-hint { display:inline-flex; align-items:center; gap:3px; font-size:9px; color:var(--text-3); white-space:nowrap; }
.account-concurrency { display:grid; justify-items:center; gap:5px; }
.account-metric-badge { display:inline-flex; align-items:center; justify-content:center; gap:4px; font-size:10px; color:var(--text-3); font-variant-numeric:tabular-nums; }
.account-metric-badge > .console-icon { color:var(--badge-purple,var(--primary)); }
.account-metric-badge strong { color:var(--text-1); font-size:11px; font-weight:600; }
.account-metric-badge.is-full strong { color:var(--badge-amber,var(--warning)); }
.account-concurrency-progress { display:block; width:52px; height:3px; border:0; border-radius:3px; overflow:hidden; appearance:none; background:var(--el-fill-color); accent-color:var(--badge-purple,var(--primary)); }
.account-concurrency-progress::-webkit-progress-value { background:var(--badge-purple,var(--primary)); }
.account-number-chip { display:inline-block; min-width:23px; padding:2px 5px; border-radius:4px; line-height:1.4; font-size:10px; font-weight:600; }
.account-number-chip.is-purple { color:var(--badge-purple,var(--primary)); background:var(--badge-purple-soft,var(--primary-soft)); }
.account-number-chip.is-blue { color:var(--badge-blue,var(--primary)); background:var(--badge-blue-soft,var(--primary-soft)); }
.accounts-page--dense .account-resource-tags { display:flex; align-items:center; gap:3px; flex-wrap:wrap; max-height:44px; overflow-y:auto; scrollbar-width:thin; }
.accounts-page--dense .account-resource-tags :deep(.el-tag) { max-width:100%; border-color:transparent; background:var(--badge-purple-soft,var(--primary-soft)); color:var(--badge-purple,var(--primary)); }
.accounts-page--dense .account-resource-tags.is-proxies :deep(.el-tag) { background:var(--badge-blue-soft,var(--primary-soft)); color:var(--badge-blue,var(--primary)); }
.accounts-page--dense .account-resource-tags :deep(.el-tag__content) { display:inline-flex; align-items:center; gap:4px; max-width:100%; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
.account-usage-line { display:flex; align-items:center; gap:7px; font-size:10px; white-space:nowrap; font-variant-numeric:tabular-nums; }
.account-usage-total { color:var(--text-1); font-weight:600; font-size:11px; }
.account-usage-success,.account-usage-errors { display:inline-flex; align-items:center; gap:2px; }
.account-usage-success { color:var(--badge-green,var(--success)); }.account-usage-errors { color:var(--text-3); }.account-usage-errors.has-errors { color:var(--danger); }
.account-token-line { display:flex; gap:7px; align-items:center; margin-top:3px; font-size:9px; color:var(--text-3); }
.account-token-line strong { font-size:10px; font-weight:500; color:var(--badge-blue,var(--primary)); }
.account-time-label { display:inline-flex; gap:4px; align-items:center; font-size:10px; white-space:nowrap; color:var(--text-2); font-variant-numeric:tabular-nums; }
.accounts-page--dense .expiry-warning { margin-top:3px; font-size:9px; }
.accounts-page--dense .muted { font-size:10px; line-height:1.45; }
.accounts-page--dense .quota-caption { margin-bottom:3px; font-size:10px; }
.accounts-page--dense .quota-progress { height:3px; margin-bottom:3px; }
.accounts-page--dense .account-row-error { margin-top:4px; font-size:9px; }
.accounts-page--dense .row-actions { gap:7px; }.accounts-page--dense .row-actions .el-button { font-size:10px; }
.accounts-page--dense .account-pagination { padding:10px 15px; gap:8px; }
.accounts-page--dense .today-usage-note { padding:8px 15px; font-size:10px; }
.accounts-page--dense .mobile-account { padding:12px; }
.accounts-page--dense .mobile-account-heading { gap:7px; }
.accounts-page--dense .mobile-account-identity .account-name { font-size:12px; }
.accounts-page--dense .mobile-account-status { margin-top:7px; gap:6px; }
.accounts-page--dense .mobile-account-metrics { grid-template-columns:repeat(2,minmax(0,1fr)); gap:7px 13px; margin:10px 0; padding:9px 10px; border-radius:6px; background:var(--el-fill-color-light); }
.accounts-page--dense .mobile-account-metrics dt { font-size:9px; margin-bottom:2px; }
.accounts-page--dense .mobile-account-metrics dd { font-size:12px; }
.accounts-page--dense .mobile-account-metrics small { font-size:10px; }
.accounts-page--dense .mobile-account-actions { margin-top:9px; gap:6px; }
.accounts-page--dense .mobile-account-resources { gap:5px 7px; font-size:10px; }
@media(max-width:1100px) and (min-width:768px) { .accounts-page--dense .account-overview-card { padding:10px 11px 10px 44px; min-height:76px; }.accounts-page--dense .account-overview-card .overview-mark { left:10px; width:25px; height:25px; }.accounts-page--dense .account-overview-card strong { font-size:20px; }.accounts-page--dense .account-overview-card small { font-size:8px; } }
@media(max-width:767px) {
 .accounts-page--dense .account-overview { grid-template-columns:repeat(2,minmax(0,1fr)); }
 .accounts-page--dense .account-overview-card { min-height:66px; padding:10px 11px 10px 44px; }
 .accounts-page--dense .account-overview-card:nth-child(3) { border-left:0; }
 .accounts-page--dense .account-overview-card:nth-child(n+3) { border-top:1px solid var(--card-border); }
 .accounts-page--dense .account-overview-card .overview-mark { left:10px; width:25px; height:25px; }
 .accounts-page--dense .account-overview-card > span:first-child { font-size:10px; }
 .accounts-page--dense .account-overview-card strong { font-size:20px; }
 .accounts-page--dense .account-overview-card small { display:none; }
 .accounts-page--dense .account-directory-heading { padding:11px 12px 8px; }
 .accounts-page--dense .account-filters { padding:0 12px 9px; }
 .accounts-page--dense .account-summary { padding:7px 12px; }
 .accounts-page--dense .account-view-options { padding:6px 12px; }
 .accounts-page--dense .account-pagination { padding:10px 12px; }
 .accounts-page--dense .account-directory-heading h3 .directory-count { display:inline; margin:0 0 0 4px; }
}
</style>

<style scoped>
.account-status-shortcuts { display: flex; flex-wrap: wrap; gap: 6px; padding: 0 24px 16px; }
.account-status-shortcuts button { min-height: 34px; border: 1px solid transparent; border-radius: 7px; padding: 6px 14px; background: transparent; color: var(--text-2); font: inherit; font-size: 13px; cursor: pointer; }
.account-status-shortcuts button:hover { background: var(--el-fill-color-light); }
.account-status-shortcuts button[aria-pressed="true"] { color: var(--el-color-primary); background: var(--el-color-primary-light-9); border-color: var(--el-color-primary-light-7); font-weight: 600; }
.account-status-shortcuts button:focus-visible, .detail-section-links button:focus-visible { outline: 2px solid var(--el-color-primary); outline-offset: 2px; }
.account-summary.has-selection { position: sticky; top: 0; z-index: 4; box-shadow: 0 4px 12px rgb(0 0 0 / 4%); }
.detail-section-links { display: flex; gap: 8px; flex-wrap: wrap; margin: 18px 0 24px; padding: 12px; background: var(--el-fill-color-light); border-radius: 8px; }
.detail-section-links button { flex: 1; min-width: 100px; min-height: 34px; padding: 7px 12px; border: 1px solid var(--card-border); border-radius: 6px; color: var(--text-2); background: var(--card-bg); font: inherit; font-size: 13px; cursor: pointer; }
.detail-section-links button:hover { color: var(--el-color-primary); border-color: var(--el-color-primary-light-5); }
.detail-section-heading { margin: 24px 0 14px; font-size: 15px; }
.account-detail-fields { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); column-gap: 24px; margin-top: 0; }
.account-detail-fields > div { grid-template-columns: 120px minmax(0, 1fr); align-items: start; }
#account-detail-configuration, #account-detail-identity, #account-detail-runtime, #account-detail-requests { scroll-margin-top: 18px; }
.detail-quick-actions { display: flex; flex-wrap: wrap; gap: 8px; }
.detail-quick-actions .el-button + .el-button { margin-left: 0; }
@media (max-width: 767px) { .account-status-shortcuts { padding: 0 12px 12px; gap: 4px; }.account-status-shortcuts button { padding: 6px 10px; }.account-detail-fields { grid-template-columns: 1fr; }.account-detail-fields > div { grid-template-columns: 105px minmax(0, 1fr); } }
</style>
