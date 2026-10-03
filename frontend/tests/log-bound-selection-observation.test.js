import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import * as observation from '../src/utils/logObservationState.js'
import { logLatency } from '../src/utils/logQueryState.js'
import { passiveTimingSource } from '../src/utils/logPassiveObservation.js'
import { managedQuerySnapshot, managementScopeParams, readManagedFilters } from '../src/utils/logManagementState.js'
import * as Vue from 'vue'
import { compile } from '@vue/compiler-dom'
import { parse, compileScript } from '@vue/compiler-sfc'
import { renderToString } from '@vue/server-renderer'
const source = readFileSync(new URL('../src/components/LogObservationDetails.vue', import.meta.url), 'utf8')
const parsed = parse(source, { filename: 'LogObservationDetails.vue' })
const render = new Function('Vue', compile(parsed.descriptor.template.content, { mode: 'function', prefixIdentifiers: true }).code)(Vue)
async function html(record) {
  return renderToString(Vue.createSSRApp({ setup: () => ({ record, ...observation, passiveTimingSource, summary: observation.observationSummary(record), attempts: observation.observationAttempts(record), finalFailure: observation.finalUpstreamFailure(record), milliseconds: value => observation.observedNumber(value) === null ? '未记录' : logLatency(value) }), render }))
}
const baseline = { bound_snapshot_state: 'exact_disabled', eligible_candidate_count: 0, snapshot_origin: 'cache_hit', snapshot_age_ms: 0, replay_class: 'opaque_or_file', recovery_outcome: 'blocked_input' }

test('missing bound observations stay absent and missing scalar values stay unknown', async () => {
  for (const value of [undefined, null, false, 0, '', [], 'private']) {
    assert.equal(observation.boundSelectionObservation({ bound_selection: value }), null)
    assert.doesNotMatch(await html({ bound_selection: value }), /会话绑定观测|observation-bound-selection/)
  }
  const unknown = observation.boundSelectionObservation({ bound_selection: {} })
  assert.equal(unknown.summary, '未记录 · 授权候选数未记录')
  assert.deepEqual(unknown.rows.map(([,value]) => value), ['未记录', '未记录', '未记录 / 未记录', '未记录', '未记录'])
})

test('known zero candidates and zero snapshot age are explicit in collapsed real template', async () => {
  const view = observation.boundSelectionObservation({ bound_selection: baseline })
  assert.equal(view.summary, '快照中已禁用 · 0 个授权候选')
  assert.deepEqual(view.rows[2], ['快照来源 / 年龄', '命中本地快照 / 0 ms'])
  const rendered = await html({ bound_selection: baseline })
  assert.match(rendered, /<details class="observation-bound-selection">/)
  assert.doesNotMatch(rendered, /<details[^>]*open/)
  assert.match(rendered, /0 个授权候选/)
  assert.match(rendered, /0 ms/)
  assert.match(rendered, /输入绑定阻止迁移/)
  assert.match(rendered, /不代表当前账号状态/)
  assert.match(rendered, /不保证可立即使用，也不等于实际派发次数/)
})

test('finite snapshot origin and recovery labels do not imply current availability', () => {
  const states = { missing_or_replaced: '快照中缺失或已替换', exact_disabled: '快照中已禁用', exact_group_denied: '快照中无组授权', unclassified: '快照状态未分类' }
  for (const [state, label] of Object.entries(states)) assert.equal(observation.boundSelectionObservation({ bound_selection: { ...baseline, bound_snapshot_state: state } }).rows[0][1], label)
  const origins = { fresh_read: '本次读取', cache_hit: '命中本地快照', stale_on_read_error: '读取失败后使用旧快照' }
  for (const [origin, label] of Object.entries(origins)) assert.equal(observation.boundSelectionObservation({ bound_selection: { ...baseline, snapshot_origin: origin, snapshot_age_ms: 5001 } }).rows[2][1], `${label} / 5001 ms`)
  const outcomes = { recovered: '已完成恢复', replacement_failed: '替换后未完成恢复', candidates_exhausted: '候选已耗尽', blocked_input: '输入绑定阻止迁移', blocked_context: '请求取消或超时', blocked_delegation: '委派绑定阻止迁移', blocked_other_guard: '其他检查阻止迁移' }
  for (const [outcome, label] of Object.entries(outcomes)) assert.equal(observation.boundSelectionObservation({ bound_selection: { ...baseline, recovery_outcome: outcome } }).rows[4][1], label)
})

