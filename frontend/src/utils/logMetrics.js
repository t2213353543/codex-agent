// Missing fields are unknown, including older omitempty-based log records.
// Aggregate only measured records; never turn omitted usage into zero.
export function logMetricNumber(value) {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null
}

const usageTokenMetrics = ['total_tokens', 'total_input_tokens', 'total_output_tokens', 'total_cached_tokens', 'total_cache_write_tokens']
const percentFormatter = new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 1 })

export function usageMetricNumber(row, metric) {
  if (metric === 'total_cache_write_tokens') {
    return logMetricNumber(row?.cache_write_samples) > 0 ? logMetricNumber(row?.total_cache_write_tokens) : null
  }
  if (metric === 'total_cached_tokens' && row?.cache_read_samples !== undefined) {
    if (!(logMetricNumber(row.cache_read_samples) > 0)) return null
    // The legacy total also retains reported subtotals from incomplete records.
    // Match the value to the observed-read sample coverage used by the UI.
    return logMetricNumber(row.cache_read_tokens)
  }
  if (usageTokenMetrics.includes(metric) && row?.usage_known_requests === 0 && logMetricNumber(row?.usage_unknown_requests) > 0) return null
  return logMetricNumber(row?.[metric])
}

export function usageMetricCoverage(row, metric) {
  if (metric === 'total_cache_write_tokens') return `${(logMetricNumber(row?.cache_write_samples) || 0).toLocaleString('zh-CN')} 条请求有创建记录`
  if (metric === 'total_cached_tokens' && row?.cache_read_samples !== undefined) return `${(logMetricNumber(row.cache_read_samples) || 0).toLocaleString('zh-CN')} 条请求有读取记录`
  if (!usageTokenMetrics.includes(metric)) return ''
  const known = logMetricNumber(row?.usage_known_requests), unknown = logMetricNumber(row?.usage_unknown_requests)
  if (known === null || unknown === null || unknown === 0) return ''
  return known === 0 ? `用量未记录（${unknown.toLocaleString('zh-CN')} 条请求）` : `已记录 ${known.toLocaleString('zh-CN')} / ${(known + unknown).toLocaleString('zh-CN')} 条请求`
}

export function logCacheRatio(row) {
  if (row?.usage_known === false || row?.cached_tokens_known === false) return null
  const input = logMetricNumber(row.input_tokens)
  const cached = logMetricNumber(row.cached_tokens)
  const observed = row?.cached_tokens_known === true || cached > 0
  return observed && input !== null && input > 0 && cached !== null && cached <= input ? cached / input : null
}

export function logPercent(value) {
  return value === null ? '—' : `${percentFormatter.format(value * 100)}%`
}

export function summarizeLogPage(rows) {
  let tokens = 0, usageCount = 0, input = 0, cached = 0, cacheCount = 0, duration = 0, durationCount = 0
  for (const row of rows) {
    const inTokens = logMetricNumber(row.input_tokens)
    const outTokens = logMetricNumber(row.output_tokens)
    if (inTokens !== null && outTokens !== null) {
      tokens += inTokens + outTokens
      usageCount++
    }
    if (logCacheRatio(row) !== null) {
      input += inTokens
      cached += row.cached_tokens
      cacheCount++
    }
    const latency = logMetricNumber(row.latency_ms)
    if (latency !== null) {
      duration += latency
      durationCount++
    }
  }
  return {
    tokens: usageCount ? tokens : null, usageCount,
    cacheRatio: input > 0 ? cached / input : null, cacheCount,
    averageDuration: durationCount ? duration / durationCount : null, durationCount,
  }
}

export const usageRankMetrics = ['total_tokens', 'total_requests', 'total_input_tokens', 'total_output_tokens', 'total_cached_tokens', 'total_cache_write_tokens', 'success_rate']
export function rankUsageRecords(rows = [], metric = 'total_tokens', limit = 50) {
  const field = usageRankMetrics.includes(metric) ? metric : 'total_tokens'
  const value = row => field === 'success_rate' ? Number.isFinite(row.total_requests) && row.total_requests > 0 && Number.isFinite(row.success) && row.success >= 0 && row.success <= row.total_requests ? row.success / row.total_requests : null : usageMetricNumber(row, field)
  return [...rows].sort((left, right) => {
    const a = value(left), b = value(right)
    if (a === null && b !== null) return 1
    if (b === null && a !== null) return -1
    return (b ?? 0) - (a ?? 0) || String(left.id || left.name).localeCompare(String(right.id || right.name))
  }).slice(0, [20, 50, 100, 200].includes(Number(limit)) ? Number(limit) : 50)
}
