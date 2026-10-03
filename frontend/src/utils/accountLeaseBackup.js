export function validLeaseManagement(management, count) {
  if (management == null) return true
  if (management.version !== 1 || management.leases !== undefined && !Array.isArray(management.leases) || (management.leases || []).length > count) return false
  const seen = new Set()
  return (management.leases || []).every(item => {
    if (!item || !Number.isSafeInteger(item.account_index) || item.account_index < 1 || item.account_index > count || seen.has(item.account_index) || !Number.isSafeInteger(item.expires_at) || item.expires_at < 0 || item.expires_at > 253402300799 || typeof item.auto_disable !== 'boolean' || typeof item.restore_on_extend !== 'boolean') return false
    if (Object.keys(item).some(key => !['account_index', 'expires_at', 'auto_disable', 'restore_on_extend'].includes(key))) return false
    seen.add(item.account_index)
    return true
  })
}

const statuses = new Set(['pending_import', 'pending', 'skipped', 'running', 'applied', 'failed', 'unknown'])
export function validLeaseRestore(receipt, expectedCount) {
  if (!receipt || typeof receipt.dry_run !== 'boolean' || !Array.isArray(receipt.items) || receipt.items.length > 10000 || expectedCount !== undefined && receipt.items.length !== expectedCount) return false
  const seen = new Set()
  return receipt.items.every(item => {
    if (!item || !Number.isSafeInteger(item.account_index) || item.account_index < 1 || seen.has(item.account_index) || !statuses.has(item.status) || !Number.isSafeInteger(item.expires_at) || item.expires_at < 0 || typeof item.auto_disable !== 'boolean' || typeof item.restore_on_extend !== 'boolean') return false
    if (['pending', 'running', 'applied', 'unknown'].includes(item.status) && (typeof item.id !== 'string' || !item.id || !Number.isSafeInteger(item.account_version) || item.account_version < 1 || !Number.isSafeInteger(item.lease_version) || item.lease_version < 0)) return false
    seen.add(item.account_index)
    return true
  })
}

export function leaseRestoreTarget(item) {
  return { account_index: item.account_index, id: item.id, account_version: item.account_version, lease_version: item.lease_version }
}
export function leaseRestoreItems(receipt) {
  if (!validLeaseRestore(receipt)) throw new Error('租期回执格式无效，请重新核对账号导入结果')
  return receipt.items.filter(item => item.status === 'pending').map(leaseRestoreTarget)
}
export function mergeLeaseRestoreResults(receipt, response, targets) {
  if (!validLeaseRestore(response, targets.length) || response.receipt_id !== receipt.receipt_id) throw new Error('租期结果不完整，必须读取回执核对，不能重复提交')
  const expected = new Map(targets.map(item => [item.account_index, item]))
  for (const item of response.items) {
    const target = expected.get(item.account_index)
    if (!target || Object.entries(target).some(([key, value]) => item[key] !== value)) throw new Error('租期结果与固定账号目标不匹配')
  }
  const updates = new Map(response.items.map(item => [item.account_index, item]))
  return { ...receipt, items: receipt.items.map(item => updates.get(item.account_index) || item) }
}
export const leaseRestoreStatus = { pending_import: '等待账号导入', pending: '待恢复租期', skipped: '未纳入恢复', running: '结果待确认', applied: '租期已恢复', failed: '未恢复', unknown: '结果未知' }
export const leaseRestoreReason = {
  account_import_failed: '账号导入失败', ambiguous_import_target: '多个文件条目对应同一账号，不能猜测租期归属', account_changed: '账号已变化，请重新导入核对',
  lease_unavailable: '租期读取失败', account_missing: '目标账号不存在', version_conflict: '账号或租期版本已变化，请重新导入后确认',
  confirm_expired_required: '此项租期已过期，需要确认到期停用', confirm_replace_required: '此项已有本地租期，需要确认覆盖',
  result_not_confirmed: '结果未持久确认，请读取回执并核对账号租期，不要重复执行', lease_restore_not_confirmed: '租期写入结果未确认，请核对账号租期',
  receipt_unavailable: '回执不可用', lease_restore_rejected: '恢复被拒绝，请核对账号当前状态',
}