test('replay categories remain descriptive rather than a fabricated allowed boolean', async () => {
  const classes = { full_plaintext: '完整明文输入', opaque_or_file: '加密状态或文件引用', previous_response: '前序响应引用', unobserved: '输入类别未观测' }
  for (const [value, label] of Object.entries(classes)) assert.equal(observation.boundSelectionObservation({ bound_selection: { ...baseline, replay_class: value } }).rows[3][1], label)
  const rendered = await html({ bound_selection: { ...baseline, replay_class: 'full_plaintext', recovery_outcome: 'blocked_other_guard' } })
  assert.match(rendered, /完整明文仍须通过其他检查后才能迁移/)
  assert.match(rendered, /其他检查阻止迁移/)
  assert.doesNotMatch(rendered, /迁移已允许|当前已禁用/)
})

test('invalid false/count strings and arbitrary private enums are never interpreted or reflected', async () => {
  const selection = { bound_snapshot_state: '<img src=x onerror=PRIVATE()>', eligible_candidate_count: false, snapshot_origin: '__proto__', snapshot_age_ms: '0', replay_class: 'PRIVATE_SESSION', recovery_outcome: 'constructor', account_id: 'PRIVATE_ACCOUNT', token: 'PRIVATE_TOKEN' }
  const view = observation.boundSelectionObservation({ bound_selection: selection })
  assert.equal(view.summary, '未记录 · 授权候选数未记录')
  assert.deepEqual(view.rows.map(([,value]) => value), ['未记录', '未记录', '未记录 / 未记录', '未记录', '未记录'])
  assert.doesNotMatch(await html({ bound_selection: selection }), /PRIVATE|<img|__proto__|constructor/)
  for (const value of [true, false, '0', -1, 1.5, Infinity, Number.MAX_SAFE_INTEGER + 1]) {
    const valueView = observation.boundSelectionObservation({ bound_selection: { eligible_candidate_count: value, snapshot_age_ms: value } })
    assert.equal(valueView.rows[1][1], '未记录')
    assert.equal(valueView.rows[2][1], '未记录 / 未记录')
  }
})

test('attachments stays a recognized observed endpoint and survives exact query filters', async () => {
  assert.equal(observation.observedPathOptions.includes('/attachments'), true)
  const record = { upstream_path: '/attachments', attempts: [{ attempt: 1, phase: 'request', endpoint: '/attachments', status_code: 200, observed_http_status_code: 200 }] }
  assert.equal(observation.observationSummary(record).upstream, '/attachments')
  assert.equal(observation.observationAttempts(record)[0].endpoint, '/attachments')
  assert.match(await html(record), /\/attachments/)
  const snapshot = managedQuerySnapshot({ upstream_path: '/attachments' })
  assert.equal(snapshot.params.upstream_path, '/attachments')
  assert.equal(managementScopeParams(snapshot.params).upstream_path, '/attachments')
  assert.equal(readManagedFilters(snapshot.params).upstream_path, '/attachments')
  for (const value of ['https://private.example/attachments', '/attachments?secret=x', '/attachments/private-file']) assert.equal(observation.observationAttempts({ attempts: [{ endpoint: value }] })[0].endpoint, '')
})

test('binding snapshot panel coexists with existing OAuth, tool policy and rejected parameter details', async () => {
  const rendered = await html({ bound_selection: baseline, error_param: 'stream_options.unknown', tool_policy: { effective_choice: 'none', catalog_at_prepare_count: 0, named_namespaces_at_prepare_count: 0, tool_search_at_prepare: false, structured: false }, attempts: [{ attempt: 1, phase: 'refresh', status_code: 401, observed_http_status_code: 200 }, { attempt: 2, phase: 'request', endpoint: '/attachments', status_code: 200, observed_http_status_code: 200 }] })
  for (const expected of ['协议状态 401 · HTTP 200', 'OAuth 刷新', '/attachments', 'stream_options.unknown', '禁用工具 · 0 项声明', '否 / 否', '快照中已禁用 · 0 个授权候选']) assert.ok(rendered.includes(expected), expected)
  assert.deepEqual(parsed.errors, [])
  assert.doesNotThrow(() => compileScript(parsed.descriptor, { id: 'bound-selection-observation' }))
})

test('top_p is a fixed rejected parameter while arbitrary suffixes remain hidden', async () => {
  assert.equal(observation.observedErrorParameter({ error_param: 'top_p' }), 'top_p')
  assert.match(await html({ error_param: 'top_p' }), /<code>top_p<\/code>/)
  assert.equal(observation.observedErrorParameter({ error_param: 'top_p.PRIVATE' }), '')
  assert.doesNotMatch(await html({ error_param: 'top_p.PRIVATE' }), /PRIVATE|拒绝参数/)
})
