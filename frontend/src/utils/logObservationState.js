export const observedPathOptions = ['/responses', '/responses/compact', '/models', '/oauth/token', '/guardian', '/guardian-classifier', '/attachments']
export const observationFilterFields = [['sent_model', '发送模型'], ['returned_model', '返回模型'], ['inbound_path', '入站标准路径'], ['upstream_path', '上游标准路径']]
export const observationBooleanFields = [['sent_model_mismatch', '请求与发送模型'], ['returned_model_mismatch', '发送与返回模型'], ['error_truncated', '错误截断'], ['websocket_warmup', 'WebSocket 预热']]
export const observationBooleanOptions = key => key === 'websocket_warmup' ? ['已观测为预热', '已观测为普通生成'] : key === 'error_truncated' ? ['已观测为截断', '已观测为未截断'] : ['已观测模型不同', '已观测模型一致']
export const websocketWarmupLabel = value => value === true ? '已观测为预热' : value === false ? '已观测为普通生成' : '预热状态未记录'
export const observationStages = [['authentication_ms', '网关授权'], ['request_preparation_ms', '请求准备'], ['account_selection_ms', '账号选择'], ['proxy_selection_ms', '代理选择'], ['official_runtime_ms', '官方运行时准备'], ['official_inference_ms', '官方推理及输出'], ['response_transfer_ms', '响应传递']]
export const observationStageTotals = [['authentication_samples', 'average_authentication_ms', '网关授权'], ['request_preparation_samples', 'average_request_preparation_ms', '请求准备'], ['account_selection_samples', 'average_account_selection_ms', '账号选择'], ['proxy_selection_samples', 'average_proxy_selection_ms', '代理选择'], ['response_transfer_samples', 'average_response_transfer_ms', '响应传递']]
export const observedNumber = value => typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null
export const observedBoolean = value => typeof value === 'boolean' ? value : null
export function observedText(value) { return typeof value === 'string' && value ? value : '' }
export function reasoningRoute(record) {
  const requested = observedText(record?.requested_reasoning_effort)
  const sent = observedText(record?.sent_reasoning_effort)
  const observed = observedText(record?.reasoning_effort)
  const returned = record?.reasoning_effort_source === 'response' ? observed : ''
  return {
    requested, sent, returned,
    label: requested && sent && requested !== sent ? `${requested} → ${sent}` : sent || observed || requested,
    path: `${requested || '未记录'} → ${sent || '未记录'} → ${returned || '未记录'}`,
    title: `推理档位 · 请求：${requested || '未记录'} · 发送：${sent || '未记录'} · 响应：${returned || '未记录'}`,
  }
}
export function modelComparison(left, right) { return observedText(left) && observedText(right) ? left === right ? '一致' : '不同' : '未记录完整模型对' }
export function observationVersion(row) { return Number.isSafeInteger(row?.observation_schema_version) && row.observation_schema_version > 0 ? row.observation_schema_version : null }
export const observedHTTPStatus = value => Number.isSafeInteger(value) && value >= 100 && value <= 599 ? value : null
export function observationAttempts(row) {
  const raw = Array.isArray(row?.attempts) ? row.attempts : []
  return raw.slice(0, 4).map(item => ({
    attempt: Number.isSafeInteger(item?.attempt) && item.attempt > 0 ? item.attempt : null,
    phase: observedText(item?.phase), endpoint: observedPathOptions.includes(item?.endpoint) ? item.endpoint : '',
    status_code: observedHTTPStatus(item?.status_code),
    observed_http_status_code: observedHTTPStatus(item?.observed_http_status_code),
    duration_ms: observedNumber(item?.duration_ms), header_ms: observedNumber(item?.header_ms),
    error_source: observedText(item?.error_source), error_code: observedText(item?.error_code),
  }))
}
export function attemptPhase(value) { return ({ request: 'HTTP 请求派发', refresh: 'OAuth 刷新', websocket: 'WebSocket 握手' })[value] || '未记录阶段' }
export function attemptStatusLabel(attempt) {
  const logical = observedHTTPStatus(attempt?.status_code)
  const http = observedHTTPStatus(attempt?.observed_http_status_code)
  return `协议状态 ${logical ?? '未记录'} · HTTP ${http ?? '未记录'}`
}
export function observationAttemptLabel(row) {
  const count = observationSummary(row).attemptCount
  return count === null ? '派发尝试未记录' : count === 0 ? '未发生上游网络派发' : `已记录 ${count} 次派发尝试`
}
export function finalUpstreamFailure(row) {
  return { code: observedText(row?.final_upstream_error_code), source: observedText(row?.final_upstream_error_source) }
}
const observedErrorParameters = new Set([
  'stream_options', 'stream_options.include_usage', 'stream_options.include_obfuscation', 'stream_options.reasoning_summary_delivery', 'stream_options.unknown',
  'service_tier', 'reasoning.effort', 'max_output_tokens', 'top_p', 'text', 'text.format',
  'text.format.type', 'text.format.name', 'text.format.strict', 'text.format.description', 'text.format.schema',
])
export function observedErrorParameter(record) {
  return typeof record?.error_param === 'string' && observedErrorParameters.has(record.error_param) ? record.error_param : ''
}
export function truncationLabel(value) { return value === true ? '已截断' : value === false ? '已观测为未截断' : '截断状态未记录' }
export function observationSummary(row) {
  const attempts = observationAttempts(row)
  const knownEmpty = observationVersion(row) === 2 && (row?.attempts == null || Array.isArray(row?.attempts)) && attempts.length === 0 && (row?.attempts_truncated == null || row.attempts_truncated === false)
  return { sent: observedText(row?.sent_model), returned: observedText(row?.returned_model), inbound: observedPathOptions.includes(row?.inbound_path) ? row.inbound_path : '', upstream: observedPathOptions.includes(row?.upstream_path) ? row.upstream_path : '', attemptCount: attempts.length || (knownEmpty ? 0 : null), attemptsTruncated: observedBoolean(row?.attempts_truncated), errorTruncated: observedBoolean(row?.error_truncated) }
}
export function observationStageAverage(stats, samples, average) { return Number.isSafeInteger(stats?.[samples]) && stats[samples] > 0 ? observedNumber(stats?.[average]) : null }

