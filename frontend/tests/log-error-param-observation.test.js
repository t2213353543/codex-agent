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
async function html(record) {
  return renderToString(Vue.createSSRApp({ setup: () => ({ record, ...observation, passiveTimingSource, summary: observation.observationSummary(record), attempts: observation.observationAttempts(record), finalFailure: observation.finalUpstreamFailure(record), milliseconds: value => observation.observedNumber(value) === null ? '未记录' : logLatency(value) }), render }))
}

test('fixed rejected protocol parameter is visible in the real detail template', async () => {
  for (const param of ['stream_options', 'stream_options.include_usage', 'stream_options.include_obfuscation', 'stream_options.reasoning_summary_delivery', 'stream_options.unknown', 'service_tier', 'reasoning.effort', 'max_output_tokens', 'text', 'text.format', 'text.format.type', 'text.format.name', 'text.format.strict', 'text.format.description', 'text.format.schema']) {
    assert.equal(observation.observedErrorParameter({ error_param: param }), param)
    const rendered = await html({ error_param: param })
    assert.match(rendered, /class="observation-fields observation-error-parameter"/)
    assert.match(rendered, /<dt>拒绝参数<\/dt>/)
    assert.ok(rendered.includes(`<code>${param}</code>`))
  }
})

test('legacy missing metadata does not invent a parameter from error text', async () => {
  for (const value of [undefined, null, '', 0, false, [], {}]) {
    const record = { error_param: value, error: 'Unsupported stream_options.include_usage' }
    assert.equal(observation.observedErrorParameter(record), '')
    assert.doesNotMatch(await html(record), /拒绝参数|observation-error-parameter/)
  }
})

test('unknown or malicious parameter text is never reflected or normalized on the client', async () => {
  for (const value of ['stream_options.PRIVATE_CUSTOM_KEY', 'text.format.PRIVATE_SCHEMA_KEY', 'stream_options.unknown<script>', '__proto__', 'constructor', ' stream_options.include_usage', 'stream_options.include_usage\n', 'HTTP 400', '<img src=x onerror=alert(1)>']) {
    const rendered = await html({ error_param: value })
    assert.equal(observation.observedErrorParameter({ error_param: value }), '')
    assert.doesNotMatch(rendered, /拒绝参数|PRIVATE|<img|<script|__proto__|constructor/)
  }
})

test('rejected parameter coexists with independent transport and tool-policy observations', async () => {
  const rendered = await html({ error_param: 'stream_options.include_usage', observation_schema_version: 2, attempts: [], tool_policy: { effective_choice: 'none', catalog_at_prepare_count: 0, named_namespaces_at_prepare_count: 0, tool_search_at_prepare: false, structured: false } })
  assert.match(rendered, /未发生上游网络派发/)
  assert.match(rendered, /<code>stream_options.include_usage<\/code>/)
  assert.match(rendered, /禁用工具 · 0 项声明/)
  assert.match(rendered, /否 \/ 否/)
  assert.match(rendered, /精确匹配声明不代表被工具策略允许/)
  assert.deepEqual(parsed.errors, [])
  assert.doesNotThrow(() => compileScript(parsed.descriptor, { id: 'error-param-observation' }))
})
