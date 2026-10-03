import test from 'node:test'
import assert from 'node:assert/strict'
import { logCacheRatio, logPercent, summarizeLogPage, usageMetricNumber, usageMetricCoverage, rankUsageRecords } from '../src/utils/logMetrics.js'

test('page cache is token weighted and incomplete usage is never counted as zero', () => {
  const metrics = summarizeLogPage([
    { input_tokens: 100, output_tokens: 10, cached_tokens: 50, latency_ms: 1000 },
    { input_tokens: 900, output_tokens: 20, cached_tokens: 810, latency_ms: 3000 },
    { input_tokens: 200, latency_ms: 0 },
    {},
  ])
  assert.deepEqual(metrics, { tokens: 1030, usageCount: 2, cacheRatio: .86, cacheCount: 2, averageDuration: 4000 / 3, durationCount: 3 })
  assert.equal(logPercent(metrics.cacheRatio), '86%')
})

test('missing, malformed, negative and impossible metrics do not manufacture measurements', () => {
  assert.deepEqual(summarizeLogPage([{}, { input_tokens: null, output_tokens: -1, cached_tokens: Infinity, latency_ms: '' }]), {
    tokens: null, usageCount: 0, cacheRatio: null, cacheCount: 0, averageDuration: null, durationCount: 0,
  })
  for (const row of [{ input_tokens: 0, cached_tokens: 0 }, { input_tokens: 10, cached_tokens: 20 }, { input_tokens: '10', cached_tokens: 5 }, { input_tokens: 10 }]) assert.equal(logCacheRatio(row), null)
  assert.equal(logPercent(null), '—')
  assert.equal(logCacheRatio({ input_tokens: 10, cached_tokens: 0, cached_tokens_known: true }), 0)
  assert.equal(summarizeLogPage([{ input_tokens: 0, output_tokens: 0, latency_ms: 0 }]).tokens, 0)
})

test('usage aggregates retain unknown, confirmed zero, partial totals and empty buckets separately', () => {
  const unknown = { usage_known_requests: 0, usage_unknown_requests: 2, total_requests: 2, total_tokens: 0, total_input_tokens: 0, total_output_tokens: 0, total_cached_tokens: 0 }
  const zero = { ...unknown, usage_known_requests: 2, usage_unknown_requests: 0 }
  const empty = { ...zero, total_requests: 0, usage_known_requests: 0 }
  for (const metric of ['total_tokens', 'total_input_tokens', 'total_output_tokens', 'total_cached_tokens']) {
    assert.equal(usageMetricNumber(unknown, metric), null)
    assert.equal(usageMetricNumber(zero, metric), 0)
    assert.equal(usageMetricNumber(empty, metric), 0)
    assert.equal(usageMetricCoverage(unknown, metric), '用量未记录（2 条请求）')
    assert.equal(usageMetricCoverage(zero, metric), '')
    const partial = { ...unknown, usage_known_requests: 1, usage_unknown_requests: 1, [metric]: 80 }
    assert.equal(usageMetricNumber(partial, metric), 80)
    assert.equal(usageMetricCoverage(partial, metric), '已记录 1 / 2 条请求')
  }
  assert.equal(usageMetricNumber(unknown, 'total_requests'), 2)
  assert.equal(usageMetricCoverage(unknown, 'total_requests'), '')
  assert.equal(usageMetricNumber({ total_tokens: 5 }, 'total_tokens'), 5)
  assert.equal(usageMetricNumber(null, 'total_tokens'), null)
})

