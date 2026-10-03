import { authGeneration, clearSession } from '../api.js'

export async function streamAccountTest(id, body, { signal, onPhase = () => {}, onDelta = () => {}, fetchImpl = fetch } = {}) {
  const generation = authGeneration
  if (signal?.aborted) throw new DOMException('Test aborted', 'AbortError')
  const response = await fetchImpl(`/api/accounts/${encodeURIComponent(id)}/test-stream`, { method: 'POST', credentials: 'same-origin', cache: 'no-store', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), signal })
  if (signal?.aborted) throw new DOMException('Test aborted', 'AbortError')
  if (response.status === 401 && generation === authGeneration) {
    clearSession()
    if (typeof window !== 'undefined') window.dispatchEvent(new CustomEvent('auth-expired'))
  }
  if (!response.ok) {
    let payload
    try { payload = await response.json() } catch { /* The status remains useful for non-JSON upstream errors. */ }
    throw Object.assign(new Error(payload?.error?.message || `测试请求失败（HTTP ${response.status}）`), { status: response.status })
  }
  if (!/text\/event-stream/i.test(response.headers.get('content-type') || '')) throw new Error('服务器未返回测试事件流')
  const reader = response.body?.getReader()
  if (!reader) throw new Error('浏览器无法读取测试事件流')
  const decoder = new TextDecoder()
  let buffer = '', eventName = '', data = [], result
  const consume = line => {
    if (line === '') {
      if (data.length) {
        let event
        try { event = JSON.parse(data.join('\n')) } catch { throw new Error('测试事件格式无效') }
        if (eventName === 'phase') onPhase(event.phase)
        else if (eventName === 'delta' && typeof event.text === 'string') onDelta(event.text)
        else if (eventName === 'result') {
          if (event.id !== id || typeof event.ok !== 'boolean' || event.kind !== 'stream') throw new Error('测试终态与当前账号不匹配')
          result = event
        }
      }
      data = []; eventName = ''
    } else if (line.startsWith('event:')) eventName = line.slice(6).trim()
    else if (line.startsWith('data:')) data.push(line.slice(5).replace(/^ /, ''))
  }
  try {
    while (true) {
      if (signal?.aborted) throw new DOMException('Test aborted', 'AbortError')
      const { done, value } = await reader.read()
      buffer += done ? decoder.decode() : decoder.decode(value, { stream: true })
      if (buffer.length > 1024 * 1024) throw new Error('测试事件超过大小限制')
      let newline
      while ((newline = buffer.indexOf('\n')) >= 0) { const line = buffer.slice(0, newline).replace(/\r$/, ''); buffer = buffer.slice(newline + 1); consume(line) }
      if (done) break
    }
    if (buffer.trim()) consume(buffer.replace(/\r$/, ''))
    if (data.length) consume('')
    if (signal?.aborted) throw new DOMException('Test aborted', 'AbortError')
    if (!result) throw new Error('测试流已中断，未收到明确完成结果')
    return result
  } catch (error) { await reader.cancel().catch(() => {}); throw error }
  finally { reader.releaseLock() }
}
