// Cancellation saves work; the sequence check also protects against transports
// that resolve after an abort (including cached and mocked responses).
export function createLatestRequest() {
  let sequence = 0
  let controller
  return {
    async run(task, commit) {
      const current = ++sequence
      controller?.abort()
      controller = new AbortController()
      const signal = controller.signal
      try {
        const result = await task(signal)
        if (current !== sequence || signal.aborted) return false
        await commit(result)
        return true
      } catch (error) {
        if (current !== sequence || signal.aborted || error?.name === 'AbortError') return false
        throw error
      }
    },
    cancel() {
      sequence++
      controller?.abort()
    },
  }
}
