import { SHANGHAI_TIMEZONE } from './format.js'
import { timezoneFormatter } from './timezoneFormatter.js'
import { usageMetricNumber } from './logMetrics.js'
import { validLogSeverity } from './logSeverity.js'
import { clientRequestIDValid } from './logPassiveObservation.js'

export const managedFilterKeys = ['status', 'key_name', 'key_id', 'account', 'account_id', 'group_id', 'model', 'upstream_model', 'sent_model', 'returned_model', 'inbound_path', 'upstream_path', 'model_mismatch', 'sent_model_mismatch', 'returned_model_mismatch', 'error_truncated', 'native_compaction', 'websocket_warmup', 'request_type', 'service_tier', 'reasoning_effort', 'endpoint', 'http_code', 'stream', 'error_category', 'error_severity', 'error_phase', 'request_id', 'client_request_id', 'ip', 'from', 'to']
export const managedControlKeys = ['sort_by', 'sort_order', 'exact_total', 'step', 'timezone']
export const logStepOptions = [['', '自动'], ['60', '每分钟'], ['300', '每 5 分钟'], ['3600', '每小时'], ['86400', '按日历日'], ['604800', '按周（周一开始）'], ['2592000', '按日历月']]
export const managedQueryKeys = [...managedFilterKeys, ...managedControlKeys]
export const managedFilterValueLimit = key => key === 'client_request_id' ? 128 : key === 'request_id' ? 1024 : ['model', 'upstream_model', 'sent_model', 'returned_model'].includes(key) ? 512 : 500
export function managedExactFilterError(filters = {}) {
  for (const key of ['model', 'upstream_model', 'sent_model', 'returned_model']) if (typeof filters[key] === 'string' && new TextEncoder().encode(filters[key]).length > 1024) return '模型筛选不能超过 1024 个 UTF-8 字节，请使用完整的合法模型名称。'
  if (typeof filters.request_id === 'string' && new TextEncoder().encode(filters.request_id).length > 1024) return '请求 / 记录 ID 不能超过 1024 个 UTF-8 字节，请使用完整的合法标识。'
  if (filters.client_request_id && !clientRequestIDValid(filters.client_request_id)) return '客户端关联 ID 最多 128 个字符，仅允许英文字母、数字、点、下划线、冒号和短横线。'
  return ''
}
export const logSortOptions = [['time', '请求时间'], ['model', '模型名称'], ['tokens', '总 Token'], ['input_tokens', '输入 Token'], ['output_tokens', '输出 Token'], ['cached_tokens', '缓存读取 Token'], ['latency', '总耗时']]
export const logErrorOptions = [
  ['unsupported_parameter', '参数不兼容'], ['system_role', '系统消息不兼容'], ['no_enabled_account', '无可用账号'], ['invalid_oauth', '账号授权失效'], ['unsupported_model', '模型不可用'], ['encrypted_history', '加密历史不兼容'], ['request_too_large', '请求过大'], ['upstream_refusal', '上游拒绝'], ['rate_limit', '请求限流'], ['overloaded', '服务繁忙'], ['upstream_error', '上游错误'], ['aborted', '请求中断'], ['timeout', '请求超时'], ['authentication', '认证失败'], ['server_error', '服务端错误'], ['unknown', '其他错误'],
]
export const logRequestTypeOptions = [['http_sse', 'HTTP 流式'], ['http_json', 'HTTP 非流式'], ['websocket', 'WebSocket'], ['compact', '上下文压缩'], ['models', '模型列表']]
export const managedFilterLabels = { status: '状态', account: '账号', account_id: '账号 ID', key_name: 'KEY', key_id: 'KEY ID', group_id: '分组 ID', model: '模型', upstream_model: '上游模型', sent_model: '发送模型', returned_model: '返回模型', inbound_path: '入站标准路径', upstream_path: '上游标准路径', model_mismatch: '模型观测差异', sent_model_mismatch: '请求与发送模型差异', returned_model_mismatch: '发送与返回模型差异', error_truncated: '错误截断', native_compaction: '原生压缩', websocket_warmup: 'WebSocket 预热', request_type: '请求类型', service_tier: '服务层', reasoning_effort: '推理强度', endpoint: '端点', http_code: 'HTTP', stream: '响应模式', error_category: '错误类别', error_severity: '错误级别', error_phase: '错误阶段', request_id: '请求 / 记录 ID', client_request_id: '客户端关联 ID', ip: 'IP', from: '开始时间', to: '结束时间' }

export function emptyManagedFilters() {
  return { ...Object.fromEntries(managedFilterKeys.map(key => [key, ''])), sort_by: 'time', sort_order: 'desc', exact_total: 'true', step: '', timezone: SHANGHAI_TIMEZONE }
}

export function readManagedFilters(query = {}) {
  const result = emptyManagedFilters()
  for (const key of managedQueryKeys) if (typeof query[key] === 'string') result[key] = ['request_id', 'client_request_id'].includes(key) ? query[key] : query[key].slice(0, managedFilterValueLimit(key))
  if (!['', 'ok', 'error', 'aborted', 'failed'].includes(result.status)) result.status = ''
  for (const key of ['from', 'to']) if (result[key] && !/^\d{1,12}$/.test(result[key])) result[key] = ''
  if (result.http_code && !/^[1-5]\d{2}$/.test(result.http_code)) result.http_code = ''
  if (!['', 'true', 'false'].includes(result.stream)) result.stream = ''
  for (const key of ['model_mismatch', 'sent_model_mismatch', 'returned_model_mismatch', 'error_truncated', 'native_compaction', 'websocket_warmup']) if (!['', 'true', 'false'].includes(result[key])) result[key] = ''
  if (result.request_type && !logRequestTypeOptions.some(([value]) => value === result.request_type)) result.request_type = ''
  if (result.error_category && !logErrorOptions.some(([value]) => value === result.error_category)) result.error_category = ''
  if (result.error_severity && !validLogSeverity(result.error_severity)) result.error_severity = ''
  if (!logSortOptions.some(([value]) => value === result.sort_by)) result.sort_by = 'time'
  if (!['asc', 'desc'].includes(result.sort_order)) result.sort_order = 'desc'
  if (!['true', 'false'].includes(result.exact_total)) result.exact_total = 'true'
  if (!logStepOptions.some(([value]) => value === result.step)) result.step = ''
  result.timezone = normalizeLogTimezone(result.timezone) || SHANGHAI_TIMEZONE
  return result
}

