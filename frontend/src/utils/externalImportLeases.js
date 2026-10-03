import { validLeaseRestore } from './accountLeaseBackup.js'
export const canPrepareDuplicateLease = (row, enabled) => enabled === true && row?.kind === 'duplicate' && Boolean(row.externalLease)
export function externalLeasePreviews(response, count, enabled) {
  if (!enabled) return new Map()
  if (!Array.isArray(response?.external_leases)) {
    if (response?.external_leases === undefined && !response?.lease_restore) return new Map()
    throw new Error('外部租期预览响应格式无效，请重新预览')
  }
  const entries = new Map()
  for (const item of response.external_leases) {
    if (!item || !Number.isSafeInteger(item.account_index) || item.account_index < 1 || item.account_index > count || entries.has(item.account_index) || item.source !== 'sub2api_outer_account' || !Number.isSafeInteger(item.expires_at) || item.expires_at < 0 || item.expires_at > 253402300799 || typeof item.auto_disable !== 'boolean' || item.restore_on_extend !== false || !['unix_seconds', 'rfc3339', 'unlimited'].includes(item.expiry_format) || !['explicit', 'sub2api_default'].includes(item.auto_disable_source)) throw new Error('外部租期与本次导入行不匹配，已阻止提交')
    entries.set(item.account_index, { expires_at: item.expires_at, auto_disable: item.auto_disable, restore_on_extend: false, source: item.source, expiry_format: item.expiry_format, auto_disable_source: item.auto_disable_source })
  }
  if (entries.size && (response.lease_restore_source !== 'sub2api_outer_account' || !validLeaseRestore(response.lease_restore, entries.size) || response.lease_restore.dry_run !== response.dry_run)) throw new Error('外部租期回执不完整，请重新核对导入结果')
  if (entries.size) {
    for (const item of response.lease_restore.items) {
      const expected = entries.get(item.account_index)
      if (!expected || item.expires_at !== expected.expires_at || item.auto_disable !== expected.auto_disable || item.restore_on_extend !== false) throw new Error('外部租期回执与预览配置不一致，已阻止提交')
    }
  }
  return entries
}
export function externalLeaseCommitReceipt(response, expectedLease, accountID) {
  const entries = externalLeasePreviews(response, 1, true)
  const item = entries.get(1)
  if (expectedLease && (!item || ['expires_at', 'auto_disable', 'restore_on_extend'].some(key => expectedLease[key] !== item[key]))) throw new Error('账号阶段已返回，但租期配置与预览不一致，租期未确认。')
  if (!item) return null
  const receipt = response.lease_restore
  if (receipt.dry_run !== false || receipt.items.some(row => row.id && row.id !== accountID)) throw new Error('账号阶段已返回，但租期回执目标与账号不匹配，租期未确认。')
  return receipt
}
export function externalLeaseSummary(item, now = Math.floor(Date.now() / 1000)) {
  if (!item) return ''
  return item.expires_at === 0 ? '不限期' : item.expires_at <= now ? item.auto_disable ? '已过期；单独确认租期后可能立即停用账号' : '已过期；未开启自动停用' : item.auto_disable ? '未来到期；单独确认后启用到期自动停用' : '未来到期；未开启自动停用'
}
