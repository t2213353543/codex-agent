import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import * as Vue from 'vue'
import { compile } from '@vue/compiler-dom'
import { renderToString } from '@vue/server-renderer'
import { reasoningRoute } from '../src/utils/logObservationState.js'

const logs = readFileSync(new URL('../src/views/Logs.vue', import.meta.url), 'utf8')
const desktop = logs.match(/<span v-if="row\.service_tier" class="model-metadata"[\s\S]*?<\/span>/)?.[0]
const mobile = logs.match(/<span v-if="row\.service_tier" class="service-tier-metadata"[\s\S]*?<\/span>/)?.[0]
const reasoning = logs.match(/<span v-if="reasoningRoute\(row\)\.label" :title="reasoningRoute\(row\)\.title">[\s\S]*?<\/span>/)?.[0]
assert.ok(desktop && mobile && reasoning, 'real desktop/mobile tier and mobile reasoning template fragments must exist')
async function render(template, row) {
  const { code } = compile(template, { mode: 'function', prefixIdentifiers: true })
  const render = new Function('Vue', code)(Vue)
  return renderToString(Vue.createSSRApp({ setup: () => ({ row, reasoningRoute }), render }))
}

test('desktop and mobile tier chips identify request-only values without implying an observed response', async () => {
  const row = { status: 'error', requested_service_tier: 'priority', service_tier: 'priority', service_tier_source: 'request', error_phase: 'request_validation' }
  for (const template of [desktop, mobile]) {
    const html = await render(template, row)
    assert.match(html, />请求 priority<\/span>/)
    assert.match(html, /title="服务层：priority · 来自请求，未确认响应值"/)
    assert.doesNotMatch(html, />响应 priority|响应已报告/)
  }
})

test('only response provenance labels a tier as an observed response', async () => {
  for (const template of [desktop, mobile]) {
    const html = await render(template, { requested_service_tier: 'auto', service_tier: 'priority', service_tier_source: 'response' })
    assert.match(html, />响应 priority<\/span>/)
    assert.match(html, /title="服务层：priority · 响应已报告"/)
    assert.doesNotMatch(html, />请求 priority/)
  }
})

test('legacy missing or unknown tier source remains a record, never a response assertion', async () => {
  for (const source of [undefined, null, '', 'legacy']) for (const template of [desktop, mobile]) {
    const html = await render(template, { service_tier: 'priority', service_tier_source: source })
    assert.match(html, />记录 priority<\/span>/)
    assert.match(html, /title="服务层：priority · 来源未记录"/)
    assert.doesNotMatch(html, />响应 priority|响应已报告/)
  }
})

test('missing tier stays absent and mobile reasoning mapping retains its own label and title', async () => {
  for (const template of [desktop, mobile]) {
    assert.match(await render(template, { requested_service_tier: 'priority' }), /^<!--[\s\S]*-->$/)
  }
  const route = { requested_reasoning_effort: 'max', sent_reasoning_effort: 'xhigh', reasoning_effort_source: 'request' }
  for (const tier of [{}, { service_tier: 'priority', service_tier_source: 'request' }]) {
    const html = await render(mobile + reasoning, { ...route, ...tier })
    assert.match(html, />max → xhigh<\/span>/)
    assert.match(html, /title="推理档位 · 请求：max · 发送：xhigh · 响应：未记录"/)
    if (tier.service_tier) assert.match(html, />请求 priority<\/span>/)
  }
})
