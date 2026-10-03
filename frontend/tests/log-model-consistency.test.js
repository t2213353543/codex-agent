import test from 'node:test'
import assert from 'node:assert/strict'
import { logModelConsistency } from '../src/utils/logModelConsistency.js'
import { sanitizedLogRecord } from '../src/utils/logExport.js'

test('model validation compares actual sent and returned models, including aliases and versions', () => {
  assert.equal(logModelConsistency({ model: 'request-alias', sent_model: 'actual', returned_model: 'actual' }).status, 'match')
  for (const returned of ['other', 'actual-2026-09-27', 'ACTUAL']) {
    const result = logModelConsistency({ model: returned, sent_model: 'actual', returned_model: returned })
    assert.equal(result.status, 'mismatch')
    assert.equal(result.label, '模型不一致')
    assert.ok(result.tooltip.includes(`发送：actual\n返回：${returned}`))
  }
})

test('missing observations never inherit the requested model or a compatibility header', () => {
  for (const pair of [{}, { sent_model: 'actual' }, { returned_model: 'actual' }, { sent_model: 1, returned_model: 1 }, { sent_model: ' ', returned_model: ' ' }]) {
    const result = logModelConsistency({ model: 'actual', upstream_model: 'actual', upstream_model_source: 'header', model_consistency: 'match', ...pair })
    assert.equal(result.status, 'unknown')
    assert.equal(result.label, '未能校验')
  }
})

test('sanitized page exports retain validation status without exporting model names', () => {
  const result = sanitizedLogRecord({ model: 'private-alias', sent_model: 'private-sent', returned_model: 'private-returned' })
  assert.equal(result.model_consistency, 'mismatch')
  assert.equal(JSON.stringify(result).includes('private-'), false)
})

test('truncated prefixes and redacted model names never produce a false match', () => {
  for (const model of ['m'.repeat(512), '模型'.repeat(85), 'm'.repeat(509), '[REDACTED]']) {
    assert.equal(logModelConsistency({ sent_model: model, returned_model: model }).status, 'unknown')
  }
})