const toolChoiceLabels = { auto: '自动选择', none: '禁用工具', required: '必须调用工具', forced: '指定工具' }
const toolRejectionLabels = { normalize_item: '工具身份校验', parallel_policy: '并行调用策略', validate_response: '整轮工具策略' }
const toolFamilyLabels = { function_call: '函数工具', custom_tool_call: '自定义工具', tool_search_call: '工具搜索调用', tool_search_output: '工具搜索结果', web_search_call: '网页搜索', other_call: '其他调用', other: '其他项' }
const toolIdentityLabels = { exact: '精确匹配声明', unique_namespace: '唯一命名空间匹配', ambiguous_namespace: '多个命名空间匹配', undeclared: '未找到声明', unobserved: '身份未观测', internal_formatter: '内部格式化工具', not_client_action: '非客户端动作' }
const trustedToolLabel = (labels, value) => typeof value === 'string' && Object.hasOwn(labels, value) ? labels[value] : '未记录'
const toolCount = value => Number.isSafeInteger(value) && value >= 0 ? String(value) : '未记录'
const toolBoolean = value => value === true ? '是' : value === false ? '否' : '未记录'
export function toolPolicyObservation(record) {
  const policy = record?.tool_policy
  if (!policy || typeof policy !== 'object' || Array.isArray(policy)) return null
  const choice = trustedToolLabel(toolChoiceLabels, policy.effective_choice)
  const count = toolCount(policy.catalog_at_prepare_count)
  const rejectionFields = ['catalog_at_rejection_count', 'named_namespaces_at_rejection_count', 'rejection_phase', 'rejected_family', 'rejected_identity', 'rejected_matches_exact_declaration']
  const rows = [
    ['生效选择策略', choice],
    ['准备时声明 / 命名空间', `${count} / ${toolCount(policy.named_namespaces_at_prepare_count)}`],
    ['工具搜索 / 结构化', `${toolBoolean(policy.tool_search_at_prepare)} / ${toolBoolean(policy.structured)}`],
  ]
  if (rejectionFields.some(key => Object.hasOwn(policy, key))) rows.push(
    ['拒绝时声明 / 命名空间', `${toolCount(policy.catalog_at_rejection_count)} / ${toolCount(policy.named_namespaces_at_rejection_count)}`],
    ['拒绝阶段 / 工具族', `${trustedToolLabel(toolRejectionLabels, policy.rejection_phase)} / ${trustedToolLabel(toolFamilyLabels, policy.rejected_family)}`],
    ['声明身份', trustedToolLabel(toolIdentityLabels, policy.rejected_identity)],
    ['精确匹配已声明工具', toolBoolean(policy.rejected_matches_exact_declaration)],
  )
  return { summary: `${choice} · ${count === '未记录' ? '声明数未记录' : `${count} 项声明`}`, rows }
}

const boundStateLabels = { missing_or_replaced: '快照中缺失或已替换', exact_disabled: '快照中已禁用', exact_group_denied: '快照中无组授权', unclassified: '快照状态未分类' }
const boundOriginLabels = { cache_hit: '命中本地快照', fresh_read: '本次读取', stale_on_read_error: '读取失败后使用旧快照' }
const boundReplayLabels = { full_plaintext: '完整明文输入', opaque_or_file: '加密状态或文件引用', previous_response: '前序响应引用', unobserved: '输入类别未观测' }
const boundRecoveryLabels = { recovered: '已完成恢复', replacement_failed: '替换后未完成恢复', candidates_exhausted: '候选已耗尽', blocked_input: '输入绑定阻止迁移', blocked_context: '请求取消或超时', blocked_delegation: '委派绑定阻止迁移', blocked_other_guard: '其他检查阻止迁移' }
export function boundSelectionObservation(record) {
  const selection = record?.bound_selection
  if (!selection || typeof selection !== 'object' || Array.isArray(selection)) return null
  const state = trustedToolLabel(boundStateLabels, selection.bound_snapshot_state)
  const candidates = toolCount(selection.eligible_candidate_count)
  const age = toolCount(selection.snapshot_age_ms)
  return {
    summary: `${state} · ${candidates === '未记录' ? '授权候选数未记录' : `${candidates} 个授权候选`}`,
    rows: [
      ['选择时绑定状态', state],
      ['选择时授权候选数', candidates],
      ['快照来源 / 年龄', `${trustedToolLabel(boundOriginLabels, selection.snapshot_origin)} / ${age === '未记录' ? age : `${age} ms`}`],
      ['输入迁移类别', trustedToolLabel(boundReplayLabels, selection.replay_class)],
      ['最终恢复结果', trustedToolLabel(boundRecoveryLabels, selection.recovery_outcome)],
    ],
  }
}
