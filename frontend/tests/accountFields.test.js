import test from 'node:test'
import assert from 'node:assert/strict'
import { accountEditForm, accountEditPatch, validateAccountConfiguration } from '../src/utils/accountFields.js'

test('metadata editing preserves the accounting window and stable optimistic version', () => {
  const account = { name: 'sample', version: 7, quota: 5000, window_days: 14, concurrency: 3, weight: 2, priority: -4 }
  const form = accountEditForm(account)
  for (const key of ['quota', 'window_days', 'reset_window']) assert.equal(Object.hasOwn(form, key), false)
  form.notes = 'new note'
  const patch = accountEditPatch(account, form)
  assert.equal(patch.version, 7)
  assert.equal(patch.concurrency, 3)
  assert.equal(patch.priority, -4)
  assert.equal(patch.weight, 2)
  assert.equal('quota' in patch, false)
  assert.equal('window_days' in patch, false)
  assert.equal('reset_window' in patch, false)
  form.quota = 10000
  form.window_days = 30
  form.reset_window = true
  assert.deepEqual(Object.fromEntries(Object.entries(accountEditPatch(account, form)).filter(([key]) => ['quota', 'window_days', 'reset_window'].includes(key))), {})
})

test('account configuration validates integer limits without silently coercing empty values', () => {
  const form = accountEditForm({ name: '合成账号' })
  assert.equal(validateAccountConfiguration(form, true), '')
  for (const [key, value] of [['concurrency', undefined], ['concurrency', 1001], ['weight', 0], ['window_days', 366], ['quota', -1], ['quota', 1.5], ['priority', NaN]]) assert.ok(validateAccountConfiguration({ ...form, [key]: value }))
  assert.ok(validateAccountConfiguration({ ...form, name: '字'.repeat(41) }, true))
})
