import test from 'node:test'
import assert from 'node:assert/strict'
import { createSanitizedLogExport, sanitizedLogRecord } from '../src/utils/logExport.js'

test('log export never includes identifying fields, arbitrary backend fields, or raw errors', () => {
  const sensitive = '<img src=x onerror=alert(1)> Bearer secret-token@example.test'
  const output = createSanitizedLogExport([{
    time: 42, status: 'error', http_code: 401, endpoint: 'responses', latency_ms: 12,
    input_tokens: 0, output_tokens: 2, cached_tokens: 0, stream: false,
    account: sensitive, account_id: sensitive, key_id: sensitive, key_name: sensitive,
    groups: [sensitive], model: sensitive, error: sensitive, proxy_name: sensitive,
    ip: sensitive, access_token: sensitive, credentials: { access_token: sensitive },
  }])
  assert.equal(output.includes(sensitive), false)
  const row = JSON.parse(output.trim())
  assert.equal(row.has_error, true)
  assert.equal(row.input_tokens, 0)
  assert.equal(row.status, 'error')
  assert.deepEqual(Object.keys(row), ['time', 'status', 'model_consistency', 'http_code', 'endpoint', 'stream', 'websocket_warmup', 'latency_ms', 'input_tokens', 'output_tokens', 'cached_tokens', 'cache_write_tokens', 'image_input_tokens', 'image_output_tokens', 'image_input_tokens_source', 'image_output_tokens_source', 'authentication_ms', 'response_transfer_ms', 'official_runtime_ms', 'official_inference_ms', 'timing_source', 'has_error', 'error_severity', 'error_severity_source'])
})

test('log export remains valid JSONL, preserves zero, rejects non-finite values', () => {
  const output = createSanitizedLogExport([{ time: 0, status: 'ok' }, { time: 1, status: 'aborted' }])
  assert.equal(output.trim().split('\n').length, 2)
  const invalid = sanitizedLogRecord({ latency_ms: Infinity, input_tokens: -1, status: '<b>ok</b>', endpoint: 'secret-token' })
  assert.equal(invalid.latency_ms, null)
  assert.equal(invalid.input_tokens, null)
  assert.equal(invalid.status, 'unknown')
  assert.equal(invalid.endpoint, 'other')
  assert.equal(createSanitizedLogExport([]), '')
})


test('cache export keeps creation separate from observed reads and preserves unknown versus zero', () => {
  const row = sanitizedLogRecord({ usage_known: true, input_tokens: 100, cached_tokens: 0, cache_write_tokens: 80 })
  assert.equal(row.cached_tokens, null)
  assert.equal(row.cache_write_tokens, 80)
  assert.equal(sanitizedLogRecord({ cached_tokens: 0, cached_tokens_known: true, cache_write_tokens: 0 }).cached_tokens, 0)
  assert.equal(sanitizedLogRecord({ cache_write_tokens: 0 }).cache_write_tokens, 0)
  assert.equal(sanitizedLogRecord({}).cache_write_tokens, null)
})
