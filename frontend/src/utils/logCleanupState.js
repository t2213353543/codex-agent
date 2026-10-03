import { managementScopeParams, managedFilterKeys } from './logManagementState.js'

export const cleanupLabels = { pending: '待执行', running: '执行中', succeeded: '已完成', failed: '失败', canceled: '已取消' }
export const cleanupCanCancel = job => Boolean(job && ['pending', 'running'].includes(job.status))

export function cleanupRangeError(filters, now = Math.floor(Date.now() / 1000)) {
  if (!/^\d+$/.test(String(filters.from || '')) || !/^\d+$/.test(String(filters.to || ''))) return '请选择明确的开始与结束时间。'
  const from = Number(filters.from), to = Number(filters.to)
  if (from < 1 || to <= from) return '开始时间必须早于结束时间。'
  if (to > now) return '结束时间不能晚于当前时间。'
  return ''
}

export function cleanupSnapshot(params, from, to) {
  const result = { ...Object.fromEntries(Object.entries(managementScopeParams(params)).filter(([key]) => managedFilterKeys.includes(key) || ['as_of', 'timezone', 'step'].includes(key))), from: String(from), to: String(to) }
  if (String(params.from || '') !== result.from || String(params.to || '') !== result.to) delete result.as_of
  return Object.freeze(result)
}

export function validCleanupPreview(preview, now = Math.floor(Date.now() / 1000)) {
  return Boolean(preview && typeof preview.preview_id === 'string' && preview.preview_id && typeof preview.confirmation_token === 'string' && preview.confirmation_token && Number.isSafeInteger(preview.total) && preview.total >= 0 && preview.scope === 'retained_request_details' && Number.isFinite(preview.expires_at) && preview.expires_at > now)
}

export function cleanupProgress(job) {
  const total = Number.isFinite(job?.total) && job.total >= 0 ? job.total : null
  const deleted = Number.isFinite(job?.deleted_rows) && job.deleted_rows >= 0 ? job.deleted_rows : null
  if (total === null || deleted === null) return { total, deleted, percentage: null, remaining: null }
  return { total, deleted, percentage: total > 0 ? Math.min(100, deleted / total * 100) : null, remaining: Number.isFinite(job.remaining_rows) && job.remaining_rows >= 0 ? job.remaining_rows : Math.max(0, total - deleted) }
}

export function validCleanupJob(job) {
  return Boolean(job && typeof job.id === 'string' && job.id && Object.hasOwn(cleanupLabels, job.status) && job.scope === 'retained_request_details')
}
