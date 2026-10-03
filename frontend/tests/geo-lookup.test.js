import test from 'node:test'
import assert from 'node:assert/strict'
import { geoIPState, geoLocationLabel, geoLookupBody, normalizeGeoIP, planGeoBatch, readGeoEntry, runGeoBatch } from '../src/utils/geoLookupState.js'
import { lookupIPGeo } from '../src/api/geoLookup.js'

test('geo literals normalize mapped IPv6 without accepting hosts, ports, zones or ambiguous IPv4', () => {
  assert.equal(normalizeGeoIP('::ffff:8.8.8.8'), '8.8.8.8')
  assert.equal(normalizeGeoIP('::FFFF:0808:0808'), '8.8.8.8')
  assert.equal(normalizeGeoIP('2001:4860:0000:0000:0000:0000:0000:8888'), '2001:4860::8888')
  for (const value of ['', 'localhost', 'http://8.8.8.8', '8.8.8.8:443', '[2001:4860::8888]', 'fe80::1%eth0', ' 8.8.8.8', '8.8.8.8\n', '010.0.0.1', '2130706433', '8.8.8.256', '::ffff:010.0.0.1', '::ffff:0x7f.0.0.1', '8.8.8.8,1.1.1.1']) {
    assert.equal(normalizeGeoIP(value), '', value)
  }
})

test('geo local classifier excludes private, CGNAT, link local, documentation and reserved ranges', () => {
  for (const value of ['0.0.0.0', '0.8.8.8', '10.0.0.1', '100.64.0.1', '100.127.255.255', '127.0.0.1', '169.254.169.254', '172.16.0.1', '172.31.255.255', '192.168.1.1', '192.0.0.9', '192.0.2.1', '192.88.99.1', '198.18.0.1', '198.19.255.255', '198.51.100.1', '203.0.113.1', '224.1.1.1', '240.1.1.1', '255.255.255.255', '::', '::1', '::ffff:192.168.1.1', '64:ff9b::808:808', '100::1', 'fc00::1', 'fd00::1', 'fe80::1', 'fec0::1', 'ff02::1', '2001::1', '2001:2::1', '2001:db8::1', '2002:0808:0808::1', '3fff::1', '5f00::1']) {
    assert.equal(geoIPState(value).status, 'private', value)
  }
  for (const value of ['8.8.8.8', '1.1.1.1', '100.63.255.255', '100.128.0.1', '172.15.255.255', '172.32.0.1', '2001:4860:4860::8888', '2606:4700:4700::1111']) assert.equal(geoIPState(value).status, 'idle', value)
})

test('explicit geo body is bounded, normalized and drops all unrelated fields', () => {
  assert.deepEqual(geoLookupBody(['8.8.8.8', '::ffff:8.8.8.8', '1.1.1.1'], true), { ips: ['8.8.8.8', '1.1.1.1'], force: true })
  assert.deepEqual(geoLookupBody(['8.8.8.8'], 'true'), { ips: ['8.8.8.8'], force: false })
  for (const values of [[], Array(51).fill('8.8.8.8'), ['8.8.8.8', 'hostname.example']]) assert.throws(() => geoLookupBody(values))
})

test('geo results require the exact IP, an explicit status, source and real timestamps', () => {
  const entry = { ip: '8.8.8.8', status: 'success', country_code: 'US', country: 'United States', city: 'Synthetic city', source: 'GeoJS', fetched_at: 1700000000, expires_at: 1700086400, cached: false }
  assert.deepEqual(readGeoEntry({ items: [entry] }, '::ffff:8.8.8.8'), entry)
  assert.equal(geoLocationLabel(entry), 'United States · Synthetic city')
  assert.throws(() => readGeoEntry({ items: [entry] }, '1.1.1.1'))
  assert.throws(() => readGeoEntry({ items: [entry, entry] }, '8.8.8.8'))
  assert.throws(() => readGeoEntry({ items: [{ ...entry, fetched_at: undefined }] }, '8.8.8.8'))
  assert.throws(() => readGeoEntry({ items: [{ ...entry, source: 'unknown' }] }, '8.8.8.8'))
  assert.equal(readGeoEntry({ items: [{ ip: '8.8.8.8', status: 'error', error_code: 'not_found' }] }, '8.8.8.8').status, 'error')
  assert.equal(readGeoEntry({ items: [{ ip: '127.0.0.1', status: 'private' }] }, '127.0.0.1').status, 'private')
})

test('geo API makes only an explicit same-origin POST with IPs, force and no log contents', async () => {
  const original = globalThis.fetch
  const calls = []
  globalThis.fetch = async (url, options) => {
    calls.push({ url, options })
    return { ok: true, status: 200, json: async () => ({ items: [] }) }
  }
  try {
    geoIPState('8.8.8.8')
    geoLocationLabel({ country_code: 'US' })
    assert.equal(calls.length, 0)
    await lookupIPGeo(['8.8.8.8'], { force: true, account: 'must-not-leak', api_key: 'must-not-leak' })
    assert.equal(calls.length, 1)
    assert.equal(calls[0].url, '/api/logs/ip-geo')
    assert.equal(calls[0].options.method, 'POST')
    assert.equal(calls[0].options.credentials, 'same-origin')
    assert.deepEqual(JSON.parse(calls[0].options.body), { ips: ['8.8.8.8'], force: true })
    assert.equal(calls[0].options.headers.Authorization, undefined)
    await assert.rejects(async () => lookupIPGeo(['hostname.example']))
    assert.equal(calls.length, 1)
  } finally { globalThis.fetch = original }
})

test('geo batch freezes visible-page public IPs, chunks by 50 and preserves per-IP failure', async () => {
  const values = Array.from({ length: 52 }, (_, i) => `8.8.4.${i + 1}`)
  values.push('::ffff:8.8.4.1', '10.0.0.1', '', 'hostname.example')
  const plan = planGeoBatch(values)
  assert.equal(plan.ips.length, 52)
  assert.equal(plan.duplicate_count, 1)
  assert.equal(plan.private_count, 1)
  assert.equal(plan.invalid_count, 1)
  assert.equal(plan.missing_count, 1)
  const requests = []
  const completed = []
  const result = await runGeoBatch(values, async ips => {
    requests.push([...ips])
    values.push('9.9.9.9')
    return { items: ips.map(ip => ({ ip, status: 'error', error_code: 'not_found', error: 'Synthetic not found' })) }
  }, { onBatch: batch => completed.push(...batch) })
  assert.deepEqual(requests.map(ips => ips.length), [50, 2])
  assert.equal(requests.flat().includes('9.9.9.9'), false)
  assert.equal(result.length, 52)
  assert.equal(completed.length, 52)
  assert.equal(result.every(entry => entry.status === 'error'), true)
  assert.throws(() => planGeoBatch(Array(501).fill('8.8.8.8')))
})

test('geo cancellation prevents later chunks and retains only completed observations', async () => {
  const values = Array.from({ length: 51 }, (_, i) => `8.8.4.${i + 1}`)
  const controller = new AbortController()
  let calls = 0
  const completed = []
  await assert.rejects(runGeoBatch(values, async ips => {
    calls++
    return { items: ips.map(ip => ({ ip, status: 'error', error_code: 'not_found' })) }
  }, { signal: controller.signal, onBatch: batch => { completed.push(...batch); controller.abort() } }), { name: 'AbortError' })
  assert.equal(calls, 1)
  assert.equal(completed.length, 50)
})
