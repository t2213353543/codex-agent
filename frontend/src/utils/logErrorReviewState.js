import { managementScopeParams } from './logManagementState.js'

export const reviewRecordRef = row => row?.event_id || (Number.isSafeInteger(row?.id) && row.id > 0 ? String(row.id) : '')
export const reviewAllowed = row => ['error', 'aborted'].includes(row?.status) && Boolean(reviewRecordRef(row))
export function validErrorReview(value) {
  return value && typeof value.record_ref === 'string' && value.record_ref !== '' && Number.isSafeInteger(value.version) && value.version >= 0 && typeof value.resolved === 'boolean' && typeof value.note === 'string' && Array.isArray(value.history)
}
export function errorReviewPayload(review, draft) {
  if (!validErrorReview(review)) throw new Error('请先读取当前处理状态。')
  if (typeof draft?.resolved !== 'boolean' || typeof draft?.note !== 'string' || Array.from(draft.note).length > 1000) throw new Error('处理备注最多 1000 字。')
  return { version: review.version, resolved: draft.resolved, note: draft.note }
}
export function errorReviewQuery(filters, resolved = 'false', cursor = '', size = 20, severity) {
  const query = managementScopeParams(severity === undefined ? filters : { ...filters, error_severity: severity })
  for (const key of ['as_of', 'cursor', 'page', 'size', 'page_size', 'exact_total', 'status']) delete query[key]
  if (resolved === 'true' || resolved === 'false') query.resolved = resolved
  query.page_size = String([20, 50, 100].includes(Number(size)) ? Number(size) : 20)
  query.exact_total = 'true'
  if (cursor) query.cursor = cursor
  return query
}
