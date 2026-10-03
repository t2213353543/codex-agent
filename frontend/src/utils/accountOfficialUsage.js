const number = value => typeof value === 'number' && Number.isFinite(value)

export function officialWindowLabel(seconds, fallback = '窗口') {
  if (!number(seconds) || seconds <= 0) return `${fallback}（周期未知）`
  if (seconds % 86400 === 0) return `${seconds / 86400} 天`
  if (seconds % 3600 === 0) return `${seconds / 3600} 小时`
  if (seconds % 60 === 0) return `${seconds / 60} 分钟`
  return `${seconds} 秒`
}

export function officialUsageWindows(snapshot, { additional = false } = {}) {
  if (!snapshot?.ok || snapshot.kind !== 'usage') return []
  const windows = []
  const add = (rate, prefix = '') => {
    for (const [key, fallback] of [['primary_window', '主要窗口'], ['secondary_window', '次要窗口']]) {
      const source = rate?.[key]
      if (!source || typeof source !== 'object') continue
      const used = number(source.used_percent) && source.used_percent >= 0 ? source.used_percent : null
      const reset = number(source.reset_at) && source.reset_at > 0 ? source.reset_at
        : number(source.reset_after_seconds) && source.reset_after_seconds >= 0 && number(snapshot.checked_at) && snapshot.checked_at > 0 ? snapshot.checked_at + source.reset_after_seconds : null
      const cost = !prefix && snapshot.local_window_costs?.[key]
      windows.push({ key: `${prefix}${key}`, label: `${prefix}${officialWindowLabel(source.limit_window_seconds, fallback)}`, used_percent: used, progress: used === null ? null : Math.min(100, used), reset_at: reset, seconds: number(source.limit_window_seconds) ? source.limit_window_seconds : null, local_cost: cost?.source === 'local_token_estimate' && cost.scope === 'retained_request_details' ? cost : null })
    }
  }
  add(snapshot.data?.rate_limit)
  if (additional) for (const item of snapshot.data?.additional_rate_limits || []) add(item.rate_limit, `${item.limit_name || item.metered_feature || '额外额度'} · `)
  return windows
}

export function accountWindowCostLabel(cost) {
  if (!cost || cost.error || !number(cost.usd) || cost.usd < 0) return '本地估算未取得'
  const amount = cost.usd === 0 ? '$0.00' : cost.usd < 0.0001 ? '< $0.0001' : `$${cost.usd.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 4 })}`
  return `${cost.unknown_requests > 0 ? '已知部分' : '本地估算'} ${amount}`
}

export function accountWindowCostHint(cost) {
  if (!cost) return '官方窗口时间不完整，无法计算美元估算。'
  if (cost.error) return '本地请求统计暂不可用。'
  return `按 Sub2API 账号统计口径计算窗口内 USD 参考金额；不是官方扣费或余额。新请求保留请求时的价格，历史未定价请求按当前价表重建。已估算 ${cost.estimated_requests || 0} 次，无法估算 ${cost.unknown_requests || 0} 次，历史重建 ${cost.reconstructed_requests || 0} 次。其他客户端及已清理日志的用量不包含在内。价格版本：${cost.price_version || '未知'}。`
}

// Sub2API AccountUsageCell.openAISevenDayEstimatedTotalCost: account cost
// multiplied by 100 / official utilization. Plans do not have invented USD caps.
export function accountWindowTotalEstimate(window, now = Date.now() / 1000) {
  const cost = window?.local_cost
  if (window?.seconds !== 604800 || cost?.error || !number(cost?.usd) || cost.usd <= 0 || !number(window.used_percent) || window.used_percent <= 0 || number(window.reset_at) && window.reset_at <= now) return null
  const value = cost.usd * 100 / window.used_percent
  return Number.isFinite(value) && value > 0 ? value : null
}

export function accountWindowTotalLabel(window) {
  const estimate = accountWindowTotalEstimate(window)
  if (estimate === null) return ''
  return `总额度估算 $${estimate.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`
}

export function accountWindowTotalHint(window) {
  return `与 Sub2API 相同：7 天窗口账号费用 ÷ 官方已用比例，即 $${window.local_cost?.usd} × 100 ÷ ${window.used_percent}%。不按套餐固定美元上限；记录覆盖范围会影响估算。${window.local_cost?.unknown_requests > 0 ? '部分请求未定价，当前结果仅按已知部分推算。' : ''}`
}

// Bound automatic reads to two batches, each with at most ten accounts. Abort
// on navigation and validate identities before displaying a returned window.
export async function readOfficialAccountUsage(items, { send, signal, force = false, onResult = () => {} }) {
  const targets = items.map(({ id, version }) => ({ id, version }))
  let offset = 0
  const worker = async () => {
    while (offset < targets.length && !signal?.aborted) {
      const batch = targets.slice(offset, offset += 10)
      try {
        const data = await send({ kind: 'usage', items: batch, force }, { signal })
        if (signal?.aborted) return
        if (!Array.isArray(data.results) || data.results.length !== batch.length || new Set(data.results.map(row => row.id)).size !== batch.length || data.results.some(row => !batch.some(target => target.id === row.id) || typeof row.ok !== 'boolean' || row.ok && (!row.snapshot?.ok || row.snapshot.id !== row.id || row.snapshot.kind !== 'usage'))) throw new Error('官方用量返回不完整')
        for (const row of data.results) onResult(row.id, { snapshot: row.snapshot || null, code: row.code || '', error: row.ok ? '' : row.snapshot?.error?.message || row.code || '官方用量查询失败' })
      } catch (error) {
        if (signal?.aborted || error.name === 'AbortError') return
        for (const row of batch) onResult(row.id, { snapshot: null, error: error.message || '官方用量查询失败' })
      }
    }
  }
  await Promise.all([worker(), worker()])
}
