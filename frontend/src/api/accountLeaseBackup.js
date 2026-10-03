import { request } from '../api.js'

export const leaseBackup = {
  receipt: (id, options = {}) => request(`/accounts/import-leases/receipts/${encodeURIComponent(id)}`, { ...options, silent: true }),
  restore: (body, options = {}) => request('/accounts/import-leases', { ...options, method: 'POST', body, silent: true }),
}
