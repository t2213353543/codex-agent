import test from 'node:test'
import assert from 'node:assert/strict'
import { createAccountListReader } from '../src/utils/accountListReader.js'
import { clearSession } from '../src/api.js'

const payload = { accounts: [{ id: 'a1', version: 1 }], total: 1, total_kind: 'exact' }
test('list conditional reads retain exact data only for the same filter and login', async () => {
  const calls = []
  const read = createAccountListReader({ fetchImpl: async (url, options) => { calls.push({ url, options }); return options.headers['If-None-Match'] ? new Response(null, { status: 304 }) : Response.json(payload, { headers: { ETag: '"fixture"' } }) } })
  assert.deepEqual(await read({ page: 1, search: 'first' }), payload)
  assert.deepEqual(await read({ search: 'first', page: 1 }), payload)
  assert.equal(calls[1].options.headers['If-None-Match'], '"fixture"')
  await read({ page: 2, search: 'first' }); assert.equal(calls[2].options.headers['If-None-Match'], undefined)
  clearSession()
  await read({ page: 1, search: 'first' }); assert.equal(calls[3].options.headers['If-None-Match'], undefined)
})
test('empty 304 and malformed pagination cannot fabricate valid account results', async () => {
  await assert.rejects(createAccountListReader({ fetchImpl: async () => new Response(null, { status: 304 }) })({}), /缓存已失效/)
  await assert.rejects(createAccountListReader({ fetchImpl: async () => Response.json({ accounts: [], total: 100 }) })({}), /完整账号分页/)
})
test('late response after account logout is rejected and never cached', async () => {
  const read = createAccountListReader({ fetchImpl: async () => { clearSession(); return Response.json(payload) } })
  await assert.rejects(read({}), { name: 'AbortError' })
})
