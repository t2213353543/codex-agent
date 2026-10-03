import test from 'node:test'
import assert from 'node:assert/strict'
import { formatDuration, formatNumber } from '../src/utils/format.js'

test('elapsed milliseconds preserve unknown, zero, fractions and full large quantities', () => {
  for (const value of [null, undefined, '', NaN, Infinity, -1, false]) assert.equal(formatDuration(value), '—')
  assert.equal(formatDuration(null, { empty: '无样本' }), '无样本')
  for (const [value, text] of [[0, '0 ms'], [.123, '0.123 ms'], [999.999, '999.999 ms'], [1000, '1 秒'], [1250, '1.25 秒'], [60000, '1 分'], [61234, '1 分 1.234 秒'], [3600100, '1 小时 0.1 秒'], [86461234, '1 天 1 分 1.234 秒']]) assert.equal(formatDuration(value), text)
  assert.equal(formatNumber(123456789), '123,456,789')
  assert.equal(formatNumber(2500.125), '2,500.125')
})
