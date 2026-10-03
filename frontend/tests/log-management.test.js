import test from 'node:test'
import assert from 'node:assert/strict'
import { managedQuerySnapshot, readManagedFilters, buildManagedQuery, managementScopeParams, validUsageStats, presentManagedRecord, usageCacheRatio, usageTrendGeometry, managedTimeParts, validLogTimezone } from '../src/utils/logManagementState.js'
import { downloadManagedLogs, logManagement } from '../src/api/logManagement.js'
import { authGeneration, clearSession, hasSession, loginSession } from '../src/api.js'

test('management snapshot fixes filters and sorting while omitting unrelated secrets', () => {
  const draft = { account_id: 'account-1', key_id: 'key-1', group_id: 'group-1', endpoint: 'responses', stream: 'false', http_code: '200', error_category: 'upstream_refusal', request_id: 'e_abc', ip: '192.0.2.1', request_type: 'websocket', upstream_model: 'gpt-5.6-sol-v2', service_tier: 'priority', reasoning_effort: 'xhigh', sort_by: 'tokens', sort_order: 'asc', password: 'secret' }
  const snapshot = managedQuerySnapshot(draft, 50)
  draft.key_id = 'key-2'
  assert.equal(snapshot.filters.key_id, 'key-1')
  assert.ok(Object.isFrozen(snapshot) && Object.isFrozen(snapshot.filters) && Object.isFrozen(snapshot.params))
  assert.equal(snapshot.params.stream, 'false')
  assert.equal(snapshot.params.exact_total, 'true')
  assert.equal(snapshot.params.request_type, 'websocket')
  assert.equal(snapshot.params.service_tier, 'priority')
  assert.equal(snapshot.params.reasoning_effort, 'xhigh')
  assert.equal(snapshot.params.upstream_model, 'gpt-5.6-sol-v2')
  assert.equal('password' in snapshot.params, false)
  assert.notEqual(snapshot.signature, managedQuerySnapshot(draft, 50).signature)
  assert.notEqual(snapshot.signature, managedQuerySnapshot({ ...draft, key_id: 'key-1', sort_order: 'desc' }, 50).signature)
})

test('detail API forwards the frozen management scope and never silently adds a snapshot to current review details', async () => {
  const original = globalThis.fetch, urls = []
  globalThis.fetch = async url => { urls.push(url); return new Response(JSON.stringify({ record: { id: 1 } }), { status: 200, headers: { 'Content-Type': 'application/json' } }) }
  try {
    await logManagement.detail('event/one', { params: { as_of: 'usage_fixed', account_id: 'account-1', key_id: 'key-1', model: 'gpt-test', from: '1700000000', to: '1700003600', cursor: 'old_cursor', token: 'must-not-forward' } })
    const url = new URL(urls[0], 'http://localhost')
    assert.equal(url.pathname, '/api/logs/event%2Fone')
    assert.equal(url.searchParams.get('as_of'), 'usage_fixed')
    assert.equal(url.searchParams.get('account_id'), 'account-1')
    assert.equal(url.searchParams.get('from'), '1700000000')
    assert.equal(url.searchParams.has('cursor'), false)
    assert.equal(url.searchParams.has('token'), false)
    await logManagement.detail('event-two', { params: {} })
    assert.equal(urls[1], '/api/logs/event-two')
  } finally { globalThis.fetch = original }
})

test('pagination and statistics use the same query and no cross-query cursor', () => {
  const filters = { model: 'gpt-5.6-sol', status: 'failed', sort_by: 'latency', sort_order: 'asc', exact_total: 'false' }
  const page = buildManagedQuery(filters, 100, 'cursor-1', 2)
  assert.equal(page.cursor, 'cursor-1')
  assert.equal('page' in page, false)
  const first = buildManagedQuery({ ...filters, key_id: 'new-key' }, 100)
  assert.equal('cursor' in first, false)
  assert.deepEqual(managementScopeParams(page), { ...filters, timezone: 'Asia/Shanghai' })
  assert.equal(readManagedFilters({ stream: '0', sort_by: 'DROP TABLE', http_code: '999', error_category: 'secret' }).http_code, '')
  assert.equal(readManagedFilters({ status: 'failed' }).status, 'failed')
  assert.equal(managementScopeParams({ model: 'm', as_of: 'usage_snapshot' }).as_of, 'usage_snapshot')
  assert.equal('as_of' in readManagedFilters({ as_of: 'usage_snapshot' }), false)
  assert.equal('as_of' in buildManagedQuery({ as_of: 'usage_snapshot' }), false)
})

