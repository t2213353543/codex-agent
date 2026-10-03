import { formatNumber, formatDuration, shanghaiTimeParts, shanghaiDateInput, parseShanghaiDateInput } from './format.js'

export const logFilterKeys = ['status', 'key_name', 'account', 'account_id', 'model', 'from', 'to']
export const emptyLogFilters = () => Object.fromEntries(logFilterKeys.map((key) => [key, '']))

export function readLogFilters(query = {}) {
  const value = emptyLogFilters()
  for (const key of logFilterKeys) if (typeof query[key] === 'string') value[key] = query[key].slice(0, 500)
  if (!['', 'ok', 'error', 'aborted'].includes(value.status)) value.status = ''
  for (const key of ['from', 'to']) if (value[key] && !/^\d{1,11}$/.test(value[key])) value[key] = ''
  return value
}

export function logFilterSignature(filters, pageSize) {
  const normalized = readLogFilters(filters)
  return JSON.stringify([Number(pageSize), ...logFilterKeys.map((key) => normalized[key])])
}

export function logRangeError(filters) {
  return filters.from && filters.to && Number(filters.from) >= Number(filters.to) ? '开始时间必须早于结束时间。' : ''
}

export function buildLogQuery(filters, pageSize, cursor = '', page = 1) {
  const query = {}
  for (const [key, value] of Object.entries(readLogFilters(filters))) if (value) query[key] = value
  query.page_size = String(pageSize)
  if (cursor) query.cursor = cursor
  if (page > 1) query.page = String(page)
  return query
}

// Filtering and paging are distinct transitions. Draft edits must never reuse a
// cursor obtained for another query, including refresh/keyboard navigation.
export function logNavigationAllowed({ loading, dirty, error, currentResult }) {
  return !loading && !dirty && !error && currentResult
}

export function logExportAllowed(state, items) {
  return logNavigationAllowed(state) && Array.isArray(items) && items.length > 0
}

export function validLogPage(data) {
  if (!data || !Array.isArray(data.items)) return false
  if (!data.items.every((item) => item && typeof item === 'object' && !Array.isArray(item))) return false
  if ('has_more' in data && typeof data.has_more !== 'boolean') return false
  if ('next_cursor' in data && typeof data.next_cursor !== 'string') return false
  if (data.has_more === true && !data.next_cursor) return false
  return true
}

export function logTimeParts(timestamp) {
  return shanghaiTimeParts(timestamp)
}

export const logLatency = formatDuration

export function logTokenCount(value) {
  if (value === null || value === undefined || value === '' || !Number.isFinite(Number(value)) || Number(value) < 0) return '—'
  return formatNumber(value)
}

export function logLocalDateInput(timestamp) {
  return shanghaiDateInput(timestamp)
}

export function logTimestampInput(value) {
  const timestamp = parseShanghaiDateInput(value)
  return Number.isFinite(timestamp) ? String(timestamp) : ''
}
