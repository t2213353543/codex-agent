import test from 'node:test'
import assert from 'node:assert/strict'
import { rankUsageRecords, usageRankMetrics } from '../src/utils/logMetrics.js'
import { createSanitizedLogExport } from '../src/utils/logExport.js'
import { readManagedFilters, buildManagedQuery } from '../src/utils/logManagementState.js'

test('operational ranking only accepts request and token metrics, preserving zero and unknown', () => {
  assert.deepEqual(usageRankMetrics, ['total_tokens', 'total_requests', 'total_input_tokens', 'total_output_tokens', 'total_cached_tokens', 'total_cache_write_tokens', 'success_rate'])
  const rows = [{ id: 'unknown', total_tokens: null, estimated_cost_usd: '999' }, { id: 'zero', total_tokens: 0, total_requests: 0, success: 0 }, { id: 'known', total_tokens: 20, total_requests: 2, success: 1 }]
  assert.deepEqual(rankUsageRecords(rows, 'total_tokens').map(row => row.id), ['known', 'zero', 'unknown'])
  assert.deepEqual(rankUsageRecords(rows, 'estimated_cost_usd').map(row => row.id), ['known', 'zero', 'unknown'])
  assert.deepEqual(rankUsageRecords(rows, 'success_rate').map(row => row.id), ['known', 'unknown', 'zero'])
})
test('legacy monetary fields cannot enter the current-page operational export', () => {
  const exported = JSON.parse(createSanitizedLogExport([{ time: 1, status: 'ok', input_tokens: 0, output_tokens: 0, cost: { amount_usd: '4' }, account_cost: { amount_usd: '5' }, sale_cost: { amount_usd: '6' }, estimated_cost_usd: '4', account_cost_usd: '5', sale_cost_usd: '6', pricing_source: 'legacy source' }]))
  assert.equal(exported.input_tokens, 0)
  assert.ok(Object.keys(exported).every(key => !/cost|price|sale|usd/.test(key)))
  assert.equal(JSON.stringify(exported).includes('legacy source'), false)
})
test('billing query controls are not accepted by list, URL or saved filter normalization', () => {
  const normalized = readManagedFilters({ account_id: 'account-1', pricing_id: 'old', sale_policy_id: 'old', sort_by: 'account_cost_usd' })
  assert.equal(normalized.sort_by, 'time')
  const query = buildManagedQuery(normalized)
  assert.equal(query.account_id, 'account-1')
  assert.equal(query.pricing_id, undefined)
  assert.equal(query.sale_policy_id, undefined)
})
