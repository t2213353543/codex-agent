import test from 'node:test'
import assert from 'node:assert/strict'
import { importReport, parseImportDocuments, parseImportText, prepareImport } from '../src/utils/importAccounts.js'

test('same JWT header and shared workspace do not collide', async () => {
  const rows = await prepareImport(JSON.stringify([
    { access_token: 'eyJhbGciOiJIUzI1NiJ9.person-one.signature', account_id: 'shared-workspace' },
    { access_token: 'eyJhbGciOiJIUzI1NiJ9.person-two.signature', account_id: 'shared-workspace' },
  ]))
  assert.equal(rows.length, 2)
  assert.notEqual(rows[0].name, rows[1].name)
  assert.ok(rows.every((row) => row.kind === 'new' && row.name.length <= 100))
})

test('complete multiline auth.json and wrapped credentials each produce one account', async () => {
  const auth = '\n{\n  "tokens": {\n    "access_token": "token-a",\n    "refresh_token": "refresh-a"\n  }\n}'
  const rows = await prepareImport(auth)
  assert.equal(rows.length, 1)
  assert.equal(rows[0].credentials.refresh_token, 'refresh-a')
  assert.equal(rows[0].line, 2)
  assert.equal(parseImportText('{"credentials":{"access_token":"token-b"}}')[0].credentials.access_token, 'token-b')
})

test('array element validation retains source line numbers', () => {
  const rows = parseImportText('[\n {"access_token":"valid"},\n {"access_token":42},\n null\n]')
  assert.equal(rows[1].line, 3)
  assert.equal(rows[1].kind, 'failed')
  assert.equal(rows[2].line, 4)
  assert.equal(rows[2].kind, 'failed')
})

test('JSONL errors have exact line and never become token credentials', () => {
  const rows = parseImportText('{"access_token":"good"}\n\n{"access_token": broken}\n{"access_token":"also-good"}')
  assert.equal(rows.length, 3)
  assert.equal(rows[1].line, 3)
  assert.equal(rows[1].kind, 'failed')
  assert.equal(rows[1].credentials, undefined)
})

test('malformed multiline JSON is a single failure, without secret snippets', () => {
  const rows = parseImportText('{\n "access_token": "secret-token",\n "oops": broken\n}')
  assert.equal(rows.length, 1)
  assert.equal(rows[0].kind, 'failed')
  assert.equal(rows[0].line, 3)
  assert.ok(!rows[0].message.includes('secret-token'))
})

test('plain token lines, malformed JSON and mixed formats are distinguished', () => {
  assert.equal(parseImportText('token-one\n\ntoken-two').length, 2)
  for (const value of ['{"access_token":}', '{\n"access_token":\n}', '[{"access_token":}]', 'name|token', 'null', '123']) {
    assert.ok(parseImportText(value).every((row) => row.kind === 'failed'), value)
  }
  assert.equal(parseImportText('token\n{"access_token":"another"}')[1].kind, 'failed')
})

test('duplicates are skipped and conflicting names require explicit replacement', async () => {
  const rows = await prepareImport('[{"name":"exists","access_token":"one"},{"name":"other","access_token":"one"},{"name":"exists","access_token":"two"}]', [{ name: 'exists' }])
  assert.equal(rows[0].kind, 'replace')
  assert.equal(rows[0].action, 'skip')
  assert.equal(rows[1].kind, 'duplicate')
  assert.equal(rows[1].action, 'skip')
  assert.equal(rows[2].kind, 'failed')
})

test('generated names are deterministic and existing tokens default to skip', async () => {
  const first = await prepareImport('access-one')
  const second = await prepareImport('access-one', [{ name: first[0].name }])
  assert.equal(first[0].name, second[0].name)
  assert.equal(second[0].kind, 'duplicate')
  assert.equal(second[0].action, 'skip')
})

test('invalid credential fields and long UTF-8 names fail before submission', () => {
  for (const object of [{ access_token: 'x', refresh_token: null }, { tokens: [] }, { access_token: 'x', expires_at: 'tomorrow' }, { access_token: 'x', expires_at: -1 }, { access_token: 'x'.repeat(128 * 1024 + 1) }, { name: '账'.repeat(34), access_token: 'x' }]) {
    assert.equal(parseImportText(JSON.stringify(object))[0].kind, 'failed')
  }
})

test('download reports contain no credentials or server-echoed secrets', async () => {
  const rows = await prepareImport('{"name":"account","access_token":"test-access-secret","refresh_token":"test-refresh-secret"}')
  const report = importReport(rows, [{ line: 1, status: 'failed', message: 'Rejected test-access-secret / test-refresh-secret' }])
  const serialized = JSON.stringify(report)
  assert.ok(!serialized.includes('test-access-secret'))
  assert.ok(!serialized.includes('test-refresh-secret'))
  assert.ok(!serialized.includes('credentials'))
  assert.equal(report[0].result, 'failed')
})

test('same-line JSON array reports keep separate results per item', () => {
  const rows = [{ sourceIndex: 0, line: 1, name: 'one', kind: 'new', action: 'add' }, { sourceIndex: 1, line: 1, name: 'two', kind: 'new', action: 'add' }]
  const report = importReport(rows, [{ sourceIndex: 0, line: 1, status: 'success' }, { sourceIndex: 1, line: 1, status: 'failed', message: 'conflict' }])
  assert.deepEqual(report.map((item) => item.result), ['success', 'failed'])
  assert.deepEqual(report.map((item) => item.item), [1, 2])
})

