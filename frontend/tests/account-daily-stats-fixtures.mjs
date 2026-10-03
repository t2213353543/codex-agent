import dayjs from 'dayjs'
import utc from 'dayjs/plugin/utc.js'
import timezonePlugin from 'dayjs/plugin/timezone.js'
import { dailyDate } from '../src/utils/accountDailyStats.js'
dayjs.extend(utc); dayjs.extend(timezonePlugin)

export const emptyDailyTotals = () => ({ total_requests: 0, total_tokens: 0, total_input_tokens: 0, total_output_tokens: 0, total_cached_tokens: 0, success: 0, errors: 0, aborted: 0, usage_known_requests: 0, usage_unknown_requests: 0 })
export function dailyStatsFixture(params, totals) {
  const from = Number(params.from), to = Number(params.to), timezone = params.timezone || 'UTC'
  const first = dailyDate(from, timezone), last = dailyDate(to - 1, timezone), series = []
  const normalized = { ...emptyDailyTotals(), ...totals }
  const cursor = new Date(first + 'T00:00:00Z')
  while (cursor.toISOString().slice(0, 10) <= last) {
    const date = cursor.toISOString().slice(0, 10)
    series.push({ ...emptyDailyTotals(), ...series.length ? {} : normalized, time: dayjs.tz(date + 'T00:00:00', timezone).unix() })
    cursor.setUTCDate(cursor.getUTCDate() + 1)
  }
  return { ...normalized, from, to, timezone, step: 86400, scope: 'retained_request_details', checked_at: Math.floor(Date.now() / 1000), series, models: [], endpoints: [], error_categories: [] }
}
