import { formatDate } from '../utils/format.js'
import { request, clearSession, authGeneration } from '../api.js'
import { managementScopeParams } from '../utils/logManagementState.js'

const queryString = params => new URLSearchParams(params).toString()
export const logManagement = {
  snapshot: (filters, options) => request('/logs/snapshot', { method: 'POST', body: { filters: managementScopeParams(filters) }, silent: true, ...options }),
  coverage: options => request('/logs/coverage', { silent: true, ...options }),
  list: (params, options) => request(`/logs?${queryString(params)}`, options),
  stats: (params, options) => request(`/logs/stats?${queryString(managementScopeParams(params))}`, { silent: true, ...options }),
  options: (field, search, params, options) => request(`/logs/options?${queryString({ ...managementScopeParams(params), field, q: search, limit: '50' })}`, { silent: true, ...options }),
  related: (kind, options) => request(kind === 'key' ? '/keys' : '/groups', { silent: true, ...options }),
  detail: (id, { params, ...options } = {}) => request(`/logs/${encodeURIComponent(id)}${params && Object.keys(params).length ? '?' + queryString(managementScopeParams(params)) : ''}`, { silent: true, ...options }),
  errors: (params, options) => request(`/logs/errors?${queryString(params)}`, { silent: true, ...options }),
  review: (id, options) => request(`/logs/${encodeURIComponent(id)}/review`, { silent: true, ...options }),
  reviewUpdate: (id, body, options) => request(`/logs/${encodeURIComponent(id)}/review`, { method: 'PUT', body, silent: true, ...options }),
  cleanupPreview: (filters, options) => request('/logs/cleanup/preview', { method: 'POST', body: { filters }, silent: true, ...options }),
  cleanupJobs: (page = 1, options) => request(`/logs/cleanup/jobs?page=${page}&page_size=10`, { silent: true, ...options }),
  cleanupCreate: (body, options) => request('/logs/cleanup/jobs', { method: 'POST', body, silent: true, ...options }),
  cleanupCancel: (id, options) => request(`/logs/cleanup/jobs/${encodeURIComponent(id)}/cancel`, { method: 'POST', silent: true, ...options }),
}

// A completed, validated response is the only source of a download. Never turn
// an HTTP error body or an aborted transfer into a successful file.
export async function downloadManagedLogs(params, { format = 'csv', redacted = true, signal, onProgress = () => {}, fetchImpl = fetch } = {}) {
  if (!['csv', 'jsonl'].includes(format)) throw new Error('不支持的导出格式')
  const generation = authGeneration
  if (signal?.aborted) throw new DOMException('Export aborted', 'AbortError')
  const response = await fetchImpl(`/api/logs/export?${queryString({ ...managementScopeParams(params), format, redacted: String(redacted !== false) })}`, { credentials: 'same-origin', cache: 'no-store', signal })
  if (signal?.aborted) throw new DOMException('Export aborted', 'AbortError')
  if (response.status === 401) {
    if (generation === authGeneration) {
      clearSession()
      if (typeof window !== 'undefined') window.dispatchEvent(new CustomEvent('auth-expired'))
    }
    throw Object.assign(new Error('认证失效，请重新登录后导出'), { status: 401 })
  }
  if (!response.ok) {
    let payload
    try { payload = await response.json() } catch { /* An HTML error is not a download. */ }
    throw Object.assign(new Error(response.status === 409 ? '查询快照已过期，请关闭导出并刷新日志。' : payload?.error?.message || (typeof payload?.error === 'string' ? payload.error : '') || `导出失败（HTTP ${response.status}）`), { status: response.status })
  }
  const contentType = response.headers.get('content-type') || ''
  const validType = format === 'csv' ? /text\/csv/i.test(contentType) : /(?:application\/(?:x-)?ndjson|application\/jsonl)/i.test(contentType)
  if (!validType || !/attachment/i.test(response.headers.get('content-disposition') || '')) throw new Error('服务器未返回有效导出文件，请重试')
  const countHeader = response.headers.get('x-export-count')
  if (!/^\d+$/.test(countHeader || '')) throw new Error('导出记录数缺失，请重试')
  const total = Number(response.headers.get('content-length')) || 0
  const count = Number(countHeader)
  const reader = response.body?.getReader()
  let received = 0
  const chunks = []
  if (reader) {
    try {
      while (true) {
        if (signal?.aborted) throw new DOMException('Export aborted', 'AbortError')
        const { done, value } = await reader.read()
        if (done) break
        chunks.push(value)
        received += value.byteLength
        onProgress({ received, total, count })
      }
    } catch (error) { await reader.cancel().catch(() => {}); throw error }
    finally { reader.releaseLock() }
  } else {
    const buffer = await response.arrayBuffer()
    chunks.push(buffer)
    received = buffer.byteLength
  }
  if (signal?.aborted) throw new DOMException('Export aborted', 'AbortError')
  if (total && total !== received) throw new Error('导出文件传输不完整，请重试')
  onProgress({ received, total: received, count })
  return { blob: new Blob(chunks, { type: contentType }), count, filename: `logs-filtered-${formatDate(Date.now() / 1000)}.${format}` }
}
