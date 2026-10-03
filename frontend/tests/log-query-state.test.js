import test from 'node:test'
import assert from 'node:assert/strict'
import { buildLogQuery, emptyLogFilters, logExportAllowed, logFilterSignature, logLatency, logLocalDateInput, logNavigationAllowed, logRangeError, logTimestampInput, logTokenCount, readLogFilters, validLogPage } from '../src/utils/logQueryState.js'

test('log filters use a fixed allowlist and normalize malformed URL values', () => {
  assert.deepEqual(readLogFilters({ account: ['one', 'two'], status: 'failed', from: 'now', to: '-2', password: 'secret', model: 'gpt-5' }), { ...emptyLogFilters(), model: 'gpt-5' })
  assert.equal(readLogFilters({ account: 'a'.repeat(1000) }).account.length, 500)
  assert.deepEqual(buildLogQuery({ status: 'error', account: 'A', unknown: 'secret' }, 50), { status: 'error', account: 'A', page_size: '50' })
})

test('filter signatures ignore key order but detect new criteria and page size', () => {
  const signature = logFilterSignature({ model: 'gpt-5', account: 'A' }, 20)
  assert.equal(signature, logFilterSignature({ account: 'A', model: 'gpt-5' }, 20))
  assert.notEqual(signature, logFilterSignature({ account: 'B', model: 'gpt-5' }, 20))
  assert.notEqual(signature, logFilterSignature({ account: 'A', model: 'gpt-5' }, 50))
})

test('draft changes, loading and failed queries block both paging and export', () => {
  const ready = { loading: false, dirty: false, error: '', currentResult: true }
  assert.equal(logNavigationAllowed(ready), true)
  assert.equal(logExportAllowed(ready, [{ status: 'ok' }]), true)
  for (const state of [{ ...ready, dirty: true }, { ...ready, loading: true }, { ...ready, error: 'failed' }, { ...ready, currentResult: false }]) {
    assert.equal(logNavigationAllowed(state), false)
    assert.equal(logExportAllowed(state, [{ status: 'ok' }]), false)
  }
  assert.equal(logExportAllowed(ready, []), false)
})

test('new filters create a first-page request with no old cursor', () => {
  const first = buildLogQuery({ account: 'new account' }, 20)
  assert.deepEqual(first, { account: 'new account', page_size: '20' })
  assert.deepEqual(buildLogQuery({ account: 'new account' }, 20, 'new-cursor', 2), { account: 'new account', page_size: '20', cursor: 'new-cursor', page: '2' })
})

test('invalid range and malformed success responses are rejected', () => {
  assert.ok(logRangeError({ from: '100', to: '100' }))
  assert.ok(logRangeError({ from: '101', to: '100' }))
  assert.equal(logRangeError({ from: '99', to: '100' }), '')
  assert.equal(validLogPage({ items: [{}], has_more: false, next_cursor: '' }), true)
  assert.equal(validLogPage({ items: [{}], total: 2 }), true)
  for (const page of [{}, { items: null }, { items: [null] }, { items: [], has_more: 'true' }, { items: [], has_more: true, next_cursor: '' }]) assert.equal(validLogPage(page), false)
})

test('table number formatting preserves measured zero and distinguishes unknown', () => {
  assert.equal(logTokenCount(12000), '12,000')
  assert.equal(logTokenCount(0), '0')
  assert.equal(logTokenCount(undefined), '—')
  assert.equal(logTokenCount(-1), '—')
  assert.equal(logLatency(0), '0 ms')
  assert.equal(logLatency(345), '345 ms')
  assert.equal(logLatency(1250), '1.25 秒')
  assert.equal(logLatency(undefined), '—')
})

test('native local datetime fields round-trip in Shanghai timezone and reject invalid calendar dates', () => {
  const input = '2026-09-17T20:30'
  const timestamp = logTimestampInput(input)
  assert.equal(timestamp, String(Date.parse('2026-09-17T20:30:00+08:00') / 1000))
  assert.equal(logLocalDateInput(timestamp), input)
  assert.equal(logTimestampInput(''), '')
  assert.equal(logLocalDateInput(''), '')
  for (const invalid of ['2026-02-30T09:00', '2026-09-17T25:00', '2026-09-17', 'now', '2026-09-17T20:30Z']) assert.equal(logTimestampInput(invalid), '')
})
