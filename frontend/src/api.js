// Authentication uses the backend's short-lived HttpOnly cookie session.
let authenticated = false
let sessionChecked = false
let sessionCheck
let authGeneration = 0
export { authGeneration }
const activeRequests = new Set()
try { localStorage.removeItem('adminKey'); sessionStorage.removeItem('adminKey') } catch { /* Optional storage. */ }
export function hasSession() { return authenticated }
export function clearSession() {
  authenticated = false
  sessionChecked = false
  sessionCheck = undefined
  authGeneration++
  for (const controller of activeRequests) controller.abort()
  activeRequests.clear()
  try { localStorage.removeItem('adminKey'); sessionStorage.removeItem('adminKey') } catch { /* Optional storage. */ }
}
export async function ensureSession() {
  if (sessionChecked) return authenticated
  if (!sessionCheck) {
    const generation = authGeneration
    const pending = request('/session', { silent: true, noRedirect: true }).then(data => {
      if (generation === authGeneration) {
        authenticated = data.authenticated === true
        sessionChecked = true
      }
      return authenticated
    }).catch(error => {
      if (generation !== authGeneration) return authenticated
      if (error.status !== 401) window.dispatchEvent(new CustomEvent('api-error', { detail: error.message }))
      authenticated = false
      sessionChecked = true
      return false
    }).finally(() => { if (sessionCheck === pending) sessionCheck = undefined })
    sessionCheck = pending
  }
  return sessionCheck
}
export async function loginSession(key) {
  const result = await request('/session', { method: 'POST', body: { admin_key: key }, silent: true, noRedirect: true })
  if (result.authenticated !== true) throw new Error('服务器未确认登录，请重试')
  authenticated = true
  sessionChecked = true
  authGeneration++
  return result
}
export async function logoutSession() {
  await request('/session', { method: 'DELETE' })
  clearSession()
}
export async function request(path, { method = 'GET', body, silent = false, noRedirect = false, signal } = {}) {
  const generation = authGeneration
  const controller = new AbortController()
  const abort = () => controller.abort()
  signal?.addEventListener('abort', abort, { once: true })
  if (signal?.aborted) controller.abort()
  activeRequests.add(controller)
  try {
    const response = await fetch(`/api${path}`, {
      method, credentials: 'same-origin', cache: 'no-store', signal: controller.signal,
      headers: {
        ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}),
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    })
    let payload
    try { payload = await response.json() } catch {
      if (response.ok) throw new Error('服务器返回了无效 JSON，请重试')
      payload = {}
    }
    if (controller.signal.aborted) throw new DOMException('Request aborted', 'AbortError')
    if (response.status === 401) {
      if (generation === authGeneration && !noRedirect) {
        clearSession()
        window.dispatchEvent(new CustomEvent('auth-expired'))
      }
      throw Object.assign(new Error(noRedirect ? (payload?.error?.message || '管理密钥无效，请检查后重试') : '认证失效，请重新登录'), { status: 401 })
    }
    if (!response.ok) throw Object.assign(new Error(payload?.error?.message || (typeof payload?.error === 'string' ? payload.error : '') || `请求失败（HTTP ${response.status}）`), { status: response.status, code: payload?.error?.code })
    if (path === '/settings' && payload?.version) window.dispatchEvent(new CustomEvent('server-info', { detail: { version: payload.version } }))
    return payload
  } catch (error) {
    if (controller.signal.aborted && error?.status !== 401) throw new DOMException('Request aborted', 'AbortError')
    if (error?.name === 'AbortError') throw error
	if (typeof error?.message === 'string') error.message = error.message.replace(/powerpoint/gi, '上游服务')
    if (!silent) window.dispatchEvent(new CustomEvent('api-error', { detail: error.message || '网络请求失败，请检查连接后重试' }))
    throw error
  } finally {
    signal?.removeEventListener('abort', abort)
    activeRequests.delete(controller)
  }
}
export const api = {
  overview: (params = {}, options = {}) => request(`/overview?${new URLSearchParams(params)}`, options),
  accounts: (params = {}, options = {}) => request(`/accounts${Object.keys(params).length ? `?${new URLSearchParams(params)}` : ''}`, options),
  accountImport: (payload) => request('/accounts', { method: 'POST', body: payload }),
  accountImportPreview: (payload, options = {}) => request('/accounts/import/preview', { ...options, method: 'POST', body: payload }),
  accountDetail: (id, options = {}) => request(`/accounts/${encodeURIComponent(id)}`, options),
  accountBatch: (payload) => request('/accounts/batch', { method: 'POST', body: payload }),
  accountUpdate: (name, patch) => request(`/accounts/${encodeURIComponent(name)}`, { method: 'PUT', body: patch }),
  accountDelete: (id, version) => request(`/accounts/${encodeURIComponent(id)}${version === undefined ? '' : `?version=${encodeURIComponent(version)}`}`, { method: 'DELETE' }),
  accountRefresh: (id) => request('/accounts/refresh', { method: 'POST', body: { id } }),
  accountRequests: (name, limit = 60, options = {}) => request(`/accounts/${encodeURIComponent(name)}/requests?limit=${limit}`, options),
  groups: (options = {}) => request('/groups', options),
  groupCreate: (payload) => request('/groups', { method: 'POST', body: payload }),
  groupUpdate: (id, payload) => request(`/groups/${id}`, { method: 'PUT', body: payload }),
  groupDelete: (id) => request(`/groups/${id}`, { method: 'DELETE' }),
  keys: (options = {}) => request('/keys', options),
  keyCreate: (payload) => request('/keys', { method: 'POST', body: payload }),
  keyUpdate: (id, payload) => request(`/keys/${id}`, { method: 'PUT', body: payload }),
  keyDelete: (id) => request(`/keys/${id}`, { method: 'DELETE' }),
  proxies: (options = {}) => request('/proxies', options),
  proxyCreate: (payload) => request('/proxies', { method: 'POST', body: payload }),
  proxyUpdate: (id, payload) => request(`/proxies/${id}`, { method: 'PUT', body: payload }),
  proxyDelete: (id) => request(`/proxies/${id}`, { method: 'DELETE' }),
  proxyTest: (payload, options = {}) => request('/proxies/test', { ...options, method: 'POST', body: payload }),
  logs: (params = {}, options = {}) => request(`/logs?${new URLSearchParams(params)}`, options),
  diagnostics: (options = {}) => request('/diagnostics', options),
  operations: (params = {}, options = {}) => request(`/operations?${new URLSearchParams(params)}`, options),
  auditLogs: (params = {}, options = {}) => request(`/audit-logs?${new URLSearchParams(params)}`, options),
  settings: (options = {}) => request('/settings', options),
  settingsUpdate: (payload) => request('/settings', { method: 'POST', body: payload }),
  rotateAdminKey: (requestId) => request('/settings/rotate-key', { method: 'POST', body: { request_id: requestId } }),
}
