import { exportedLogSeverity } from './logSeverity.js'
import { imageTokenObservation } from './logPassiveObservation.js'
import { logModelConsistency } from './logModelConsistency.js'

// Export only non-identifying operational fields. Raw errors, account/key names,
// proxy names, IPs, groups and IDs are deliberately excluded: the legacy API does
// not guarantee those strings have already been redacted.
export function sanitizedLogRecord(record) {
  const numeric = (value) => value !== null && value !== undefined && value !== '' && Number.isFinite(Number(value)) && Number(value) >= 0 ? Number(value) : null
  const status = ['ok', 'error', 'aborted'].includes(record.status) ? record.status : 'unknown'
  const imageInput = imageTokenObservation(record, 'image_input_tokens', 'image_input_tokens_source')
  const imageOutput = imageTokenObservation(record, 'image_output_tokens', 'image_output_tokens_source')
  const observedTiming = value => typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null
  return {
    time: numeric(record.time),
    status,
    model_consistency: logModelConsistency(record).status,
    http_code: numeric(record.http_code),
    endpoint: ['responses', 'compact'].includes(record.endpoint) ? record.endpoint : 'other',
    stream: record.stream === true,
    websocket_warmup: typeof record.websocket_warmup === 'boolean' ? record.websocket_warmup : null,
    latency_ms: numeric(record.latency_ms),
    input_tokens: numeric(record.input_tokens),
    output_tokens: numeric(record.output_tokens),
    cached_tokens: record.usage_known !== false && (record.cached_tokens_known === true || record.cached_tokens_known !== false && Number(record.cached_tokens) > 0) ? numeric(record.cached_tokens) : null,
    cache_write_tokens: numeric(record.cache_write_tokens),
    image_input_tokens: imageInput.value,
    image_output_tokens: imageOutput.value,
    image_input_tokens_source: imageInput.source || null,
    image_output_tokens_source: imageOutput.source || null,
    authentication_ms: observedTiming(record.authentication_ms),
    response_transfer_ms: observedTiming(record.response_transfer_ms),
    official_runtime_ms: observedTiming(record.official_runtime_ms),
    official_inference_ms: observedTiming(record.official_inference_ms),
    timing_source: record.timing_source === 'gateway_handler_v1' ? record.timing_source : null,
    has_error: Boolean(record.error),
    ...exportedLogSeverity(record),
  }
}

export function createSanitizedLogExport(records) {
  return records.map((record) => JSON.stringify(sanitizedLogRecord(record))).join('\n') + (records.length ? '\n' : '')
}
