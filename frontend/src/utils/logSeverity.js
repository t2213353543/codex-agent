export const logSeverityOptions = [['P1', 'P1'], ['P2', 'P2'], ['P3', 'P3'], ['unknown', '级别未知']]
export const managementSeveritySource = 'management_rules_v1'
export const validLogSeverity = value => logSeverityOptions.some(([key]) => key === value)

export function logSeverity(record) {
  if (!['error', 'aborted'].includes(record?.status)) return null
  if (record.error_severity_source !== managementSeveritySource) return { value: 'unknown', label: '级别未知', tone: 'unknown', source: '', explanation: '级别来源未验证，不能将返回值当作管理规则等级或上游原生分级。' }
  if (!validLogSeverity(record.error_severity)) return { value: 'unknown', label: '级别未知', tone: 'unknown', source: managementSeveritySource, explanation: '服务端未返回有效级别。来源：管理规则 v1；不表示上游原生分级。' }
  const value = record.error_severity
  return { value, label: value === 'unknown' ? '级别未知' : value, tone: value.toLowerCase(), source: managementSeveritySource, explanation: value === 'unknown' ? '当前记录不足以确定级别。来源：管理规则 v1（management_rules_v1）；不表示上游原生分级。' : `来源：管理规则 v1（management_rules_v1），依据已记录的错误类别和 HTTP 状态派生 ${value}；不是上游原生分级，不改变请求结果或用量。` }
}

export function exportedLogSeverity(record) {
  const result = logSeverity(record)
  return result ? { error_severity: result.value, error_severity_source: result.source || null } : {}
}

export function severityDistribution(stats) {
  if (stats?.error_severity_source !== managementSeveritySource) return { rows: [], note: '错误级别分布来源未验证，暂不展示分级或提供级别筛选。' }
  const rows = stats.error_severities
  if (!Array.isArray(rows) || rows.some(row => !validLogSeverity(row?.name))) return { rows: [], note: '错误级别分布格式无效，请刷新统计。' }
  return { rows, note: '仅统计失败与中断请求。来源：管理规则 v1，不代表上游原生分级。' }
}
