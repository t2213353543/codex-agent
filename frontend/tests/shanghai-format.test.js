import test from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import { formatNumber, formatTime, parseShanghaiDateInput, shanghaiDayStart } from '../src/utils/format.js'

test('Shanghai display, filter round-trips and day boundaries ignore the host timezone', () => {
  const formatURL = new URL('../src/utils/format.js', import.meta.url).href
  const logURL = new URL('../src/utils/logQueryState.js', import.meta.url).href
  const accountURL = new URL('../src/utils/accountManagement.js', import.meta.url).href
  const script = `
    import { formatTime, formatDate, shanghaiDayStart } from ${JSON.stringify(formatURL)};
    import { logTimeParts, logLocalDateInput, logTimestampInput } from ${JSON.stringify(logURL)};
    import { localAccountDate, compactAccountFilters } from ${JSON.stringify(accountURL)};
    const stamp = Date.parse('2026-09-26T16:00:00Z') / 1000;
    console.log(JSON.stringify({
      display: formatTime(stamp), date: formatDate(stamp), parts: logTimeParts(stamp),
      input: logLocalDateInput(String(stamp)), timestamp: logTimestampInput('2026-09-27T00:00'),
      start: shanghaiDayStart(stamp + 3600), account: localAccountDate('2026-09-26T16:00:00Z'),
      filter: compactAccountFilters({ expires_after: '2026-09-27T00:00' }).expires_after
    }));`
  const stamp = Date.parse('2026-09-26T16:00:00Z') / 1000
  for (const TZ of ['UTC', 'America/Los_Angeles', 'Asia/Shanghai']) {
    const result = JSON.parse(execFileSync(process.execPath, ['--input-type=module', '-e', script], { env: { ...process.env, TZ }, encoding: 'utf8' }))
    assert.deepEqual(result, {
      display: '2026-09-27 00:00:00', date: '2026-09-27', parts: { date: '2026-09-27', time: '00:00:00' },
      input: '2026-09-27T00:00', timestamp: String(stamp), start: stamp, account: '2026-09-27T00:00', filter: stamp,
    }, TZ)
  }
})

test('date parsing preserves explicit offsets and rejects invalid wall times', () => {
  assert.equal(formatTime('2026-09-27T00:00:00Z'), '2026-09-27 08:00:00')
  assert.equal(formatTime('2026-09-27T00:00:00+08:00'), '2026-09-27 00:00:00')
  assert.equal(formatTime('2026-09-27 00:00:00'), '2026-09-27 00:00:00')
  for (const value of [null, undefined, '', 'invalid']) assert.equal(formatTime(value), '—')
  for (const value of ['2026-02-30T09:00', '2026-09-27T24:00', '', null]) assert.ok(Number.isNaN(parseShanghaiDateInput(value)))
  assert.equal(shanghaiDayStart(Date.parse('2026-12-31T23:59:59Z') / 1000), Date.parse('2026-12-31T16:00:00Z') / 1000)
})

test('large usage numbers retain full quantities without changing missing values', () => {
  for (const [value, expected] of [[0, '0'], [999, '999'], [1200, '1,200'], [1234567, '1,234,567'], [2500000000, '2,500,000,000'], [1000000000000, '1,000,000,000,000'], [null, '—'], [NaN, '—']]) assert.equal(formatNumber(value), expected)
})
