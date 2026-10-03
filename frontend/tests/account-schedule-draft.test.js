import test from 'node:test'
import assert from 'node:assert/strict'
import { clearScheduleDraft, definiteScheduleRejection, readScheduleDraft, rememberScheduleDraft } from '../src/utils/accountScheduleDraft.js'
import { accountDateFilterError, compactAccountFilters, normalizeAccountFilters } from '../src/utils/accountManagement.js'
import { canonicalDailyTimezone } from '../src/utils/accountDailyStats.js'

const draft = () => ({ kind: 'stream', model: 'synthetic-model', cron: '0 * * * *', timezone: 'UTC', enabled: true, max_results: 50, confirm_inference: true, request_id: 'synthetic-request-12345' })
const memoryStorage = () => { const data = new Map(); return { getItem: key => data.get(key), setItem: (key, value) => data.set(key, value), removeItem: key => data.delete(key) } }

test('schedule retry retains exact identity and submitted values independently of mutable form', () => {
  const store = memoryStorage(), body = draft()
  const saved = rememberScheduleDraft('draft-original', body, store)
  body.model = 'changed'; saved.cron = '* * * * *'
  assert.deepEqual(readScheduleDraft('draft-original', store), draft())
  assert.equal(readScheduleDraft('other-account', store), null)
  clearScheduleDraft('draft-original', store)
  assert.equal(readScheduleDraft('draft-original', store), null)
})

test('schedule retry restores a session record and never keeps unknown extra fields', () => {
  const store = memoryStorage()
  store.setItem('codex-account-schedule-draft:restored-account', JSON.stringify({ ...draft(), ignored: 'not-a-plan-field' }))
  assert.deepEqual(readScheduleDraft('restored-account', store), draft())
  clearScheduleDraft('restored-account', store)
  store.setItem('codex-account-schedule-draft:invalid-account', JSON.stringify({ ...draft(), request_id: 'bad' }))
  assert.equal(readScheduleDraft('invalid-account', store), null)
  assert.throws(() => rememberScheduleDraft('invalid', { ...draft(), request_id: 'bad' }, store))
})

test('transport, server and conflict outcomes retain identity; definite rejection can unlock draft', () => {
  for (const error of [new Error('network'), { status: 409 }, { status: 500 }, { status: 502 }]) assert.equal(definiteScheduleRejection(error), false)
  for (const status of [400, 401, 403, 404, 413, 422]) assert.equal(definiteScheduleRejection({ status }), true)
})

test('out-of-range expiry cannot throw and preserves other valid account filters', () => {
  for (const value of ['99999999999999', '8640000000001', '1e100', 'invalid-date']) {
    const raw = { search: 'kept', expires_before: value, expires_after: 1700000000, sort_by: 'last_used_at', sort_order: 'asc' }
    assert.ok(accountDateFilterError(raw))
    assert.deepEqual(compactAccountFilters(normalizeAccountFilters(raw)), { search: 'kept', expires_after: 1700000000, sort_by: 'last_used_at', sort_order: 'asc' })
  }
  assert.equal(accountDateFilterError({ expires_before: '1700000000' }), '')
  assert.equal(accountDateFilterError({}), '')
})

test('account daily statistics submit canonical IANA names accepted by Go', () => {
  assert.equal(canonicalDailyTimezone('asia/shanghai'), 'Asia/Shanghai')
  assert.equal(canonicalDailyTimezone('utc'), 'UTC')
  for (const value of ['+08:00', 'Local', ' Asia/Shanghai ', 'not/a-zone']) assert.equal(canonicalDailyTimezone(value), '')
})