test('usage distinguishes confirmed zero, missing usage, and historical nonzero evidence', () => {
  assert.equal(presentManagedRecord({ usage_known: true, input_tokens: 0, output_tokens: 0, cached_tokens: 0 }).input_tokens, 0)
	const partial = presentManagedRecord({ usage_known: true, input_tokens: 100, output_tokens: 10, cached_tokens: 0, cached_tokens_known: false })
	assert.equal(partial.input_tokens, 100)
	assert.equal(partial.cached_tokens, null)
	assert.equal(presentManagedRecord({ usage_known: true, cached_tokens: 0, cached_tokens_known: true }).cached_tokens, 0)
  assert.equal(presentManagedRecord({ input_tokens: 0, output_tokens: 0, cached_tokens: 0 }).input_tokens, null)
  assert.equal(presentManagedRecord({ usage_known: null, input_tokens: 0, output_tokens: 0 }).output_tokens, null)
  const historical = presentManagedRecord({ input_tokens: 100, output_tokens: 0, cached_tokens: 0 })
  assert.equal(historical.usage_known, true)
  assert.equal(historical.output_tokens, 0)
  assert.equal(usageCacheRatio({ total_input_tokens: 0, total_cached_tokens: 0 }), null)
  assert.equal(usageCacheRatio({ total_input_tokens: 100, total_cached_tokens: 200 }), null)
  assert.equal(usageCacheRatio({ total_input_tokens: 100, total_cached_tokens: 40, cache_read_samples: 1, cache_read_rate: .4 }), .4)
})

test('stats validation requires full aggregates and charts preserve timestamp spacing', () => {
  assert.equal(validUsageStats({ total_requests: 1 }), false)
  assert.equal(validUsageStats({ total_requests: 1001, success: 1000, errors: 1, total_input_tokens: 10, total_output_tokens: 20, total_cached_tokens: 2, total_tokens: 30, series: [] }), true)
  const geometry = usageTrendGeometry([{ time: 10, total_requests: 0 }, { time: 20, total_requests: 5 }, { time: 50, total_requests: 10 }])
  assert.equal(geometry.max, 10)
  assert.equal(geometry.points[1].x, 160)
  assert.equal(geometry.points[2].y, 20)
  assert.deepEqual(usageTrendGeometry([{ time: 10, total_requests: 2 }, { time: 30, total_requests: 8 }], 'total_requests', 10).points.map(point => point.total_requests), [2, 0, 8])
})

const exportResponse = (body = 'time,status\n1,ok\n', overrides = {}) => new Response(body, { headers: { 'content-type': 'text/csv; charset=utf-8', 'content-disposition': 'attachment; filename="usage.csv"', 'x-export-count': '1001', 'content-length': String(new TextEncoder().encode(body).byteLength), ...overrides } })

test('complete export uses full applied filter without cursor and validates file before saving', async () => {
  const progress = []
  let captured
  const result = await downloadManagedLogs({ account_id: 'a1', model: 'gpt-5.6-sol', cursor: 'old-page', page: 2, page_size: 20 }, { onProgress: value => progress.push(value), fetchImpl: async (url, options) => { captured = { url, options }; return exportResponse() } })
  const url = new URL(captured.url, 'http://localhost')
  assert.equal(url.searchParams.get('account_id'), 'a1')
  assert.equal(url.searchParams.get('format'), 'csv')
  assert.equal(url.searchParams.get('redacted'), 'true')
  assert.equal(url.searchParams.has('cursor'), false)
  assert.equal(url.searchParams.has('page_size'), false)
  assert.equal(captured.options.credentials, 'same-origin')
  assert.equal(result.count, 1001)
  assert.match(await result.blob.text(), /time,status/)
  assert.equal(progress.at(-1).received, progress.at(-1).total)
})

