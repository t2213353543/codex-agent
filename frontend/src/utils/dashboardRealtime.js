import { createLatestRequest } from './latestRequest.js'

export function validDashboardRealtime(value) {
  if (!value || value.scope !== 'current_process' || value.window_seconds !== 60) return false
  const count = number => Number.isSafeInteger(number) && number >= 0
  return count(value.rpm) && (value.tpm === null || count(value.tpm)) && count(value.updated_at) && value.updated_at > 0 &&
    count(value.coverage_seconds) && value.coverage_seconds <= 60 && typeof value.warming_up === 'boolean' && typeof value.tokens_partial === 'boolean'
}

// Independent from the selected historical range. Sequence protection is still
// needed when fetch mocks/caches resolve after AbortController cancellation.
export function createDashboardRealtimePoller({ fetch, commit, error, loading = () => {}, setTimer = setTimeout, clearTimer = clearTimeout, interval = 5000 }) {
  const latest = createLatestRequest()
  let active = false, visible = true, timer, generation = 0
  function cancel() {
    generation++
    clearTimer(timer)
    timer = undefined
    latest.cancel()
    loading(false)
  }
  async function refresh() {
    if (!active || !visible) return
    clearTimer(timer)
    timer = undefined
    const current = ++generation
    loading(true)
    try {
      await latest.run(signal => fetch(signal), value => {
        if (!validDashboardRealtime(value)) throw new Error('实时吞吐数据无效')
        commit(value)
      })
    } catch (failure) {
      if (current === generation && active && visible) error(failure)
    } finally {
      if (current === generation && active && visible) {
        loading(false)
        timer = setTimer(refresh, interval)
      }
    }
  }
  return {
    start(isVisible = true) { if (active) return; active = true; visible = isVisible; if (visible) refresh() },
    stop() { active = false; cancel() },
    visibilityChanged(isVisible) {
      if (visible === isVisible) return
      visible = isVisible
      cancel()
      if (active && visible) refresh()
    },
    refresh,
  }
}
