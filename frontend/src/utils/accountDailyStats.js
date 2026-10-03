import { timezoneFormatter } from './timezoneFormatter.js'
const dailyNumberFormatter = new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 2 })

export function canonicalDailyTimezone(value) {
  if (typeof value !== 'string' || !value || value.length > 100 || value !== value.trim() || value === 'Local' || !/^[A-Za-z][A-Za-z0-9_+\-/]*$/.test(value)) return ''
  try { return timezoneFormatter(value).resolvedOptions().timeZone } catch { return '' }
}
export const validDailyTimezone = value => Boolean(canonicalDailyTimezone(value))
const count = value => Number.isSafeInteger(value) && value >= 0
export function dailyDate(stamp, timezone) {
  const parts = timezoneFormatter(timezone, true).formatToParts(new Date(stamp * 1000))
  const fields = Object.fromEntries(parts.map(part => [part.type, part.value]))
  return `${fields.year}-${fields.month}-${fields.day}`
}
const dayNumber = date => Date.parse(date + 'T00:00:00Z') / 86400000
export const dailyNumber = value => value === null || !Number.isFinite(value) ? '无分母' : dailyNumberFormatter.format(value)
export function validateDailyStats(stats, query) {
  if (!validDailyTimezone(query.timezone) || !stats || stats.scope !== 'retained_request_details' || stats.timezone !== query.timezone || stats.step !== 86400 || stats.from !== Number(query.from) || stats.to !== Number(query.to) || !Array.isArray(stats.series) || stats.series.length > 5000 || !count(stats.total_requests) || !count(stats.total_tokens)) throw new Error('服务器未返回完整的日历日统计范围。')
  if (!stats.series.length) throw new Error('日历日统计缺少完整日期序列，请重新查询。')
  let previous = -Infinity, requests = 0, tokens = 0
  const dates = new Set()
  for (const row of stats.series) {
    if (!Number.isSafeInteger(row.time) || row.time <= previous || !count(row.total_requests) || !count(row.total_tokens) || !count(row.usage_known_requests) || !count(row.usage_unknown_requests) || row.usage_known_requests + row.usage_unknown_requests !== row.total_requests) throw new Error('每日用量返回不完整，未生成日均。')
    const date = dailyDate(row.time, query.timezone)
    if (dates.has(date)) throw new Error('统计包含重复日历日，不能计算日均。')
    dates.add(date); previous = row.time; requests += row.total_requests; tokens += row.total_tokens
  }
  const first = dailyDate(stats.from, query.timezone), last = dailyDate(stats.to - 1, query.timezone)
  const actual = [...dates]
  if (actual[0] !== first || actual.at(-1) !== last || actual.length !== dayNumber(last) - dayNumber(first) + 1 || requests !== stats.total_requests || tokens !== stats.total_tokens) throw new Error('日统计与区间总量不一致，请重新建立查询快照。')
  return stats
}
function peaks(rows, value) {
  let maximum = null, days = []
  for (const row of rows) {
    if (row.total_requests === 0) continue
    const amount = value(row)
    if (amount === null) continue
    if (maximum === null || amount > maximum) { maximum = amount; days = [row.date] }
    else if (amount === maximum) days.push(row.date)
  }
  return { value: maximum, days }
}
export function accountDailySummary(stats, mode = 'active') {
  const days = stats.series.map(row => ({ ...row, date: dailyDate(row.time, stats.timezone) }))
  const activeDays = days.filter(row => row.total_requests > 0).length
  const divisor = mode === 'calendar' ? days.length : activeDays
  const peakRequests = peaks(days, row => row.total_requests)
  const peakTokens = peaks(days, row => row.usage_known_requests === 0 ? null : row.total_tokens)
  const todayKey = dailyDate(stats.checked_at, stats.timezone)
  return { days, activeDays, calendarDays: days.length, divisor, averageRequests: divisor ? stats.total_requests / divisor : null, averageTokens: divisor ? stats.total_tokens / divisor : null, tokenPartial: stats.usage_unknown_requests > 0, peakRequests, peakTokens, today: days.find(row => row.date === todayKey) || null }
}
export function dailyTrendValue(row, metric) {
  if (metric === 'requests') return { value: row.total_requests, text: dailyNumber(row.total_requests), state: 'complete' }
  if (metric === 'tokens') {
    if (row.usage_known_requests === 0 && row.total_requests > 0) return { value: null, text: '未知', state: 'unknown' }
    return { value: row.total_tokens, text: dailyNumber(row.total_tokens), state: row.usage_unknown_requests > 0 ? 'partial' : 'complete' }
  }
  return { value: null, text: '未提供', state: 'unknown' }
}