test('HTTP error, deceptive success JSON, incomplete body and missing count never become exports', async () => {
  const variants = [
    new Response(JSON.stringify({ error: { message: 'export unavailable' } }), { status: 503, headers: { 'content-type': 'application/json' } }),
    exportResponse('{"error":"failed"}', { 'content-type': 'application/json' }),
    exportResponse('partial', { 'content-length': '100' }),
    exportResponse('empty', { 'x-export-count': '' }),
  ]
  for (const response of variants) await assert.rejects(downloadManagedLogs({}, { fetchImpl: async () => response }))
})

test('cancelled transfer rejects without producing a blob result', async () => {
  const controller = new AbortController()
  const response = new Response(new ReadableStream({ start(source) { source.enqueue(new TextEncoder().encode('first')); source.enqueue(new TextEncoder().encode('second')); source.close() } }), { headers: { 'content-type': 'application/x-ndjson', 'content-disposition': 'attachment', 'x-export-count': '2' } })
  await assert.rejects(downloadManagedLogs({}, { format: 'jsonl', signal: controller.signal, onProgress: () => controller.abort(), fetchImpl: async () => response }), { name: 'AbortError' })
})

test('download authentication expiration notifies the shell and does not save a file', async () => {
  const previousWindow = globalThis.window
  const events = []
  globalThis.window = { dispatchEvent: event => events.push(event.type) }
  try {
    const generation = authGeneration
    await assert.rejects(downloadManagedLogs({}, { fetchImpl: async () => new Response('{}', { status: 401 }) }), { status: 401 })
    assert.deepEqual(events, ['auth-expired'])
    assert.equal(authGeneration, generation + 1)
    assert.equal(hasSession(), false)
  } finally { if (previousWindow === undefined) delete globalThis.window; else globalThis.window = previousWindow }
})

test('old or cancelled export 401 cannot clear a newer login session', async () => {
  const previousFetch = globalThis.fetch, previousWindow = globalThis.window
  const events = []
  globalThis.window = { dispatchEvent: event => events.push(event.type) }
  globalThis.fetch = async () => new Response(JSON.stringify({ authenticated: true }), { headers: { 'content-type': 'application/json' } })
  try {
    let release
    const old = downloadManagedLogs({}, { fetchImpl: () => new Promise(resolve => { release = resolve }) })
    await loginSession('synthetic-unit-key')
    const generation = authGeneration
    release(new Response('{}', { status: 401 }))
    await assert.rejects(old, { status: 401 })
    assert.equal(authGeneration, generation)
    assert.equal(hasSession(), true)
    const controller = new AbortController()
    await assert.rejects(downloadManagedLogs({}, { signal: controller.signal, fetchImpl: async () => { controller.abort(); return new Response('{}', { status: 401 }) } }), { name: 'AbortError' })
    assert.equal(authGeneration, generation)
    assert.equal(hasSession(), true)
    assert.deepEqual(events, [])
  } finally { clearSession(); globalThis.fetch = previousFetch; if (previousWindow === undefined) delete globalThis.window; else globalThis.window = previousWindow }
})

