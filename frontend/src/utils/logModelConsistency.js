// Compare only the observed wire model pair. Request aliases and model headers
// cannot prove which model the response body actually reported.
export function logModelConsistency(record) {
  const model = value => typeof value === 'string' && value.trim() ? value : ''
  const sent = model(record?.sent_model)
  const returned = model(record?.returned_model)
  // The 512-byte collector backs up at most 3 bytes for UTF-8 boundaries.
  // Boundary-length historical values may therefore be truncated prefixes.
  const incomplete = [sent, returned].some(value => new TextEncoder().encode(value).length >= 509 || value.includes('[REDACTED]'))
  const status = !sent || !returned || incomplete ? 'unknown' : sent === returned ? 'match' : 'mismatch'
  const label = { match: '模型一致', mismatch: '模型不一致', unknown: '未能校验' }[status]
  const reason = status === 'unknown' ? incomplete ? '模型记录可能被截断或脱敏，无法校验完整名称。' : '缺少实际发送或响应返回模型，无法校验。' : status === 'match' ? '实际发送与响应返回的模型名称完全一致。' : '实际发送与响应返回的模型名称不同。'
  return { status, label, sent, returned, tooltip: `发送：${sent || '未记录'}\n返回：${returned || '未记录'}\n${reason}` }
}
