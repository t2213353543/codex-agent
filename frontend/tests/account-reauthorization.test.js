import test from 'node:test'
import assert from 'node:assert/strict'
import { parseBatchReauthorization } from '../src/utils/accountReauthorization.js'
const targets = [{ id: 'a1', version: 7, name: '账号1' }, { id: 'a2', version: 9 }]
test('batch replacement uses selected stable IDs and versions, preserving identity fields', () => {
  const [row] = parseBatchReauthorization(JSON.stringify([{ id: 'a1', credentials: { access_token: 'synthetic', agent_identity: { user_id: 'u1', workspace_id: 'w1' } } }]), targets)
  assert.equal(row.version, 7); assert.equal(row.allow_identity_change, false); assert.equal(row.credentials.agent_identity.user_id, 'u1')
})
test('unselected, duplicate, stale and malformed targets cannot enter a credential batch', () => {
  for (const rows of [[{ id: 'a3', credentials: { access_token: 's' } }], [{ id: 'a1', version: 6, credentials: { access_token: 's' } }], [{ id: 'a1', credentials: { access_token: 's' } }, { id: 'a1', credentials: { access_token: 't' } }], [{ id: 'a1', credentials: { access_token: 'invalid space' } }]]) assert.throws(() => parseBatchReauthorization(JSON.stringify(rows), targets))
})
test('JSON failures never echo a supplied token or parser fragment', () => {
  assert.throws(() => parseBatchReauthorization('[{"credentials":"synthetic-private', targets), error => !error.message.includes('synthetic-private'))
})
