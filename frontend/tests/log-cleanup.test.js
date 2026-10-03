import test from 'node:test'
import assert from 'node:assert/strict'
import { cleanupCanCancel, cleanupProgress, cleanupRangeError, cleanupSnapshot, validCleanupJob, validCleanupPreview } from '../src/utils/logCleanupState.js'

test('cleanup requires an explicit bounded past range, never implicit all-time deletion', () => {
  for (const filters of [{}, { from: '1' }, { from: '20', to: '20' }, { from: '21', to: '20' }, { from: '0', to: '20' }, { from: '1', to: '101' }]) assert.ok(cleanupRangeError(filters, 100))
  assert.equal(cleanupRangeError({ from: '1', to: '100' }, 100), '')
})

test('cleanup freezes all applied filters while removing pagination and unknown fields', () => {
  const applied = { key_id: 'key-1', request_type: 'websocket', http_code: '200', service_tier: 'priority', cursor: 'page-2', password: 'secret' }
  const snapshot = cleanupSnapshot(applied, '10', '20')
  applied.key_id = 'changed'
  assert.equal(snapshot.key_id, 'key-1')
  assert.equal(snapshot.request_type, 'websocket')
  assert.equal(snapshot.service_tier, 'priority')
  assert.equal(snapshot.http_code, '200')
  assert.equal(snapshot.from, '10')
  assert.ok(Object.isFrozen(snapshot))
  assert.equal('cursor' in snapshot, false)
  assert.equal('exact_total' in snapshot, false)
  assert.equal('sort_by' in snapshot, false)
  assert.equal('password' in snapshot, false)
  const scope = { from: '10', to: '20', as_of: 'usage_fixed', timezone: 'Asia/Shanghai', step: '86400' }
  assert.equal(cleanupSnapshot(scope, '10', '20').as_of, 'usage_fixed')
  assert.equal('as_of' in cleanupSnapshot(scope, '11', '20'), false)
  assert.equal(cleanupSnapshot(scope, '10', '20').timezone, 'Asia/Shanghai')
})

test('preview confirmation requires signed identity, correct scope and a live expiry', () => {
  const valid = { preview_id: 'p1', confirmation_token: 'confirm', total: 17, scope: 'retained_request_details', expires_at: 101 }
  assert.equal(validCleanupPreview(valid, 100), true)
  for (const delta of [{ expires_at: 100 }, { total: -1 }, { total: 1.5 }, { scope: 'all_accounting' }, { confirmation_token: '' }, { preview_id: '' }]) assert.equal(validCleanupPreview({ ...valid, ...delta }, 100), false)
})

test('job outcomes never manufacture full progress and only active jobs can be cancelled', () => {
  assert.deepEqual(cleanupProgress({ status: 'failed', total: 100, deleted_rows: 25, remaining_rows: 75 }), { total: 100, deleted: 25, remaining: 75, percentage: 25 })
  assert.equal(cleanupProgress({ status: 'succeeded', total: 100, deleted_rows: 60 }).percentage, 60)
  assert.equal(cleanupProgress({ status: 'succeeded', total: 0, deleted_rows: 0 }).percentage, null)
  assert.equal(cleanupCanCancel({ status: 'running' }), true)
  assert.equal(cleanupCanCancel({ status: 'pending' }), true)
  for (const status of ['failed', 'succeeded', 'canceled', 'unknown']) assert.equal(cleanupCanCancel({ status }), false)
  assert.equal(validCleanupJob({ id: 'j1', scope: 'retained_request_details', status: 'running' }), true)
  assert.equal(validCleanupJob({ id: 'j1', scope: 'retained_request_details', status: 'unexpected' }), false)
})
