import test from 'node:test'
import assert from 'node:assert/strict'
import { officialUsageWindows, readOfficialAccountUsage, accountWindowCostLabel, accountWindowCostHint, accountWindowTotalEstimate, accountWindowTotalLabel } from '../src/utils/accountOfficialUsage.js'

const snapshot = (id, rate_limit, extra = {}) => ({ id, kind: 'usage', ok: true, checked_at: 2000000000, data: { plan_type: 'team', rate_limit, ...extra } })
test('Sub2API weekly total estimate divides account cost by official utilization without a plan cap', () => {
  const window = { seconds: 604800, used_percent: 25, reset_at: 2000000000, local_cost: { usd: 12.5, unknown_requests: 0 } }
  assert.equal(accountWindowTotalEstimate(window, 1900000000), 50)
  assert.equal(accountWindowTotalLabel(window), '总额度估算 $50.00')
  assert.equal(accountWindowTotalEstimate({ ...window, local_cost: { usd: 12.5, unknown_requests: 2 } }, 1900000000), 50)
  assert.equal(accountWindowTotalEstimate({ ...window, used_percent: 125 }, 1900000000), 10)
  for (const patch of [{ seconds: 18000 }, { used_percent: 0 }, { used_percent: null }, { used_percent: Infinity }, { local_cost: { usd: 0 } }, { local_cost: { usd: -1 } }, { local_cost: { usd: 12, error: 'query_failed' } }, { reset_at: 1800000000 }]) assert.equal(accountWindowTotalEstimate({ ...window, ...patch }, 1900000000), null)
})
test('window dollars keep local coverage separate from official percentages and unknown amounts', () => {
  const data = snapshot('a', { primary_window: { used_percent: 50 } })
  data.local_window_costs = { primary_window: { source: 'local_token_estimate', scope: 'retained_request_details', usd: .33, estimated_requests: 1, unknown_requests: 2 } }
  const window = officialUsageWindows(data)[0]
  assert.equal(window.used_percent, 50)
  assert.equal(accountWindowCostLabel(window.local_cost), '已知部分 $0.33')
  assert.match(accountWindowCostHint(window.local_cost), /不是官方扣费或余额/)
  assert.equal(accountWindowCostLabel({ usd: null }), '本地估算未取得')
  assert.equal(accountWindowCostLabel({ usd: 0 }), '本地估算 $0.00')
  assert.equal(accountWindowCostLabel({ usd: .000001 }), '本地估算 < $0.0001')
  data.local_window_costs.primary_window.source = 'official_percent_guess'
  assert.equal(officialUsageWindows(data)[0].local_cost, null)
})
test('official window durations define labels even for weekly primary slots and custom plans', () => {
  const rows = officialUsageWindows(snapshot('a', { primary_window: { limit_window_seconds: 604800, used_percent: 42, reset_after_seconds: 900 }, secondary_window: { limit_window_seconds: 7200, used_percent: 0, reset_at: 2000000200 } }))
  assert.deepEqual(rows.map(row => [row.label, row.used_percent, row.reset_at]), [['7 天', 42, 2000000900], ['2 小时', 0, 2000000200]])
})
test('missing official fields remain unknown and local quota cannot fill them', () => {
  const data = snapshot('a', { primary_window: {}, secondary_window: null })
  Object.assign(data, { quota_used: 123, quota: 456, window_days: 30 })
  assert.deepEqual(officialUsageWindows(data).map(row => [row.used_percent, row.progress, row.reset_at]), [[null, null, null]])
  assert.deepEqual(officialUsageWindows({ ...data, ok: false }), [])
  assert.deepEqual(officialUsageWindows({ ok: true, quota_used: 0, window_days: 30 }), [])
  assert.equal(officialUsageWindows(snapshot('a', { primary_window: { used_percent: '0', reset_at: 0 } }))[0].used_percent, null)
})
test('extra feature windows are independent and included only on request', () => {
  const data = snapshot('a', null, { additional_rate_limits: [{ limit_name: 'Spark', rate_limit: { primary_window: { limit_window_seconds: 18000, used_percent: 101 } } }] })
  assert.deepEqual(officialUsageWindows(data), [])
  assert.deepEqual(officialUsageWindows(data, { additional: true }).map(row => [row.label, row.used_percent, row.progress]), [['Spark · 5 小时', 101, 100]])
})
test('automatic official reads freeze versions, bound batch size/concurrency and report partial failures', async () => {
  const items = Array.from({ length: 23 }, (_, i) => ({ id: String(i), version: 3, quota_used: 42 }))
  const sent = [], results = new Map()
  let active = 0, max = 0
  await readOfficialAccountUsage(items, { force: true, onResult: (id, row) => results.set(id, row), send: async payload => {
    sent.push(payload); active++; max = Math.max(max, active)
    items[22].version = 99
    await new Promise(resolve => setTimeout(resolve, 1)); active--
    return { results: payload.items.map(row => row.id === '12' ? { id: row.id, ok: false, code: 'version_conflict' } : { id: row.id, ok: true, snapshot: snapshot(row.id, {}) }) }
  } })
  assert.deepEqual(sent.map(row => row.items.length), [10, 10, 3])
  assert.equal(max, 2)
  assert.ok(sent.every(row => row.kind === 'usage' && row.force && row.items.every(item => item.version === 3 && item.quota_used === undefined)))
  assert.equal(results.get('12').error, 'version_conflict')
  assert.equal(results.get('12').code, 'version_conflict')
  assert.equal(results.size, 23)
})
test('wrong-account snapshots are rejected and canceled pages do not receive old results', async () => {
  const results = []
  await readOfficialAccountUsage([{ id: 'a', version: 1 }], { onResult: (_, row) => results.push(row), send: async () => ({ results: [{ id: 'a', ok: true, snapshot: snapshot('other', {}) }] }) })
  assert.match(results[0].error, /不完整/)
  const controller = new AbortController()
  await readOfficialAccountUsage([{ id: 'a', version: 1 }], { signal: controller.signal, onResult: () => assert.fail('aborted page received data'), send: async () => { controller.abort(); return { results: [{ id: 'a', ok: true, snapshot: snapshot('a', {}) }] } } })
})