test('unknown token ranks follow confirmed zero without changing request or success rankings', () => {
  const rows = [
    { id: 'a-unknown', total_requests: 10, success: 10, total_tokens: 0, usage_known_requests: 0, usage_unknown_requests: 10 },
    { id: 'b-zero', total_requests: 1, success: 0, total_tokens: 0, usage_known_requests: 1, usage_unknown_requests: 0 },
    { id: 'c-partial', total_requests: 2, success: 1, total_tokens: 80, usage_known_requests: 1, usage_unknown_requests: 1 },
  ]
  assert.deepEqual(rankUsageRecords(rows).map(row => row.id), ['c-partial', 'b-zero', 'a-unknown'])
  assert.deepEqual(rankUsageRecords(rows, 'total_requests').map(row => row.id), ['a-unknown', 'c-partial', 'b-zero'])
  assert.deepEqual(rankUsageRecords(rows, 'success_rate').map(row => row.id), ['a-unknown', 'c-partial', 'b-zero'])
  assert.equal(rows[0].id, 'a-unknown')
})


test('cache writes never improve read hit rate and absent read observations stay unknown', () => {
  assert.equal(logCacheRatio({ input_tokens: 100, cached_tokens: 10, cache_write_tokens: 80 }), .1)
  assert.equal(logCacheRatio({ input_tokens: 100, cached_tokens: 0, cache_write_tokens: 100 }), null)
  assert.equal(logCacheRatio({ input_tokens: 100, cached_tokens: 50, cached_tokens_known: false }), null)
  assert.equal(usageMetricNumber({ total_cache_write_tokens: 0, cache_write_samples: 0 }, 'total_cache_write_tokens'), null)
  assert.equal(usageMetricNumber({ total_cache_write_tokens: 0, cache_write_samples: 1 }, 'total_cache_write_tokens'), 0)
  assert.equal(usageMetricNumber({ total_cached_tokens: 0, cache_read_samples: 0 }, 'total_cached_tokens'), null)
})

test('cache read metrics and rankings use the same observed sample coverage', () => {
  // The backend preserves the legacy subtotal (8 known + 7 from an incomplete
  // connection), while only the two complete read observations enter coverage.
  const partial = { id: 'partial', total_cached_tokens: 15, cache_read_tokens: 8, cache_read_samples: 2, cache_read_unknown_requests: 2, usage_known_requests: 4, usage_unknown_requests: 0 }
  const complete = { id: 'complete', total_cached_tokens: 10, cache_read_tokens: 10, cache_read_samples: 1, cache_read_unknown_requests: 0, usage_known_requests: 1, usage_unknown_requests: 0 }
  assert.equal(usageMetricNumber(partial, 'total_cached_tokens'), 8)
  assert.equal(usageMetricCoverage(partial, 'total_cached_tokens'), '2 条请求有读取记录')
  assert.deepEqual(rankUsageRecords([partial, complete], 'total_cached_tokens').map(row => row.id), ['complete', 'partial'])
  assert.equal(usageMetricNumber({ ...partial, cache_read_tokens: undefined }, 'total_cached_tokens'), null)
  assert.equal(usageMetricNumber({ ...partial, cache_read_tokens: 0 }, 'total_cached_tokens'), 0)
  assert.equal(usageMetricNumber({ total_cached_tokens: 15 }, 'total_cached_tokens'), 15)
})

test('reported cache creation remains known when main token usage is unknown', () => {
  const unknownUsage = { total_requests: 1, usage_known_requests: 0, usage_unknown_requests: 1, total_input_tokens: 0, cache_write_samples: 1 }
  for (const value of [0, 100]) {
    const row = { ...unknownUsage, total_cache_write_tokens: value }
    assert.equal(usageMetricNumber(row, 'total_cache_write_tokens'), value)
    assert.equal(usageMetricCoverage(row, 'total_cache_write_tokens'), '1 条请求有创建记录')
    assert.equal(usageMetricNumber(row, 'total_input_tokens'), null)
    assert.equal(usageMetricNumber({ ...row, cache_write_samples: 0 }, 'total_cache_write_tokens'), null)
  }
  assert.equal(usageMetricNumber({ ...unknownUsage, total_cache_write_tokens: null }, 'total_cache_write_tokens'), null)
})