export function validLogTimezone(value) {
  return normalizeLogTimezone(value) !== null
}

export function normalizeLogTimezone(value) {
  if (value === '') return ''
  // Intl also accepts numeric offsets and case-insensitive aliases; Go expects a named zone.
  if (typeof value !== 'string' || value.length > 100 || value === 'Local' || !/^[A-Za-z][A-Za-z0-9_+\-/]*$/.test(value)) return null
  try { return timezoneFormatter(value).resolvedOptions().timeZone } catch { return null }
}

export function managedTimeParts(time, timezone = SHANGHAI_TIMEZONE) {
  if (!Number.isFinite(time)) return { date: '—', time: '—' }
  const parts = timezoneFormatter(timezone || SHANGHAI_TIMEZONE).formatToParts(new Date(time * 1000))
  const values = Object.fromEntries(parts.map(part => [part.type, part.value]))
  return { date: `${values.year}-${values.month}-${values.day}`, time: `${values.hour}:${values.minute}:${values.second}` }
}

export function managedSignature(filters, size = 20) {
  const normalized = readManagedFilters(filters)
  return JSON.stringify([Number(size), ...managedQueryKeys.map(key => normalized[key])])
}

export function buildManagedQuery(filters, size = 20, cursor = '', page = 1) {
  const query = Object.fromEntries(Object.entries(readManagedFilters(filters)).filter(([, value]) => value !== ''))
  query.page_size = String(size)
  if (cursor) query.cursor = cursor
  else if (page > 1) query.page = String(page)
  return query
}

export function managedQuerySnapshot(filters, size = 20) {
  const normalized = Object.freeze(readManagedFilters(filters))
  return Object.freeze({ filters: normalized, size, signature: managedSignature(normalized, size), params: Object.freeze(buildManagedQuery(normalized, size)) })
}

export function managementScopeParams(params = {}) {
  const result = Object.fromEntries(Object.entries(buildManagedQuery(params)).filter(([key]) => !['page_size', 'page', 'cursor'].includes(key)))
  if (typeof params.as_of === 'string' && params.as_of.length <= 200 && params.as_of) result.as_of = params.as_of
  return result
}

export function validUsageStats(value) {
  if (!value || typeof value !== 'object') return false
  for (const key of ['total_requests', 'success', 'errors', 'total_input_tokens', 'total_output_tokens', 'total_cached_tokens', 'total_tokens']) {
    if (!Number.isFinite(value[key]) || value[key] < 0) return false
  }
  return ['models', 'upstream_models', 'sent_models', 'returned_models', 'inbound_paths', 'upstream_paths', 'endpoints', 'error_categories', 'error_severities', 'series'].every(key => !value[key] || Array.isArray(value[key]))
}

export function presentManagedRecord(record) {
  const known = record.usage_known === true || (record.usage_known === undefined || record.usage_known === null) && ['input_tokens', 'output_tokens', 'cached_tokens'].some(key => Number(record[key]) > 0)
  const cacheKnown = record.cached_tokens_known === true || record.cached_tokens_known !== false && Number(record.cached_tokens) > 0
  if (known) return { ...record, usage_known: true, ...(!cacheKnown ? { cached_tokens: null } : {}) }
  // A historical omitted zero cannot prove that the upstream supplied usage.
  return { ...record, usage_known: record.usage_known ?? null, input_tokens: null, output_tokens: null, cached_tokens: null }
}

export function usageCacheRatio(stats) {
  if (!(stats?.cache_read_samples > 0)) return null
  const ratio = stats.cache_read_rate
  return Number.isFinite(ratio) && ratio >= 0 && ratio <= 1 ? ratio : null
}

export function usageTrendGeometry(series = [], metric = 'total_requests', step = 0) {
  let points = series.filter(row => Number.isFinite(row.time)).map(row => ({ ...row, [metric]: usageMetricNumber(row, metric) })).sort((a, b) => a.time - b.time)
  // Day/week/month buckets can have variable lengths across DST and calendars.
  // Only the server may create those bucket boundaries.
  if (step > 0 && step < 86400 && points.length > 1 && (points.at(-1).time - points[0].time) / step <= 600) {
    const existing = new Map(points.map(row => [row.time, row]))
    const last = points.at(-1).time
    const filled = []
    for (let time = points[0].time; time <= last; time += step) filled.push(existing.get(time) || { time, [metric]: 0 })
    points = filled
  }
  const max = Math.max(0, ...points.map(row => row[metric] ?? 0))
  const start = points[0]?.time || 0, end = points.at(-1)?.time || start + 1
  return { max, points: points.map(row => ({ ...row, x: 20 + ((row.time - start) / Math.max(1, end - start)) * 560, y: row[metric] === null ? null : 130 - row[metric] / (max || 1) * 110 })) }
}
