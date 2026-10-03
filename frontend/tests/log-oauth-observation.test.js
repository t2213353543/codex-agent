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

const component = readFileSync(new URL('../src/components/LogObservationDetails.vue', import.meta.url), 'utf8')
const logs = readFileSync(new URL('../src/views/Logs.vue', import.meta.url), 'utf8')
const descriptor = parse(component).descriptor
const render = new Function('Vue', compile(descriptor.template.content, { mode: 'function', prefixIdentifiers: true }).code)(Vue)
async function details(record) {
  return renderToString(Vue.createSSRApp({
    setup: () => ({ record, ...observation, passiveTimingSource, summary: observation.observationSummary(record), attempts: observation.observationAttempts(record), finalFailure: observation.finalUpstreamFailure(record), milliseconds: value => observation.observedNumber(value) === null ? '未记录' : logLatency(value) }),
    render,
  }))
}

test('logical protocol status and received HTTP headers stay independent', () => {
  const rows = observation.observationAttempts({ attempts: [
    { attempt: 1, phase: 'request', endpoint: '/responses', status_code: 429, observed_http_status_code: 200, header_ms: 0, duration_ms: 0 },
    { attempt: 2, phase: 'refresh', endpoint: '/oauth/token', status_code: 401, observed_http_status_code: 200, error_code: 'protocol' },
  ] })
  assert.equal(observation.attemptStatusLabel(rows[0]), '协议状态 429 · HTTP 200')
  assert.equal(observation.attemptStatusLabel(rows[1]), '协议状态 401 · HTTP 200')
  assert.equal(observation.attemptPhase(rows[1].phase), 'OAuth 刷新')
  assert.equal(observation.attemptPhase(rows[0].phase), 'HTTP 请求派发')
  assert.equal(rows[0].header_ms, 0)
  assert.equal(rows[0].duration_ms, 0)
})

test('historical logical status cannot become observed HTTP or default zero', () => {
  const [old] = observation.observationAttempts({ observation_schema_version: 1, attempts: [{ status_code: 401, phase: 'request' }] })
  assert.equal(old.observed_http_status_code, null)
  assert.equal(observation.attemptStatusLabel(old), '协议状态 401 · HTTP 未记录')
  for (const invalid of [0, 99, 600, null, undefined, '200', true, NaN, Infinity, 200.5]) {
    assert.equal(observation.observedHTTPStatus(invalid), null)
    assert.equal(observation.attemptStatusLabel({ observed_http_status_code: invalid }), '协议状态 未记录 · HTTP 未记录')
  }
  assert.equal(observation.attemptStatusLabel({ status_code: 101, observed_http_status_code: 101 }), '协议状态 101 · HTTP 101')
})

test('schema 2 local exits have zero dispatch while old or incomplete records remain unknown', () => {
  for (const attempts of [undefined, null, []]) {
    const record = { observation_schema_version: 2, attempts }
    assert.equal(observation.observationSummary(record).attemptCount, 0)
    assert.equal(observation.observationAttemptLabel(record), '未发生上游网络派发')
  }
  for (const record of [{}, { observation_schema_version: 1, attempts: [] }, { observation_schema_version: 2, attempts: {}, attempts_truncated: false }, { observation_schema_version: 2, attempts: [], attempts_truncated: true }, { observation_schema_version: 3, attempts: [] }]) {
    assert.equal(observation.observationSummary(record).attemptCount, null)
    assert.equal(observation.observationAttemptLabel(record), '派发尝试未记录')
  }
  for (const attempts_truncated of ['true', 'false', 0, 1, {}]) {
    assert.equal(observation.observationSummary({ observation_schema_version: 2, attempts: [], attempts_truncated }).attemptCount, null)
  }
})

