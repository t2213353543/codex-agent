import test from 'node:test'
import assert from 'node:assert/strict'
import { relatedLogResource } from '../src/utils/logRelatedResource.js'
test('related resources resolve stable identity and retain only display metadata', () => {
  const result = relatedLogResource({ keys: [{ id: 'other', name: 'Same' }, { id: 'wanted', name: 'Same', enabled: false, groups: ['g'], key: 'secret', key_masked: 'masked', access_token: 'token', arbitrary: 'unsafe' }] }, 'key', 'wanted')
  assert.deepEqual(result, { id: 'wanted', name: 'Same', enabled: false, groups: ['g'] })
  assert.equal(relatedLogResource({ keys: [{ id: 'other', name: 'Same' }] }, 'key', 'deleted'), null)
  assert.deepEqual(relatedLogResource({ groups: [{ id: 'g', name: 'Group', description: 'Current', accounts: 0, keys: 0, secret: 'not-displayed' }] }, 'group', 'g'), { id: 'g', name: 'Group', description: 'Current', accounts: 0, keys: 0 })
  assert.throws(() => relatedLogResource({}, 'group', 'g'))
})
