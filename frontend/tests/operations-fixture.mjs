// Synthetic operations data for isolated UI tests only. Live metrics come from
// the backend's retained request records and runtime/pool counters.
export function operationsFixture(range = '1h', now = 1800000000) {
  const seconds = ({ '15m': 900, '1h': 3600, '6h': 21600, '24h': 86400 })[range] || 3600
  const empty = { requests: 0, success: 0, errors: 0, aborted: 0, tokens: 0, usage_known_requests: 0, latency_samples: 0, average_latency_ms: null, ttft_samples: 0, average_ttft_ms: null }
  const traffic = { requests: 100, success: 98, errors: 2, aborted: 1, tokens: 2400000, usage_known_requests: 97, latency_samples: 99, average_latency_ms: 2100, ttft_samples: 90, average_ttft_ms: 220 }
  return {
    checked_at: now, from: now + 1 - seconds, to: now + 1, step_seconds: seconds / 60, scope: 'retained_request_details', traffic,
    series: Array.from({ length: 60 }, (_, i) => ({ time: now + 1 - seconds + i * seconds / 60, ...(i === 59 ? traffic : empty) })),
    latency_histogram: [{ label: '≤ 1 秒', upper_ms: 1000, requests: 10 }, { label: '1–5 秒', upper_ms: 5000, requests: 89 }, { label: '> 5 秒', upper_ms: -1, requests: 0 }],
    error_categories: [{ name: 'rate_limit', requests: 1 }, { name: 'aborted', requests: 1 }],
    recent_errors: [{ id: 2, time: now, model: '<b>literal-model</b>', status: 'aborted', http_code: 499, category: 'aborted' }, { id: 1, time: now - 60, model: 'gpt-test', status: 'error', http_code: 429, category: 'rate_limit' }],
    runtime: { host: { supported: true, cpu_status: 'ok', cpu_percent: 23.5, cpu_sample_seconds: 30, memory_total_bytes: 16 * 1024 ** 3, memory_available_bytes: 12 * 1024 ** 3 }, scope: 'current_process', in_flight: 3, cpu_cores: 4, go_max_procs: 4, goroutines: 42, heap_bytes: 64 * 1024 ** 2, heap_in_use_bytes: 72 * 1024 ** 2, go_reserved_bytes: 128 * 1024 ** 2, stack_bytes: 2 * 1024 ** 2, gc_count: 10, last_gc_pause_ms: 0.2, pending_writes: 0, metrics_status: 'ok', read_failures: 0, postgres_pool: { total: 4, idle: 3, in_use: 1, max: 12, wait_count: 1, canceled_acquires: 0 }, redis_pool: { total: 3, idle: 2, hits: 30, misses: 3, timeouts: 0 } },
  }
}
