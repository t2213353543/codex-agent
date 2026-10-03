import { shanghaiDateInput, timestampMillis } from './format.js'

export const accountStatuses = ['ok', 'error', 'disabled', 'expired', 'unverified']
export const accountStatusLabels = { ok: '最近状态正常', error: '请求或凭据异常', disabled: '已停用', expired: '令牌已过期', unverified: '尚未验证' }
export const accountStatusLabel = status => Object.hasOwn(accountStatusLabels, status) ? accountStatusLabels[status] : '状态未知'
export const accountSortFields = { created_at: '创建时间', updated_at: '更新时间', name: '账号名称', id: '账号 ID', status: '账号状态', priority: '优先级', weight: '权重', concurrency: '并发容量', quota: 'Token 额度', expires_at: '令牌到期', last_used_at: '最近业务请求' }
const text = (value) => typeof value === 'string' ? value : ''
const flag = (value) => ['true', 'false'].includes(String(value)) ? String(value) : ''
function accountExpiryTime(value) {
  const time = typeof value === 'number' || /^\d+$/.test(String(value || '')) ? Number(value) * 1000 : timestampMillis(text(value))
  return time > 0 && Number.isFinite(new Date(time).getTime()) ? time : NaN
}
export function accountDateFilterError(raw = {}) {
  return ['expires_after', 'expires_before'].some(key => raw[key] !== undefined && raw[key] !== '' && !Number.isFinite(accountExpiryTime(raw[key])))
    ? '令牌到期筛选包含无效或超出范围的日期。请清除无效日期，或重新填写后查询。' : ''
}

export function normalizeAccountFilters(raw = {}) {
  const legacySort = { server: ['created_at', 'desc'], 'name-asc': ['name', 'asc'], 'name-desc': ['name', 'desc'], 'expiry-asc': ['expires_at', 'asc'] }[raw.sort]
  const filters = {
    search: text(raw.search).slice(0, 300), status: accountStatuses.includes(raw.status) ? raw.status : '',
    group_id: text(raw.group_id ?? raw.group), proxy_id: text(raw.proxy_id ?? raw.proxy),
    tag: text(raw.tag).slice(0, 100), enabled: flag(raw.enabled), has_refresh_token: flag(raw.has_refresh_token),
    privacy: ['disabled', 'unknown'].includes(raw.privacy) ? raw.privacy : '',
    ungrouped: String(raw.ungrouped) === 'true' ? 'true' : '', no_proxy: String(raw.no_proxy) === 'true' ? 'true' : '',
    expires_after: '', expires_before: '',
    sort_by: Object.hasOwn(accountSortFields, raw.sort_by) ? raw.sort_by : legacySort?.[0] || 'created_at',
    sort_order: ['asc', 'desc'].includes(raw.sort_order) ? raw.sort_order : legacySort?.[1] || 'desc',
  }
  for (const key of ['expires_after', 'expires_before']) {
    const value = raw[key]
    const time = accountExpiryTime(value)
    if (value !== undefined && value !== '' && Number.isFinite(time) && time > 0) filters[key] = new Date(time).toISOString()
  }
  if (filters.ungrouped) filters.group_id = ''
  if (filters.no_proxy) filters.proxy_id = ''
  return filters
}

export function compactAccountFilters(filters) {
  return Object.fromEntries(Object.entries(normalizeAccountFilters(filters)).filter(([, value]) => value !== '').map(([key, value]) => [key, key.startsWith('expires_') ? Math.floor(Date.parse(value) / 1000) : ['enabled', 'has_refresh_token', 'ungrouped', 'no_proxy'].includes(key) ? value === 'true' : value]))
}

export function validateAccountDateRange(filters) {
  if (filters.expires_after && filters.expires_before && Date.parse(filters.expires_after) >= Date.parse(filters.expires_before)) return '令牌到期的结束时间必须晚于开始时间'
  return ''
}

export function localAccountDate(iso) {
  return shanghaiDateInput(iso)
}

export function validateAccountSelection(response) {
  if (!response || !Array.isArray(response.items) || !Number.isSafeInteger(response.total) || response.total < 0 || response.items.length !== response.total || response.total > 10000) throw new Error('服务器返回的全部选择不完整，已阻止操作')
  const ids = new Set()
  return response.items.map((item) => {
    if (!item || typeof item.id !== 'string' || !item.id || ids.has(item.id) || !Number.isSafeInteger(item.version) || item.version < 1) throw new Error('服务器返回的账号选择无效，已阻止操作')
    ids.add(item.id)
    return { id: item.id, version: item.version }
  })
}

// Freeze identity/version before work starts. A failed transport has an unknown
// outcome: never retry it automatically or silently continue the next batch.
export async function runAccountBatches({ items, action, patch, send, stopped = () => false, onProgress = () => {} }) {
  const targets = items.map(({ id, version, name }) => ({ id, version, name }))
  const size = action === 'refresh' ? 10 : 100
  const results = []
  for (let offset = 0; offset < targets.length; offset += size) {
    if (stopped()) break
    const batch = targets.slice(offset, offset + size)
    try {
      const response = await send({ action, items: batch.map(({ id, version }) => ({ id, version })), ...(patch === undefined ? {} : { patch }) })
      const byID = new Map((response?.results || []).map((result) => [result.id, result]))
      for (const target of batch) {
        const result = byID.get(target.id)
        results.push({ ...target, ...result, id: target.id, state: result?.ok === true ? 'success' : result?.ok === false ? 'failed' : 'unknown', message: result?.ok === true ? '已完成' : result?.code === 'version_conflict' ? '版本已变化，请重新核对并选择' : result?.error || '未返回明确结果，请刷新核对后再操作' })
      }
      onProgress([...results])
      if (batch.some((target) => !byID.has(target.id))) break
    } catch (error) {
      results.push(...batch.map((target) => ({ ...target, state: 'unknown', message: error.message || '请求结果未确认，请刷新核对' })))
      onProgress([...results])
      break
    }
  }
  const processed = new Set(results.map((result) => result.id))
  results.push(...targets.filter((target) => !processed.has(target.id)).map((target) => ({ ...target, state: 'pending', message: '尚未执行' })))
  onProgress([...results])
  return results
}

export function accountSelectionTarget(items, filters, mode) {
  return mode === 'selected' ? { items: items.map(({ id, version }) => ({ id, version })) } : { filters: compactAccountFilters(filters) }
}
