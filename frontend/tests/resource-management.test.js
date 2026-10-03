import test from 'node:test'
import assert from 'node:assert/strict'
import { createResourceCSV, indexResources, resourceStatusSnapshot, runResourceStatusBatch, sortResources } from '../src/utils/resourceManagement.js'

test('resource sorting covers all rows before pagination and preserves source order', () => {
  const rows = [{ name: 'App 10', requests: 1 }, { name: 'App 2', requests: 80 }, { name: 'App 1', requests: 4 }]
  assert.deepEqual(sortResources(rows, 'requests:desc').slice(0, 2).map((row) => row.requests), [80, 4])
  assert.deepEqual(sortResources(rows, 'name:asc').map((row) => row.name), ['App 1', 'App 2', 'App 10'])
  assert.deepEqual(rows.map((row) => row.requests), [1, 80, 4])
  assert.equal(sortResources(rows, ''), rows)
})

test('batch snapshots discard credentials and duplicate or invalid IDs', () => {
  assert.deepEqual(resourceStatusSnapshot([
    { id: 'a', name: 'A', enabled: true, version: 5, key: 'secret', password: 'secret', username: 'secret' },
    { id: 'a', name: 'duplicate' }, { id: '' }, null,
  ]), [{ id: 'a', name: 'A', enabled: true, version: 5 }])
})

test('resource joins preserve missing IDs, first duplicate and distinct numeric/string IDs', () => {
  const groups = [{ id: 'a', name: 'First' }, { id: 'a', name: 'Later' }, { id: 1, name: 'Numeric' }, { id: '1', name: 'String' }, { id: '__proto__', name: 'Literal' }]
  const index = indexResources(groups)
  assert.equal(index.get('a'), groups[0])
  assert.equal(index.get(1).name, 'Numeric')
  assert.equal(index.get('1').name, 'String')
  assert.equal(index.get('__proto__').name, 'Literal')
  assert.equal(index.get('missing'), undefined)
  const csv = createResourceCSV('keys', [{ name: 'Key', groups: ['a', 'missing', 1, '1', '__proto__'] }], groups)
  assert.ok(csv.includes('First、missing、Numeric、String、Literal'))
  assert.ok(!csv.includes('Later'))
})

test('batch writes are sequential, versioned, and do not alter already-correct state', async () => {
  const calls = []
  let active = 0
  let maximum = 0
  const rows = [
    { id: 'a', name: 'A', enabled: true, version: 7 },
    { id: 'b', name: 'B', enabled: false, version: 8 },
    { id: 'c', name: 'C', enabled: true, version: 9 },
  ]
  const result = await runResourceStatusBatch(rows, false, async (id, patch) => {
    maximum = Math.max(maximum, ++active)
    calls.push({ id, patch })
    await new Promise((resolve) => setTimeout(resolve, 5))
    active--
    return { version: patch.version + 1, password: 'response-secret' }
  })
  assert.equal(maximum, 1)
  assert.deepEqual(calls, [{ id: 'a', patch: { enabled: false, version: 7 } }, { id: 'c', patch: { enabled: false, version: 9 } }])
  assert.deepEqual(result.map((row) => [row.id, row.ok, row.changed, row.version]), [['a', true, true, 8], ['b', true, false, 8], ['c', true, true, 10]])
  assert.equal(rows[0].enabled, true)
  assert.ok(!JSON.stringify(result).includes('response-secret'))
})

test('a conflict is reported independently and a retry can contain only failed fresh rows', async () => {
  const calls = []
  const first = await runResourceStatusBatch([{ id: 'a', enabled: true, version: 2 }, { id: 'b', enabled: true, version: 3 }], false, async (id) => {
    calls.push(id)
    if (id === 'a') throw Object.assign(new Error('raw-secret'), { status: 409 })
    return { version: 4 }
  })
  assert.deepEqual(first.map((row) => row.ok), [false, true])
  const retry = await runResourceStatusBatch(first.filter((row) => !row.ok).map((row) => ({ ...row, version: 8 })), false, async (id, patch) => {
    calls.push(id)
    assert.equal(patch.version, 8)
    return { version: 9 }
  })
  assert.deepEqual(calls, ['a', 'b', 'a'])
  assert.equal(retry[0].ok, true)
  assert.ok(!JSON.stringify(first).includes('raw-secret'))
})

test('invalid versions never write and auth failure stops subsequent submissions', async () => {
  const calls = []
  const result = await runResourceStatusBatch([
    { id: 'missing', enabled: false }, { id: 'zero', enabled: false, version: 0 },
    { id: 'string', enabled: false, version: '5' }, { id: 'a', enabled: false, version: 2 },
    { id: 'b', enabled: false, version: 3 },
  ], true, async (id) => { calls.push(id); throw Object.assign(new Error('hidden'), { status: 401 }) })
  assert.deepEqual(calls, ['a'])
  assert.equal(result.filter((row) => !row.ok).length, 5)
})

test('unmount cancellation stops the queue after the in-flight result', async () => {
  let mounted = true
  const calls = []
  const result = await runResourceStatusBatch([{ id: 'a', enabled: false, version: 2 }, { id: 'b', enabled: false, version: 3 }], true, async (id) => {
    calls.push(id)
    mounted = false
    return { version: 3 }
  }, { shouldContinue: () => mounted })
  assert.deepEqual(calls, ['a'])
  assert.equal(result.length, 1)
})

test('CSV uses metadata allowlists and excludes all credential fragments and proxy URLs', () => {
  const secretFields = { key: 'FULL_SECRET', key_masked: 'MASK_SECRET', password: 'PASS_SECRET', username: 'USER_SECRET', access_token: 'ACCESS_SECRET', refresh_token: 'REFRESH_SECRET', host: 'http://URL_SECRET@example.test', error: 'ERROR_SECRET' }
  for (const kind of ['keys', 'proxies', 'groups']) {
    const csv = createResourceCSV(kind, [{ ...secretFields, id: 'INTERNAL_SECRET', name: 'Safe resource', description: 'Metadata only', groups: ['g1'], enabled: true, requests: 3, tokens: 9, errors: 0, type: 'http' }], [{ id: 'g1', name: 'Group A' }])
    assert.ok(csv.startsWith('\ufeff'))
    assert.ok(csv.includes('Safe resource'))
    assert.ok(!csv.includes('_SECRET'))
    if (kind === 'keys') assert.ok(csv.includes('Group A'))
  }
})

test('CSV escapes quotes/newlines and prevents formula execution including leading whitespace', () => {
  const csv = createResourceCSV('groups', [{ name: '\t =HYPERLINK("evil")', description: 'a,"b"\r\nc' }])
  assert.ok(csv.includes('"\'\t =HYPERLINK(""evil"")"'))
  assert.ok(csv.includes('"a,""b""\r\nc"'))
  assert.throws(() => createResourceCSV('unknown', []), /Unsupported/)
})
