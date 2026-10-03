import test from 'node:test'
import assert from 'node:assert/strict'
import { errorReviewPayload, errorReviewQuery, reviewAllowed, reviewRecordRef, validErrorReview } from '../src/utils/logErrorReviewState.js'

test('review targets stable event IDs and never treats successful requests as reviewable failures', () => {
  assert.equal(reviewRecordRef({ event_id: 'event', id: 7 }), 'event')
  assert.equal(reviewRecordRef({ id: 7 }), '7')
  for (const row of [{ id: -1 }, { id: NaN }, {}, { id: 7, status: 'ok' }]) assert.equal(reviewAllowed(row), false)
  assert.equal(reviewAllowed({ id: 7, status: 'error', http_code: 200 }), true)
  assert.equal(reviewAllowed({ event_id: 'event', status: 'aborted' }), true)
})
test('review save carries exact version and only management fields, preserving explicit false and literal text', () => {
  const review = { record_ref: 'event', version: 3, resolved: true, note: '', history: [] }
  assert.equal(validErrorReview(review), true)
  const draft = { resolved: false, note: '<img src=x onerror=alert(1)>', status: 'ok', token: 'secret' }
  assert.deepEqual(errorReviewPayload(review, draft), { version: 3, resolved: false, note: draft.note })
  assert.throws(() => errorReviewPayload(review, { ...draft, note: 'a'.repeat(1001) }), /1000/)
  assert.equal(errorReviewPayload(review, { ...draft, note: '🙂'.repeat(1000) }).note.length, 2000)
  assert.throws(() => errorReviewPayload({ ...review, version: -1 }, draft), /读取/)
})
test('error list retains entity filters while removing fixed snapshots and overriding main result state', () => {
  const query = errorReviewQuery({ account_id: 'account-stable', key_id: 'key-stable', from: '1700000000', to: '1700003600', status: 'ok', as_of: 'usage_old', cursor: 'main_cursor', model_mismatch: 'false', native_compaction: 'false', step: '3600', timezone: 'UTC' }, 'false', 'error-cursor', 50)
  assert.equal(query.account_id, 'account-stable'); assert.equal(query.key_id, 'key-stable')
  assert.equal(query.from, '1700000000'); assert.equal(query.to, '1700003600')
  assert.equal(query.resolved, 'false'); assert.equal(query.cursor, 'error-cursor'); assert.equal(query.page_size, '50')
  assert.equal(query.model_mismatch, 'false'); assert.equal(query.native_compaction, 'false')
  assert.equal(query.as_of, undefined); assert.equal(query.status, undefined)
  assert.equal(errorReviewQuery({}, '', '', 999).resolved, undefined)
  assert.equal(errorReviewQuery({}, '', '', 999).page_size, '20')
})
