import { request } from '../api'
import { createAccountListReader } from '../utils/accountListReader'

const post = (path, body, options = {}) => request(path, { ...options, method: 'POST', body, silent: true })
export const accountManagement = {
  list: createAccountListReader(),
  runtime: (id, model = '', options = {}) => request(`/accounts/${encodeURIComponent(id)}/runtime${model ? `?model=${encodeURIComponent(model)}` : ''}`, { ...options, silent: true }),
  lastUsed: (ids, options = {}) => post('/accounts/last-used', { ids }, options),
  selection: (filters, options = {}) => post('/accounts/selection', { filters }, options),
  usageBatch: (body, options = {}) => post('/accounts/usage-batch', body, options),
  actions: (body, options = {}) => post('/accounts/actions', body, options),
  reauthorize: (id, body, options = {}) => post(`/accounts/${encodeURIComponent(id)}/reauthorize`, body, options),
  reauthorizeBatch: (body, options = {}) => post('/accounts/reauthorize-batch', body, options),
  metadata: (id, options = {}) => request(`/accounts/${encodeURIComponent(id)}/metadata`, { ...options, silent: true }),
  export: (target, credentials = false, options = {}) => post(`/accounts/export${credentials ? '/credentials' : ''}`, target, options),
  importBundle: (body, options = {}) => post('/accounts/import-bundle', body, options),
  importResources: (body, options = {}) => post('/accounts/import-resources', body, options),
  oauthStart: (body, options = {}) => post('/account-oauth/start', body, options),
  oauthExchange: (body, options = {}) => post('/account-oauth/exchange', body, options),
  oauthImport: (body, options = {}) => post('/account-oauth/import', body, options),
  oauthApply: (body, options = {}) => post('/account-oauth/apply', body, options),
  oauthStatus: (id, options = {}) => request(`/account-oauth/${encodeURIComponent(id)}`, { ...options, silent: true }),
  oauthCancel: (id) => request(`/account-oauth/${encodeURIComponent(id)}`, { method: 'DELETE', silent: true }),
  upstreamSnapshots: (id, options = {}) => request(`/accounts/${encodeURIComponent(id)}/upstream`, { ...options, silent: true }),
  upstreamCheck: (id, body, options = {}) => post(`/accounts/${encodeURIComponent(id)}/upstream`, body, options),
  upstreamBatch: (body, options = {}) => post('/accounts/upstream/batch', body, options),
  privacy: (id, body) => post(`/accounts/${encodeURIComponent(id)}/privacy`, body),
  resetCredit: (id, body) => post(`/accounts/${encodeURIComponent(id)}/reset-credit`, body),
  autoReset: (id, options = {}) => request(`/accounts/${encodeURIComponent(id)}/auto-reset`, { ...options, silent: true }),
  updateAutoReset: (id, body) => request(`/accounts/${encodeURIComponent(id)}/auto-reset`, { method: 'PUT', body, silent: true }),
  schedules: (id, options = {}) => request(`/accounts/${encodeURIComponent(id)}/schedules`, { ...options, silent: true }),
  createSchedule: (id, body) => post(`/accounts/${encodeURIComponent(id)}/schedules`, body),
  updateSchedule: (id, body) => request(`/account-schedules/${encodeURIComponent(id)}`, { method: 'PUT', body, silent: true }),
  deleteSchedule: (id, version) => request(`/account-schedules/${encodeURIComponent(id)}?version=${version}`, { method: 'DELETE', silent: true }),
  scheduleResults: (id, options = {}) => request(`/account-schedules/${encodeURIComponent(id)}/results?limit=50`, { ...options, silent: true }),
  lease: (id, options = {}) => request(`/accounts/${encodeURIComponent(id)}/lease`, { ...options, silent: true }),
  updateLease: (id, body) => request(`/accounts/${encodeURIComponent(id)}/lease`, { method: 'PUT', body, silent: true }),
  crsPreview: (body) => post('/accounts/sync/crs/preview', body),
  crsSync: (body) => post('/accounts/sync/crs', body),
}

export function downloadAccountJSON(data, filename) {
  const url = URL.createObjectURL(new Blob([JSON.stringify(data, null, 2)], { type: 'application/json;charset=utf-8' }))
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = filename
  anchor.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
