import { onBeforeUnmount, reactive, ref } from 'vue'
import { createLatestRequest } from './latestRequest.js'

// Page preferences stay in memory across tab navigation; credentials never enter this state.
export const managementFilters = reactive({
  groups: { search: '' },
  proxies: { search: '', type: '' },
  keys: { search: '', group: '' },
})
// Keep an uncertain rotation retryable across page switches. This is an operation
// identifier, never a credential; it is cleared after the response is confirmed.
export const managementRotationRequestId = ref('')

export function useManagementRead() {
  const loading = ref(false)
  const error = ref('')
  const request = createLatestRequest()
  let generation = 0
  let disposed = false
  async function read(task, commit) {
    if (disposed) return false
    const current = ++generation
    loading.value = true
    error.value = ''
    try {
      await request.run(task, commit)
    } catch (cause) {
      if (current === generation && cause?.name !== 'AbortError') error.value = cause?.message || '读取失败，请重试'
    } finally {
      if (current === generation) loading.value = false
    }
  }
  function cancel() { generation++; request.cancel(); loading.value = false }
  onBeforeUnmount(() => { disposed = true; cancel() })
  return { loading, error, read, cancel }
}

export function isValidExitIP(value) {
  if (typeof value !== 'string' || value !== value.trim() || !value) return false
  if (/^\d{1,3}(\.\d{1,3}){3}$/.test(value)) {
    return value.split('.').every((part) => Number(part) <= 255 && (part === '0' || !part.startsWith('0')))
  }
  if (!value.includes(':') || !/^[a-fA-F0-9:.]+$/.test(value)) return false
  try { return Boolean(new URL(`http://[${value}]/`).hostname) } catch { return false }
}

export function proxyCheckPassed(result) {
  const status = Number(result?.http_code)
  return result?.ok === true && status >= 200 && status < 300 && isValidExitIP(result.ip)
}
