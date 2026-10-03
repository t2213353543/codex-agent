import { createHash } from 'node:crypto'
import { parseImportText } from '../src/utils/importAccounts.js'
import { operationsFixture } from './operations-fixture.mjs'
export const mockKey = 'mock-admin-key-for-tests'
export const htmlText = '<img src=x onerror="window.__injected=1"><b>原样日志文本</b>'
const now = Math.floor(Date.now() / 1000)
export function account(id, name, extra = {}) {
  return { id, name, version: 1, enabled: true, status: 'unverified', credential_status: 'unverified', expires_at: now + 3600, created_at: now - 86400, updated_at: now, notes: '隔离测试备注', tags: ['测试'], last_checked_at: 0, last_error: '', concurrency: 8, in_flight: 2, priority: 3, weight: 5, quota: 10000, quota_used: 1000, quota_percent: 10, window_days: 14, window_start: now - 86400, groups: ['g1'], proxies: ['p1'], has_refresh_token: true, usage: { requests: 10, ok: 8, errors: 2, tokens: 1000, input: 900, output: 100, cached: 800, avg_latency_ms: 100 }, ...extra }
}
export async function installMockApi(context) {
  const state = {
    authenticated: false, loginCount: 0, unexpected: [], requests: [], imported: [],
    accounts: [account('a1', '示例账号 Alpha'), account('a2', '很长的账号名称用于验证换行-'.repeat(5), { status: 'error', last_error: htmlText }), account('a3', 'Expired sample', { status: 'expired', expires_at: now - 60 })],
    groups: [{ id: 'g1', name: '测试分组', description: '仅合成数据', accounts: 3, keys: 1, created_at: now, version: 1 }],
    proxies: [{ id: 'p1', name: '测试代理', type: 'http', host: '127.0.0.1', port: 8080, username: 'demo', has_password: true, enabled: true, accounts: 3, version: 1 }],
  }
  await context.route('**/*', async route => {
    const request = route.request()
    const url = new URL(request.url())
    if (!['127.0.0.1', 'localhost'].includes(url.hostname)) { state.unexpected.push(request.url()); await route.abort(); return }
    if (!url.pathname.startsWith('/api/')) { await route.continue(); return }
    const path = url.pathname.slice(4)
    const method = request.method()
    const body = request.postDataJSON()
    state.requests.push({ path, method, params: Object.fromEntries(url.searchParams), body })
    const reply = async (json, status = 200) => { try { await route.fulfill({ status, json }) } catch { /* A canceled fetch may already have closed. */ } }
    if (path === '/session') {
      if (method === 'POST') { state.loginCount++; state.authenticated = body?.admin_key === mockKey; return reply(state.authenticated ? { authenticated: true, expires_at: now + 1800 } : { error: { message: '管理密钥无效' } }, state.authenticated ? 200 : 401) }
      if (method === 'DELETE') { state.authenticated = false; return reply({ ok: true }) }
      return reply({ authenticated: state.authenticated, expires_at: now + 1800 }, state.authenticated ? 200 : 401)
    }
    if (!state.authenticated) return reply({ error: { message: '会话已过期' } }, 401)
    if (path === '/settings' && method === 'GET') return reply({ admin_key: '', admin_key_masked: 'sk-••••1234', admin_api: false, settings_version: 1, version: '0.2.0-mock', uptime_seconds: 3600 })
    if (path === '/operations' && method === 'GET') return reply(operationsFixture(url.searchParams.get('range'), now))
    if (path === '/overview/realtime') return reply({ scope: 'current_process', window_seconds: 60, rpm: 12, tpm: 3456, updated_at: now, coverage_seconds: 60, warming_up: false, tokens_partial: false, completed_requests: 10, usage_known_requests: 10, usage_unknown_requests: 0 })
    if (path === '/overview') return reply({
      cards: { accounts: 3, accounts_enabled: 3, groups: 1, keys: 2, keys_enabled: 1, requests: 10, success: 8, tokens: 1000, errors: 2, input: 1000, cached: 800, cache_rate: 0.8, cache_rate_has_samples: true, requests_today: 5, tokens_today: 500, tokens_total: 9000, tokens_retained: 7000, avg_latency_ms: 100, avg_latency_today: 90, rpm: 2, tpm: 200, peak_rpm: 5, peak_tpm: 400 },
      prev_cards: { requests: 5, tokens: 500, errors: 1 }, timezone: 'UTC', retention_days: 92, totals_scope: { tokens_total: 'lifetime' },
      series: [0, 1, 2, 3].map(i => ({ ts: now - (4 - i) * 600, requests: i + 1, tokens: 100 + i * 20, errors: i % 2, input: 100, output: 20, cached: 80 })),
      account_top: [{ id: 'a1', name: '示例账号 Alpha', requests: 10, errors: 2, tokens: 1000 }], status: [{ name: '成功', value: 8 }, { name: '失败', value: 2 }], recent_errors: [{ time: now, account: '示例账号 Alpha', error: htmlText }],
    })
    if (path === '/accounts/manage' && method === 'GET') {
      const search = url.searchParams.get('search') || ''
      const rows = state.accounts.filter(row => (!search || row.name.toLowerCase().includes(search.toLowerCase())) && (!url.searchParams.get('status') || row.status === url.searchParams.get('status')))
      const page = Math.max(1, Number(url.searchParams.get('page')) || 1), pageSize = Number(url.searchParams.get('page_size')) || 20
      return reply({ accounts: rows.slice((page - 1) * pageSize, page * pageSize), total: rows.length, total_kind: 'exact', page, page_size: pageSize, has_more: page * pageSize < rows.length })
    }
    if (path === '/accounts' && method === 'GET') {
      const search = url.searchParams.get('search') || ''
      if (search === 'slow') await new Promise(resolve => setTimeout(resolve, 350))
      if (['slow', 'fast'].includes(search)) return reply({ accounts: [account(search, `${search} result`)], has_more: false, next_cursor: '', total_kind: 'not_computed' })
      const rows = state.accounts.filter(a => (!search || a.name.toLowerCase().includes(search.toLowerCase())) && (!url.searchParams.get('status') || a.status === url.searchParams.get('status')))
      const offset = url.searchParams.get('cursor') ? 2 : 0
      const page = rows.slice(offset, offset + 2)
      return reply({ accounts: page, has_more: rows.length > offset + 2, next_cursor: rows.length > offset + 2 ? 'mock-cursor-page-2' : '', total_kind: 'not_computed' })
    }
    if (path === '/accounts/import/preview' || path === '/accounts' && method === 'POST') {
      const preview = path.endsWith('/preview')
      const rows = parseImportText(body.raw)
      const seen = new Set()
      const results = rows.map((row, index) => {
        const token = row.credentials?.access_token
        const name = row.name || (token ? `account-${createHash('sha256').update(token).digest('hex').slice(0, 24)}` : '—')
        const exists = state.accounts.find(a => a.name === name)
        let status = row.kind === 'failed' ? 'failed' : exists ? (body.replace ? 'replaced' : 'failed') : seen.has(token) ? 'duplicate' : 'added'
        if (token) seen.add(token)
        if (!preview && ['added', 'replaced'].includes(status)) {
          if (exists) exists.version++
          else state.accounts.push(account(`imported-${state.accounts.length}`, name))
        }
        return { index: index + 1, line: row.line, name, status, ...(status === 'failed' ? { message: exists ? '同名账号已存在' : row.message, code: exists ? 'name_conflict' : 'invalid_credentials' } : {}), id: exists?.id || `preview-${index}` }
      })
      const counts = Object.fromEntries(['added', 'duplicate', 'replaced', 'failed'].map(s => [s, results.filter(r => r.status === s).length]))
      if (!preview) state.imported.push(body)
      return reply({ ...counts, imported: counts.added + counts.replaced, results, dry_run: preview })
    }
    if (path === '/accounts/upstream/batch') return reply({ results: body.items.map(item => ({ id: item.id, ok: true, snapshot: { id: item.id, kind: body.kind, ok: true, checked_at: now, source: 'codex_usage', data: { plan_type: 'team', rate_limit: null } } })), succeeded: body.items.length, failed: 0 })
    if (path === '/accounts/batch') return reply({ results: body.items.map(item => ({ id: item.id, ok: item.id !== 'a2', version: 2, ...(item.id === 'a2' ? { error: '模拟版本冲突', code: 'version_conflict' } : {}) })), succeeded: body.items.filter(i => i.id !== 'a2').length, failed: body.items.filter(i => i.id === 'a2').length })
    if (path === '/accounts/refresh') return reply({ ok: true, id: body.id, version: 2, expires_at: now + 7200 })
    if (/^\/accounts\/[^/]+\/requests$/.test(path)) return reply({ items: [{ time: now, status: 'error', model: htmlText, error: htmlText, proxy_name: htmlText, ip: '127.0.0.1', input_tokens: 10, output_tokens: 2, cached_tokens: 8, latency_ms: 100 }], has_more: false })
    if (/^\/accounts\/[^/]+$/.test(path)) {
      const entry = state.accounts.find(a => a.id === decodeURIComponent(path.split('/')[2]))
      if (method === 'GET') return reply({ account: entry || state.accounts[0] })
      if (method === 'PUT') { if (entry) Object.assign(entry, body, { version: entry.version + 1 }); return reply({ ok: true, id: entry?.id, version: entry?.version }) }
      if (method === 'DELETE') { state.accounts = state.accounts.filter(a => a !== entry); return reply({ ok: true }) }
    }
    if (path === '/groups' && method === 'GET') return reply({ groups: state.groups })
    if (path === '/proxies' && method === 'GET') return reply({ proxies: state.proxies })
    if (path === '/keys' && method === 'GET') return reply({ keys: [{ id: 'k1', name: '合成测试 KEY', key_masked: 'sk-••••demo', version: 1, enabled: true, groups: ['g1'], created_at: now, requests: 0 }] })
    if (path === '/logs/snapshot') return reply({ as_of: 'shared-mock-snapshot', created_at: now, expires_at: Math.floor(Date.now() / 1000) + 600, scope: 'retained_request_details' })
    if (path === '/logs/stats') return reply({ scope: 'retained_request_details', checked_at: now, total_requests: 1, success: 0, errors: 1, aborted: 0, total_input_tokens: 10, total_output_tokens: 2, total_cached_tokens: 8, total_tokens: 12, average_latency_ms: 100, usage_known_requests: 1, usage_unknown_requests: 0, series: [] })
    if (path === '/logs/coverage') return reply({ scope: 'retained_request_details', detail_count: 1, first_record_at: now, last_record_at: now, checked_at: now, retention_policy: 'explicit_pruning_and_cleanup', storage: 'postgres' })
    if (path === '/logs') {
      const value = url.searchParams.get('account') || '示例账号 Alpha'
      if (value === 'slow') await new Promise(resolve => setTimeout(resolve, 350))
      return reply({ items: [{ id: 'log1', time: now, account: value, account_id: 'a1', model: htmlText, error: htmlText, proxy_name: '测试代理', ip: '192.0.2.1', key_name: '合成测试 KEY', endpoint: 'responses', status: 'error', input_tokens: 10, output_tokens: 2, cached_tokens: 8, latency_ms: 100 }], total: -1, total_kind: 'not_computed', has_more: !url.searchParams.has('cursor'), next_cursor: url.searchParams.has('cursor') ? '' : 'mock-log-next', page_size: 20 })
    }
    state.unexpected.push(`${method} ${path}`)
    return reply({ error: { message: `Unmocked endpoint: ${method} ${path}` } }, 501)
  })
  return state
}
