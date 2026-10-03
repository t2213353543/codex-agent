import { clientRequestIDValid } from './logPassiveObservation.js'

export const ingressReasons = [
  ['invalid_api_key', 'API Key 无效'], ['settings_unavailable', '设置不可用'],
  ['key_store_unavailable', '密钥授权存储不可用'],
  ['method_not_allowed', '请求方法不支持'], ['unknown_endpoint', '端点不支持'], ['websocket_disabled', 'WebSocket 已关闭'],
]
export const ingressReasonLabel = value => ingressReasons.find(([name]) => name === value)?.[1] || '未识别的准入原因'
export const emptyIngressFilters = () => ({ from: '', to: '', status: '', reason: '', client_request_id: '' })
export function ingressInitialFilters(query = {}) {
  const result = emptyIngressFilters()
  for (const key of ['from', 'to', 'client_request_id']) if (typeof query[key] === 'string') result[key] = query[key]
  return result
}
export function ingressQueryError(filters) {
  for (const key of ['from', 'to']) if (filters[key] && (!/^\d{1,12}$/.test(filters[key]) || !Number.isSafeInteger(Number(filters[key])) || Number(filters[key]) < 1 || Number(filters[key]) > 253402300799)) return '请选择有效的准入事件时间范围。'
  if (filters.from && filters.to && Number(filters.from) >= Number(filters.to)) return '准入事件开始时间必须早于结束时间。'
  if (filters.status && !/^[45]\d{2}$/.test(filters.status)) return '准入 HTTP 状态须为 400–599 的三位数字。'
  if (filters.reason && !ingressReasons.some(([name]) => name === filters.reason)) return '准入原因筛选无效。'
  if (!clientRequestIDValid(filters.client_request_id)) return '客户端关联 ID 最多 128 个字符，仅允许英文字母、数字、点、下划线、冒号和短横线。'
  return ''
}
export function ingressQuery(filters, limit = 50, cursor = '') {
  const error = ingressQueryError(filters)
  if (error) throw new Error(error)
  const result = Object.fromEntries(Object.keys(emptyIngressFilters()).filter(key => filters[key]).map(key => [key, filters[key]]))
  result.limit = String([50, 100, 200].includes(Number(limit)) ? Number(limit) : 50)
  if (cursor) result.cursor = cursor
  return result
}
const count = value => Number.isSafeInteger(value) && value >= 0
export function validateIngressPage(data) {
  if (data?.scope !== 'retained_ingress_events' || data.source !== 'gateway_ingress_v1' || !Array.isArray(data.items) || data.items.length > 200 || !count(data.total) || data.total_kind !== 'exact' || typeof data.has_more !== 'boolean' || typeof data.next_cursor !== 'string' || data.has_more && !data.next_cursor || !count(data.checked_at) || !count(data.retention_limit) || !count(data.evicted_total)) throw new Error('准入事件响应不完整或来源无法验证，请刷新。')
  for (const row of data.items) {
    if (!row || typeof row.id !== 'string' || !row.id || !count(row.time_ms) || !Number.isSafeInteger(row.http_code) || row.http_code < 400 || row.http_code > 599 || !ingressReasons.some(([reason]) => reason === row.reason)) throw new Error('准入事件记录无效，未更新当前结果。')
  }
  return data
}
export function ingressCollectorStats(value = {}) {
  return {
    state: value.configured === false ? '未配置采集' : value.configured === true && value.running === true ? '采集运行中' : value.configured === true && value.running === false ? '采集已停止' : '采集状态未报告',
    processScoped: value.counter_scope === 'current_process',
    counts: Object.fromEntries(['queue_capacity', 'queued', 'pending', 'queue_dropped', 'persistence_failures', 'persistence_unconfirmed', 'unpersisted_at_shutdown'].map(key => [key, count(value[key]) ? value[key] : null])),
    lastPersistedAt: count(value.last_persisted_at) && value.last_persisted_at > 0 ? value.last_persisted_at : null,
    error: value.last_error === 'storage_unavailable' ? '事件存储暂时不可用' : value.last_error ? '采集器报告错误，详细原因未提供' : '',
  }
}