test('Sub2API exports preserve source configuration and credential metadata for commit', () => {
  const original = { name: 'exported-account', platform: 'openai', type: 'oauth', concurrency: 6, priority: 3, enabled: false, notes: 'source note', credentials: { access_token: 'synthetic-access', refresh_token: 'synthetic-refresh', id_token: 'synthetic-id', chatgpt_account_id: 'workspace', expires_at: 1800000000, model_mapping: { source: 'target' } }, extra: { privacy_mode: 'training_disabled' }, rate_multiplier: 2 }
  const rows = parseImportText(JSON.stringify({ exported_at: '2026-09-17T00:00:00Z', proxies: [], accounts: [original] }, null, 2))
  assert.equal(rows.length, 1)
  assert.equal(rows[0].kind, undefined)
  assert.equal(rows[0].format, 'Sub2API')
  assert.deepEqual(rows[0].payload, original)
  assert.equal(rows[0].line, 5)
  assert.equal(rows[0].externalBindings, false)
})

test('export headers and empty envelopes fail with useful errors', () => {
  for (const object of [{ accounts: [] }, { accounts: {} }, { accounts: [{ access_token: 'x' }], proxies: null }, { accounts: [{ access_token: 'x' }], proxies: {} }, { type: 'another-app', accounts: [{ access_token: 'x' }] }, { version: 2, accounts: [{ access_token: 'x' }] }]) {
    const rows = parseImportText(JSON.stringify(object))
    assert.equal(rows.length, 1)
    assert.equal(rows[0].kind, 'failed')
  }
  assert.equal(parseImportText(JSON.stringify({ type: 'sub2api-bundle', version: 1, accounts: [{ access_token: 'x' }] }))[0].credentials.access_token, 'x')
})

test('multiple files retain their names, row indices and independent parse failures', () => {
  const rows = parseImportDocuments([{ name: 'one.json', text: '{"accounts":[{"access_token":"one"}]}' }, { name: 'broken.json', text: '{"access_token": broken}' }, { name: 'three.jsonl', text: '{"access_token":"two"}\n{"access_token":"three"}' }])
  assert.equal(rows.length, 4)
  assert.deepEqual(rows.map((row) => row.sourceIndex), [0, 1, 2, 3])
  assert.deepEqual(rows.map((row) => row.sourceFile), ['one.json', 'broken.json', 'three.jsonl', 'three.jsonl'])
  assert.equal(rows[1].kind, 'failed')
  assert.equal(rows[3].line, 2)
})

test('named token lines preserve a Chinese alias and reject empty sides', () => {
  const row = parseImportText('  账号一 ---- synthetic-token  ')[0]
  assert.equal(row.name, '账号一')
  assert.equal(row.credentials.access_token, 'synthetic-token')
  assert.equal(row.payload.name, '账号一')
  for (const source of ['----token', 'name----', 'name----bad token']) assert.equal(parseImportText(source)[0].kind, 'failed')
})

test('Sub2API management expiry does not become credential expiry and external bindings are flagged', () => {
  const row = parseImportText(JSON.stringify({ platform: 'openai', expires_at: 1800000000, proxy_key: 'remote-proxy', group_ids: [99], credentials: { access_token: 'synthetic-access' } }))[0]
  assert.equal(row.credentials.expires_at, undefined)
  assert.equal(row.payload.expires_at, 1800000000)
  assert.equal(row.externalBindings, true)
  assert.equal(parseImportText('{"tokens":{"access_token":"synthetic"},"expires_at":1800000001}')[0].credentials.expires_at, 1800000001)
})

test('identity tokens are redacted from failure reports', () => {
  const rows = parseImportDocuments([{ name: 'account.json', text: '{"credentials":{"access_token":"access-secret","id_token":"identity-secret"}}' }])
  const report = importReport(rows, [{ sourceIndex: 0, status: 'failed', message: 'Rejected identity-secret and access-secret' }])
  assert.ok(!JSON.stringify(report).includes('identity-secret'))
  assert.ok(!JSON.stringify(report).includes('access-secret'))
})

test('account count limit applies equally to tokens, JSONL, arrays and multiple files', () => {
  const count = 10001
  for (const source of [Array(count).fill('synthetic-token').join('\n'), Array(count).fill('{"access_token":"synthetic"}').join('\n'), JSON.stringify(Array(count).fill({ access_token: 'synthetic' }))]) {
    const rows = parseImportText(source)
    assert.equal(rows.length, 1)
    assert.equal(rows[0].kind, 'failed')
    assert.match(rows[0].message, /10000/)
  }
  const rows = parseImportDocuments([{ name: 'first.txt', text: Array(6000).fill('synthetic').join('\n') }, { name: 'second.txt', text: Array(4001).fill('synthetic').join('\n') }])
  assert.equal(rows.length, 1)
  assert.equal(rows[0].kind, 'failed')
})

test('historical nested credential wrappers unwrap without dropping outer metadata', () => {
  const row = parseImportText('{"name":"nested","concurrency":5,"credentials":{"tokens":{"access_token":"nested-token","refresh_token":"nested-refresh"}}}')[0]
  assert.equal(row.credentials.access_token, 'nested-token')
  assert.equal(row.payload.concurrency, 5)
  assert.deepEqual(row.payload.credentials, { access_token: 'nested-token', refresh_token: 'nested-refresh' })
  let nested = { access_token: 'depth-token' }
  for (let depth = 0; depth < 8; depth++) nested = { tokens: nested }
  assert.equal(parseImportText(JSON.stringify(nested))[0].credentials.access_token, 'depth-token')
  assert.equal(parseImportText(JSON.stringify({ tokens: nested }))[0].kind, 'failed')
})
