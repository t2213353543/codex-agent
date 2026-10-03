import test from 'node:test'
import assert from 'node:assert/strict'
import { isValidExitIP, proxyCheckPassed } from '../src/utils/management.js'

test('proxy diagnostics require successful HTTP and a valid exit IP', () => {
  for (const http_code of [403, 407, 429, 500, 0, undefined]) {
    assert.equal(proxyCheckPassed({ ok: true, http_code, ip: '203.0.113.1' }), false)
  }
  for (const ip of ['', '未知', '999.1.1.1', '<b>203.0.113.1</b>', 'http://example.com', '1:2:3', ' 203.0.113.1']) {
    assert.equal(proxyCheckPassed({ ok: true, http_code: 200, ip }), false)
  }
  assert.equal(proxyCheckPassed({ ok: false, http_code: 200, ip: '203.0.113.1' }), false)
  assert.equal(proxyCheckPassed({ ok: true, http_code: 200, ip: '203.0.113.1' }), true)
  assert.equal(proxyCheckPassed({ ok: true, http_code: 200, ip: '2001:db8::1' }), true)
})

test('IP validation accepts IPv6 compression and rejects invalid literals', () => {
  for (const ip of ['::1', '::ffff:192.0.2.1', '2001:db8:0:0:0:0:0:1']) assert.equal(isValidExitIP(ip), true)
  for (const ip of ['[::1]', '::g', '1::2::3', '192.168.000.1', null]) assert.equal(isValidExitIP(ip), false)
})
