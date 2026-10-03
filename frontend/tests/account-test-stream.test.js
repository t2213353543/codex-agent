import test from 'node:test'
import assert from 'node:assert/strict'
import { streamAccountTest } from '../src/utils/accountTestStream.js'
import { authGeneration, clearSession } from '../src/api.js'

const result = { id: 'a1', kind: 'stream', ok: true, model: 'test' }
const streaming = (chunks, options = {}) => new Response(new ReadableStream({ start(controller) { for (const value of chunks) controller.enqueue(new TextEncoder().encode(value)); controller.close() } }), { headers: { 'Content-Type': 'text/event-stream' }, ...options })
test('SSE parser preserves fragmented UTF-8, CRLF and a real completion event', async () => {
  const phases = [], text = []
  const response = streaming(['event: phase\r\ndata: {"phase":"streaming"}\r\n\r\nevent: del', 'ta\ndata: {"text":"合成"}\n\nevent: result\ndata: ' + JSON.stringify(result) + '\n\n'])
  assert.deepEqual(await streamAccountTest('a1', {}, { fetchImpl: async () => response, onPhase: value => phases.push(value), onDelta: value => text.push(value) }), result)
  assert.deepEqual(phases, ['streaming']); assert.deepEqual(text, ['合成'])
})
test('HTTP 200 without completion and a mismatched account cannot pass', async () => {
  await assert.rejects(streamAccountTest('a1', {}, { fetchImpl: async () => streaming(['event: delta\ndata: {"text":"partial"}\n\n']) }), /未收到明确/)
  await assert.rejects(streamAccountTest('a2', {}, { fetchImpl: async () => streaming(['event: result\ndata: ' + JSON.stringify(result) + '\n\n']) }), /不匹配/)
})
test('HTTP 200 failed terminal is retained and abort cannot become success', async () => {
  const failure = { ...result, ok: false, error: { code: 'failed' } }
  assert.equal((await streamAccountTest('a1', {}, { fetchImpl: async () => streaming(['event: result\ndata: ' + JSON.stringify(failure) + '\n\n']) })).ok, false)
  const controller = new AbortController(); controller.abort()
  await assert.rejects(streamAccountTest('a1', {}, { signal: controller.signal, fetchImpl: async () => streaming(['event: result\ndata: ' + JSON.stringify(result) + '\n\n']) }), { name: 'AbortError' })
})
test('late SSE 401 from an earlier login cannot clear a newer session', async () => {
  const initial = authGeneration
  await assert.rejects(streamAccountTest('a1', {}, { fetchImpl: async () => { clearSession(); return Response.json({ error: { message: 'stale login' } }, { status: 401 }) } }), { status: 401 })
  assert.equal(authGeneration, initial + 1)
})