test('observed flags, error phase, calendar settings and model sorting are snapshot fields', () => {
  const snapshot = managedQuerySnapshot({ model_mismatch: 'false', native_compaction: 'false', error_phase: 'upstream', step: '2592000', timezone: 'America/New_York', sort_by: 'model' })
  assert.equal(snapshot.params.model_mismatch, 'false')
  assert.equal(snapshot.params.native_compaction, 'false')
  assert.equal(snapshot.params.error_phase, 'upstream')
  assert.equal(snapshot.params.step, '2592000')
  assert.equal(snapshot.params.timezone, 'America/New_York')
  assert.equal(snapshot.params.sort_by, 'model')
  assert.equal(validLogTimezone('Not/A_Timezone'), false)
  assert.equal(managedTimeParts(Date.UTC(2026, 2, 8, 6, 30) / 1000, 'America/New_York').time, '01:30:00')
  assert.equal(managedTimeParts(Date.UTC(2026, 2, 8, 7, 30) / 1000, 'America/New_York').time, '03:30:00')
  assert.equal(usageTrendGeometry([{ time: 100, total_requests: 2 }, { time: 100 + 31 * 86400, total_requests: 3 }], 'total_requests', 2592000).points.length, 2)
})

test('operational chart keeps unobserved buckets as gaps and preserves measured zero', () => {
  const result = usageTrendGeometry([{ time: 60, total_input_tokens: 10 }, { time: 120, total_input_tokens: null }, { time: 180, total_input_tokens: 20 }], 'total_input_tokens', 60)
  assert.equal(result.max, 20)
  assert.equal(result.points[1].total_input_tokens, null)
  assert.equal(result.points[1].y, null)
  assert.equal(result.points[2].y, 20)
  const zeros = usageTrendGeometry([{ time: 60, total_requests: 0 }])
  assert.equal(zeros.max, 0)
  assert.equal(zeros.points[0].y, 130)
})

test('token trend excludes unknown observations but keeps explicit zero and partially observed totals', () => {
  const points = usageTrendGeometry([
    { time: 60, total_tokens: 0, usage_known_requests: 0, usage_unknown_requests: 1 },
    { time: 120, total_tokens: 0, usage_known_requests: 1, usage_unknown_requests: 0 },
    { time: 180, total_tokens: 80, usage_known_requests: 1, usage_unknown_requests: 1 },
    { time: 240, total_tokens: 0, usage_known_requests: 0, usage_unknown_requests: 0, total_requests: 0 },
  ], 'total_tokens', 60).points
  assert.deepEqual(points.map(point => point.total_tokens), [null, 0, 80, 0])
  assert.deepEqual(points.map(point => point.y), [null, 130, 20, 130])
  assert.equal(points[2].usage_unknown_requests, 1)
})

test('log timezone rejects Intl offset extensions and canonicalizes names before backend queries', () => {
  for (const value of ['+08:00', '-01:30', '+0800', 'Local', ' Asia/Shanghai', 'Asia/Shanghai ', 'Not/A_Zone', null, undefined, 8]) assert.equal(validLogTimezone(value), false, String(value))
  for (const value of ['', 'UTC', 'Asia/Shanghai', 'Etc/GMT+8', 'America/New_York', 'US/Eastern', 'america/new_york']) assert.equal(validLogTimezone(value), true, value)
  const query = managedQuerySnapshot({ timezone: 'america/new_york', step: '86400' })
  assert.equal(query.params.timezone, 'America/New_York')
  assert.equal(readManagedFilters({ timezone: 'US/Eastern' }).timezone, 'America/New_York')
  assert.equal(readManagedFilters({ timezone: '+08:00' }).timezone, 'Asia/Shanghai')
  assert.equal(buildManagedQuery({ timezone: 'Etc/GMT+8' }).timezone, 'Etc/GMT+8')
  assert.equal(managementScopeParams(query.params).timezone, 'America/New_York')
})


test('cache ratio uses matched observed reads instead of all-input totals', () => {
  assert.equal(usageCacheRatio({ total_input_tokens: 1000, total_cached_tokens: 80, total_cache_write_tokens: 920, cache_read_samples: 1, cache_read_input_tokens: 100, cache_read_tokens: 80, cache_read_rate: .8 }), .8)
  assert.equal(usageCacheRatio({ total_input_tokens: 100, total_cached_tokens: 0 }), null)
  assert.equal(usageCacheRatio({ cache_read_samples: 1, cache_read_rate: 0 }), 0)
  assert.equal(presentManagedRecord({ usage_known: true, cached_tokens: 0 }).cached_tokens, null)
})
