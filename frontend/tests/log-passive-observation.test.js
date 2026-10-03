import test from 'node:test'
import assert from 'node:assert/strict'
import { clientRequestIDValid, clientRequestIdentity, imageTokenObservation, imageTokenTotal, passiveTimingSource } from '../src/utils/logPassiveObservation.js'
import { managedQuerySnapshot, managedExactFilterError, managementScopeParams, readManagedFilters } from '../src/utils/logManagementState.js'
import { cleanupSnapshot } from '../src/utils/logCleanupState.js'
import { observationStageAverage, observationStages, observationStageTotals } from '../src/utils/logObservationState.js'
import { sanitizedLogRecord } from '../src/utils/logExport.js'

test('client association uses only a validated explicit caller ID and preserves independent query identity', () => {
  for (const id of ['client.id_a:2-z', 'c'.repeat(128)]) {
    assert.equal(clientRequestIDValid(id), true)
    const snapshot = managedQuerySnapshot({ client_request_id: id, request_id: 'upstream-different', from: '100', to: '200' })
    assert.equal(snapshot.params.client_request_id, id)
    assert.equal(snapshot.params.request_id, 'upstream-different')
    assert.equal(managementScopeParams(snapshot.params).client_request_id, id)
    assert.equal(cleanupSnapshot(snapshot.params, '100', '200').client_request_id, id)
    assert.equal(readManagedFilters(Object.fromEntries(new URLSearchParams(snapshot.params))).client_request_id, id)
  }
  for (const value of ['c'.repeat(129), '中文', 'space id', 'https://host', 'new\nline']) {
    assert.equal(clientRequestIDValid(value), false)
    const filters = readManagedFilters({ client_request_id: value })
    assert.equal(filters.client_request_id, value)
    assert.match(managedExactFilterError(filters), /128/)
  }
  assert.equal(clientRequestIdentity({ request_id: 'upstream', event_id: 'local' }).value, '')
  assert.match(clientRequestIdentity({ client_request_id: 'ws-id', client_request_id_source: 'request_header', client_request_id_scope: 'connection' }).explanation, /多个轮次可能共用/)
  assert.equal(clientRequestIdentity({ client_request_id: 'http-id', client_request_id_source: 'request_header', client_request_id_scope: 'request' }).scope, 'request')
  assert.equal(clientRequestIdentity({ client_request_id: 'id', client_request_id_source: 'guessed', client_request_id_scope: 'request' }).scope, '')
})

test('image token zero, unknown and model or tool sources remain distinct without changing total tokens', () => {
  const model = { input_tokens: 100, output_tokens: 20, image_input_tokens: 0, image_input_tokens_source: 'usage.input_tokens_details.image_tokens' }
  assert.equal(imageTokenObservation(model, 'image_input_tokens', 'image_input_tokens_source').value, 0)
  assert.match(imageTokenObservation(model, 'image_input_tokens', 'image_input_tokens_source').explanation, /模型用量/)
  for (const value of [null, undefined, '', '0', -1, false, Infinity]) assert.equal(imageTokenObservation({ image_input_tokens: value }, 'image_input_tokens', 'image_input_tokens_source').value, null)
  const tool = { image_output_tokens: 120, image_output_tokens_source: 'tool_usage.image_gen.output_tokens_details.image_tokens' }
  assert.match(imageTokenObservation(tool, 'image_output_tokens', 'image_output_tokens_source').explanation, /不假定包含/)
  assert.equal(imageTokenObservation({ ...tool, image_output_tokens_source: '<script>secret</script>' }, 'image_output_tokens', 'image_output_tokens_source').source, '')
  assert.equal(imageTokenObservation({ image_input_tokens: Number.MAX_SAFE_INTEGER + 1 }, 'image_input_tokens', 'image_input_tokens_source').precisionExceeded, true)
  assert.equal(imageTokenObservation({ image_input_tokens: Number.MAX_SAFE_INTEGER + 1 }, 'image_input_tokens', 'image_input_tokens_source').value, null)
  const result = sanitizedLogRecord({ ...model, ...tool, client_request_id: 'private-id', timing_source: 'raw-secret', authentication_ms: 0, response_transfer_ms: null })
  assert.equal(result.input_tokens, 100); assert.equal(result.output_tokens, 20)
  assert.equal(result.image_input_tokens, 0); assert.equal(result.image_output_tokens, 120)
  assert.equal(result.authentication_ms, 0); assert.equal(result.response_transfer_ms, null)
  assert.equal(result.timing_source, null); assert.equal('client_request_id' in result, false)
})

test('image and timing aggregates require actual samples, preserve true zero and avoid synthetic stage sums', () => {
  assert.equal(imageTokenTotal({ samples: 0, total: 0 }, 'samples', 'total'), null)
  assert.equal(imageTokenTotal({ samples: 1, total: 0 }, 'samples', 'total'), 0)
  assert.equal(imageTokenTotal({ samples: 2, total: 140 }, 'samples', 'total'), 140)
  assert.equal(imageTokenTotal({ samples: 2, total: null }, 'samples', 'total'), null)
  for (const [samples, average] of observationStageTotals.filter(([key]) => ['authentication_samples', 'response_transfer_samples'].includes(key))) {
    assert.equal(observationStageAverage({ [samples]: 1, [average]: 0 }, samples, average), 0)
    assert.equal(observationStageAverage({ [samples]: 0, [average]: 0 }, samples, average), null)
  }
  assert.ok(observationStages.some(([field]) => field === 'authentication_ms'))
  assert.ok(observationStages.some(([field]) => field === 'response_transfer_ms'))
  assert.match(passiveTimingSource('gateway_handler_v1'), /网关处理器/)
  assert.match(passiveTimingSource('other'), /无法验证/)
})
