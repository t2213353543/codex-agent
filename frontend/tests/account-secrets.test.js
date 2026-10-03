import test from 'node:test'
import assert from 'node:assert/strict'
import { redactImportMessage } from '../src/utils/importAccounts.js'
test('nested OAuth, imported credential and agent-key errors redact all supplied secrets', () => {
  const row = { payload: { credentials: { tokens: { access_token: 'opaque-access', refresh_token: 'opaque-refresh' }, agent_identity: { agent_private_key: 'synthetic-private-key' } }, password: 'synthetic-password', accessToken: 'camel-access' } }
  const message = 'opaque-access opaque-refresh synthetic-private-key synthetic-password camel-access'
  assert.equal(redactImportMessage(message, [row]), '[REDACTED] [REDACTED] [REDACTED] [REDACTED] [REDACTED]')
})
test('redactor supports previous flat credential rows and cyclic defensive inputs', () => {
  const row = { credentials: { access_token: 'old-format-secret' } }; row.self = row
  assert.equal(redactImportMessage('upstream old-format-secret failed', [row]), 'upstream [REDACTED] failed')
})
