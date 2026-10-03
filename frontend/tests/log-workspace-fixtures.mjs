// Synthetic operational examples only. Names and messages are representative;
// no exported account, production identity or credential is used in screenshots.
const base = { endpoint: 'responses', stream: true, input_tokens: 0, output_tokens: 0, cached_tokens: 0, proxy_name: '', ip: '' }
export const logWorkspacePreviewRows = [
  { ...base, time: 1789650210, status: 'ok', http_code: 200, account: 'team-production-01@example.test', key_name: '生产服务', model: 'gpt-5.6-sol', input_tokens: 16832, output_tokens: 924, cached_tokens: 12288, latency_ms: 7248 },
  { ...base, time: 1789650173, status: 'error', http_code: 400, account: 'developer-tooling-plus-account@example.test', key_name: '本地开发 · Codex CLI', model: 'gpt-5.6-luna', latency_ms: 327, error: 'Unsupported parameter: temperature. This endpoint does not accept the supplied sampling parameter.' },
  { ...base, time: 1789650102, status: 'error', http_code: 429, account: 'shared-pool-research-02@example.test', key_name: '研究工作流', model: 'gpt-6-astra', latency_ms: 1094, error: 'Rate limit reached for requests. Please try again later or review the account usage limits.' },
  { ...base, time: 1789650040, status: 'error', http_code: 200, account: 'team-production-03@example.test', key_name: '生产服务', model: 'gpt-5.6-sol', latency_ms: 641, error: 'Your request was flagged for possible policy violation. Please adjust the input and try again.' },
  { ...base, time: 1789649979, status: 'error', http_code: 502, account: 'integration-worker-account@example.test', key_name: 'CI 自动检查', model: 'gpt-5.5', latency_ms: 30752, error: 'Upstream connection terminated before a complete response was received.' },
  { ...base, time: 1789649946, status: 'ok', http_code: 200, account: 'developer-tooling-plus-account@example.test', key_name: '本地开发 · Codex CLI', model: 'gpt-5.6-luna', input_tokens: 3412, output_tokens: 156, cached_tokens: 0, latency_ms: 1268 },
  { ...base, time: 1789649910, status: 'error', http_code: 401, account: 'shared-pool-research-04@example.test', key_name: '研究工作流', model: 'gpt-5.6-terra', latency_ms: 188, error: 'Invalidated OAuth token. The token has been revoked and must be replaced with a new authorization.' },
  { ...base, time: 1789649852, status: 'aborted', http_code: 499, account: 'team-production-01@example.test', key_name: '生产服务', model: 'gpt-5.6-sol', latency_ms: 485, error: 'context canceled: client disconnected before the response completed' },
  { ...base, time: 1789649791, status: 'error', http_code: 400, account: 'integration-worker-account@example.test', key_name: 'CI 自动检查', model: 'gpt-5.4', latency_ms: 284, error: 'The requested model is not supported for this account. Please select an available model.' },
  { ...base, time: 1789649706, status: 'ok', http_code: 200, account: 'team-production-02@example.test', key_name: '会话摘要', model: 'gpt-5.6-luna', endpoint: 'compact', stream: false, input_tokens: 84210, output_tokens: 2048, cached_tokens: 65536, latency_ms: 9361 },
]
