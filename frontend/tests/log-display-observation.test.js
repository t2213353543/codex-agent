import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import * as Vue from 'vue'
import { compile } from '@vue/compiler-dom'
import { renderToString } from '@vue/server-renderer'
import { usageTrendGeometry } from '../src/utils/logManagementState.js'
import { observationSummary } from '../src/utils/logObservationState.js'

// Exercise the actual display expressions without a browser or shared build.
const analytics = readFileSync(new URL('../src/components/LogUsageAnalytics.vue', import.meta.url), 'utf8')
const logs = readFileSync(new URL('../src/views/Logs.vue', import.meta.url), 'utf8')
const coverageStart = analytics.indexOf('const trendCoverage = computed(')
const coverageEnd = analytics.indexOf('const trendTime =', coverageStart)
assert.ok(coverageStart >= 0 && coverageEnd > coverageStart)
const coverageCode = analytics.slice(coverageStart, coverageEnd)
function coverage(row, metric) {
  const geometry = usageTrendGeometry([row], metric)
  return new Function('computed', 'geometry', 'metric', `${coverageCode}; return trendCoverage.value`)(Vue.computed, { value: geometry }, { value: metric })
}

test('creation trend coverage follows creation observations independently of main usage', () => {
  const base = { time: 60, total_requests: 1, usage_known_requests: 0, usage_unknown_requests: 1, cache_write_samples: 1 }
  for (const value of [0, 80]) assert.equal(coverage({ ...base, total_cache_write_tokens: value }, 'total_cache_write_tokens'), '')
  assert.equal(coverage({ ...base, total_requests: 2, total_cache_write_tokens: 80 }, 'total_cache_write_tokens'), '1 个时段仅显示已记录合计')
  assert.equal(coverage({ ...base, usage_known_requests: 1, usage_unknown_requests: 0, cache_write_samples: 0, total_cache_write_tokens: null }, 'total_cache_write_tokens'), '1 个时段用量未记录，曲线保留空缺')
})

test('complete creation coverage excludes models and warmup control requests from its denominator', () => {
  const base = { time: 60, total_requests: 3, usage_known_requests: 1, usage_unknown_requests: 2, cache_write_samples: 1, cache_read_samples: 1, cache_read_unknown_requests: 0 }
  // One inference with observed creation plus a models request and a warmup.
  for (const value of [0, 80]) assert.equal(coverage({ ...base, total_cache_write_tokens: value }, 'total_cache_write_tokens'), '')
  // Creation remains complete when the inference's read usage was unobserved.
  assert.equal(coverage({ ...base, cache_read_samples: 0, cache_read_unknown_requests: 1, total_cache_write_tokens: 80 }, 'total_cache_write_tokens'), '')
})

test('creation remains partial when an eligible inference lacks creation observations', () => {
  const base = { time: 60, total_requests: 3, usage_known_requests: 2, usage_unknown_requests: 1, cache_write_samples: 1, cache_read_samples: 1, cache_read_unknown_requests: 1, total_cache_write_tokens: 80 }
  // Two eligible inferences (only one with creation) and one control request.
  assert.equal(coverage(base, 'total_cache_write_tokens'), '1 个时段仅显示已记录合计')
  // Older schemas without the eligible-count pair retain total-request fallback.
  assert.equal(coverage({ ...base, cache_read_samples: undefined, cache_read_unknown_requests: undefined }, 'total_cache_write_tokens'), '1 个时段仅显示已记录合计')
  assert.equal(coverage({ ...base, cache_read_unknown_requests: undefined }, 'total_cache_write_tokens'), '1 个时段仅显示已记录合计')
})

test('read trend coverage follows observed read samples and matching unknown requests', () => {
  const base = { time: 60, total_requests: 2, usage_known_requests: 2, usage_unknown_requests: 0, cache_read_samples: 1, cache_read_unknown_requests: 1, cache_read_tokens: 40 }
  assert.equal(coverage(base, 'total_cached_tokens'), '1 个时段仅显示已记录合计')
  assert.equal(coverage({ ...base, cache_read_samples: 0, cache_read_tokens: 0 }, 'total_cached_tokens'), '1 个时段用量未记录，曲线保留空缺')
  assert.equal(coverage({ ...base, cache_read_unknown_requests: 0 }, 'total_cached_tokens'), '')
})

test('ordinary token coverage preserves complete, partial, unknown and request behavior', () => {
  const base = { time: 60, total_requests: 2, total_tokens: 20, usage_known_requests: 1, usage_unknown_requests: 1 }
  assert.equal(coverage(base, 'total_tokens'), '1 个时段仅显示已记录合计')
  assert.equal(coverage({ ...base, usage_known_requests: 0 }, 'total_tokens'), '1 个时段用量未记录，曲线保留空缺')
  assert.equal(coverage({ ...base, usage_unknown_requests: 0 }, 'total_tokens'), '')
  assert.equal(coverage(base, 'total_requests'), '')
})

test('desktop and mobile model chips keep header declarations separate from returned models', async () => {
  const start = logs.indexOf('<span v-if="row.returned_model || row.upstream_model" class="observed-model"')
  const end = logs.indexOf('<span v-if="row.service_tier"', start)
  assert.ok(start >= 0 && end > start)
  const mobile = logs.match(/<span v-if="row\.returned_model \|\| row\.upstream_model">[\s\S]*?<\/span>/)?.[0]
  assert.ok(mobile)
  for (const template of [logs.slice(start, end), mobile]) {
    const { code } = compile(template, { mode: 'function', prefixIdentifiers: true })
    const render = new Function('Vue', code)(Vue)
    const row = { upstream_model: 'header-only-model', upstream_model_source: 'header' }
    const header = await renderToString(Vue.createSSRApp({ setup: () => ({ row }), render }))
    assert.match(header, /上游声明 header-only-model/)
    assert.doesNotMatch(header, /返回(?:模型：| )header-only-model/)
    assert.equal(observationSummary(row).returned, '')
    const returned = await renderToString(Vue.createSSRApp({ setup: () => ({ row: { ...row, returned_model: 'response-model', upstream_model_source: 'response' } }), render }))
    assert.match(returned, /返回 response-model/)
    const missing = await renderToString(Vue.createSSRApp({ setup: () => ({ row: {} }), render }))
    assert.match(missing, /^<!--[\s\S]*-->$/)
  }
})
