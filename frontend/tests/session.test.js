import test from 'node:test'
import assert from 'node:assert/strict'

let moduleSequence = 0
const response = (payload, status = 200) => ({ ok: status >= 200 && status < 300, status, json: async () => payload })
const deferred = () => {
  let resolve
  let reject
  const promise = new Promise((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}
async function fixture(t, fetchImplementation) {
  const originals = Object.fromEntries(['fetch', 'window', 'localStorage', 'sessionStorage'].map((key) => [key, Object.getOwnPropertyDescriptor(globalThis, key)]))
  const local = new Map([['adminKey', 'old-persisted-secret']])
  const session = new Map([['adminKey', 'old-session-secret']])
  const storage = (map) => ({ getItem: (key) => map.get(key) ?? null, setItem: (key, value) => map.set(key, String(value)), removeItem: (key) => map.delete(key) })
  const events = []
  const calls = []
  globalThis.localStorage = storage(local)
  globalThis.sessionStorage = storage(session)
  globalThis.window = { dispatchEvent(event) { events.push(event); return true } }
  globalThis.fetch = (url, options) => {
    calls.push({ url, options })
    return fetchImplementation(url, options)
  }
  t.after(() => {
    for (const [key, descriptor] of Object.entries(originals)) {
      if (descriptor) Object.defineProperty(globalThis, key, descriptor)
      else delete globalThis[key]
    }
  })
  const client = await import(`../src/api.js?session-test=${++moduleSequence}`)
  return { client, events, calls, local, session }
}

test('cookie login purges legacy keys and never saves or sends a bearer key', async (t) => {
  const { client, calls, local, session } = await fixture(t, async () => response({ authenticated: true }))
  assert.equal(local.has('adminKey'), false)
  assert.equal(session.has('adminKey'), false)
  await client.loginSession('mock-admin-key')
  assert.equal(client.hasSession(), true)
  assert.equal(calls[0].url, '/api/session')
  assert.equal(calls[0].options.method, 'POST')
  assert.equal(calls[0].options.credentials, 'same-origin')
  assert.equal(calls[0].options.headers.Authorization, undefined)
  assert.deepEqual(JSON.parse(calls[0].options.body), { admin_key: 'mock-admin-key' })
  assert.equal(local.size, 0)
  assert.equal(session.size, 0)
})

test('concurrent initial session checks share one request and cache the result', async (t) => {
  const pending = deferred()
  const { client, calls } = await fixture(t, () => pending.promise)
  const first = client.ensureSession()
  const second = client.ensureSession()
  assert.equal(calls.length, 1)
  pending.resolve(response({ authenticated: true }))
  assert.deepEqual(await Promise.all([first, second]), [true, true])
  assert.equal(await client.ensureSession(), true)
  assert.equal(calls.length, 1)
})

test('an unauthenticated session check does not emit a duplicate API error', async (t) => {
  const { client, events } = await fixture(t, async () => response({}, 401))
  assert.equal(await client.ensureSession(), false)
  assert.equal(client.hasSession(), false)
  assert.equal(events.length, 0)
})

test('only an explicit authenticated response completes login', async (t) => {
  const { client } = await fixture(t, async () => response({ authenticated: false }))
  await assert.rejects(client.loginSession('mock-key'), /服务器未确认登录/)
  assert.equal(client.hasSession(), false)
})

test('current-session 401 expires authentication and aborts other in-flight work', async (t) => {
  const pending = deferred()
  const { client, calls, events } = await fixture(t, async (url) => {
    if (url === '/api/session') return response({ authenticated: true })
    if (url === '/api/slow') return pending.promise
    return response({}, 401)
  })
  await client.loginSession('mock-key')
  const slow = client.request('/slow').catch((error) => error)
  await assert.rejects(client.request('/expired'), (error) => error.status === 401)
  assert.equal(client.hasSession(), false)
  assert.equal(calls.find((call) => call.url === '/api/slow').options.signal.aborted, true)
  pending.resolve(response({ shouldNotCommit: true }))
  assert.equal((await slow).name, 'AbortError')
  assert.equal(events.filter((event) => event.type === 'auth-expired').length, 1)
})

test('an old request 401 cannot invalidate a newer successful login', async (t) => {
  const pending = deferred()
  const { client, events } = await fixture(t, async (url) => url === '/api/slow' ? pending.promise : response({ authenticated: true }))
  const stale = client.request('/slow').catch((error) => error)
  await client.loginSession('mock-key')
  pending.resolve(response({}, 401))
  assert.equal((await stale).status, 401)
  assert.equal(client.hasSession(), true)
  assert.equal(events.filter((event) => event.type === 'auth-expired').length, 0)
})

test('logout failure keeps authentication and reports the server failure', async (t) => {
  const { client, events } = await fixture(t, async (_url, options) => options.method === 'DELETE' ? response({ error: { message: 'mock logout failed' } }, 503) : response({ authenticated: true }))
  await client.loginSession('mock-key')
  await assert.rejects(client.logoutSession(), /mock logout failed/)
  assert.equal(client.hasSession(), true)
  assert.equal(events.find((event) => event.type === 'api-error')?.detail, 'mock logout failed')
})

test('successful logout aborts pending requests even if the transport ignores abort', async (t) => {
  const pending = deferred()
  const { client } = await fixture(t, async (url) => url === '/api/slow' ? pending.promise : response({ authenticated: true }))
  await client.loginSession('mock-key')
  const stale = client.request('/slow').catch((error) => error)
  await client.logoutSession()
  pending.resolve(response({ stale: true }))
  assert.equal((await stale).name, 'AbortError')
  assert.equal(client.hasSession(), false)
})

test('explicit cancellation is forwarded and remains silent', async (t) => {
  const pending = deferred()
  const { client, calls, events } = await fixture(t, () => pending.promise)
  const controller = new AbortController()
  const task = client.api.logs({ account: 'mock' }, { signal: controller.signal }).catch((error) => error)
  controller.abort()
  assert.equal(calls[0].options.signal.aborted, true)
  pending.resolve(response({ items: [] }))
  assert.equal((await task).name, 'AbortError')
  assert.equal(events.length, 0)
})

test('cancelling while JSON response body is read remains an AbortError without a toast', async (t) => {
  const body = deferred()
  const reading = deferred()
  const { client, events } = await fixture(t, async () => ({ ok: true, status: 200, json() { reading.resolve(); return body.promise } }))
  const controller = new AbortController()
  const task = client.request('/slow-body', { signal: controller.signal }).catch((error) => error)
  await reading.promise
  controller.abort()
  body.reject(new DOMException('The operation was aborted', 'AbortError'))
  assert.equal((await task).name, 'AbortError')
  assert.equal(events.length, 0)
})

test('an old session probe cannot overwrite a newer successful login', async (t) => {
  const pending = deferred()
  const { client } = await fixture(t, async (_url, options) => options.method === 'POST' ? response({ authenticated: true }) : pending.promise)
  const check = client.ensureSession()
  await client.loginSession('mock-key')
  pending.resolve(response({}, 401))
  await check
  assert.equal(client.hasSession(), true)
})

test('a cancelled old session probe stays silent and cannot clear a newer shared check', async (t) => {
  const oldProbe = deferred()
  const newProbe = deferred()
  let count = 0
  const { client, events, calls } = await fixture(t, () => ++count === 1 ? oldProbe.promise : newProbe.promise)
  const oldCheck = client.ensureSession()
  client.clearSession()
  const currentCheck = client.ensureSession()
  oldProbe.resolve(response({ authenticated: true }))
  await oldCheck
  const sharedCurrentCheck = client.ensureSession()
  newProbe.resolve(response({ authenticated: true }))
  assert.deepEqual(await Promise.all([currentCheck, sharedCurrentCheck]), [true, true])
  assert.equal(calls.length, 2)
  assert.equal(events.length, 0)
})
