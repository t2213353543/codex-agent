import test from 'node:test'
import assert from 'node:assert/strict'
import { createRequire } from 'node:module'
const { requestAllowed } = createRequire(import.meta.url)('../../integration/tests/management-readonly-policy.cjs')
const origin = 'http://127.0.0.1:43210', assets = { 'assets/index.js': 'digest' }
test('release browser permits only explicit read endpoints, query POSTs and session lifecycle', () => {
  for (const path of ['/api/logs', '/api/logs/errors', '/api/ingress-events', '/api/accounts/manage']) assert.equal(requestAllowed(origin + path, 'GET', origin), true)
  for (const path of ['/api/logs/snapshot', '/api/accounts/last-used', '/api/accounts/usage-batch', '/api/session']) assert.equal(requestAllowed(origin + path, 'POST', origin), true)
  assert.equal(requestAllowed(origin + '/api/session', 'DELETE', origin), true)
  assert.equal(requestAllowed(origin + '/assets/index.js', 'GET', origin, assets), true)
})
test('release browser rejects configuration, account writes, exports, probes and other origins', () => {
  for (const [method, path] of [['POST', '/api/settings'], ['POST', '/api/accounts'], ['DELETE', '/api/accounts/id'], ['POST', '/api/accounts/id/upstream'], ['POST', '/api/accounts/id/test-stream'], ['PUT', '/api/logs/id/review'], ['POST', '/api/logs/cleanup/jobs'], ['POST', '/api/logs/ip-geo'], ['GET', '/api/logs/export'], ['POST', '/responses'], ['GET', '/models'], ['GET', '/v1/models'], ['GET', '/assets/not-in-candidate.js']]) assert.equal(requestAllowed(origin + path, method, origin, assets), false, method + path)
  assert.equal(requestAllowed('https://other.test/api/session', 'POST', origin), false)
  assert.equal(requestAllowed('http://127.0.0.1:43211/api/session', 'POST', origin), false)
})
