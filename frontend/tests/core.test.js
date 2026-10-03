import { test } from 'node:test'
import assert from 'node:assert/strict'
import { formatPercent, formatNumber } from '../src/utils/format.js'
import { createLatestRequest } from '../src/utils/latestRequest.js'
import { readPreference, writePreference } from '../src/utils/preferences.js'

test('percentage ratio, absent values and zero denominators', () => {
  assert.equal(formatPercent(0.8), '80%')
  assert.equal(formatPercent(0), '0%')
  assert.equal(formatPercent(1), '100%')
  assert.equal(formatPercent(null), '—')
  assert.equal(formatPercent(0, 0), '暂无样本')
  assert.equal(formatNumber(0), '0')
  assert.equal(formatNumber(undefined), '—')
})

test('a late response cannot replace the newest filter, even when abort is ignored', async () => {
  const latest = createLatestRequest()
  let releaseOld
  let oldSignal
  let value
  const first = latest.run(signal => { oldSignal = signal; return new Promise(resolve => { releaseOld = resolve }) }, data => { value = data })
  assert.equal(await latest.run(async () => 'new', data => { value = data }), true)
  releaseOld('old')
  assert.equal(await first, false)
  assert.equal(oldSignal.aborted, true)
  assert.equal(value, 'new')
  let releaseCanceled
  const pending = latest.run(() => new Promise(resolve => { releaseCanceled = resolve }), data => { value = data })
  latest.cancel()
  releaseCanceled('canceled')
  await pending
  assert.equal(value, 'new')
})

test('preferences reject corrupt JSON, unknown versions and invalid values', () => {
  const data = new Map()
  globalThis.localStorage = { getItem: key => data.get(key), setItem: (key, value) => data.set(key, value) }
  const valid = value => Array.isArray(value) && value.every(item => ['name', 'status'].includes(item))
  for (const invalid of ['{broken', '{"version":9,"value":["name"]}', '{"version":1,"value":["credential"]}']) {
    data.set('columns', invalid)
    assert.deepEqual(readPreference('columns', ['name'], valid), ['name'])
  }
  writePreference('columns', ['name', 'status'])
  assert.deepEqual(readPreference('columns', [], valid), ['name', 'status'])
})
