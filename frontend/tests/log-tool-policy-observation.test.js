import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import * as observation from '../src/utils/logObservationState.js'
import { logLatency } from '../src/utils/logQueryState.js'
import { passiveTimingSource } from '../src/utils/logPassiveObservation.js'
import * as Vue from 'vue'
import { compile } from '@vue/compiler-dom'
import { parse, compileScript } from '@vue/compiler-sfc'
import { renderToString } from '@vue/server-renderer'
const source = readFileSync(new URL('../src/components/LogObservationDetails.vue', import.meta.url), 'utf8')
const parsed = parse(source, { filename: 'LogObservationDetails.vue' })
const render = new Function('Vue', compile(parsed.descriptor.template.content, { mode: 'function', prefixIdentifiers: true }).code)(Vue)
const base = { effective_choice: 'auto', catalog_at_prepare_count: 0, named_namespaces_at_prepare_count: 0, tool_search_at_prepare: false, structured: false }
async function html(record) {
  return renderToString(Vue.createSSRApp({ setup: () => ({ record, ...observation, passiveTimingSource, summary: observation.observationSummary(record), attempts: observation.observationAttempts(record), finalFailure: observation.finalUpstreamFailure(record), milliseconds: value => observation.observedNumber(value) === null ? '未记录' : logLatency(value) }), render }))
}

test('old records omit tool-policy details instead of inventing zero or false', async () => {
  for (const value of [undefined, null, false, 0, '', [], 'policy']) {
    assert.equal(observation.toolPolicyObservation({ tool_policy: value }), null)
    assert.doesNotMatch(await html({ tool_policy: value }), /工具策略观测|0 项声明/)
  }
  const unknown = observation.toolPolicyObservation({ tool_policy: {} })
  assert.match(unknown.summary, /声明数未记录/)
  assert.deepEqual(unknown.rows[1], ['准备时声明 / 命名空间', '未记录 / 未记录'])
  assert.deepEqual(unknown.rows[2], ['工具搜索 / 结构化', '未记录 / 未记录'])
})

test('known zero counts and false booleans remain explicit and the panel starts collapsed', async () => {
  const view = observation.toolPolicyObservation({ tool_policy: base })
  assert.equal(view.summary, '自动选择 · 0 项声明')
  assert.deepEqual(view.rows[1], ['准备时声明 / 命名空间', '0 / 0'])
  assert.deepEqual(view.rows[2], ['工具搜索 / 结构化', '否 / 否'])
  const rendered = await html({ tool_policy: base })
  assert.match(rendered, /<details class="observation-tool-policy">/)
  assert.doesNotMatch(rendered, /<details[^>]*open/)
  assert.match(rendered, /0 项声明/)
  assert.match(rendered, /否 \/ 否/)
  assert.doesNotMatch(rendered, /拒绝阶段 \/ 工具族/)
})

test('rejection catalogs retain their own counts and exact matching does not mean policy allowance', async () => {
  const policy = { ...base, effective_choice: 'none', catalog_at_prepare_count: 4, named_namespaces_at_prepare_count: 2, tool_search_at_prepare: true, structured: true, catalog_at_rejection_count: 7, named_namespaces_at_rejection_count: 3, rejection_phase: 'parallel_policy', rejected_family: 'function_call', rejected_identity: 'exact', rejected_matches_exact_declaration: true }
  const view = observation.toolPolicyObservation({ tool_policy: policy })
  assert.equal(view.summary, '禁用工具 · 4 项声明')
  assert.deepEqual(view.rows.find(([label]) => label === '拒绝时声明 / 命名空间'), ['拒绝时声明 / 命名空间', '7 / 3'])
  assert.deepEqual(view.rows.find(([label]) => label === '精确匹配已声明工具'), ['精确匹配已声明工具', '是'])
  const rendered = await html({ tool_policy: policy })
  assert.match(rendered, /并行调用策略 \/ 函数工具/)
  assert.match(rendered, /精确匹配声明不代表被工具策略允许/)
  assert.doesNotMatch(rendered, /已允许|授权成功/)
})

