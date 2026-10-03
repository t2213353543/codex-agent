import test from 'node:test'
import assert from 'node:assert/strict'
import { createDashboardRealtimePoller, validDashboardRealtime } from '../src/utils/dashboardRealtime.js'

const sample = (rpm = 12, tpm = 3456) => ({ scope: 'current_process', window_seconds: 60, rpm, tpm, updated_at: 1790519000, coverage_seconds: 60, warming_up: false, tokens_partial: false })
const settle = () => new Promise(resolve => setImmediate(resolve))
const deferred = () => { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no }); return { promise, resolve, reject } }

test('real-time response distinguishes zero, missing usage, partial coverage and invalid data', () => {
  assert.equal(validDashboardRealtime(sample(0, 0)), true)
  assert.equal(validDashboardRealtime(sample(1, null)), true)
  assert.equal(validDashboardRealtime({ ...sample(), coverage_seconds: 12, warming_up: true, tokens_partial: true }), true)
  for (const invalid of [{ ...sample(), window_seconds: 300 }, { ...sample(), rpm: null }, { ...sample(), tpm: -1 }, { ...sample(), coverage_seconds: 61 }, { ...sample(), updated_at: null }]) assert.equal(validDashboardRealtime(invalid), false)
})

test('polls every five seconds independently, pauses hidden and discards stale responses after abort', async () => {
  const requests = [], committed = [], errors = [], timers = new Map()
  let nextTimer = 0
  const poller = createDashboardRealtimePoller({
    fetch: signal => { const pending = deferred(); requests.push({ ...pending, signal }); return pending.promise },
    commit: value => committed.push(value), error: error => errors.push(error.message),
    setTimer: (callback, milliseconds) => { assert.equal(milliseconds, 5000); timers.set(++nextTimer, callback); return nextTimer },
    clearTimer: id => timers.delete(id),
  })
  poller.start()
  requests[0].resolve(sample())
  await settle()
  assert.equal(committed.length, 1)
  assert.equal(timers.size, 1)
  const tick = [...timers.values()][0]
  tick()
  assert.equal(requests.length, 2)
  poller.visibilityChanged(false)
  assert.equal(requests[1].signal.aborted, true)
  requests[1].resolve(sample(999))
  await settle()
  assert.equal(committed.length, 1)
  assert.equal(timers.size, 0)
  poller.visibilityChanged(true)
  assert.equal(requests.length, 3)
  requests[2].reject(Object.assign(new Error('stale login'), { status: 409 }))
  await settle()
  assert.deepEqual(errors, ['stale login'])
  assert.equal(committed[0].rpm, 12)
  assert.equal(timers.size, 1)
  poller.refresh()
  poller.stop()
  requests[3].resolve(sample(1000))
  await settle()
  assert.equal(committed.length, 1)
  assert.equal(timers.size, 0)
})

test('manual refresh supersedes slow requests and does not permit stale errors to replace success', async () => {
  const requests = [], commits = [], errors = []
  const poller = createDashboardRealtimePoller({
    fetch: signal => { const d = deferred(); requests.push({ ...d, signal }); return d.promise },
    commit: value => commits.push(value), error: error => errors.push(error), setTimer: () => 1, clearTimer: () => {},
  })
  poller.start(false)
  assert.equal(requests.length, 0)
  poller.visibilityChanged(true)
  poller.refresh()
  assert.equal(requests[0].signal.aborted, true)
  requests[1].resolve(sample(20, null)); await settle()
  requests[0].reject(new Error('old request')); await settle()
  assert.equal(commits.length, 1)
  assert.equal(commits[0].rpm, 20)
  assert.equal(commits[0].tpm, null)
  assert.deepEqual(errors, [])
  poller.stop()
})
