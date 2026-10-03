export const auditActions = {
  'account.import': '导入账号', 'account.update': '修改账号', 'account.delete': '删除账号',
  'account.select': '选择账号', 'account.credentials.update': '更新账号凭据',
  'group.create': '创建分组', 'group.update': '修改分组', 'group.delete': '删除分组',
  'proxy.create': '创建代理', 'proxy.update': '修改代理', 'proxy.delete': '删除代理',
  'settings.update': '更新设置', 'admin_key.rotate': '轮换管理密钥',
}
export const dependencyLabels = { ok: '连接正常', not_configured: '未配置', unavailable: '无法连接', timeout: '检查超时' }
export const dependencyType = (status) => ({ ok: 'success', not_configured: 'info', unavailable: 'danger', timeout: 'warning' })[status] || 'info'
export const auditResultLabel = (result) => ({ ok: '成功', success: '成功', failed: '失败', error: '失败' })[result] || '未识别结果'

export function auditQuery(query = {}) {
  const text = (key, max) => typeof query[key] === 'string' ? query[key].slice(0, max) : ''
  const timestamp = (key) => /^\d{1,11}$/.test(text(key, 20)) ? text(key, 20) : ''
  return {
    action: text('action', 128), entity_id: text('entity_id', 256),
    from: timestamp('from'), to: timestamp('to'),
    limit: [20, 50, 100, 200].includes(Number(query.limit)) ? Number(query.limit) : 50,
    cursor: text('cursor', 4096),
  }
}
export function auditRequestParams(value) {
  return Object.fromEntries(Object.entries(value).filter(([, entry]) => entry !== '' && entry !== null && entry !== undefined).map(([key, entry]) => [key, String(entry)]))
}
export function auditRangeError(value) {
  if (value.from && value.to && Number(value.from) >= Number(value.to)) return '开始时间必须早于结束时间。'
  return ''
}
export function validAuditPage(value) {
  return value && typeof value.supported === 'boolean' && Array.isArray(value.items) &&
    typeof value.has_more === 'boolean' && typeof value.next_cursor === 'string' &&
    (!value.has_more || !!value.next_cursor) &&
    value.items.every(item => item && typeof item.id === 'string' && typeof item.action === 'string' && Number.isFinite(item.created_at))
}
// Share operational results without identifiers, credentials or server addresses.
export function diagnosticExport(value) {
  const safeNumber = number => Number.isFinite(number) && number >= 0 ? number : null
  return {
    version: typeof value?.version === 'string' && /^[\w.+-]{1,80}$/.test(value.version) ? value.version : '',
    checked_at: safeNumber(value?.checked_at), uptime_seconds: safeNumber(value?.uptime_seconds),
    storage_mode: ['file', 'postgres'].includes(value?.storage_mode) ? value.storage_mode : 'unknown',
    status: ['ok', 'degraded'].includes(value?.status) ? value.status : 'unknown',
    dependencies: Object.fromEntries(['postgres', 'redis'].map(name => {
      const item = value?.dependencies?.[name] || {}
      return [name, { configured: item.configured === true, status: Object.hasOwn(dependencyLabels, item.status) ? item.status : 'unknown', latency_ms: safeNumber(item.latency_ms) }]
    })),
  }
}
export function auditExport(items) {
  return items.map(item => ({
    created_at: Number.isFinite(item.created_at) && item.created_at >= 0 ? item.created_at : null,
    action: Object.hasOwn(auditActions, item.action) ? item.action : 'other',
    result: ['ok', 'success', 'failed', 'error'].includes(item.result) ? item.result : 'unknown',
  }))
}

export function downloadJSON(data, filename) {
  const url = URL.createObjectURL(new Blob([JSON.stringify(data, null, 2) + '\n'], { type: 'application/json;charset=utf-8' }))
  const link = document.createElement('a')
  link.href = url
  link.download = filename
  document.body.append(link)
  try { link.click() } finally { link.remove(); setTimeout(() => URL.revokeObjectURL(url), 1000) }
}
