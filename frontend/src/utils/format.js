export function formatPercent(ratio, denominator) {
  if (denominator !== undefined && (!Number.isFinite(Number(denominator)) || Number(denominator) <= 0)) return '暂无样本'
  if (ratio === null || ratio === undefined || ratio === '' || !Number.isFinite(Number(ratio))) return '—'
  return `${Number((Number(ratio) * 100).toFixed(1))}%`
}

export function formatNumber(value) {
  if (value === null || value === undefined || value === '' || !Number.isFinite(Number(value))) return '—'
  return Number(value).toLocaleString('en-US', { maximumFractionDigits: 10 })
}

// Milliseconds are the API's unit. Do not confuse a missing sample with a
// measured zero, or abbreviate elapsed milliseconds with a token-count suffix.
export function formatDuration(value, { empty = '—' } = {}) {
  if (value === null || value === undefined || value === '' || typeof value === 'boolean' || !Number.isFinite(Number(value)) || Number(value) < 0) return empty
  const milliseconds = Number(value)
  if (milliseconds < 1000) return `${formatNumber(Number(milliseconds.toFixed(3)))} ms`
  let seconds = Math.round(milliseconds) / 1000
  const parts = []
  for (const [size, label] of [[86400, '天'], [3600, '小时'], [60, '分']]) {
    const count = Math.floor(seconds / size)
    if (count) parts.push(`${formatNumber(count)} ${label}`)
    seconds -= count * size
  }
  if (seconds || !parts.length) parts.push(`${formatNumber(Number(seconds.toFixed(3)))} 秒`)
  return parts.join(' ')
}

export const SHANGHAI_TIMEZONE = 'Asia/Shanghai'
const shanghaiFormatter = new Intl.DateTimeFormat('en-CA', {
  timeZone: SHANGHAI_TIMEZONE, year: 'numeric', month: '2-digit', day: '2-digit',
  hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23',
})

// API numbers are Unix seconds. Zone-less wall times entered in the console
// belong to Shanghai, regardless of the browser's operating-system timezone.
export function timestampMillis(value) {
  if (value === null || value === undefined || value === '') return NaN
  if (typeof value === 'number' || /^\d+(?:\.\d+)?$/.test(String(value))) return Number(value) * 1000
  if (value instanceof Date) return value.getTime()
  const text = String(value).trim()
  const wall = /^\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?)?$/.test(text)
  return Date.parse(wall ? `${text.length === 10 ? text + 'T00:00:00' : text.replace(' ', 'T')}+08:00` : text)
}

export function shanghaiTimeParts(value) {
  const date = new Date(timestampMillis(value))
  if (!Number.isFinite(date.getTime())) return { date: '—', time: '—' }
  const parts = Object.fromEntries(shanghaiFormatter.formatToParts(date).map(part => [part.type, part.value]))
  return { date: `${parts.year}-${parts.month}-${parts.day}`, time: `${parts.hour}:${parts.minute}:${parts.second}` }
}

export function formatTime(value) {
  const parts = shanghaiTimeParts(value)
  return parts.date === '—' ? '—' : `${parts.date} ${parts.time}`
}

export const formatDate = value => shanghaiTimeParts(value).date

export function shanghaiDateInput(value) {
  const parts = shanghaiTimeParts(value)
  return parts.date === '—' ? '' : `${parts.date}T${parts.time.slice(0, 5)}`
}

export function parseShanghaiDateInput(value) {
  if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(value)) return NaN
  const millis = timestampMillis(value)
  return shanghaiDateInput(millis / 1000) === value ? Math.floor(millis / 1000) : NaN
}

export function shanghaiDayStart(value = Date.now() / 1000) {
  return parseShanghaiDateInput(`${formatDate(value)}T00:00`)
}