test('terminal generation provenance never falls back to an OAuth attempt error', () => {
  const refreshOnly = { attempts: [{ phase: 'refresh', status_code: 401, observed_http_status_code: 200, error_code: 'invalid_grant', error_source: 'upstream' }] }
  assert.deepEqual(observation.finalUpstreamFailure(refreshOnly), { code: '', source: '' })
  assert.deepEqual(observation.finalUpstreamFailure({ ...refreshOnly, final_upstream_error_code: 'rateLimitExceeded', final_upstream_error_source: 'upstream' }), { code: 'rateLimitExceeded', source: 'upstream' })
})

test('actual detail template shows logical failure and actual HTTP 200 on the same compact line', async () => {
  const html = await details({ observation_schema_version: 2, attempts: [
    { attempt: 1, phase: 'request', endpoint: '/responses', status_code: 401, observed_http_status_code: 401, header_ms: 0 },
    { attempt: 2, phase: 'refresh', endpoint: '/oauth/token', status_code: 401, observed_http_status_code: 200, error_source: 'upstream', error_code: 'protocol' },
    { attempt: 3, phase: 'request', endpoint: '/responses', status_code: 429, observed_http_status_code: 200 },
  ], final_upstream_error_source: 'upstream', final_upstream_error_code: 'rateLimitExceeded' })
  assert.match(html, /尝试 1 · HTTP 请求派发/)
  assert.match(html, /尝试 2 · OAuth 刷新/)
  assert.match(html, /协议状态 401 · HTTP 200/)
  assert.match(html, /协议状态 429 · HTTP 200/)
  assert.match(html, /最终生成错误/)
  assert.match(html, /rateLimitExceeded/)
  assert.equal((html.match(/class="attempt-heading"/g) || []).length, 3)
  assert.doesNotMatch(html, /HTTP 0(?:<|\s)/)
})

test('actual detail template distinguishes old unknown dispatches and schema 2 local rejection', async () => {
  const legacy = await details({ observation_schema_version: 1, attempts: [] })
  assert.match(legacy, /不表示发生 0 次发送或重试/)
  assert.doesNotMatch(legacy, /未发生上游网络派发|最终生成错误/)
  const local = await details({ observation_schema_version: 2, attempts: [] })
  assert.match(local, /未发生上游网络派发/)
  assert.match(local, /处理在发送前结束/)
  assert.doesNotMatch(local, /不表示发生 0 次发送或重试|observation-attempt-list/)
  const oldAttempt = await details({ observation_schema_version: 1, attempts: [{ attempt: 1, phase: 'refresh', status_code: 401 }] })
  assert.match(oldAttempt, /协议状态 401 · HTTP 未记录/)
  assert.doesNotMatch(oldAttempt, /HTTP 401/)
})

test('new error provenance is escaped by Vue and is never injected as HTML', async () => {
  const html = await details({ final_upstream_error_code: '<img src=x onerror=alert(1)>', final_upstream_error_source: '<script>bad</script>' })
  assert.doesNotMatch(html, /<img|<script/)
  assert.match(html, /&lt;img/)
  assert.match(html, /&lt;script/)
})

test('desktop and mobile count labels share schema-aware helper without adding table rows', async () => {
  const snippets = [...logs.matchAll(/<small>\{\{ observationAttemptLabel\(row\) \}\}[\s\S]*?<\/small>/g)]
  assert.equal(snippets.length, 2)
  for (const [template] of snippets) {
    const renderCount = new Function('Vue', compile(template, { mode: 'function', prefixIdentifiers: true }).code)(Vue)
    const html = await renderToString(Vue.createSSRApp({ setup: () => ({ row: { observation_schema_version: 2, attempts: [] }, observationAttemptLabel: observation.observationAttemptLabel }), render: renderCount }))
    assert.match(html, /未发生上游网络派发/)
    assert.doesNotMatch(html, /未记录|HTTP 0/)
  }
  for (const [filename, source] of [['LogObservationDetails.vue', component], ['Logs.vue', logs]]) {
    const parsed = parse(source, { filename })
    assert.deepEqual(parsed.errors, [])
    assert.doesNotThrow(() => compileScript(parsed.descriptor, { id: filename }))
  }
})
