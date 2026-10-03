import { formatDate } from './format.js'
const resourceNameCollator = new Intl.Collator('zh-CN', { numeric: true })

// Keep the first occurrence, matching Array.find for legacy duplicate IDs.
export function indexResources(rows) {
  const index = new Map()
  for (const row of rows) if (!index.has(row.id)) index.set(row.id, row)
  return index
}

// Sort the entire filtered result before pagination, without mutating API rows.
export function sortResources(rows, order = '') {
  if (!order) return rows
  const descending = order.endsWith(':desc')
  const field = order.replace(/:(asc|desc)$/, '')
  return [...rows].sort((a, b) => {
    const difference = field === 'name'
      ? resourceNameCollator.compare(String(a.name || ''), String(b.name || ''))
      : (Number(a[field]) || 0) - (Number(b[field]) || 0)
    return descending ? -difference : difference
  })
}

// Deliberately copy metadata fields rather than retain credential-bearing API rows.
export function resourceStatusSnapshot(rows) {
  const seen = new Set()
  return rows.filter((row) => {
    if (!row || typeof row.id !== 'string' || !row.id || seen.has(row.id)) return false
    seen.add(row.id)
    return true
  }).map((row) => ({ id: row.id, name: String(row.name || row.id), enabled: row.enabled === true, version: row.version }))
}

function statusError(cause) {
  const status = Number(cause?.status)
  if (status === 409) return '数据已被修改，请刷新失败项并核对最新状态'
  if (status === 401 || status === 403) return '登录或操作权限已失效，请重新登录后操作'
  if (status === 404) return '资源已不存在，请刷新列表'
  if (status === 429) return '请求过于频繁，请稍后重试'
  return status >= 400 ? `更新失败（HTTP ${status}），请刷新失败项后重试` : '未能确认更新结果，请刷新失败项后重试'
}

// Sequential execution bounds concurrency to one. A version is required for every
// mutation; retries are fed fresh snapshots only after the user reviews them.
export async function runResourceStatusBatch(rows, enabled, update, { onResult = () => {}, shouldContinue = () => true } = {}) {
  const results = []
  let authFailed = false
  for (const item of resourceStatusSnapshot(rows)) {
    if (!shouldContinue()) break
    let result
    if (authFailed) result = { ...item, ok: false, changed: false, message: '登录或操作权限已失效，本项未提交' }
    else if (!Number.isSafeInteger(item.version) || item.version <= 0) result = { ...item, ok: false, changed: false, message: '缺少有效数据版本，请刷新列表后操作' }
    else if (item.enabled === enabled) result = { ...item, ok: true, changed: false, message: '已是目标状态，无需修改' }
    else {
      try {
        const response = await update(item.id, { enabled, version: item.version })
        result = { ...item, enabled, version: Number.isSafeInteger(response?.version) && response.version > 0 ? response.version : item.version, ok: true, changed: true, message: enabled ? '已启用' : '已停用' }
      } catch (cause) {
        authFailed = cause?.status === 401 || cause?.status === 403
        result = { ...item, ok: false, changed: false, message: statusError(cause) }
      }
    }
    results.push(result)
    onResult(result)
  }
  return results
}

function csvCell(value) {
  let text = value == null ? '' : String(value)
  // Spreadsheet programs may execute formulas even behind whitespace/control characters.
  if (/^[\s\u0000-\u001f]*[=+@-]/.test(text)) text = `'${text}`
  return `"${text.replaceAll('"', '""')}"`
}

function count(value) { return Number.isFinite(Number(value)) && Number(value) >= 0 ? Number(value) : 0 }

// No secret field, credential fragment, username, password or raw error can enter
// the export. Proxy addresses are omitted because legacy servers may return URLs
// with credentials in host. Names and descriptions remain administrator metadata.
export function createResourceCSV(kind, rows, groups = []) {
  const groupIndex = indexResources(groups)
  const groupName = (id) => groupIndex.get(id)?.name || id
  const schemas = {
    keys: {
      columns: ['名称', '状态', '分组', '请求数', 'Token', '失败数'],
      record: (row) => [row.name, row.enabled ? '启用' : '停用', (row.groups || []).map(groupName).join('、'), count(row.requests), count(row.tokens), count(row.errors)],
    },
    proxies: {
      columns: ['名称', '类型', '状态', '绑定账号数', '已配置认证'],
      record: (row) => [row.name, ['http', 'socks5'].includes(row.type) ? row.type.toUpperCase() : '其他', row.enabled ? '启用' : '停用', count(row.accounts), row.username || row.has_password ? '是' : '否'],
    },
    groups: {
      columns: ['名称', '描述', '账号数', 'KEY 数'],
      record: (row) => [row.name, row.description, count(row.accounts), count(row.keys)],
    },
  }
  const schema = schemas[kind]
  if (!schema) throw new Error('Unsupported resource export')
  return '\ufeff' + [schema.columns, ...rows.map(schema.record)].map((cells) => cells.map(csvCell).join(',')).join('\r\n') + '\r\n'
}

export function downloadResourceCSV(kind, rows, groups = []) {
  if (!rows.length) return
  const url = URL.createObjectURL(new Blob([createResourceCSV(kind, rows, groups)], { type: 'text/csv;charset=utf-8' }))
  const link = document.createElement('a')
  link.href = url
  link.download = `codex-${kind}-${formatDate(Date.now() / 1000)}.csv`
  document.body.appendChild(link)
  link.click()
  link.remove()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}
