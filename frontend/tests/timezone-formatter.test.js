import test from 'node:test'
import assert from 'node:assert/strict'
import { managedTimeParts, normalizeLogTimezone } from '../src/utils/logManagementState.js'
import { dailyDate, canonicalDailyTimezone } from '../src/utils/accountDailyStats.js'

test('interleaved log and daily formatters preserve timezone, midnight and both DST transitions', () => {
  const stamps = ['2024-03-10T06:59:59Z', '2024-03-10T07:00:00Z', '2024-11-03T05:59:59Z', '2024-11-03T06:00:00Z', '2024-01-01T15:59:59Z', '2024-01-01T16:00:00Z'].map(value => Date.parse(value) / 1000)
  const zones = ['UTC', 'America/New_York', 'Asia/Shanghai', 'Pacific/Chatham', 'Europe/Berlin']
  for (let round = 0; round < 3; round++) for (const zone of zones) for (const stamp of stamps) {
    const parts = Object.fromEntries(new Intl.DateTimeFormat('en-CA', { timeZone: zone, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' }).formatToParts(new Date(stamp * 1000)).map(part => [part.type, part.value]))
    const date = `${parts.year}-${parts.month}-${parts.day}`
    assert.equal(dailyDate(stamp, zone), date)
    assert.deepEqual(managedTimeParts(stamp, zone), { date, time: `${parts.hour}:${parts.minute}:${parts.second}` })
  }
  for (const zone of ['', 'Local', '+08:00', 'Invalid/Zone', null]) {
    assert.equal(canonicalDailyTimezone(zone), '')
    assert.equal(normalizeLogTimezone(zone), zone === '' ? '' : null)
  }
})
