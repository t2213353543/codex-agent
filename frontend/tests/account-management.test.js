import test from 'node:test'
import assert from 'node:assert/strict'
import { accountStatusLabel, compactAccountFilters, normalizeAccountFilters, runAccountBatches, validateAccountDateRange, validateAccountSelection, accountSelectionTarget } from '../src/utils/accountManagement.js'

test('GET and JSON selections use the same typed filters, including false and Unix expiry', () => {
  const raw = { enabled: 'false', has_refresh_token: true, expires_after: '2026-09-17T00:00:00Z', group_id: 'g1', sort: 'name-desc', cursor: 'old-page' }
  const normalized = compactAccountFilters(raw)
  assert.deepEqual(normalized, { group_id: 'g1', enabled: false, has_refresh_token: true, expires_after: 1789603200, sort_by: 'name', sort_order: 'desc' })
  assert.deepEqual(compactAccountFilters(Object.fromEntries(new URLSearchParams(normalized))), normalized)
  assert.equal(normalizeAccountFilters({ expires_after: 1789603200 }).expires_after, '2026-09-17T00:00:00.000Z')
})
test('empty association filters are explicit and invalid date ranges stay invalid', () => {
  assert.equal(compactAccountFilters({ group_id: 'g1', ungrouped: true }).group_id, undefined)
  assert.equal(compactAccountFilters({ proxy_id: 'p1', no_proxy: true }).proxy_id, undefined)
  assert.match(validateAccountDateRange({ expires_after: '2026-09-18', expires_before: '2026-09-17' }), /结束/)
})
test('all-results selection refuses truncated, duplicate and missing-version snapshots', () => {
  const one = { id: 'a1', version: 2 }
  assert.deepEqual(validateAccountSelection({ items: [one], total: 1 }), [one])
  for (const result of [{ items: [one], total: 2 }, { items: [one, one], total: 2 }, { items: [{ id: 'a1', version: 0 }], total: 1 }]) assert.throws(() => validateAccountSelection(result))
})
test('update/delete batches freeze versions and never send unrelated fields', async () => {
  const source = Array.from({ length: 201 }, (_, index) => ({ id: `a${index}`, version: 3, name: `Account ${index}` }))
  const sent = []
  const results = await runAccountBatches({ items: source, action: 'update', patch: { groups: [], concurrency: 7 }, send: async payload => { sent.push(payload); source[0].version = 99; return { results: payload.items.map(item => ({ id: item.id, ok: true, version: 4 })) } } })
  assert.deepEqual(sent.map(payload => payload.items.length), [100, 100, 1])
  assert.equal(sent[0].items[0].version, 3)
  assert.deepEqual(sent[0].patch, { groups: [], concurrency: 7 })
  assert.equal(results.filter(result => result.state === 'success').length, 201)
})
test('refresh batches stop after current batch when user cancels', async () => {
  let stopped = false
  const results = await runAccountBatches({ items: Array.from({ length: 25 }, (_, id) => ({ id: String(id), version: 1 })), action: 'refresh', stopped: () => stopped, send: async payload => { assert.equal(payload.items.length, 10); stopped = true; return { results: payload.items.map(item => ({ id: item.id, ok: true })) } } })
  assert.equal(results.filter(result => result.state === 'success').length, 10)
  assert.equal(results.filter(result => result.state === 'pending').length, 15)
})
test('uncertain transport outcome stops subsequent mutations and is never called a failure to retry', async () => {
  let calls = 0
  const results = await runAccountBatches({ items: Array.from({ length: 101 }, (_, id) => ({ id: String(id), version: 1 })), action: 'delete', send: async () => { calls++; throw new Error('connection lost') } })
  assert.equal(calls, 1)
  assert.equal(results.filter(result => result.state === 'unknown').length, 100)
  assert.equal(results.filter(result => result.state === 'pending').length, 1)
})
test('partial result omission stops later batches; explicit conflicts remain visible', async () => {
  let calls = 0
  const results = await runAccountBatches({ items: Array.from({ length: 11 }, (_, id) => ({ id: String(id), version: 1 })), action: 'refresh', send: async () => { calls++; return { results: [{ id: '0', ok: false, code: 'version_conflict' }] } } })
  assert.equal(calls, 1)
  assert.equal(results[0].state, 'failed')
  assert.match(results[0].message, /版本/)
  assert.equal(results[1].state, 'unknown')
  assert.equal(results[10].state, 'pending')
})
test('export selection excludes UI data and filters never carry pagination or arbitrary properties', () => {
  assert.deepEqual(accountSelectionTarget([{ id: 'a1', version: 4, name: 'test', access_token: 'not-in-target' }], {}, 'selected'), { items: [{ id: 'a1', version: 4 }] })
  assert.deepEqual(accountSelectionTarget([], { page: 5, search: 'abc', sort_by: 'name', sort_order: 'asc', token: 'not-in-target' }, 'filtered'), { filters: { search: 'abc', sort_by: 'name', sort_order: 'asc' } })
})

test('stable ID and status sorting survive URL, selection and export normalization in both directions', () => {
  for (const sort_by of ['id', 'status']) for (const sort_order of ['asc', 'desc']) {
    const filters = { search: 'stable', enabled: false, status: 'unverified', sort_by, sort_order }
    const fromURL = normalizeAccountFilters(Object.fromEntries(new URLSearchParams(filters)))
    assert.deepEqual(compactAccountFilters(fromURL), filters)
    assert.deepEqual(accountSelectionTarget([], { ...fromURL, page: 9, page_size: 10 }, 'filtered'), { filters })
  }
})

test('account status labels reflect local summary states and never infer healthy from unknown', () => {
  assert.equal(accountStatusLabel('ok'), '最近状态正常')
  assert.equal(accountStatusLabel('error'), '请求或凭据异常')
  assert.equal(accountStatusLabel('disabled'), '已停用')
  assert.equal(accountStatusLabel('expired'), '令牌已过期')
  assert.equal(accountStatusLabel('unverified'), '尚未验证')
  for (const status of [undefined, null, '', 'active', 'future_state', '__proto__']) assert.equal(accountStatusLabel(status), '状态未知')
})
