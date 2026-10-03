import { authGeneration, clearSession } from '../api.js'

// Response validators stay in memory and are scoped to the current login.
export function createAccountListReader({ fetchImpl = fetch } = {}) {
  const entries = new Map()
  let cacheGeneration = authGeneration
  return async function read(filters, { signal } = {}) {
    const generation = authGeneration
    if (cacheGeneration !== generation) { entries.clear(); cacheGeneration = generation }
    const query = new URLSearchParams(Object.entries(filters).sort(([left], [right]) => left.localeCompare(right))).toString()
    const cached = entries.get(query)
    if (signal?.aborted) throw new DOMException('List aborted', 'AbortError')
    const response = await fetchImpl(`/api/accounts/manage?${query}`, { credentials: 'same-origin', cache: 'no-store', signal, headers: cached?.etag ? { 'If-None-Match': cached.etag } : {} })
    if (signal?.aborted) throw new DOMException('List aborted', 'AbortError')
    if (response.status === 401 && generation === authGeneration) { clearSession(); if (typeof window !== 'undefined') window.dispatchEvent(new CustomEvent('auth-expired')) }
    if (generation !== authGeneration) throw new DOMException('Session changed', 'AbortError')
    if (response.status === 304) {
      if (!cached) throw new Error('账号缓存已失效，请重新读取')
      return cached.data
    }
    let data
    try { data = await response.json() } catch { if (response.ok) throw new Error('服务器返回了无效账号数据'); data = {} }
    if (signal?.aborted || generation !== authGeneration) throw new DOMException('List aborted', 'AbortError')
    if (!response.ok) throw Object.assign(new Error(data?.error?.message || (typeof data?.error === 'string' ? data.error : '') || `账号读取失败（HTTP ${response.status}）`), { status: response.status })
    if (!Array.isArray(data.accounts) || !Number.isSafeInteger(data.total) || data.total < 0 || data.total_kind !== 'exact') throw new Error('服务器未返回完整账号分页信息')
    const etag = response.headers.get('etag')
    if (etag) {
      entries.delete(query); entries.set(query, { etag, data })
      if (entries.size > 20) entries.delete(entries.keys().next().value)
    } else entries.delete(query)
    return data
  }
}
