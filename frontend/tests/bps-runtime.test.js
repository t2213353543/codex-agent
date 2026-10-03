import test from 'node:test'
import assert from 'node:assert/strict'
import { inferenceRuntime, productName } from '../src/utils/bpsRuntime.js'

test('PowerPoint runtime uses explicit server values and does not invent effective defaults', () => {
  assert.equal(productName, '高级反代系统')
  const unknown = inferenceRuntime()
  assert.equal(unknown.known, false)
  assert.equal(unknown.search, '未提供')
  assert.equal(unknown.baseURL, '')
  const runtime = inferenceRuntime({ backend: 'powerpoint_bps', profile: 'powerpoint', base_url: 'https://bps.openai.com/basispoints/api', tools_version: 'actual-tools-version', web_search: 'disabled', codex_fallback: false })
  assert.equal(runtime.known, true)
  assert.equal(runtime.toolsVersion, 'actual-tools-version')
  assert.equal(runtime.search, '已关闭')
  assert.equal(runtime.codexFallback, '未启用')
})

test('official runtime modes display the selected backend without claiming PowerPoint', () => {
  for (const [backend, label] of [['official_codex_cli', 'Codex 直连']]) {
    const runtime = inferenceRuntime({ backend, base_url: 'https://chatgpt.com/backend-api/codex' })
    assert.equal(runtime.known, true)
    assert.equal(runtime.label, label)
    assert.equal(runtime.mode, backend)
    assert.equal(runtime.toolsVersion, '')
  }
  assert.equal(inferenceRuntime({ backend: 'official_codex_cli', profile: 'app_server_cli' }).label, 'Codex 直连')
})

test('removed runtime is not offered', () => {
  assert.equal(inferenceRuntime({ backend: 'official_app_server' }).known, false)
})

test('runtime display excludes URL credentials, query secrets and unsupported protocols', () => {
  assert.equal(inferenceRuntime({ base_url: 'https://user:secret@example.test/api' }).baseURL, '')
  assert.equal(inferenceRuntime({ base_url: 'javascript:alert(1)' }).baseURL, '')
  assert.equal(inferenceRuntime({ base_url: 'https://example.test/api?token=secret#fragment' }).baseURL, 'https://example.test/api')
})