test('missing rejection item is not reported as undeclared or non-matching', () => {
  const view = observation.toolPolicyObservation({ tool_policy: { ...base, effective_choice: 'required', rejection_phase: 'validate_response', catalog_at_rejection_count: 0, named_namespaces_at_rejection_count: 0 } })
  assert.equal(view.summary, '必须调用工具 · 0 项声明')
  assert.deepEqual(view.rows.find(([label]) => label === '声明身份'), ['声明身份', '未记录'])
  assert.deepEqual(view.rows.find(([label]) => label === '精确匹配已声明工具'), ['精确匹配已声明工具', '未记录'])
  assert.deepEqual(view.rows.find(([label]) => label === '拒绝阶段 / 工具族'), ['拒绝阶段 / 工具族', '整轮工具策略 / 未记录'])
})

test('only trusted finite enums and well-typed observations enter the helper or template', async () => {
  const policy = { effective_choice: '__proto__', catalog_at_prepare_count: '0', named_namespaces_at_prepare_count: -1, tool_search_at_prepare: 'false', structured: 0, rejection_phase: '<script>private</script>', rejected_family: 'PRIVATE_TOOL_NAME', rejected_identity: 'constructor', rejected_matches_exact_declaration: 'true', catalog_at_rejection_count: Number.MAX_SAFE_INTEGER + 1, named_namespaces_at_rejection_count: Infinity, tool_name: 'PRIVATE_NAME', arguments: 'PRIVATE_ARGUMENTS', schema: 'PRIVATE_SCHEMA' }
  const rendered = await html({ tool_policy: policy })
  const view = observation.toolPolicyObservation({ tool_policy: policy })
  assert.match(view.summary, /未记录/)
  for (const value of [rendered, JSON.stringify(view)]) assert.doesNotMatch(value, /PRIVATE|<script>|__proto__|constructor|Infinity/)
  assert.equal(view.rows.slice(1).every(([, value]) => value === '未记录' || value === '未记录 / 未记录'), true)
})

test('trusted identity distinctions remain finite and known false exact-match remains false', () => {
  const names = { unique_namespace: '唯一命名空间匹配', ambiguous_namespace: '多个命名空间匹配', undeclared: '未找到声明', unobserved: '身份未观测', internal_formatter: '内部格式化工具', not_client_action: '非客户端动作' }
  for (const [identity, label] of Object.entries(names)) {
    const view = observation.toolPolicyObservation({ tool_policy: { ...base, rejected_identity: identity, rejected_matches_exact_declaration: false } })
    assert.deepEqual(view.rows.find(([name]) => name === '声明身份'), ['声明身份', label])
    assert.deepEqual(view.rows.find(([name]) => name === '精确匹配已声明工具'), ['精确匹配已声明工具', '否'])
  }
})

test('tool-policy details coexist with 0.4.13 OAuth actual-HTTP observations', async () => {
  const rendered = await html({ observation_schema_version: 2, attempts: [{ attempt: 1, phase: 'refresh', status_code: 401, observed_http_status_code: 200 }], tool_policy: { ...base, effective_choice: 'forced', rejected_family: 'tool_search_output', rejection_phase: 'normalize_item', rejected_identity: 'undeclared', rejected_matches_exact_declaration: false } })
  assert.match(rendered, /协议状态 401 · HTTP 200/)
  assert.match(rendered, /OAuth 刷新/)
  assert.match(rendered, /指定工具/)
  assert.match(rendered, /工具身份校验 \/ 工具搜索结果/)
  assert.match(rendered, /未找到声明/)
  assert.deepEqual(parsed.errors, [])
  assert.doesNotThrow(() => compileScript(parsed.descriptor, { id: 'tool-policy-detail' }))
})
