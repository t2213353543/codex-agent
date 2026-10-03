import test from 'node:test'
import assert from 'node:assert/strict'
import { auditQuery, auditRequestParams, auditRangeError, validAuditPage, diagnosticExport, auditExport } from '../src/utils/observability.js'

test('audit queries reject array pollution and bound cursor/filter values', () => {
  const result = auditQuery({ action: ['account.delete'], entity_id: 'a'.repeat(300), limit: '999', from: '-4', to: '1700000000', cursor: 'b'.repeat(5000) })
  assert.equal(result.action, '')
  assert.equal(result.entity_id.length, 256)
  assert.equal(result.limit, 50)
  assert.equal(result.from, '')
  assert.equal(result.to, '1700000000')
  assert.equal(result.cursor.length, 4096)
  assert.equal(auditRangeError({ from: '20', to: '10' }), '开始时间必须早于结束时间。')
  assert.equal(auditRangeError({ from: '10', to: '20' }), '')
  assert.deepEqual(auditRequestParams({ limit: 20, action: '', from: null, cursor: 'opaque' }), { limit: '20', cursor: 'opaque' })
})
test('audit pagination requires an explicit complete contract, including a continuation cursor', () => {
  const page = { supported: true, items: [{ id: '1', created_at: 1700000000, action: 'account.update' }], has_more: false, next_cursor: '' }
  assert.equal(validAuditPage(page), true)
  assert.equal(validAuditPage({ ...page, has_more: true }), false)
  assert.equal(validAuditPage({ items: [] }), false)
  assert.equal(validAuditPage({ ...page, items: [{ id: 999, action: 'x' }] }), false)
  assert.equal(validAuditPage({ ...page, supported: false, items: [] }), true)
})
test('diagnostic export cannot include credentials, dependency addresses or arbitrary errors', () => {
  const result = diagnosticExport({ version: '0.2.0', status: 'degraded', storage_mode: 'postgres', checked_at: 5, uptime_seconds: 4,
    secret: 'private-marker', dependencies: { postgres: { status: 'unavailable', configured: true, latency_ms: 2, dsn: 'private-marker', error: 'private-marker' }, redis: { status: 'private-marker', latency_ms: -1 } } })
  assert.ok(!JSON.stringify(result).includes('private-marker'))
  assert.equal(result.dependencies.redis.status, 'unknown')
  assert.equal(result.dependencies.redis.latency_ms, null)
  assert.equal(result.dependencies.postgres.status, 'unavailable')
})
test('audit export includes only known operational actions and outcomes without identity fields', () => {
  const result = auditExport([{ id: 'private-marker', entity_id: 'private-marker', actor: 'private-marker', created_at: 20, action: 'account.update', result: 'ok' },
    { created_at: -1, action: 'private-marker', result: 'private-marker' }])
  assert.ok(!JSON.stringify(result).includes('private-marker'))
  assert.deepEqual(result, [{ created_at: 20, action: 'account.update', result: 'ok' }, { created_at: null, action: 'other', result: 'unknown' }])
})
