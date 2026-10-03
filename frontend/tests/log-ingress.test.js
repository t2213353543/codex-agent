import test from 'node:test'
import assert from 'node:assert/strict'
import { emptyIngressFilters, ingressInitialFilters, ingressQuery, ingressQueryError, validateIngressPage, ingressCollectorStats, ingressReasons, ingressReasonLabel } from '../src/utils/logIngressState.js'
import { readIngressEvents } from '../src/api/ingressEvents.js'

const page = () => ({ scope: 'retained_ingress_events', source: 'gateway_ingress_v1', items: [{ id: 'event-1', sequence: 1, time_ms: 1789650000123, http_code: 401, reason: 'invalid_api_key' }], total: 1, total_kind: 'exact', has_more: false, next_cursor: '', checked_at: 1789650000, retention_limit: 1000, evicted_total: 0 })
test('ingress query only inherits time and validated caller identity, never business filters or snapshots', () => {
  const filters = ingressInitialFilters({ from: '100', to: '200', client_request_id: 'caller:1', account_id: 'sensitive', status: 'failed', model: 'model', as_of: 'usage-snapshot' })
  assert.deepEqual(filters, { ...emptyIngressFilters(), from: '100', to: '200', client_request_id: 'caller:1' })
  assert.deepEqual(ingressQuery({ ...filters, status: '401', reason: 'invalid_api_key', token: 'secret' }, 100, 'cursor'), { from: '100', to: '200', client_request_id: 'caller:1', status: '401', reason: 'invalid_api_key', limit: '100', cursor: 'cursor' })
  for (const invalid of [{ status: '200' }, { status: '600' }, { reason: 'invented' }, { client_request_id: 'Bearer secret' }, { client_request_id: 'x'.repeat(129) }, { from: '200', to: '100' }, { from: '0' }, { to: '253402300800' }]) assert.notEqual(ingressQueryError({ ...emptyIngressFilters(), ...invalid }), '')
  assert.equal(ingressQueryError({ ...emptyIngressFilters(), from: '1', to: '253402300799' }), '')
})
test('ingress response cannot masquerade as usage or silently omit cursor and retention boundaries', () => {
  assert.equal(validateIngressPage(page()).total, 1)
  for (const patch of [{ scope: 'retained_request_details' }, { source: 'unknown' }, { total_kind: 'estimate' }, { has_more: true }, { retention_limit: undefined }, { evicted_total: -1 }]) assert.throws(() => validateIngressPage({ ...page(), ...patch }))
  for (const row of [{ ...page().items[0], time_ms: null }, { ...page().items[0], http_code: 200 }, { ...page().items[0], reason: 'unknown-string' }]) assert.throws(() => validateIngressPage({ ...page(), items: [row] }))
})
test('key authorization storage failures are accepted, labeled and filterable without allowing unknown reasons', () => {
  const reason = 'key_store_unavailable'
  const data = page()
  data.items[0] = { ...data.items[0], http_code: 503, reason }
  assert.equal(validateIngressPage(data), data)
  assert.equal(ingressReasonLabel(reason), '密钥授权存储不可用')
  assert.deepEqual(ingressReasons.find(([value]) => value === reason), [reason, '密钥授权存储不可用'])
  assert.deepEqual(ingressQuery({ ...emptyIngressFilters(), status: '503', reason }), { status: '503', reason, limit: '50' })
  assert.throws(() => validateIngressPage({ ...data, items: [{ ...data.items[0], reason: 'unrecognized_storage_failure' }] }))
  assert.throws(() => ingressQuery({ ...emptyIngressFilters(), status: '503', reason: 'unrecognized_storage_failure' }))
  assert.equal(ingressReasonLabel('unrecognized_storage_failure'), '未识别的准入原因')
})
test('collector coverage is process-scoped and does not expose raw storage errors or manufacture missing zero', () => {
  const value = ingressCollectorStats({ configured: true, running: false, queue_capacity: 256, queued: 0, pending: 2, queue_dropped: 3, persistence_failures: 1, last_persisted_at: 100, last_error: 'raw-db-secret', counter_scope: 'current_process' })
  assert.equal(value.state, '采集已停止'); assert.equal(value.counts.queued, 0); assert.equal(value.counts.unpersisted_at_shutdown, null)
  assert.equal(value.processScoped, true); assert.doesNotMatch(JSON.stringify(value), /raw-db-secret/)
  assert.equal(ingressCollectorStats().processScoped, false)
  assert.equal(ingressCollectorStats({ configured: false }).state, '未配置采集')
})
test('ingress API is a read-only shared-session request and accepts cancellation', async () => {
  const previousFetch = globalThis.fetch, requests = []
  globalThis.fetch = async (url, options) => { requests.push({ url, options }); return new Response(JSON.stringify(page()), { headers: { 'content-type': 'application/json' } }) }
  try {
    await readIngressEvents({ ...emptyIngressFilters(), client_request_id: 'caller' }, 50, '')
    assert.equal(requests[0].options.method, 'GET'); assert.equal(requests[0].options.body, undefined)
    assert.equal(new URL(requests[0].url, 'http://localhost').pathname, '/api/ingress-events')
    assert.equal(requests[0].options.credentials, 'same-origin')
    const controller = new AbortController(); controller.abort()
    await assert.rejects(readIngressEvents(emptyIngressFilters(), 50, '', { signal: controller.signal }), { name: 'AbortError' })
  } finally { globalThis.fetch = previousFetch }
})
