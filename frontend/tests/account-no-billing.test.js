import test from 'node:test'
import assert from 'node:assert/strict'
import { accountBundleInput } from '../src/utils/accountBundleInput.js'
import { validLeaseManagement } from '../src/utils/accountLeaseBackup.js'

test('legacy billing metadata is omitted in memory while credentials/resources/leases and original file object survive', () => {
  const original = { type: 'codex-agent-accounts', version: 1, accounts: [{ name: 'a', quota: 100, credentials: { access_token: 'synthetic-token' } }], resources: { version: 1, groups: [], proxies: [] }, management: { version: 1, costs: [{ intentionally: 'obsolete-unvalidated-metadata' }], leases: [{ account_index: 1, expires_at: 2100000000, auto_disable: true, restore_on_extend: false }] } }
  const before = JSON.stringify(original), result = accountBundleInput(original)
  assert.equal(JSON.stringify(original), before)
  assert.equal(Object.hasOwn(result.management, 'costs'), false)
  assert.equal(validLeaseManagement(result.management, 1), true)
  assert.equal(result.accounts[0].credentials.access_token, 'synthetic-token')
  assert.equal(result.accounts[0].quota, 100)
  assert.equal(result.resources, original.resources)
})
test('cost-only old management segment is removed and lease-free plain backups still import', () => {
  const plain = { type: 'codex-agent-accounts', version: 1, accounts: [] }
  assert.deepEqual(accountBundleInput(plain), plain)
  const result = accountBundleInput({ ...plain, management: { version: 1, costs: { malformed: true } } })
  assert.equal(Object.hasOwn(result, 'management'), false)
  const invalid = accountBundleInput({ ...plain, management: { version: 2, costs: [], leases: [{ bad: true }] } })
  assert.equal(validLeaseManagement(invalid.management, 1), false)
})
