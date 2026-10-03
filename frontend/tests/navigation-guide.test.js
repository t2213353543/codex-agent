import test from 'node:test'
import assert from 'node:assert/strict'
import { managementPages, searchManagementPages } from '../src/utils/navigation.js'
import { gatewayBaseURL, clientConfiguration, clientKeyCommand } from '../src/utils/connectionGuide.js'
test('quick access searches every management page in Chinese and English', () => {
  assert.equal(new Set(managementPages.map(page => page.path)).size, managementPages.length)
  assert.equal(searchManagementPages('账号')[0].path, '/accounts')
  assert.equal(searchManagementPages('API key')[0].path, '/keys')
  assert.equal(searchManagementPages('health')[0].path, '/diagnostics')
  assert.equal(searchManagementPages('unavailable-page').length, 0)
})
test('connection templates normalize the origin and reject model or URL injection', () => {
  assert.equal(gatewayBaseURL('https://example.test:18879/#/connection'), 'https://example.test:18879/v1')
  assert.throws(() => gatewayBaseURL('javascript:alert(1)'))
  assert.throws(() => gatewayBaseURL('https://user:secret@example.test'))
  assert.throws(() => clientConfiguration('https://example.test', 'model"\nenv_key="attack'))
  assert.match(clientConfiguration('https://example.test', 'example-model'), /wire_api = "responses"/)
  assert.match(clientConfiguration('https://example.test', 'example-model'), /env_key = "CODEX_AGENT_KEY"/)
  assert.match(clientConfiguration('https://example.test', 'example-model'), /name = "OpenAI"/)
  assert.match(clientConfiguration('https://example.test', 'example-model'), /model_provider = "powerpoint_gateway"/)
  assert.match(clientConfiguration('https://example.test', 'example-model'), /\[model_providers\.powerpoint_gateway\]/)
  assert.match(clientConfiguration('https://example.test', 'example-model'), /supports_websockets = true/)
  assert.match(clientConfiguration('https://example.test', 'example-model'), /model_reasoning_effort = "xhigh"/)
  assert.match(clientKeyCommand('powershell'), /^\$env:CODEX_AGENT_KEY/)
  assert.match(clientKeyCommand('shell'), /^export CODEX_AGENT_KEY=/)
})
