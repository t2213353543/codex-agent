import test from 'node:test'
import assert from 'node:assert/strict'
import { leaseRestoreItems, mergeLeaseRestoreResults, validLeaseManagement, validLeaseRestore } from '../src/utils/accountLeaseBackup.js'
const config = { account_index: 1, expires_at: 2100000000, auto_disable: true, restore_on_extend: true }
const item = { ...config, name: 'fixture', id: 'new-stable-id', account_version: 4, lease_version: 2, status: 'pending', replaces_existing: true }
test('lease backup permits old bundles and only configuration indexed by account row', () => {
  assert.equal(validLeaseManagement(undefined, 1), true)
  assert.equal(validLeaseManagement({ version: 1, leases: [config] }, 1), true)
  for (const value of [{ ...config, disabled_by_lease: true }, { ...config, account_index: 2 }, { ...config, expires_at: -1 }, { ...config, auto_disable: undefined }]) assert.equal(validLeaseManagement({ version: 1, leases: [value] }, 1), false)
  assert.equal(validLeaseManagement({ version: 1, leases: [config, config] }, 2), false)
})
test('lease targets freeze returned ID and dual versions without names or secrets', () => {
  const receipt = { receipt_id: 'receipt-1', dry_run: false, items: [item, { ...item, account_index: 2, status: 'applied' }, { ...item, account_index: 3, status: 'unknown' }] }
  assert.deepEqual(leaseRestoreItems(receipt), [{ account_index: 1, id: 'new-stable-id', account_version: 4, lease_version: 2 }])
  assert.equal(validLeaseRestore({ ...receipt, items: [{ ...item, account_version: 0 }] }), false)
})
test('lease receipt merging rejects missing, duplicate and swapped targets; retains completed rows', () => {
  const receipt = { receipt_id: 'receipt-1', dry_run: false, items: [item, { ...item, account_index: 2, status: 'applied' }] }
  const targets = leaseRestoreItems(receipt)
  const response = { receipt_id: 'receipt-1', dry_run: false, items: [{ ...item, status: 'applied', result_lease_version: 3 }] }
  const merged = mergeLeaseRestoreResults(receipt, response, targets)
  assert.equal(merged.items[0].status, 'applied'); assert.equal(merged.items[1].status, 'applied'); assert.equal(receipt.items[0].status, 'pending')
  for (const invalid of [{ ...response, receipt_id: 'other' }, { ...response, items: [] }, { ...response, items: [{ ...item, id: 'other-target' }] }, { ...response, items: [{ ...item, account_version: 5 }] }]) assert.throws(() => mergeLeaseRestoreResults(receipt, invalid, targets))
})
