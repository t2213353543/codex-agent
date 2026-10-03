import test from 'node:test'
import assert from 'node:assert/strict'
import { accountDailySummary, dailyDate, dailyTrendValue, validateDailyStats } from '../src/utils/accountDailyStats.js'
import { dailyStatsFixture, emptyDailyTotals } from './account-daily-stats-fixtures.mjs'
const query = (from = '2026-03-07T00:00:00-05:00', to = '2026-03-10T00:00:00-04:00', timezone = 'America/New_York') => ({ from: String(Date.parse(from) / 1000), to: String(Date.parse(to) / 1000), timezone, step: '86400' })
const totals = () => ({ ...emptyDailyTotals(), total_requests: 4, total_tokens: 1000, usage_known_requests: 4 })
test('calendar daily response spans spring 23-hour and autumn 25-hour days without changing day denominator', () => {
  for (const q of [query(), query('2026-10-31T00:00:00-04:00', '2026-11-03T00:00:00-05:00')]) {
    const stats = dailyStatsFixture(q, totals()); validateDailyStats(stats, q)
    assert.equal(stats.series.length, 3); assert.equal(accountDailySummary(stats).calendarDays, 3)
    assert.notEqual(stats.series[2].time - stats.series[1].time, 86400)
  }
})
test('active-day average matches real reference denominator while natural-day mode includes empty days', () => {
  const stats = dailyStatsFixture(query(), totals())
  const active = accountDailySummary(stats), calendar = accountDailySummary(stats, 'calendar')
  assert.equal(active.activeDays, 1); assert.equal(active.averageRequests, 4); assert.equal(calendar.divisor, 3)
  assert.equal(active.averageTokens, 1000); assert.equal(calendar.averageTokens, 1000 / 3)
  assert.equal(Object.hasOwn(active, 'costs'), false)
})
test('zero active days stay zero with no active denominator; natural-day averages remain real zero', () => {
  const stats = dailyStatsFixture(query(), emptyDailyTotals())
  const active = accountDailySummary(stats), calendar = accountDailySummary(stats, 'calendar')
  assert.equal(active.activeDays, 0); assert.equal(active.averageRequests, null); assert.equal(active.averageTokens, null)
  assert.equal(calendar.averageRequests, 0); assert.equal(calendar.averageTokens, 0); assert.deepEqual(active.peakRequests.days, [])
})
test('partial token coverage remains partial, all equal peak days are retained, unknown token is a chart gap', () => {
  const stats = dailyStatsFixture(query(), totals())
  stats.series[0] = { ...stats.series[0], total_requests: 2, total_tokens: 500, usage_known_requests: 1, usage_unknown_requests: 1 }
  stats.series[1] = { ...stats.series[0], time: stats.series[1].time }
  stats.usage_unknown_requests = 2; stats.usage_known_requests = 2
  const summary = accountDailySummary(stats)
  assert.deepEqual(summary.peakRequests.days, ['2026-03-07', '2026-03-08'])
  assert.deepEqual(summary.peakTokens.days, ['2026-03-07', '2026-03-08']); assert.equal(summary.tokenPartial, true)
  const unknown = { ...stats.series[0], usage_known_requests: 0, usage_unknown_requests: 2 }
  assert.equal(dailyTrendValue(unknown, 'tokens').value, null)
  assert.equal(dailyTrendValue(emptyDailyTotals(), 'tokens').value, 0)
})
test('request-only API contracts need no monetary fields and retired metrics produce no value', () => {
  const q = query(), stats = dailyStatsFixture(q, totals())
  assert.equal(validateDailyStats(stats, q), stats)
  assert.equal(dailyTrendValue(stats.series[0], 'procurement').value, null)
  assert.equal(dailyTrendValue(stats.series[0], 'sale').value, null)
})
test('hourly/monthly, missing/duplicate days, wrong timezones and inconsistent totals are rejected', () => {
  const q = query(), stats = dailyStatsFixture(q, totals())
  for (const bad of [{ ...stats, step: 3600 }, { ...stats, step: 2592000 }, { ...stats, timezone: 'UTC' }, { ...stats, series: stats.series.slice(1) }, { ...stats, series: [stats.series[0], stats.series[0], stats.series[2]] }, { ...stats, total_requests: 9 }]) assert.throws(() => validateDailyStats(bad, q))
  assert.throws(() => validateDailyStats(stats, { ...q, timezone: '+08:00' }))
})
test('timezone midnight changes calendar attribution and half-open end does not add an extra day', () => {
  assert.equal(dailyDate(Date.parse('2026-03-08T01:00:00Z') / 1000, 'America/New_York'), '2026-03-07')
  assert.equal(dailyDate(Date.parse('2026-03-08T01:00:00Z') / 1000, 'Asia/Shanghai'), '2026-03-08')
  const q = query('2026-03-07T18:00:00-05:00', '2026-03-09T00:00:00-04:00'), stats = dailyStatsFixture(q, totals())
  validateDailyStats(stats, q); assert.equal(accountDailySummary(stats).calendarDays, 2)
})
