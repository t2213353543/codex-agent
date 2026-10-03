export const clientRequestIDValid = value => typeof value === 'string' && value.length <= 128 && /^[A-Za-z0-9._:-]*$/.test(value)
export const passiveTimingSource = source => source === 'gateway_handler_v1' ? '网关处理器边界观测（gateway_handler_v1）' : '计时来源未记录或无法验证'

export function clientRequestIdentity(record = {}) {
  const value = clientRequestIDValid(record.client_request_id) && record.client_request_id ? record.client_request_id : ''
  const trusted = record.client_request_id_source === 'request_header'
  const scope = trusted && ['request', 'connection'].includes(record.client_request_id_scope) ? record.client_request_id_scope : ''
  return { value, source: trusted ? '入站 X-Client-Request-ID' : '来源未记录或无法验证', scope, explanation: scope === 'connection' ? '来自 WebSocket 握手；连接内多个轮次可能共用，不是逐轮响应 ID。' : scope === 'request' ? '来自本次 HTTP 请求头；不是上游请求 ID 或本地记录 ID。' : '关联范围未记录，不推断为独立请求或连接标识。' }
}

export const imageTokenFields = [
  ['image_input_tokens', 'image_input_tokens_source', '图片输入 Token'],
  ['image_output_tokens', 'image_output_tokens_source', '图片输出 Token'],
]
export const imageTokenTotals = [
  ['image_input_samples', 'total_image_input_tokens', '图片输入 Token', 'image_input_total_overflow'],
  ['image_output_samples', 'total_image_output_tokens', '图片输出 Token', 'image_output_total_overflow'],
]
export const imageTokenPrecisionExceeded = value => typeof value === 'number' && Number.isFinite(value) && value > Number.MAX_SAFE_INTEGER
const imageSources = new Set([
  'usage.input_tokens_details.image_tokens', 'usage.output_tokens_details.image_tokens',
  'usage.prompt_tokens_details.image_tokens', 'usage.completion_tokens_details.image_tokens',
  'tool_usage.image_gen.input_tokens_details.image_tokens', 'tool_usage.image_gen.output_tokens_details.image_tokens',
])
export function imageTokenObservation(record, field, sourceField) {
  const raw = record?.[field], value = Number.isSafeInteger(raw) && raw >= 0 ? raw : null
  const source = imageSources.has(record?.[sourceField]) ? record[sourceField] : ''
  return { value, source, precisionExceeded: imageTokenPrecisionExceeded(raw), explanation: !source ? '来源未记录或无法验证，不推断图像与总量的关系。' : source.startsWith('tool_usage.') ? '图像工具报告的独立明细；不假定包含在模型输入或输出内，不再累加到总 Token。' : '模型用量中的图像分解；不再累加到输入、输出或总 Token。' }
}
export function imageTokenTotal(stats, samples, field) {
  return Number.isSafeInteger(stats?.[samples]) && stats[samples] > 0 && Number.isSafeInteger(stats?.[field]) && stats[field] >= 0 ? stats[field] : null
}
